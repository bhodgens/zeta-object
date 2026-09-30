// Package s3 is the S3 wire-protocol frontend: request dispatch (path →
// bucket/object parse), SigV4 authentication (header + presigned +
// aws-chunked), method+query routing to the operation handlers, and XML
// marshaling. All storage access goes through the injected
// backend.Backend seam — this package performs no filesystem I/O of its
// own (multipart part staging is the documented v1 exception: it remains
// direct-fs orchestration ABOVE the seam per backend-interface master
// Contract 4, accessed via the bucket's fs root path).
//
// This package is the leaf-02 extraction of the former package-main S3
// layer (main.go rootHandler/dispatch, sigv4.go, object/bucket/multipart
// handler files, xml.go, types.go) into a frontend.Frontend with zero
// wire change: same routes, same status codes, same XML bytes.
package s3

import (
	"net/http"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// Frontend implements frontend.Frontend for the S3 protocol. Construct
// with New; the zero value is not usable.
type Frontend struct {
	creds auth.CredentialSource
	authz *sigv4Authenticator
}

// Compile-time assertion: *Frontend must satisfy the frontend seam
// interface. A signature drift breaks the build here instead of failing
// at Registry.Register time.
var _ frontend.Frontend = (*Frontend)(nil)

// Option configures a Frontend at construction time.
type Option func(*Frontend)

// WithCredentialSource injects the SigV4 credential lookup. When absent,
// New falls back to the single-pair source supplied by package main's
// serverCredentials adapter.
func WithCredentialSource(cs auth.CredentialSource) Option {
	return func(f *Frontend) { f.creds = cs }
}

// New constructs the S3 frontend. The backend parameter is intentionally
// unused in v1 (underscore): the data plane resolves per-bucket through
// the installed backendLookup seam (seam.go/backendFor), which honors the
// config's per-bucket backend selections — a single stored Backend could
// not. The parameter stays for API stability of the constructor.
func New(_ backend.Backend, opts ...Option) *Frontend {
	f := &Frontend{}
	for _, opt := range opts {
		opt(f)
	}
	if f.creds == nil {
		f.creds = credentialSourceFor()
	}
	f.authz = &sigv4Authenticator{creds: f.creds}
	return f
}

// Name returns the frontend's registry name.
func (f *Frontend) Name() string { return "s3" }

// Handler returns the S3 request pipeline (the former rootHandler chain).
func (f *Frontend) Handler() http.Handler { return http.HandlerFunc(f.serveHTTP) }

// Authenticator returns the SigV4 adapter behind the auth seam.
func (f *Frontend) Authenticator() auth.Authenticator { return f.authz }

// Capabilities reports what the S3 protocol can express here. Versioning
// is false: a versioning request degrades at the seam (501
// NotImplemented), matching today's behavior — never emulation.
func (f *Frontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{
		Buckets:          true,
		Versioning:       false,
		ConditionalReads: true,
		Multipart:        true,
		PresignedURLs:    true,
	}
}
