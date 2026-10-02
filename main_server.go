package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// runServer owns main's listener/shutdown half: start the default and
// dedicated listeners, the SIGHUP reload loop, and the concurrent
// graceful-drain fan on SIGINT/SIGTERM. Extracted from main so each
// function stays under the gocyclo gate; behavior is byte-identical to
// the pre-split inline code (same ordering, same logging, same error
// semantics — bughunt C5/C6 invariants preserved).
func runServer(srv *http.Server, extraServers []*http.Server, nonHTTPServers []nonHTTPServer) {
	// Graceful shutdown: SIGINT/SIGTERM stop accepting new connections and
	// drain in-flight requests within serverShutdownTimeout (default
	// listener first, then every dedicated-listener server).
	shutdownCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1+len(extraServers))
	go func() {
		log.Printf("Starting S3 server on %s (HTTPS, cert=%s, key=%s)",
			srv.Addr, serverConfig.CertFile, serverConfig.KeyFile)
		serverErr <- srv.ListenAndServeTLS(serverConfig.CertFile, serverConfig.KeyFile)
	}()

	// Design-leaf 08 (key rotation/revocation): SIGHUP reloads the auth
	// identity registry — re-running the exact startup build against the
	// same config file and swapping it in atomically. Fail-closed: a bad
	// edit (parse error, duplicate access key, ...) keeps the OLD registry
	// serving and logs the validator's named-offender error. Signals are
	// handled serially in this loop, so a reload in flight cannot race the
	// next one. SIGHUP is POSIX: on Windows the channel simply never
	// fires (no behavior regression vs. the pre-leaf restart-only flow).
	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		for range sighupCh {
			started := time.Now()
			log.Printf("SIGHUP received: reloading auth identities from %s", serverConfigPath)
			if err := reloadIdentityRegistry(); err != nil {
				continue // already logged fail-closed; keep serving
			}
			log.Printf("SIGHUP auth reload complete in %s", time.Since(started).Round(time.Microsecond))
		}
	}()
	for _, es := range extraServers {
		go func() {
			log.Printf("Starting dedicated frontend listener on %s (HTTPS)", es.Addr)
			// A dedicated listener that fails to bind must surface like
			// the default one: swallow only the intentional
			// ErrServerClosed from graceful shutdown (bughunt C5).
			if err := es.ListenAndServeTLS(serverConfig.CertFile, serverConfig.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		}()
	}
	for _, nhs := range nonHTTPServers {
		startNonHTTPFrontend(nhs, serverErr)
	}

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ListenAndServeTLS failed: %v. Please ensure %s and %s are correctly generated and in place.",
				err, serverConfig.CertFile, serverConfig.KeyFile)
		}
	case <-shutdownCtx.Done():
		log.Println("Shutdown signal received, draining in-flight requests...")
		drainCtx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		// Initiate Shutdown on ALL servers concurrently so they share the
		// drain budget (a sequential drain lets late listeners keep
		// accepting until the window is spent — bughunt C6).
		servers := append([]*http.Server{srv}, extraServers...)
		var wg sync.WaitGroup
		errs := make([]error, len(servers))
		for i, s := range servers {
			wg.Add(1)
			go func(i int, s *http.Server) {
				defer wg.Done()
				errs[i] = s.Shutdown(drainCtx)
			}(i, s)
		}
		// Non-HTTP frontends drain through their own Stop (graceful:
		// stop accepting, close sessions) in the same concurrent fan.
		drainNonHTTPFrontends(nonHTTPServers, &wg)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				log.Printf("Server %s shutdown failed (forcing close): %v", servers[i].Addr, err)
			}
		}
		if errs[0] == nil {
			log.Println("Server shut down cleanly")
		}
	}
}
