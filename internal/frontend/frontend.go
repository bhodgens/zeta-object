// Package frontend defines the pluggable protocol seam: a Frontend decodes a
// wire protocol into the neutral object model via internal/backend.Backend
// and encodes protocol-appropriate responses.
package frontend

import (
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
