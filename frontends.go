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
// that need dedicated listeners. It returns the shared handlers registered
// (for asserts in tests).
func mountFrontends(mux *http.ServeMux, mounts []frontendMount) (shared []frontend.Frontend, extra []listenerSpec) {
	for _, m := range mounts {
		if m.listenAddr == "" {
			mux.Handle("/", m.frontend.Handler())
			shared = append(shared, m.frontend)
			continue
		}
		extra = append(extra, listenerSpec{frontend: m.frontend, addr: m.listenAddr})
	}
	return shared, extra
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
	shared, listeners := mountFrontends(mux, mounts)
	return startupPlanT{registry: reg, mux: mux, shared: shared, listeners: listeners}, nil
}

// applyListenAddrOverride applies the MINIS3_LISTEN_ADDR env override to the
// DEFAULT listener only; per-frontend listenAddr values are untouched.
func applyListenAddrOverride(cfg *ServerConfig) {
	if listenAddr := os.Getenv("MINIS3_LISTEN_ADDR"); listenAddr != "" {
		cfg.ListenAddr = listenAddr
	}
}
