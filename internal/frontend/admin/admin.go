// Package admin is the management frontend: a dedicated TLS listener that
// authenticates every request by TLS client certificate (mTLS) and serves a
// JSON surface deliberately distinct from the S3 wire shape.
//
// This package owns transport and authentication only. The route
// implementations for configuration and buckets are injected through the
// Services value (management-api-2026-10 leaf 01 Contract 2; leaf 04 fills
// the services in).
package admin

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// KnownOptionKeys lists every option key the admin frontend accepts from a
// frontends entry's options map. Unknown keys are a loud startup error
// naming the key and this set (frontends.go validateOptions).
var KnownOptionKeys = map[string]bool{
	"clientCAFile":     true,
	"adminPrincipals":  true,
	"allowNonLoopback": true,
}

// Options configures the admin frontend. New validates it fail-loud.
type Options struct {
	// ListenAddr is this frontend's dedicated listen address. It is
	// REQUIRED (an empty value is a construction error) and must resolve to
	// a loopback host unless AllowNonLoopback is set. Port 0 is accepted.
	ListenAddr string
	// ClientCAFile is the REQUIRED PEM bundle of trusted client CAs. It is
	// read at construction and re-read by leaf 04's /auth/reload path.
	ClientCAFile string
	// AdminPrincipals is an optional allow-list of certificate Subject
	// Common Names. Empty means every CA-verified principal is admitted.
	AdminPrincipals []string
	// AllowNonLoopback opts out of the loopback guard for an explicit
	// public bind.
	AllowNonLoopback bool
	// Services is the injected route surface (leaf 04 fills it in).
	Services Services

	// CertFile and KeyFile are the process server-certificate pair used as
	// THIS listener's own certificate. They mirror serverConfig.CertFile /
	// serverConfig.KeyFile: unlike the process-wide HTTPS listener, this
	// package loads the pair itself so its TLSConfig() can carry ClientCAs
	// and ClientAuth (a ListenAndServeTLS(cert, key) path would rebuild the
	// config and drop them).
	CertFile string
	KeyFile  string
}

// Services is the injected route surface. Every field may be nil in this
// leaf; a nil service answers 503 {"error":{"code":"NotImplemented",...}}.
type Services struct {
	Status         func(ctx context.Context) (StatusReport, error)
	GetConfig      func(ctx context.Context) (json.RawMessage, error)
	PutConfig      func(ctx context.Context, patch json.RawMessage) (ConfigApplyResult, error)
	SaveConfig     func(ctx context.Context) error
	ReloadAuth     func(ctx context.Context) error
	ListBuckets    func(ctx context.Context) (json.RawMessage, error)
	CreateBucket   func(ctx context.Context, name string) error
	DeleteBucket   func(ctx context.Context, name string) error
	BucketDetail   func(ctx context.Context, name string) (json.RawMessage, error)
	BucketSettings func(ctx context.Context, name string, patch json.RawMessage) error
	Purge          func(ctx context.Context, dataset string) error
}

// StatusReport is the GET /status payload. Leaf 04 fills the values; the
// metadata-provider availability is reported honestly (never invented).
type StatusReport struct {
	Version          string                 `json:"version"`
	Uptime           string                 `json:"uptime"`
	Listeners        []string               `json:"listeners"`
	Frontends        []string               `json:"frontends"`
	Backends         []string               `json:"backends"`
	RestartRequired  []string               `json:"restartRequired"`
	MetadataProvider MetadataProviderStatus `json:"metadataProvider"`
}

// MetadataProviderStatus reports metadata-provider availability as an
// explicit {available, reason} pair so an unavailable provider is never
// silently omitted (leaf 04 Contract 5).
type MetadataProviderStatus struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// ConfigApplyResult is the PUT /config payload (leaf 04): the keys that took
// effect and the keys that require a restart.
type ConfigApplyResult struct {
	Applied         []string `json:"applied"`
	RestartRequired []string `json:"restartRequired"`
}

// adminFrontend implements frontend.Frontend and frontend.TLSListenerFrontend.
type adminFrontend struct {
	opts       Options
	caPool     *x509.CertPool
	principals map[string]bool // empty = admit every CA-verified principal
}

var (
	_ frontend.Frontend            = (*adminFrontend)(nil)
	_ frontend.TLSListenerFrontend = (*adminFrontend)(nil)
)

// New constructs the admin frontend. It is fail-loud: an empty listen
// address, a missing/unreadable client CA bundle, or a non-loopback address
// without the explicit opt-out aborts construction.
func New(opts Options) (frontend.Frontend, error) {
	if strings.TrimSpace(opts.ListenAddr) == "" {
		return nil, fmt.Errorf("admin: listenAddr is required (an empty Addr is a construction error)")
	}
	pool, err := loadClientCAs(opts.ClientCAFile)
	if err != nil {
		return nil, err
	}
	if err := validateListenAddr(opts.ListenAddr, opts.AllowNonLoopback); err != nil {
		return nil, err
	}
	principals := make(map[string]bool, len(opts.AdminPrincipals))
	for _, p := range opts.AdminPrincipals {
		if p = strings.TrimSpace(p); p != "" {
			principals[p] = true
		}
	}
	return &adminFrontend{opts: opts, caPool: pool, principals: principals}, nil
}

// Name returns the wire identity of this frontend.
func (f *adminFrontend) Name() string { return "admin" }

// Capabilities declares the bucket-management surface; the JSON management
// API expresses no S3-protocol capability.
func (f *adminFrontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{Buckets: true}
}

// Addr returns this frontend's dedicated listen address.
func (f *adminFrontend) Addr() string { return f.opts.ListenAddr }

// Authenticator returns nil: the admin frontend's authentication is the TLS
// client certificate verified in the handler wrapper (authenticate), not the
// auth.Authenticator credential model. This mirrors the ftp/sftp precedent
// for dedicated listeners whose auth is not the shared HTTP identity model.
func (f *adminFrontend) Authenticator() auth.Authenticator { return nil }

// Handler returns the auth-wrapped JSON request handler. Every route,
// including /status, requires a verified client certificate.
func (f *adminFrontend) Handler() http.Handler {
	return http.HandlerFunc(f.serveHTTP)
}
