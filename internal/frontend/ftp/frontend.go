// Package ftp implements the FTP/FTPS protocol frontend
// (sftp-ftp-2026-09 leaf 03) on github.com/fclairamb/ftpserverlib
// (MIT, see docs/licenses/THIRD-PARTY-LICENSES.md).
//
// Commands map onto internal/backend.Backend per the tree's Contract C:
// LIST/NLST/MLSD → List (Delimiter "/"), STOR → Put, RETR → Get,
// DELE → Delete, RMD → Delete of the "dir/" marker, MKD → Put of the
// zero-byte "dir/" marker, SIZE/MDTM/STAT → Stat. Buckets are top-level
// directories. Capability degradation (Contract D): no conditional reads,
// no multipart, no rename atomicity guarantee — REST (restart) is rejected,
// never silently emulated.
//
// Auth: USER/PASS flows through the PasswordVerifier seam (wired to the
// auth identity registry by the factory); grant enforcement is
// frontend.AuthorizeRequest before every storage-touching call (leaf 05).
//
// This package performs no filesystem access of its own: every storage
// touch goes through the injected backend.Backend seam.
package ftp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strings"

	ftpserver "github.com/fclairamb/ftpserverlib"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// Config carries the per-frontend configuration (factory-validated).
type Config struct {
	// ListenAddr is the dedicated FTP listen address (required — non-HTTP
	// frontends cannot share the HTTPS mux).
	ListenAddr string
	// PassivePortMin/Max bound the passive data-channel ports (0/0 =
	// kernel-assigned). Max > Min > 0 when set.
	PassivePortMin int
	PassivePortMax int
	// PublicIP is the address advertised in PASV replies ("" = listener IP).
	PublicIP string
	// DataConnTimeoutSeconds bounds how long the server waits for a client to
	// open a passive data connection after a PASV/EPSV reply, in seconds.
	// 0 means "library default" (30s).
	//
	// WHY THIS IS CONFIGURABLE (bughunt 2026-10-08): ftpserverlib applies this
	// as a deadline on listener.Accept() (transfer_pasv.go:Open ->
	// ConnectionWait). The client's connect is a normal scheduling race - on a
	// loaded host (a CI runner, a parallel package suite, a machine also
	// running something else) the control channel can be scheduled late enough
	// that 30s elapses before the data connect is serviced, and the server
	// answers 425 "failed to accept passive transfer connection" even though
	// nothing is wrong with the server or the client. A real deployment on a
	// saturated host hits the same wall; a site that terminates FTP through a
	// proxy with a slow control-to-data handoff needs a longer window, and a
	// test that wants to fail fast needs a shorter one. Hard-coding either
	// value was the defect.
	DataConnTimeoutSeconds int
	// TLSConfig enables explicit FTPS (AUTH TLS). Nil = plain FTP only.
	TLSConfig *tls.Config
	// Verifier authenticates USER/PASS pairs to identities. Required.
	Verifier PasswordVerifier
}

// Frontend implements frontend.Frontend + frontend.NonHTTPFrontend for FTP.
// Construct with New; the zero value is not usable.
type Frontend struct {
	be       backend.Backend
	cfg      Config
	server   *ftpserver.FtpServer
	listener net.Listener // set by Serve (handed to settings)
}

// Compile-time seam assertions.
var (
	_ frontend.Frontend        = (*Frontend)(nil)
	_ frontend.NonHTTPFrontend = (*Frontend)(nil)
)

// New constructs the FTP frontend. All config validation is fail-loud.
func New(be backend.Backend, cfg Config) (*Frontend, error) {
	if be == nil {
		return nil, errors.New("ftp: backend is required")
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New(`ftp: listenAddr is required (non-HTTP frontends cannot share the default HTTPS mux)`)
	}
	if cfg.Verifier == nil {
		return nil, errors.New("ftp: password verifier is required")
	}
	if cfg.PassivePortMin < 0 || cfg.PassivePortMax < 0 {
		return nil, errors.New("ftp: passive ports must not be negative")
	}
	if cfg.DataConnTimeoutSeconds < 0 {
		return nil, errors.New("ftp: dataConnTimeoutSeconds must not be negative (0 = library default)")
	}
	if cfg.PassivePortMin != 0 || cfg.PassivePortMax != 0 {
		if cfg.PassivePortMin == 0 || cfg.PassivePortMax == 0 {
			return nil, errors.New("ftp: passivePortMin and passivePortMax must be set together")
		}
		if cfg.PassivePortMax <= cfg.PassivePortMin {
			return nil, fmt.Errorf("ftp: passivePortMax (%d) must be greater than passivePortMin (%d)",
				cfg.PassivePortMax, cfg.PassivePortMin)
		}
	}
	f := &Frontend{be: be, cfg: cfg}
	f.server = ftpserver.NewFtpServer(&mainDriver{f: f})
	return f, nil
}

// Name returns the frontend's registry name (config "type" value).
func (f *Frontend) Name() string { return "ftp" }

// Handler returns a stub that always writes 501 Not Implemented. FTP is a
// raw-TCP protocol and is NEVER mounted on the HTTP mux; this exists only
// so the http-shaped legacy seam keeps a total function (mountFrontends
// rejects any such mount loudly before this could ever serve).
func (f *Frontend) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ftp is a non-HTTP frontend; it is never served over HTTP", http.StatusNotImplemented)
	})
}

// Authenticator returns nil: FTP does not speak HTTP auth; its USER/PASS
// adapter is the PasswordVerifier seam wired at construction.
func (f *Frontend) Authenticator() auth.Authenticator { return nil }

// Capabilities per master Contract D: buckets yes; conditional reads,
// multipart, presigned URLs, versioning have no FTP expression.
func (f *Frontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{Buckets: true}
}

// NonHTTPAddr returns the dedicated FTP listen address.
func (f *Frontend) NonHTTPAddr() string { return cfgAddr(f.cfg.ListenAddr) }

func cfgAddr(a string) string { return a }

// Serve runs the FTP server on l (blocking) until Stop or listener close.
// ftpserverlib picks the listener up from Settings.Listener during Listen,
// so the address binding stays under this process's control (main opens
// the net.Listener); Listen also initializes the settings + passive
// listener manager Serve needs.
func (f *Frontend) Serve(l net.Listener) error {
	f.listener = l
	if err := f.server.Listen(); err != nil {
		return err
	}
	return f.server.Serve()
}

// Stop gracefully stops the FTP server (close listener + passive listeners;
// sessions terminate with their connections).
func (f *Frontend) Stop() error { return f.server.Stop() }

// setListener stores l so mainDriver.GetSettings can return it.
func (f *Frontend) setListener(l net.Listener) { f.listener = l }

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

// bucketOf splits "/bucket/key" into (bucket, key). "/" → ("", "").
