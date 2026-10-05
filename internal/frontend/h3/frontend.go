// Package h3 implements the HTTP/3 (QUIC) frontend (quic-h3-2026-10 leaf
// 02): it wraps a webdav frontend built with the SAME constructor options
// the webdav factory entry uses (Basic auth over the identity registry,
// per-bucket lock store root), but authenticates with mTLS — the TLS peer
// certificate's Subject CN resolved through the identity registry by
// auth.CertAuthenticator — and exposes its own UDP listen address and
// pre-built TLS configuration through the QUICListenerFrontend seam.
//
// It is a dedicated-listener frontend, never a shared-mux frontend:
// Handler() exists for seam completeness but main never mux-mounts it; the
// HTTP/3 server (serve.go) serves the wrapped handler over QUIC.
package h3

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/frontend/webdav"
)

// Compile-time: the h3 frontend is a QUICListenerFrontend (and therefore a
// Frontend). It is never a shared-mux frontend by construction.
var (
	_ frontend.Frontend             = (*Frontend)(nil)
	_ frontend.QUICListenerFrontend = (*Frontend)(nil)
)

// KnownOptionKeys is the v1 option-key set the factory validates
// fail-loud: ONLY clientCAFile (the PEM bundle of trusted client CAs —
// REQUIRED). The process certFile/keyFile pair is reused; no other keys.
var KnownOptionKeys = map[string]bool{
	"clientCAFile": true,
}

// Config carries the h3 frontend's construction inputs (the factory maps
// the FrontendConfig entry onto this).
type Config struct {
	// ListenAddr is the dedicated UDP host:port. REQUIRED: an empty
	// address is NEVER a shared-mux fallback (QUICListenerFrontend seam
	// rule) — construction fails loudly instead.
	ListenAddr string
	// Bucket is REQUIRED: the wrapped webdav runs in single-bucket mode
	// (the cache client speaks one re-rooted namespace).
	Bucket string
	// ClientCAFile is the REQUIRED path to a PEM bundle of the trusted
	// client CA(s). Missing/unreadable is a construction error naming
	// the path.
	ClientCAFile string
	// CertFile/KeyFile are the process cert pair the listener presents.
	CertFile string
	KeyFile  string
}

// Frontend serves the wrapped webdav handler over HTTP/3 on its own UDP
// listener. Construct with New; the zero value is not usable.
type Frontend struct {
	listenAddr string
	certFile   string
	keyFile    string
	caPool     *x509.CertPool
	wrapped    *webdav.Frontend
}

// New constructs the h3 frontend. Loud failures:
//   - empty listenAddr (never a shared-mux fallback);
//   - empty bucket (the webdav single-bucket re-root needs it);
//   - missing/unreadable clientCAFile (error names the path);
//   - nil registry or nil backend (the webdav constructor's own rules).
func New(be backend.Backend, cfg Config, reg auth.IdentityRegistry, lockRoot func(bucket string) string) (*Frontend, error) {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return nil, fmt.Errorf("h3 frontend requires a non-empty listenAddr (a QUIC-listener frontend cannot share the default HTTPS mux)")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("h3 frontend requires a non-empty bucket (the wrapped webdav runs in single-bucket mode for the cache client)")
	}
	caPool, err := loadClientCAs(cfg.ClientCAFile)
	if err != nil {
		return nil, err
	}
	if reg == nil {
		return nil, fmt.Errorf("h3 frontend requires an identity registry (auth configuration failed earlier?)")
	}
	// The wrapped webdav is built with the SAME constructor options the
	// webdav factory entry uses (frontends.go), but with the mTLS
	// CertAuthenticator instead of Basic: the TLS handshake has already
	// verified the client certificate; the CN is the principal.
	authnr := auth.NewCertAuthenticator(reg)
	wd, err := webdav.New(be, webdav.Config{Bucket: cfg.Bucket}, webdav.WithAuthenticator(authnr),
		webdav.WithLockStoreRoot(lockRoot))
	if err != nil {
		return nil, fmt.Errorf("h3 frontend: building wrapped webdav: %w", err)
	}
	return &Frontend{
		listenAddr: cfg.ListenAddr,
		certFile:   cfg.CertFile,
		keyFile:    cfg.KeyFile,
		caPool:     caPool,
		wrapped:    wd,
	}, nil
}

// loadClientCAs reads the REQUIRED client CA bundle (fail-loud, naming the
// path). Same shape as the admin frontend's loader (admin/mtls.go).
func loadClientCAs(path string) (*x509.CertPool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("h3: clientCAFile is required (PEM bundle of trusted client CAs)")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("h3: reading clientCAFile %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("h3: clientCAFile %q contains no usable PEM certificates", path)
	}
	return pool, nil
}

// Name returns the frontend's registry name (config "type" value).
func (f *Frontend) Name() string { return "h3" }

// Handler returns the wrapped webdav request pipeline. It exists for seam
// completeness — the h3 frontend is never mux-mounted; the HTTP/3 server
// serves this handler over QUIC.
func (f *Frontend) Handler() http.Handler { return f.wrapped.Handler() }

// Authenticator returns the wrapped webdav's adapter (the mTLS
// CertAuthenticator this package built).
func (f *Frontend) Authenticator() auth.Authenticator { return f.wrapped.Authenticator() }

// Capabilities delegates to the wrapped webdav frontend (single-bucket
// mode: Buckets false, ConditionalReads true).
func (f *Frontend) Capabilities() frontend.ProtocolCaps { return f.wrapped.Capabilities() }

// Wrapped returns the wrapped webdav frontend (diagnostics/tests).
func (f *Frontend) Wrapped() *webdav.Frontend { return f.wrapped }

// Addr returns the dedicated UDP listen address (QUICListenerFrontend).
func (f *Frontend) Addr() string { return f.listenAddr }

// TLSConfig builds the QUIC listener's TLS configuration: the process cert
// pair as the listener's own certificate, TLS 1.3 minimum (HTTP/3
// requires it), client certificates REQUIRED and verified against the
// configured CA bundle. The caller (package main / serve.go) uses it
// as-is; the server's serving path adds the h3 ALPN via quic-go.
func (f *Frontend) TLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.certFile, f.keyFile)
	if err != nil {
		return nil, fmt.Errorf("h3: loading server certificate pair (%s, %s): %w", f.certFile, f.keyFile, err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    f.caPool,
		NextProtos:   []string{"h3"},
	}, nil
}
