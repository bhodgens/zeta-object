// altsvc.go — the Alt-Svc advertisement middleware (quic-h3-2026-10 leaf 02
// Task 3, RFC 7838). When any mounted frontend implements
// QUICListenerFrontend, main wraps every HTTP-serving frontend's handler so
// each response carries `alt-svc: h3="<port>"; persist=1`, pointing
// h3-capable clients at the UDP endpoint; they migrate automatically and
// fall back to TCP when UDP is blocked. Purely additive response headers —
// no frontend code changes. The h3 frontend's own responses never carry the
// header (its handler is never wrapped).
package main

import (
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/frontend"
)

// altSvcHeaderValue builds the advertisement value from the h3 frontend's
// UDP address: `h3="<port>"; persist=1`. The port is extracted from Addr();
// a ":port" form (no host) is the common config shape and works directly.
// PINNED shape (append-with-comma on collision, persist=1): both the value
// format and the merge rule are pinned by altsvc_test.go.
func altSvcHeaderValue(h3 frontend.QUICListenerFrontend) (string, error) {
	_, portStr, err := net.SplitHostPort(h3.Addr())
	if err != nil {
		return "", fmt.Errorf("extracting Alt-Svc port from frontend %q addr %q: %w", h3.Name(), h3.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("frontend %q addr %q has no usable UDP port for Alt-Svc", h3.Name(), h3.Addr())
	}
	return fmt.Sprintf("h3=\":%d\"; persist=1", port), nil
}

// findQUICFrontend returns the first mounted frontend implementing
// QUICListenerFrontend, or nil when none is mounted (no advertisement).
func findQUICFrontend(mounts []frontendMount) frontend.QUICListenerFrontend {
	for _, m := range mounts {
		if q, ok := m.frontend.(frontend.QUICListenerFrontend); ok {
			return q
		}
	}
	return nil
}

// wrapWithAltSvc wraps handler so every response carries the Alt-Svc
// advertisement. An existing Alt-Svc value is PRESERVED and appended to
// with a comma (pinned: append-with-comma, never clobbered). The header is
// injected at WriteHeader time so a wrapped handler that (defensively) set
// its own Alt-Svc value is merged, not silently overridden.
func wrapWithAltSvc(handler http.Handler, value string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", value) // default; overwritten at WriteHeader if the handler set one
		handler.ServeHTTP(&altSvcWriter{ResponseWriter: w, value: value}, r)
	})
}

// altSvcWriter merges the advertisement into the final header set at
// WriteHeader time (append-with-comma when the handler already set a value).
type altSvcWriter struct {
	http.ResponseWriter
	value    string
	wroteHdr bool
}

func (a *altSvcWriter) WriteHeader(code int) {
	if a.wroteHdr {
		return
	}
	a.wroteHdr = true
	if prev := a.Header().Get("Alt-Svc"); prev != "" && prev != a.value {
		a.Header().Set("Alt-Svc", prev+", "+a.value)
	}
	a.ResponseWriter.WriteHeader(code)
}

// applyAltSvcAdvertisement is the main()-side wiring (called from runServer's
// caller after buildDedicatedListeners): when any mount is a
// QUICListenerFrontend, every HTTP-serving frontend's handler — the shared
// mux AND every dedicated-listener http.Server — is wrapped with the Alt-Svc
// advertisement. The QUIC frontend's own handler is never wrapped. When no
// QUIC frontend is mounted, nothing changes.
// Returns the mux to serve: the shared-mux re-wrap REBUILDS the mux (a
// ServeMux cannot re-register "/" in place - duplicate-pattern panic - and
// cannot be struct-copied - it carries a mutex), so the caller must serve
// the returned value. Call BEFORE any listener starts (startup ordering in
// main()).
func applyAltSvcAdvertisement(mux *http.ServeMux, shared []frontend.Frontend, mounts []frontendMount, extraServers []*http.Server) *http.ServeMux {
	q := findQUICFrontend(mounts)
	if q == nil {
		return mux
	}
	value, err := altSvcHeaderValue(q)
	if err != nil {
		// A mounted QUIC frontend whose Addr cannot yield a port is a
		// startup bug: loud, consistent with the fail-loud config rules.
		panic(err)
	}
	// Shared-mux frontends: re-mount each non-QUIC handler wrapped. The
	// QUIC frontend itself is never mux-mounted (dedicated listener only),
	// but the guard keeps the rule explicit. The mux already carries each
	// shared frontend's handler under "/" (mountFrontends), so the wrapped
	// handler goes into a FRESH mux: re-registering "/" on the SAME mux
	// panics (duplicate pattern), and a ServeMux cannot be struct-copied
	// over (it carries a mutex). main serves the returned mux.
	if len(shared) > 0 {
		rebuilt := http.NewServeMux()
		for _, f := range shared {
			if _, isQUIC := f.(frontend.QUICListenerFrontend); isQUIC {
				continue
			}
			rebuilt.Handle("/", wrapWithAltSvc(f.Handler(), value))
		}
		mux = rebuilt
	}
	// Dedicated-listener HTTP servers: wrap in place.
	for _, es := range extraServers {
		if es.Handler != nil {
			es.Handler = wrapWithAltSvc(es.Handler, value)
		}
	}
	return mux
}
