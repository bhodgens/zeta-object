// Package frontend defines the pluggable protocol seam: a Frontend decodes a
// wire protocol into the neutral object model via internal/backend.Backend
// and encodes protocol-appropriate responses.
package frontend

import (
	"crypto/tls"
	"net"
	"net/http"

	"github.com/bhodgens/zeta-object/internal/auth"
)

type Frontend interface {
	Name() string                      // "s3", "webdav", "ftp", ...
	Handler() http.Handler             // mounts itself under the server mux
	Authenticator() auth.Authenticator // per-frontend auth adapter; auth model decided by the open auth GH issue
	Capabilities() ProtocolCaps        // what this protocol can express
}

type ProtocolCaps struct {
	Buckets          bool
	Versioning       bool
	ConditionalReads bool
	Multipart        bool
	PresignedURLs    bool
}

// NonHTTPFrontend is an OPTIONAL extension implemented by frontends whose
// wire protocol is not expressible as http.Handler (FTP, SFTP). A frontend
// implementing it MUST NOT be mounted on the shared mux: main() opens a
// plain net.Listener at NonHTTPAddr() and runs Serve until shutdown.
// The frozen Frontend interface and Registry are untouched by this
// extension (sftp-ftp-2026-09 master Contract A).
type NonHTTPFrontend interface {
	Frontend
	// NonHTTPAddr returns this frontend's dedicated listen address from its
	// config ("" => config error, rejected at construction).
	NonHTTPAddr() string
	// Serve accepts connections on l until Stop is called or l is closed.
	// Blocking.
	Serve(l net.Listener) error
	// Stop gracefully stops: stop accepting, close sessions.
	Stop() error
}

// TLSListenerFrontend is an OPTIONAL extension for frontends whose dedicated
// listener needs its OWN TLS configuration (client-certificate verification).
// A frontend implementing it is served on a dedicated listener built from
// TLSConfig(), NOT on the shared mux and NOT with the process-wide
// ListenAndServeTLS(cert, key) path (which rebuilds the TLS config from the
// pair and would silently drop ClientCAs and ClientAuth).
//
// The frozen Frontend interface and Registry are untouched by this extension
// (management-api-2026-10 master Contract 1); the precedent is
// NonHTTPFrontend above.
type TLSListenerFrontend interface {
	Frontend
	// Addr returns this frontend's dedicated listen address from its config
	// ("" => config error, rejected at construction). An empty address is
	// NEVER a shared-mux fallback.
	Addr() string
	// TLSConfig returns the listener's TLS configuration. The caller
	// (package main) serves the listener with this pre-built config as-is;
	// ServeTLS is invoked with empty cert and key paths so these settings
	// (ClientCAs, ClientAuth, Certificates) survive.
	TLSConfig() (*tls.Config, error)
}

// QUICListenerFrontend is an OPTIONAL extension for frontends served
// over QUIC (HTTP/3). The caller (package main) opens a UDP socket on
// Addr() and serves HTTP/3 with the pre-built TLS config from
// TLSConfig(). The precedent is TLSListenerFrontend above it.
//
// IsQUICListener is a MARKER: both this interface and
// TLSListenerFrontend consist of Addr + TLSConfig, so every
// TLSListenerFrontend structurally satisfies this one (a Go interface
// is structural). Without the marker the QUIC-first classification in
// mountFrontends misroutes every TLS-listener frontend (the admin
// frontend) to a UDP listener - the regression quic-h3-2026-10 leaf
// 02 shipped and leaf 07's e2e run caught. Only a frontend that
// explicitly opts in returns true; classification checks this FIRST.
type QUICListenerFrontend interface {
	Frontend
	// IsQUICListener opts this frontend into QUIC (UDP/HTTP-3)
	// listener classification. Structural coincidence with
	// TLSListenerFrontend is not opt-in.
	IsQUICListener() bool
	// Addr returns this frontend's dedicated UDP listen address from
	// its config ("" => config error, rejected at construction). An
	// empty address is NEVER a shared-mux fallback.
	Addr() string
	// TLSConfig returns the QUIC listener's TLS configuration, built
	// from the process cert pair. The caller uses it as-is.
	TLSConfig() (*tls.Config, error)
}
