// serve.go — the HTTP/3 serving half of the h3 frontend (quic-h3-2026-10
// leaf 02 Task 4). main (package main's UDP branch) calls Listen, then
// Serve in a goroutine, and Close for the graceful drain — the same
// start/stop shape the TCP dedicated listeners follow, over quic-go.
package h3

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// Server is the running HTTP/3 server for one h3 frontend. It owns the UDP
// packet conn and the quic-go listener; Close tears both down.
type Server struct {
	srv     *http3.Server
	ln      *quic.EarlyListener
	udpConn *net.UDPConn
}

// Listen opens the frontend's UDP socket, builds the QUIC listener with the
// frontend's pre-built TLS config, and returns the server ready to Serve.
// The TLS config is h3's own (mTLS, TLS 1.3) — it is NEVER passed through a
// TCP ListenAndServeTLS path, which would rebuild the config from the cert
// pair and silently drop ClientCAs/ClientAuth.
func (f *Frontend) Listen() (*Server, error) {
	tlsCfg, err := f.TLSConfig()
	if err != nil {
		return nil, fmt.Errorf("h3 frontend %s: building TLS config: %w", f.Addr(), err)
	}
	udpAddr, err := udpAddr(f.Addr())
	if err != nil {
		return nil, fmt.Errorf("h3 frontend %s: parsing UDP listen address: %w", f.Addr(), err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("h3 frontend %s: binding UDP listener: %w", f.Addr(), err)
	}
	// Listener construction shape (quic-go v0.63.0): quic.ListenEarly over
	// the caller-owned UDP packet conn. Early so 0-RTT-arriving clients are
	// answered at the transport layer; the h3 layer uses standard 1-RTT.
	ln, err := quic.ListenEarly(udpConn, tlsCfg, &quic.Config{})
	if err != nil {
		udpConn.Close()
		return nil, fmt.Errorf("h3 frontend %s: opening QUIC listener: %w", f.Addr(), err)
	}
	return &Server{
		srv:     &http3.Server{Handler: f.wrapped.Handler()},
		ln:      ln,
		udpConn: udpConn,
	}, nil
}

// Serve accepts QUIC connections until Close. Blocking. Returns
// http.ErrServerClosed after Close (the same contract the TCP
// http.Server.Serve family gives main's error funnel).
func (s *Server) Serve() error {
	// Server-serving shape: http3.Server.ServeListener over the pre-built
	// quic listener (it returns http.ErrServerClosed on Close/Shutdown).
	return s.srv.ServeListener(s.ln)
}

// Addr returns the bound UDP address (useful for 127.0.0.1:0 tests).
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// TLSConfig returns the listener's TLS configuration as built.
func (f *Frontend) ListenerTLSConfig() *tls.Config { cfg, _ := f.TLSConfig(); return cfg }

// Close gracefully stops the server within a bounded default drain budget:
// Shutdown first (GOAWAY, in-flight responses finish, socket still open),
// then the QUIC listener and UDP socket are torn down underneath it. A caller
// with its own drain budget (main's graceful-stop fan) uses Shutdown directly
// with that context instead.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultDrainTimeout)
	defer cancel()
	return s.Shutdown(ctx)
}

// defaultDrainTimeout bounds Shutdown when no caller budget is supplied (the
// no-arg Close path: test cleanup, and the fan's own timeout once it passes a
// context). It matches the process-wide serverShutdownTimeout default so a
// Close can never out-wait systemd's TimeoutStopSec.
const defaultDrainTimeout = 30 * time.Second

// Shutdown gracefully stops the server within the caller's drain budget:
// stop accepting (GOAWAY), let every in-flight request finish, and only then
// release the QUIC listener and the UDP socket.
//
// Two properties this pins, both of which the previous socket-first shape
// broke:
//
//   - The socket and listener stay OPEN across the drain. Closing them first
//     destroys the QUIC transport underneath a live stream, so a large
//     in-flight GET/PUT is cut mid-body instead of draining.
//   - The budget is the CALLER's context, not context.Background(). The
//     previous unbounded wait let a client that keeps advancing its
//     last-packet timer keep the whole listener-drain fan alive; now a spent
//     budget returns the context error promptly and main's wg.Wait() can
//     never hang past TimeoutStopSec.
//
// The socket/listener teardown is deferred, so it runs on every path — a
// graceful drain, a budget overrun, or an error.
func (s *Server) Shutdown(ctx context.Context) error {
	// Deferred so no return path can leak the UDP fd: the graceful-stop fan
	// and a test cleanup may both tear the server down.
	defer func() { s.closeTransport() }()
	// http3.Server.Shutdown returns nil after a clean drain and the
	// context's own error once the budget is spent (it force-closes the
	// connections on that path), so the error passes straight through —
	// matching net/http's Shutdown contract. main's fan logs it and keeps
	// tearing the rest down.
	return s.srv.Shutdown(ctx)
}

// closeTransport releases the QUIC listener and the UDP socket. Both closes
// are best-effort and only their errors are joined: this runs after the
// graceful drain, so a "use of closed network connection" here is expected
// on the double-close paths (drain fan + test cleanup).
func (s *Server) closeTransport() {
	var errs []error
	if err := s.ln.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := s.udpConn.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		log.Printf("h3: closing QUIC transport: %v", errors.Join(errs...))
	}
}

// udpAddr parses a host:port UDP address. A parse failure returns an error
// (the caller surfaces it named) — never a silent zero address that would
// bind a random interface and mask the config typo.
func udpAddr(addr string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr("udp", addr)
}
