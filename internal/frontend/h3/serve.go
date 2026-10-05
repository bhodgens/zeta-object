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
	"net"

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

// Close gracefully stops the server: stop accepting, close the QUIC
// listener and the UDP socket (the harness must not leak UDP fds). Wired
// into the same graceful-stop fan the TCP listeners use by main.
func (s *Server) Close() error {
	// Graceful-stop shape: http3.Server.Shutdown sends GOAWAY and waits
	// within the caller's drain budget; the listener and UDP conn close
	// underneath it so Accept unblocks immediately.
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.srv.Shutdown(context.Background()) }()
	lnErr := s.ln.Close()
	udpErr := s.udpConn.Close()
	srvErr := <-shutdownDone
	switch {
	case srvErr != nil && !errors.Is(srvErr, context.DeadlineExceeded):
		return srvErr
	case lnErr != nil:
		return lnErr
	default:
		return udpErr
	}
}

// udpAddr parses a host:port UDP address. A parse failure returns an error
// (the caller surfaces it named) — never a silent zero address that would
// bind a random interface and mask the config typo.
func udpAddr(addr string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr("udp", addr)
}
