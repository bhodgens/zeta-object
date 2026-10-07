// main_nonhttp.go — the non-HTTP dedicated-listener builder extracted from
// main() (sftp-ftp-2026-09 leaf 01 Contract A): NonHTTPFrontend entries get
// a raw net.Listener + Serve goroutine instead of an http.Server, tracked
// alongside the HTTP dedicated listeners for the same graceful-shutdown fan.
// quic-h3-2026-10 leaf 02: QUICListenerFrontend specs get a UDP packet conn
// + quic-go HTTP/3 server (never the TCP ListenAndServeTLS path), tracked in
// the same graceful-stop fan.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/frontend"
	h3 "github.com/bhodgens/zeta-object/internal/frontend/h3"
)

// nonHTTPServer pairs a NonHTTPFrontend with its already-bound listener.
type nonHTTPServer struct {
	nh   frontend.NonHTTPFrontend
	l    net.Listener
	addr string
}

// quicServer pairs an h3 HTTP/3 server with the frontend that owns it
// (quic-h3-2026-10 leaf 02): started via Serve, drained via Close in the
// same graceful-stop fan as the TCP listeners.
type quicServer struct {
	frontend frontend.QUICListenerFrontend
	srv      *h3.Server
	addr     string
}

// buildDedicatedListeners splits the dedicated-listener specs into
// http.Server entries (default cert/key pair), non-HTTP frontends with
// their bound net.Listeners, and QUIC/HTTP-3 servers on UDP. A bind failure
// aborts startup loudly (same semantic as the default listener's
// ListenAndServeTLS failure).
func buildDedicatedListeners(listeners []listenerSpec) ([]*http.Server, []nonHTTPServer, []quicServer) {
	var extra []*http.Server
	var nonHTTP []nonHTTPServer
	var quic []quicServer
	for _, ls := range listeners {
		// A QUICListenerFrontend is served on UDP via quic-go with its
		// pre-built TLS config. NEVER through the TCP ListenAndServeTLS
		// path (quic-h3-2026-10 leaf 02 Contract 2).
		if q, ok := ls.frontend.(frontend.QUICListenerFrontend); ok {
			w, err := h3Listen(q)
			if err != nil {
				quitQUIC("QUIC frontend %s listener on %s failed to bind: %v", q.Name(), ls.addr, err)
			}
			quic = append(quic, quicServer{frontend: q, srv: w, addr: ls.addr})
			continue
		}
		if nh, ok := ls.frontend.(frontend.NonHTTPFrontend); ok {
			l, err := net.Listen("tcp", ls.addr)
			if err != nil {
				log.Fatalf("Non-HTTP frontend listener %s failed to bind: %v", ls.addr, err)
			}
			nonHTTP = append(nonHTTP, nonHTTPServer{nh: nh, l: l, addr: ls.addr})
			continue
		}
		srv := newServer(ls.addr, ls.frontend.Handler(),
			serverConfig().CertFile, serverConfig().KeyFile)
		if ls.tlsConfig != nil {
			// A TLSListenerFrontend supplies its own pre-built listener TLS
			// configuration (client-certificate verification); main serves
			// this server with it (main_server.go serveDedicatedListener)
			// instead of the process-wide cert pair.
			srv.TLSConfig = ls.tlsConfig
		}
		extra = append(extra, srv)
	}
	return extra, nonHTTP, quic
}

// quitQUIC is the process-exit seam for a QUIC bind failure (main calls
// log.Fatalf; tests swap it so the loud-failure path is assertable without
// killing the test binary).
var quitQUIC = log.Fatalf

// h3Listen opens the UDP/QUIC listener for one QUICListenerFrontend. The
// concrete type is the h3 package's frontend; the seam interface keeps the
// buildDedicatedListeners table honest (a foreign QUICListenerFrontend is a
// construction error, loud at startup).
func h3Listen(q frontend.QUICListenerFrontend) (*h3.Server, error) {
	hf, ok := q.(*h3.Frontend)
	if !ok {
		return nil, fmt.Errorf("frontend %q implements QUICListenerFrontend but is not the h3 frontend (%T) — no QUIC serving path exists for it", q.Name(), q)
	}
	return hf.Listen()
}

// startNonHTTPFrontend launches one NonHTTPFrontend's Serve goroutine.
// Serve returns on Stop() or listener close; only an unexpected error
// surfaces (mirrors bughunt C5 semantics for the HTTP listeners).
func startNonHTTPFrontend(s nonHTTPServer, serverErr chan error) {
	go func() {
		log.Printf("Starting non-HTTP frontend %s on %s (plain TCP)", s.nh.Name(), s.addr)
		if err := s.nh.Serve(s.l); err != nil {
			serverErr <- fmt.Errorf("non-HTTP frontend %s on %s: %w", s.nh.Name(), s.addr, err)
		}
	}()
}

// startQUICFrontend launches one HTTP/3 server's Serve goroutine
// (quic-h3-2026-10 leaf 02). ServeListener returns http.ErrServerClosed on
// graceful close; only an unexpected error surfaces (bughunt C5 semantics).
func startQUICFrontend(s quicServer, serverErr chan error) {
	go func() {
		log.Printf("Starting HTTP/3 frontend %s on %s (QUIC, UDP)", s.frontend.Name(), s.addr)
		if err := s.srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("HTTP/3 frontend %s on %s: %w", s.frontend.Name(), s.addr, err)
		}
	}()
}

// drainNonHTTPFrontends stops every non-HTTP frontend in the shared
// graceful-shutdown fan (Stop: stop accepting, close sessions).
func drainNonHTTPFrontends(servers []nonHTTPServer, wg *sync.WaitGroup) {
	for _, s := range servers {
		wg.Add(1)
		go func(s nonHTTPServer) {
			defer wg.Done()
			if err := s.nh.Stop(); err != nil {
				log.Printf("Non-HTTP frontend %s shutdown failed: %v", s.nh.Name(), err)
			}
		}(s)
	}
}

// drainQUICFrontends stops every HTTP/3 server in the same
// graceful-shutdown fan using its OWN bounded drain budget. Callers that
// already hold the process-wide shutdown budget (runServer) call
// drainQUICFrontendsWithContext instead, so every listener in the fan shares
// one budget; this form exists for callers without one (tests, cleanups).
//
// Within the budget each server sends GOAWAY, lets in-flight responses
// finish, and only then releases the QUIC listener and UDP socket
// (quic-h3-2026-10 leaf 02: the harness must not leak UDP fds).
func drainQUICFrontends(servers []quicServer, wg *sync.WaitGroup) {
	ctx, cancel := context.WithTimeout(context.Background(), quicDrainTimeout)
	defer cancel()
	drainQUICFrontendsWithContext(ctx, servers, wg)
}

// quicDrainTimeout bounds drainQUICFrontends when no caller budget is
// supplied. It matches the process-wide serverShutdownTimeout default so a
// no-budget drain can never out-wait systemd's TimeoutStopSec.
const quicDrainTimeout = 30 * time.Second

// drainQUICFrontendsWithContext stops every HTTP/3 server in the same
// graceful-shutdown fan within the caller's SHARED drain budget (main's
// drainCtx): GOAWAY to connected clients, in-flight responses finish, then
// the QUIC listener and UDP socket are released.
//
// The caller's budget is used on purpose: a per-server
// context.Background() (the pre-fix shape) let a single client keep its
// connection alive indefinitely, so wg.Wait() in runServer never returned and
// systemd SIGKILLed the process mid-write — after in-flight s3 writes had
// already drained cleanly. One shared budget also keeps this fan consistent
// with the TCP listeners, which all receive the same drainCtx.
func drainQUICFrontendsWithContext(ctx context.Context, servers []quicServer, wg *sync.WaitGroup) {
	for _, s := range servers {
		wg.Add(1)
		go func(s quicServer) {
			defer wg.Done()
			if err := s.srv.Shutdown(ctx); err != nil {
				// A spent drain budget is expected on a slow drain, not a
				// crash: log it and keep tearing the rest down.
				log.Printf("HTTP/3 frontend %s shutdown: %v", s.frontend.Name(), err)
			}
		}(s)
	}
}
