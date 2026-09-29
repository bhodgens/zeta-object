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
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"

	"mini-s3/internal/auth"
	"mini-s3/internal/backend"
	"mini-s3/internal/frontend"
	s3 "mini-s3/internal/frontend/s3"
)

// frontendFactories maps config Type -> constructor. Future frontends
// (webdav, ftp/sftp, owncloud — see their GH issues) add one entry each.
var frontendFactories = map[string]func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error){
	"s3": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		return s3.New(b, s3.WithCredentialSource(creds)), nil
	},
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

// applyListenAddrOverride applies the MINIS3_LISTEN_ADDR env override to the
// DEFAULT listener only; per-frontend listenAddr values are untouched.
// A set-but-EMPTY value is warned about and ignored, matching how the
// credential env vars treat empty (bughunt E7) — it must not silently
// behave like an unset variable.
func applyListenAddrOverride(cfg *ServerConfig) {
	value, ok := os.LookupEnv("MINIS3_LISTEN_ADDR")
	if !ok {
		return
	}
	if value == "" {
		log.Printf("Warning: environment variable MINIS3_LISTEN_ADDR is set but empty; ignoring (config listenAddr stays %s)", cfg.ListenAddr)
		return
	}
	cfg.ListenAddr = value
}
