// main_nonhttp.go — the non-HTTP dedicated-listener builder extracted from
// main() (sftp-ftp-2026-09 leaf 01 Contract A): NonHTTPFrontend entries get
// a raw net.Listener + Serve goroutine instead of an http.Server, tracked
// alongside the HTTP dedicated listeners for the same graceful-shutdown fan.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/bhodgens/zeta-object/internal/frontend"
)

// nonHTTPServer pairs a NonHTTPFrontend with its already-bound listener.
type nonHTTPServer struct {
	nh   frontend.NonHTTPFrontend
	l    net.Listener
	addr string
}

// buildDedicatedListeners splits the dedicated-listener specs into
// http.Server entries (default cert/key pair) and non-HTTP frontends with
// their bound net.Listeners. A bind failure aborts startup loudly (same
// semantic as the default listener's ListenAndServeTLS failure).
func buildDedicatedListeners(listeners []listenerSpec) ([]*http.Server, []nonHTTPServer) {
	var extra []*http.Server
	var nonHTTP []nonHTTPServer
	for _, ls := range listeners {
		if nh, ok := ls.frontend.(frontend.NonHTTPFrontend); ok {
			l, err := net.Listen("tcp", ls.addr)
			if err != nil {
				log.Fatalf("Non-HTTP frontend listener %s failed to bind: %v", ls.addr, err)
			}
			nonHTTP = append(nonHTTP, nonHTTPServer{nh: nh, l: l, addr: ls.addr})
			continue
		}
		srv := newServer(ls.addr, ls.frontend.Handler(),
			serverConfig.CertFile, serverConfig.KeyFile)
		if ls.tlsConfig != nil {
			// A TLSListenerFrontend supplies its own pre-built listener TLS
			// configuration (client-certificate verification); main serves
			// this server with it (main_server.go serveDedicatedListener)
			// instead of the process-wide cert pair.
			srv.TLSConfig = ls.tlsConfig
		}
		extra = append(extra, srv)
	}
	return extra, nonHTTP
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
