// Package webdav implements the WebDAV protocol frontend (RFC 4918, class 1
// subset): OPTIONS, PROPFIND (Depth 0/1), GET, HEAD, PUT, DELETE, MKCOL,
// COPY, MOVE — decoded onto the neutral object model through
// internal/backend.Backend, authenticated with HTTP Basic credentials via
// internal/auth's BasicAuthenticator, and authorized with the shared
// frontend.AuthorizeRequest grant helper.
//
// Binding semantic rules (webdav-2026-09 master):
//
//   - Never silent emulation: LOCK/UNLOCK/PROPPATCH and every other
//     unimplemented method are rejected with 405 + Allow; Depth-infinity
//     PROPFIND and collection COPY/MOVE are rejected with 403.
//   - Collections are virtual prefixes: MKCOL writes no marker object; a
//     collection exists iff its prefix lists (or the bucket itself exists).
//   - Every request is authenticated; missing or invalid credentials get
//     401 + WWW-Authenticate: Basic, a valid identity without the grant
//     for the effective bucket gets 403.
//
// This package performs no filesystem access of its own: every storage
// touch goes through the injected backend.Backend seam.
package webdav

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// Config carries the per-frontend configuration (webdav-2026-09 master
// Contract 1/2).
type Config struct {
	// Bucket non-empty => single-bucket mode (master Contract 3, mode B):
	// "/" IS that bucket's root and no bucket concept is exposed on the
	// wire. Empty => multi-bucket mode (mode A): top-level collections are
	// the server's buckets. Whitespace-only values are rejected (a typo
	// like " " must not silently fall back to exposing every bucket).
	Bucket string
}

// Frontend implements frontend.Frontend for WebDAV. Construct with New;
// the zero value is not usable.
type Frontend struct {
	be     backend.Backend
	authnr auth.Authenticator
	bucket string // non-empty => single-bucket mode (mode B)
}

// Compile-time assertion: *Frontend satisfies the frozen frontend seam.
var _ frontend.Frontend = (*Frontend)(nil)

// Option configures a Frontend at construction time.
type Option func(*Frontend)

// WithAuthenticator injects the Basic-auth adapter (main wires
// auth.NewBasicAuthenticator over the identity registry). Required: New
// fails without one (fail-loud — a webdav handler without an
// authenticator would 500 on every request).
func WithAuthenticator(a auth.Authenticator) Option {
	return func(f *Frontend) { f.authnr = a }
}

// New constructs the WebDAV frontend.
//   - be is the Backend for ALL storage access (required, non-nil).
//   - cfg carries the per-frontend config entry (Contract 2).
//   - opts may inject the authenticator (WithAuthenticator).
func New(be backend.Backend, cfg Config, opts ...Option) (*Frontend, error) {
	if be == nil {
		return nil, errors.New("webdav: backend is required")
	}
	if cfg.Bucket != "" && strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("webdav: bucket %q must not be whitespace-only (drop the key for multi-bucket mode)", cfg.Bucket)
	}
	f := &Frontend{be: be, bucket: cfg.Bucket}
	for _, opt := range opts {
		opt(f)
	}
	if f.authnr == nil {
		return nil, errors.New("webdav: authenticator is required (use WithAuthenticator)")
	}
	return f, nil
}

// Name returns the frontend's registry name (config "type" value).
func (f *Frontend) Name() string { return "webdav" }

// Handler returns the WebDAV request pipeline (auth → dispatch → handler).
func (f *Frontend) Handler() http.Handler { return http.HandlerFunc(f.serveHTTP) }

// Authenticator returns the injected Basic-auth adapter.
func (f *Frontend) Authenticator() auth.Authenticator { return f.authnr }

// Capabilities reports what the WebDAV subset can express here. Versioning,
// multipart, and presigned URLs have no WebDAV expression; conditional reads
// (If-None-Match on GET/HEAD) are honored. Buckets is true only in
// multi-bucket mode (master Contract 1).
func (f *Frontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{
		Buckets:          f.bucket == "",
		ConditionalReads: true,
	}
}
