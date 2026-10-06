// frontends.go — registry-driven frontend construction and mounting
// (frontend-interface leaf 03).
//
// main() no longer hardcodes the s3 mount. It builds a startupPlan from
// the config's "frontends" array: each entry is constructed via a factory
// map, registered into a frontend.Registry, and split into shared-mux
// mounts vs dedicated-listener mounts. Absent/empty config == S3 on the
// default listener — exact backward compatibility.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
	ftp "github.com/bhodgens/zeta-object/internal/frontend/ftp"
	h3 "github.com/bhodgens/zeta-object/internal/frontend/h3"
	"github.com/bhodgens/zeta-object/internal/frontend/owncloud"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
	sftp "github.com/bhodgens/zeta-object/internal/frontend/sftp"
	"github.com/bhodgens/zeta-object/internal/frontend/webdav"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// frontendFactories maps config Type -> constructor. Future frontends
// (owncloud — see its GH issue) add one entry each. The webdav factory
// builds the Basic authenticator over the process identity registry
// (webdav-2026-09 leaf 04 Task 3: same auth model as the s3 frontend's
// adapter, rendered as a Basic challenge). The ftp/sftp factories build the
// non-HTTP frontends (sftp-ftp-2026-09 leaves 03/04): each takes the
// process backend resolver (same data plane as s3), the identity registry
// (USER/PASS / pubkey adapters), and the shared TLS cert pair (ftp AUTH TLS
// reuses it; sftp uses its own SSH host key instead).
var frontendFactories = map[string]func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error){
	"s3": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		return s3.New(b, s3.WithCredentialSource(creds)), nil
	},
	"webdav": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		if identityRegistry == nil {
			return nil, fmt.Errorf("webdav frontend requires an identity registry (auth configuration failed earlier?)")
		}
		authnr := auth.NewBasicAuthenticator(identityRegistry)
		// WebDAV locking (webdav-locking-2026-10): the per-bucket lock
		// store lives under the bucket's own .metadata/.locks/ — the same
		// getBucketPath math the s3 staging uses.
		return webdav.New(b, webdav.Config{Bucket: cfg.Bucket}, webdav.WithAuthenticator(authnr),
			webdav.WithLockStoreRoot(getBucketPath))
	},
	"owncloud": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		if identityRegistry == nil {
			return nil, fmt.Errorf("owncloud frontend requires an identity registry (auth configuration failed earlier?)")
		}
		authnr := auth.NewBasicAuthenticator(identityRegistry)
		wd, err := webdav.New(b, webdav.Config{Bucket: cfg.Bucket}, webdav.WithAuthenticator(authnr),
			webdav.WithLockStoreRoot(getBucketPath))
		if err != nil {
			return nil, err
		}
		// Mode B: the classic client speaks /remote.php/webdav/<bucket>/<path>
		// and the wrapped webdav re-roots at the configured bucket, so the
		// bucket segment must be stripped before delegation (MKCOL of
		// "<anything>/dir" 409'd on the phantom parent). The wrapper's
		// serveHTTP strips the classic /remote.php/webdav prefix first
		// (owncloud.go); this second strip removes the bucket segment that
		// remains, whether or not the classic prefix was present.
		ocPrefix := ""
		if cfg.Bucket != "" {
			ocPrefix = "/" + cfg.Bucket
		}
		oc, err := owncloud.NewWithPathPrefix(wd, ocPrefix)
		if err != nil {
			return nil, err
		}
		// The owncloud frontend implements the frontend seam directly;
		// the wrapped webdav frontend stays unregistered (one wire
		// identity per listener: "owncloud").
		return oc, nil
	},
	"ftp": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		if identityRegistry == nil {
			return nil, fmt.Errorf("ftp frontend requires an identity registry (auth configuration failed earlier?)")
		}
		if cfg.Bucket != "" {
			return nil, fmt.Errorf(`frontend type "ftp" does not accept the "bucket" key (webdav only)`)
		}
		if err := validateOptions(cfg.Type, cfg.Options, ftp.KnownOptionKeys); err != nil {
			return nil, err
		}
		tlsCfg, err := loadServerTLSCertPair()
		if err != nil {
			return nil, err
		}
		ftpCfg, err := ftp.ConfigFromOptions(cfg.ListenAddr, cfg.Options, tlsCfg,
			ftp.NewRegistryVerifier(identityRegistry))
		if err != nil {
			return nil, err
		}
		return ftp.New(backendResolver(), ftpCfg)
	},
	"sftp": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		if identityRegistry == nil {
			return nil, fmt.Errorf("sftp frontend requires an identity registry (auth configuration failed earlier?)")
		}
		if cfg.Bucket != "" {
			return nil, fmt.Errorf(`frontend type "sftp" does not accept the "bucket" key (webdav only)`)
		}
		if err := validateOptions(cfg.Type, cfg.Options, sftp.KnownOptionKeys); err != nil {
			return nil, err
		}
		sftpCfg, err := sftp.ConfigFromOptions(cfg.ListenAddr, cfg.Options, identityRegistry)
		if err != nil {
			return nil, err
		}
		return sftp.New(backendResolver(), sftpCfg)
	},
	// admin is the mTLS management frontend (management-api-2026-10 leaf 01):
	// it owns a dedicated TLS listener whose client-certificate verification
	// is expressed through the positional interface. The CA bundle is REQUIRED
	// and is read fail-loud; the route services and the write-only audit seam
	// are injected here (leaf 04).
	"admin": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		if cfg.Bucket != "" {
			return nil, fmt.Errorf(`frontend type "admin" does not accept the "bucket" key (webdav only)`)
		}
		if err := validateOptions(cfg.Type, cfg.Options, admin.KnownOptionKeys); err != nil {
			return nil, err
		}
		wiring := buildAdminWiring(s3.AppendAudit)
		f, err := admin.New(admin.Options{
			ListenAddr:       cfg.ListenAddr,
			ClientCAFile:     cfg.Options["clientCAFile"],
			AdminPrincipals:  splitOptionList(cfg.Options["adminPrincipals"]),
			AllowNonLoopback: cfg.Options["allowNonLoopback"] == "true",
			CertFile:         serverConfig.CertFile,
			KeyFile:          serverConfig.KeyFile,
			Services:         wiring.services,
			Audit:            wiring.audit,
		})
		if err != nil {
			return nil, err
		}
		// Register the frontend's CA-reload entry point so /auth/reload can
		// refresh the trusted client CA without a restart (leaf 04 Task 4).
		// The registry holds MANY registrants: this one must not displace the
		// h3 frontend's own entry, or the QUIC listener would keep trusting its
		// startup CA for the process lifetime.
		if ca, ok := f.(admin.ClientCAReloader); ok {
			registerClientCAReloader(f.Name(), ca.ReloadClientCA)
		}
		return f, nil
	},
	// h3 is the HTTP/3 (QUIC) frontend (quic-h3-2026-10 leaf 02): it wraps
	// a webdav frontend built with the SAME constructor options as the
	// webdav entry above (single-bucket re-root, per-bucket lock store
	// root) but authenticates with mTLS — the client certificate's Subject
	// CN resolved through the identity registry (CertAuthenticator). The
	// CA bundle is REQUIRED and read fail-loud. It owns a dedicated UDP
	// listener (QUICListenerFrontend seam) served by quic-go in main.
	"h3": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		if err := validateOptions(cfg.Type, cfg.Options, h3.KnownOptionKeys); err != nil {
			return nil, err
		}
		f, err := h3.New(b, h3.Config{
			ListenAddr:   cfg.ListenAddr,
			Bucket:       cfg.Bucket,
			ClientCAFile: cfg.Options["clientCAFile"],
			CertFile:     serverConfig.CertFile,
			KeyFile:      serverConfig.KeyFile,
		}, identityRegistry, getBucketPath)
		if err != nil {
			return nil, err
		}
		// Same registration the admin frontend gets above, through the same
		// optional interface: POST /auth/reload must swap the QUIC listener's
		// trusted CA pool too, or replacing clientCAFile to revoke a stolen
		// device certificate is a silent no-op over HTTP/3 (h3.Frontend
		// satisfies admin.ClientCAReloader by shape - the reload path stays off
		// the frozen frontend seams).
		var fe frontend.Frontend = f
		if ca, ok := fe.(admin.ClientCAReloader); ok {
			registerClientCAReloader(fe.Name(), ca.ReloadClientCA)
		}
		return fe, nil
	},
}

// splitOptionList splits a comma-separated option value ("alice,bob") into a
// trimmed, non-empty list. Absent/empty yields nil.
func splitOptionList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validateOptions checks every options key is known to the owning frontend
// (fail-loud; the error names the key and the known set).
func validateOptions(frontendType string, options map[string]string, known map[string]bool) error {
	for k := range options {
		if !known[k] {
			knownList := make([]string, 0, len(known))
			for name := range known {
				knownList = append(knownList, name)
			}
			sort.Strings(knownList)
			return fmt.Errorf("frontend %q: unknown option key %q (known: %v)", frontendType, k, knownList)
		}
	}
	return nil
}

// backendResolver resolves a bucket to its Backend through the same
// config-driven table the s3 handlers use (installed by initBackendLookup
// before frontends are constructed in main(); errors here abort startup).
func backendResolver() backend.Backend {
	return &perBucketBackend{lookup: backendFor}
}

// perBucketBackend routes each bucket to its configured Backend via the
// process resolver (the ftp/sftp frontends speak whole-bucket paths, so the
// per-bucket selection semantics match s3 exactly).
type perBucketBackend struct {
	lookup func(bucket string) (backend.Backend, error)
}

func (p *perBucketBackend) backendForBucket(bucket string) (backend.Backend, error) {
	if p == nil || p.lookup == nil {
		return nil, fmt.Errorf("backend resolver not installed")
	}
	return p.lookup(bucket)
}

func (p *perBucketBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	b, err := p.backendForBucket(bucket)
	if err != nil {
		return nil, objectmodel.Object{}, err
	}
	return b.Get(ctx, bucket, key, opts)
}

func (p *perBucketBackend) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	b, err := p.backendForBucket(bucket)
	if err != nil {
		return objectmodel.Object{}, err
	}
	return b.Put(ctx, bucket, key, data, size, opts)
}

func (p *perBucketBackend) Delete(ctx context.Context, bucket, key string) error {
	b, err := p.backendForBucket(bucket)
	if err != nil {
		return err
	}
	return b.Delete(ctx, bucket, key)
}

func (p *perBucketBackend) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	b, err := p.backendForBucket(bucket)
	if err != nil {
		return objectmodel.Object{}, err
	}
	return b.Stat(ctx, bucket, key)
}

func (p *perBucketBackend) List(ctx context.Context, bucket string, params objectmodel.ListParams) (objectmodel.ListPage, error) {
	b, err := p.backendForBucket(bucket)
	if err != nil {
		return objectmodel.ListPage{}, err
	}
	return b.List(ctx, bucket, params)
}

func (p *perBucketBackend) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	b, err := p.backendForBucket("")
	if err != nil {
		return nil, err
	}
	return b.Buckets(ctx)
}

func (p *perBucketBackend) Capabilities() objectmodel.CapabilitySet {
	b, err := p.backendForBucket("")
	if err != nil {
		return objectmodel.CapabilitySet{}
	}
	return b.Capabilities()
}

// loadServerTLSCertPair loads the server certFile/keyFile for FTP AUTH TLS
// (explicit TLS reuses the HTTPS cert pair — no new config keys).
func loadServerTLSCertPair() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(serverConfig.CertFile, serverConfig.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("ftp frontend: loading certFile/keyFile for AUTH TLS: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// frontendMountKey is the uniqueness key for configured frontend entries:
// the SAME (type, listenAddr, bucket) triple repeated is an ambiguous
// mount (which handler would serve which listener?); any differing member
// makes the second entry a legal distinct mount (e.g. webdav mode A +
// mode B in the README / e2e case 19 shape).
type frontendMountKey struct {
	typ        string
	listenAddr string
	bucket     string
}

// frontendMount pairs a constructed frontend with its listen address
// (empty = share the default listener's mux).
type frontendMount struct {
	frontend   frontend.Frontend
	listenAddr string
}

// listenerSpec is a frontend needing its own dedicated TLS listener.
type listenerSpec struct {
	frontend frontend.Frontend
	addr     string
	// tlsConfig is non-nil only for a TLSListenerFrontend: the pre-built
	// configuration main must serve this listener with (client-certificate
	// verification). A nil tlsConfig keeps the process-wide cert pair path.
	tlsConfig *tls.Config
	// quicConfig is non-nil only for a QUICListenerFrontend (HTTP/3): the
	// pre-built TLS configuration for the UDP/QUIC listener (quic-h3-2026-10
	// leaf 02 Contract 2). A QUIC-listener frontend is NEVER served through
	// the TCP ListenAndServeTLS path — main's UDP branch consumes this.
	quicConfig *tls.Config
}

// buildFrontends constructs each configured frontend, registers it, and
// returns the registry plus mount plans. Startup is loud: an unknown type
// fails with the known-type list, an AMBIGUOUS duplicate fails (same type
// on the same listenAddr with the same bucket), and a factory error is
// wrapped and returned — all abort startup.
//
// The duplicate key is (type, listenAddr, bucket): a second webdav entry
// with its own listenAddr (mode A + mode B) or its own bucket is a legal
// distinct mount (README / e2e case 19), while the identical shape is an
// ambiguous mount and is rejected.
//
// Registry names are unique per type, so a repeated type shares one
// registry entry; the mount plan (below) is what gives each entry its own
// listener. The first constructed frontend wins the registry slot; the
// collision (same Name()) is ignored and left to the ambiguity check above.
func buildFrontends(cfg []FrontendConfig, b backend.Backend, creds auth.CredentialSource) (*frontend.Registry, []frontendMount, error) {
	if len(cfg) == 0 {
		cfg = []FrontendConfig{{Type: "s3"}}
	}
	reg := frontend.NewRegistry()
	var mounts []frontendMount
	seen := map[frontendMountKey]bool{}
	seenTypes := map[string]bool{} // for the registry-tolerance check below
	for _, fc := range cfg {
		key := frontendMountKey{typ: fc.Type, listenAddr: fc.ListenAddr, bucket: fc.Bucket}
		if seen[key] {
			return nil, nil, fmt.Errorf("frontend type %q configured more than once with identical listenAddr %q and bucket %q (an ambiguous mount — give each entry its own listenAddr or bucket)",
				fc.Type, fc.ListenAddr, fc.Bucket)
		}
		seen[key] = true
		repeatType := seenTypes[fc.Type]
		factory, ok := frontendFactories[fc.Type]
		if !ok {
			known := make([]string, 0, len(frontendFactories))
			for k := range frontendFactories {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, nil, fmt.Errorf("unknown frontend type %q (known: %v)", fc.Type, known)
		}
		f, err := factory(fc, b, creds)
		if err != nil {
			return nil, nil, fmt.Errorf("build frontend %q: %w", fc.Type, err)
		}
		if err := reg.Register(f); err != nil {
			// A repeated config type (mode A + mode B webdav) reaches
			// this branch with "already registered": that is fine —
			// both mounts carry the same handler kind, and each mount
			// below gets its own listener. Any OTHER register failure
			// (a foreign name collision, nil frontend, empty name) is
			// still a loud startup error.
			if !repeatType {
				return nil, nil, fmt.Errorf("register frontend %q: %w", fc.Type, err)
			}
		}
		seenTypes[fc.Type] = true
		mounts = append(mounts, frontendMount{frontend: f, listenAddr: fc.ListenAddr})
	}
	return reg, mounts, nil
}

// mountFrontends registers shared-mux handlers ("/") and returns the specs
// that need dedicated listeners. Two or more shared mounts would double-
// register "/" and panic (http.ServeMux panics on duplicate patterns), so
// that condition is rejected up front with a config error naming the
// frontends (bughunt C7). It returns the shared handlers registered (for
// asserts in tests).
func mountFrontends(mux *http.ServeMux, mounts []frontendMount) (shared []frontend.Frontend, extra []listenerSpec, err error) {
	for _, m := range mounts {
		if m.listenAddr == "" {
			// A QUIC-listener frontend (HTTP/3) owns its dedicated UDP
			// listener and its TLS configuration; without a listenAddr it
			// would fall back to the shared mux — but an empty address is
			// NEVER a shared-mux fallback for QUIC (quic-h3-2026-10 leaf 02
			// Contract 2). Loud startup error naming the rule. Checked
			// BEFORE the TLS-listener branch: a QUIC frontend must never
			// be reported (or served) through the TCP TLS-listener path.
			if q, ok := m.frontend.(frontend.QUICListenerFrontend); ok {
				return nil, nil, fmt.Errorf("frontend %q requires its own listenAddr (a QUIC-listener frontend cannot share the default HTTPS mux)", q.Name())
			}
			// A TLS-listener frontend owns its dedicated listener and its
			// TLS configuration; without a listenAddr it would fall back to
			// the shared mux, silently dropping its mTLS settings. Loud
			// startup error naming the rule (management-api-2026-10
			// Contract 1).
			if tl, ok := m.frontend.(frontend.TLSListenerFrontend); ok {
				return nil, nil, fmt.Errorf("frontend %q requires its own listenAddr (a TLS-listener frontend cannot share the default HTTPS mux)", tl.Name())
			}
			// A non-HTTP frontend (FTP/SFTP) cannot express itself as an
			// http.Handler — mounting it on the shared mux would serve its
			// 501 stub on every path. Loud startup error instead
			// (sftp-ftp-2026-09 master Contract A).
			if nh, ok := m.frontend.(frontend.NonHTTPFrontend); ok {
				return nil, nil, fmt.Errorf("frontend %q is a non-HTTP frontend and requires its own listenAddr (it cannot share the default HTTPS mux)", nh.Name())
			}
			if len(shared) > 0 {
				return nil, nil, fmt.Errorf("frontend %q cannot share the default listener: frontend %q is already mounted on it (give one of them its own listenAddr)",
					m.frontend.Name(), shared[0].Name())
			}
			mux.Handle("/", m.frontend.Handler())
			shared = append(shared, m.frontend)
			continue
		}
		spec := listenerSpec{frontend: m.frontend, addr: m.listenAddr}
		if q, ok := m.frontend.(frontend.QUICListenerFrontend); ok && q.IsQUICListener() {
			cfg, err := q.TLSConfig()
			if err != nil {
				return nil, nil, fmt.Errorf("frontend %q: building QUIC listener config: %w", q.Name(), err)
			}
			spec.quicConfig = cfg
		} else if tl, ok := m.frontend.(frontend.TLSListenerFrontend); ok {
			// else-if: a QUIC-listener frontend is NEVER classified as a
			// TCP TLS listener (its pre-built config goes to main's UDP
			// branch only — quic-h3-2026-10 leaf 02 Contract 2).
			cfg, err := tl.TLSConfig()
			if err != nil {
				return nil, nil, fmt.Errorf("frontend %q: building TLS listener config: %w", tl.Name(), err)
			}
			spec.tlsConfig = cfg
		}
		extra = append(extra, spec)
	}
	return shared, extra, nil
}

// startupPlan is the extracted pure function from main(): given the
// frontends config it returns the mux mounts and dedicated listener specs
// WITHOUT opening any listener (backward compatibility is testable without
// binding ports).
type startupPlanT struct {
	registry  *frontend.Registry
	mux       *http.ServeMux
	shared    []frontend.Frontend // mounted on the default mux
	mounts    []frontendMount     // every configured mount (shared + dedicated); findQUICFrontend scans this
	listeners []listenerSpec      // dedicated TLS listeners
}

func startupPlan(cfg []FrontendConfig, b backend.Backend, creds auth.CredentialSource) (startupPlanT, error) {
	reg, mounts, err := buildFrontends(cfg, b, creds)
	if err != nil {
		return startupPlanT{}, err
	}
	mux := http.NewServeMux()
	shared, listeners, err := mountFrontends(mux, mounts)
	if err != nil {
		return startupPlanT{}, err
	}
	return startupPlanT{registry: reg, mux: mux, shared: shared, mounts: mounts, listeners: listeners}, nil
}

// applyListenAddrOverride applies the ZETAOBJECT_LISTEN_ADDR env override to the
// DEFAULT listener only; per-frontend listenAddr values are untouched.
// A set-but-EMPTY value is warned about and ignored, matching how the
// credential env vars treat empty (bughunt E7) — it must not silently
// behave like an unset variable.
func applyListenAddrOverride(cfg *ServerConfig) {
	value, ok := os.LookupEnv("ZETAOBJECT_LISTEN_ADDR")
	if !ok {
		// Deprecated-prefix fallback (bughunt H3): operators with the
		// pre-rename variable in unit files must not silently lose the
		// override.
		value, ok = os.LookupEnv("MINIS3_LISTEN_ADDR")
		if !ok {
			return
		}
		log.Printf("Note: MINIS3_LISTEN_ADDR is deprecated; set ZETAOBJECT_LISTEN_ADDR instead")
	}
	if value == "" {
		log.Printf("Warning: environment variable ZETAOBJECT_LISTEN_ADDR is set but empty; ignoring (config listenAddr stays %s)", cfg.ListenAddr)
		return
	}
	cfg.ListenAddr = value
}
