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

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
	ftp "github.com/bhodgens/zeta-object/internal/frontend/ftp"
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
		return webdav.New(b, webdav.Config{Bucket: cfg.Bucket}, webdav.WithAuthenticator(authnr))
	},
	"owncloud": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		if identityRegistry == nil {
			return nil, fmt.Errorf("owncloud frontend requires an identity registry (auth configuration failed earlier?)")
		}
		authnr := auth.NewBasicAuthenticator(identityRegistry)
		wd, err := webdav.New(b, webdav.Config{Bucket: cfg.Bucket}, webdav.WithAuthenticator(authnr))
		if err != nil {
			return nil, err
		}
		oc, err := owncloud.New(wd)
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
}

// buildFrontends constructs each configured frontend, registers it, and
// returns the registry plus mount plans. Startup is loud: an unknown type
// fails with the known-type list, a duplicate type fails, and a factory
// error is wrapped and returned — all abort startup.
func buildFrontends(cfg []FrontendConfig, b backend.Backend, creds auth.CredentialSource) (*frontend.Registry, []frontendMount, error) {
	if len(cfg) == 0 {
		cfg = []FrontendConfig{{Type: "s3"}}
	}
	reg := frontend.NewRegistry()
	var mounts []frontendMount
	seen := map[string]bool{}
	for _, fc := range cfg {
		if seen[fc.Type] {
			return nil, nil, fmt.Errorf("frontend type %q configured more than once", fc.Type)
		}
		seen[fc.Type] = true
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
			return nil, nil, fmt.Errorf("register frontend %q: %w", fc.Type, err)
		}
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
		extra = append(extra, listenerSpec{frontend: m.frontend, addr: m.listenAddr})
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
	return startupPlanT{registry: reg, mux: mux, shared: shared, listeners: listeners}, nil
}

// applyListenAddrOverride applies the ZETAOBJECT_LISTEN_ADDR env override to the
// DEFAULT listener only; per-frontend listenAddr values are untouched.
// A set-but-EMPTY value is warned about and ignored, matching how the
// credential env vars treat empty (bughunt E7) — it must not silently
// behave like an unset variable.
func applyListenAddrOverride(cfg *ServerConfig) {
	value, ok := os.LookupEnv("ZETAOBJECT_LISTEN_ADDR")
	if !ok {
		return
	}
	if value == "" {
		log.Printf("Warning: environment variable ZETAOBJECT_LISTEN_ADDR is set but empty; ignoring (config listenAddr stays %s)", cfg.ListenAddr)
		return
	}
	cfg.ListenAddr = value
}
