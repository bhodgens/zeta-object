// Package frontend defines the pluggable protocol seam: a Frontend decodes a
// wire protocol into the neutral object model via internal/backend.Backend
// and encodes protocol-appropriate responses.
package frontend

import (
	"net/http"

	"mini-s3/internal/auth"
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
