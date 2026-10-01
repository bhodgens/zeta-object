// Package sftp implements the SFTP protocol frontend (sftp-ftp-2026-09
// leaf 04) on github.com/pkg/sftp (BSD-2-Clause) over golang.org/x/crypto/ssh
// (BSD-3-Clause) — see docs/licenses/THIRD-PARTY-LICENSES.md.
//
// The request-server Handlers map onto internal/backend.Backend per the
// tree's Contract C: Get → Get, Put → Put, Remove → Delete, readdir →
// List(Delimiter "/"), mkdir/rmdir → "dir/" marker Put/Delete, stat → Stat.
// Capability degradation (Contract D): setstat/fsetstat →
// SSH_FX_PERMISSION_DENIED; symlink/readlink → SSH_FX_OP_UNSUPPORTED; grant
// denials → SSH_FX_PERMISSION_DENIED (leaf 05).
//
// Auth: password + public-key flows through the PasswordVerifier /
// PublicKeyChecker seams (wired to the auth identity registry by the
// factory — LookupByBasicCredential / AuthenticatePublicKey).
//
// This package performs no filesystem access of its own except host-key
// load-or-generate (hostkey.go): every storage touch goes through the
// injected backend.Backend seam.
package sftp

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// Config carries the per-frontend configuration (factory-validated).
type Config struct {
	// ListenAddr is the dedicated SFTP listen address (required).
	ListenAddr string
	// HostKeyFile is the SSH host key path (REQUIRED by the factory — the
	// file itself is load-or-generated on first start, hostkey.go).
	HostKeyFile string
	// AllowPasswordAuth (default true; "false" = pubkey only).
	AllowPasswordAuth bool
	// Verifier authenticates passwords (nil + AllowPasswordAuth = error).
	Verifier PasswordVerifier
	// KeyChecker resolves public keys (nil = pubkey auth disabled).
	KeyChecker PublicKeyChecker
	// RichGrants re-resolves an identity's rich grant table by access key
	// ID at session start (leaf 09; nil = legacy-only behavior — the
	// CriticalOptions floor is the whole story).
	RichGrants RichGrantsResolver
}

// Frontend implements frontend.Frontend + frontend.NonHTTPFrontend for SFTP.
// Construct with New; the zero value is not usable.
type Frontend struct {
	be       backend.Backend
	cfg      Config
	cfgFn    sshAddrs
	l        net.Listener
	quit     chan struct{}
	sessions sync.WaitGroup
}

// sshAddrs is a small indirection for the listener address accessor.
type sshAddrs struct{ addr string }

// Compile-time seam assertions.
var (
	_ frontend.Frontend        = (*Frontend)(nil)
	_ frontend.NonHTTPFrontend = (*Frontend)(nil)
)

// New constructs the SFTP frontend. All config validation is fail-loud.
func New(be backend.Backend, cfg Config) (*Frontend, error) {
	if be == nil {
		return nil, errors.New("sftp: backend is required")
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New(`sftp: listenAddr is required (non-HTTP frontends cannot share the default HTTPS mux)`)
	}
	if cfg.HostKeyFile == "" {
		return nil, errors.New(`sftp: hostKeyFile option is required (clients pin host keys; the file is load-or-generated on first start)`)
	}
	if cfg.AllowPasswordAuth && cfg.Verifier == nil {
		return nil, errors.New("sftp: password auth is enabled but no password verifier is wired")
	}
	if !cfg.AllowPasswordAuth && cfg.KeyChecker == nil {
		return nil, errors.New("sftp: allowPasswordAuth=false requires a public-key checker")
	}
	f := &Frontend{
		be:    be,
		cfg:   cfg,
		cfgFn: sshAddrs{addr: cfg.ListenAddr},
		quit:  make(chan struct{}),
	}
	return f, nil
}

// Name returns the frontend's registry name (config "type" value).
func (f *Frontend) Name() string { return "sftp" }

// Handler returns a stub that always writes 501 Not Implemented. SFTP rides
// SSH and is NEVER mounted on the HTTP mux; this exists only so the
// http-shaped legacy seam keeps a total function (mountFrontends rejects
// any such mount loudly before this could ever serve).
func (f *Frontend) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sftp is a non-HTTP frontend; it is never served over HTTP", http.StatusNotImplemented)
	})
}

// Authenticator returns nil: SFTP does not speak HTTP auth; its adapters
// are the PasswordVerifier/PublicKeyChecker seams wired at construction.
func (f *Frontend) Authenticator() auth.Authenticator { return nil }

// Capabilities per master Contract D: buckets yes; conditional reads,
// multipart, presigned URLs, versioning have no SFTP expression.
func (f *Frontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{Buckets: true}
}

// NonHTTPAddr returns the dedicated SFTP listen address.
func (f *Frontend) NonHTTPAddr() string { return f.cfgFn.addr }

// Serve accepts SSH connections on l until Stop or listener close
// (blocking). Each connection is handled on its own goroutine, tracked in
// the sessions WaitGroup for the graceful Stop.
func (f *Frontend) Serve(l net.Listener) error {
	f.l = l
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-f.quit:
				return nil // graceful stop: listener closed under us
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("sftp: accept: %w", err)
		}
		c := conn
		f.sessions.Go(func() { f.handleConn(c) })
	}
}

// Stop gracefully stops: close the listener and wait for open sessions to
// drain (their SFTP channels terminate with their connections).
func (f *Frontend) Stop() error {
	close(f.quit)
	if f.l != nil {
		if err := f.l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("sftp: listener close: %v", err)
		}
	}
	f.sessions.Wait()
	return nil
}

// bucketOf splits "/bucket/key" into (bucket, key). "/" → ("", "").
func bucketOf(p string) (bucket, key string) {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if p == "" || p == "." {
		return "", ""
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}
