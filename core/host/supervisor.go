// Copyright 2017 Gerasimos Maropoulos, ΓΜ. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package host

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"

	"github.com/kataras/iris/core/errors"
	"github.com/kataras/iris/core/nettools"
	"golang.org/x/crypto/acme/autocert"
)

// Supervisor is the wrapper and the manager for a compatible server
// and it's relative actions, called Tasks.
//
// Interfaces are separated to return relative functionality to them.
type Supervisor struct {
	Scheduler
	server         *http.Server
	closedManually int32 // future use, accessed atomically (non-zero means we've called the Shutdown)

	shouldWait   int32 // non-zero means that the host should wait for unblocking
	unblockChan  chan struct{}
	shutdownChan chan struct{}
	errChan      chan error

	mu sync.Mutex
}

func New(srv *http.Server) *Supervisor {
	return &Supervisor{
		server:       srv,
		unblockChan:  make(chan struct{}, 1),
		shutdownChan: make(chan struct{}),
		errChan:      make(chan error),
	}
}

func (su *Supervisor) DeferFlow() {
	atomic.StoreInt32(&su.shouldWait, 1)
}

func (su *Supervisor) RestoreFlow() {
	if su.isWaiting() {
		atomic.StoreInt32(&su.shouldWait, 0)
		su.mu.Lock()
		su.unblockChan <- struct{}{}
		su.mu.Unlock()
	}
}

func (su *Supervisor) isWaiting() bool {
	return atomic.LoadInt32(&su.shouldWait) != 0
}

// Done is being received when in server Shutdown.
// This can be used to gracefully shutdown connections that have
// undergone NPN/ALPN protocol upgrade or that have been hijacked.
// This function should start protocol-specific graceful shutdown,
// but should not wait for shutdown to complete.
func (su *Supervisor) Done() <-chan struct{} {
	return su.shutdownChan
}

// Err refences to the return value of Server's .Serve, not the server's specific error logger.
func (su *Supervisor) Err() <-chan error {
	return su.errChan
}

func (su *Supervisor) notifyShutdown() {
	go func() {
		su.shutdownChan <- struct{}{}
	}()

	su.Scheduler.notifyShutdown()
}

func (su *Supervisor) notifyErr(err error) {
	// if err == http.ErrServerClosed {
	// 	return
	// }

	go func() {
		su.errChan <- err
	}()

	su.Scheduler.notifyErr(err)
}

/// TODO:
// Remove all channels, do it with events
// or with channels but with a different channel on each task proc
// I don't know channels are not so safe, when go func and race risk..
// so better with callbacks....
func (su *Supervisor) supervise(blockFunc func() error) error {
	// println("Running Serve from Supervisor")

	// su.server: in order to Serve and Shutdown the underline server and no re-run the supervisors when .Shutdown -> .Serve.
	// su.GetBlocker: set the Block() and Unblock(), which are checked after a shutdown or error.
	// su.GetNotifier: only one supervisor is allowed to be notified about Close/Shutdown and Err.
	// su.log: set this builder's logger in order to supervisor to be able to share a common logger.
	host := createTaskHost(su)
	// run the list of supervisors in different go-tasks by-design.
	su.Scheduler.runOnServe(host)

	if len(su.Scheduler.onInterruptTasks) > 0 {
		// this can't be moved to the task interrupt's `Run` function
		// because it will not catch more than one ctrl/cmd+c, so
		// we do it here. These tasks are canceled already too.
		go func() {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, os.Interrupt, os.Kill)
			select {
			case <-ch:
				su.Scheduler.runOnInterrupt(host)
			}
		}()
	}

	err := blockFunc()
	su.notifyErr(err)

	if su.isWaiting() {
	blockStatement:
		for {
			select {
			case <-su.unblockChan:
				break blockStatement
			}
		}
	}

	return err // start the server
}

func (su *Supervisor) newListener() (net.Listener, error) {
	// this will not work on "unix" as network
	// because UNIX doesn't supports the kind of
	// restarts we may want for the server.
	//
	// User still be able to call .Serve instead.
	l, err := nettools.TCPKeepAlive(su.server.Addr)
	if err != nil {
		return nil, err
	}

	if nettools.IsTLS(su.server) {
		// means tls
		tlsl := tls.NewListener(l, su.server.TLSConfig)
		return tlsl, nil
	}

	return l, nil
}

func (su *Supervisor) Serve(l net.Listener) error {
	return su.supervise(func() error { return su.server.Serve(l) })
}

func (su *Supervisor) ListenAndServe() error {
	l, err := su.newListener()
	if err != nil {
		return err
	}
	return su.Serve(l)
}

func setupHTTP2(cfg *tls.Config) {
	cfg.NextProtos = append(cfg.NextProtos, "h2") // HTTP2
}

func (su *Supervisor) ListenAndServeTLS(certFile string, keyFile string) error {
	if certFile == "" || keyFile == "" {
		return errors.New("certFile or keyFile missing")
	}
	cfg := new(tls.Config)
	var err error
	cfg.Certificates = make([]tls.Certificate, 1)
	if cfg.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return err
	}

	setupHTTP2(cfg)
	su.server.TLSConfig = cfg

	return su.ListenAndServe()
}

func (su *Supervisor) ListenAndServeAutoTLS() error {
	autoTLSManager := autocert.Manager{
		Prompt: autocert.AcceptTOS,
	}

	cfg := new(tls.Config)
	cfg.GetCertificate = autoTLSManager.GetCertificate
	setupHTTP2(cfg)
	su.server.TLSConfig = cfg
	return su.ListenAndServe()
}

// Shutdown gracefully shuts down the server without interrupting any
// active connections. Shutdown works by first closing all open
// listeners, then closing all idle connections, and then waiting
// indefinitely for connections to return to idle and then shut down.
// If the provided context expires before the shutdown is complete,
// then the context's error is returned.
//
// Shutdown does not attempt to close nor wait for hijacked
// connections such as WebSockets. The caller of Shutdown should
// separately notify such long-lived connections of shutdown and wait
// for them to close, if desired.
func (su *Supervisor) Shutdown(ctx context.Context) error {
	// println("Running Shutdown from Supervisor")

	atomic.AddInt32(&su.closedManually, 1) // future-use
	su.notifyShutdown()
	return su.server.Shutdown(ctx)
}