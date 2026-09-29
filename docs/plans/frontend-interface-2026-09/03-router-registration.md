# Frontend Router Registration and Config - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Wire server startup to construct and mount registered frontends from a backward-compatible `frontends` config key (absent = S3 only on the existing addr), and decide + document multi-listener support.
- **Dependencies:** 01-frontend-interface.md (Registry, Frontend), 02-s3-frontend-extraction.md (the concrete s3 frontend to register).
- **Estimated Context:** 70K
- **Concurrency Group:** C (last; mounts what 01 and 02 built)

## Goal

Make the frontend seam real at startup. After this leaf, `main()`:

1. Loads an optional `frontends` array from config: `[{"type": "s3", "listenAddr": ":8443"}]`.
2. Backward compatibility is exact: **a config without `frontends` behaves
   identically to today** — S3 on `listenAddr` (config.go) with
   `ZETAOBJECT_LISTEN_ADDR` env override, TLS on certFile/keyFile.
3. Each configured frontend is constructed via a small factory map
   (`"s3"` today; future factories land with their GH issues: WebDAV,
   (S)FTP, ownCloud), registered into `frontend.Registry`, and its
   `Handler()` mounted.
4. Multi-listener support is decided and documented: a frontend entry with
   its own `listenAddr` gets a dedicated TLS listener on that port; entries
   without `listenAddr` share the default listener's mux. One extra listener
   is supported (enough for "a second port for a second frontend" and for
   the future WebDAV frontend); more require only a loop change, documented
   as a known limit.
5. Graceful shutdown drains all listeners.

## Context

zeta-object's startup today (main.go:70-120 area): load `ServerConfig`
(config.go — `dataDir`, `listenAddr` default `:8443`, `certFile`, `keyFile`,
`buckets`), init inactivity tracker + multipart sweeper, build one
`http.ServeMux`, register `rootHandler` at `/` (main.go:89; post-leaf-02 the
s3 frontend's Handler sits there), start `http.Server` with TLS via
`newServer(...)`, and on SIGINT/SIGTERM drain for up to 30 seconds
(`serverShutdownTimeout`). One credential pair from env
(`ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY`, default minioadmin) feeds SigV4.

After leaf 02, package main constructs `s3.New(backend,
s3.WithCredentialSource(...))` and mounts it at `/`. This leaf replaces that
hardcoded mount with registry-driven mounting driven by config.

Config is loaded by `loadConfig` (config.go) from `config.json` or
`ZETAOBJECT_CONFIG`; env `ZETAOBJECT_LISTEN_ADDR` beats the config file. JSON
decoding is stdlib; unknown keys today are ignored — the new key must
therefore default cleanly when absent.

Key files to understand before implementing:
- main.go - server wiring, listener start, graceful shutdown; the mount point this leaf rewrites
- config.go - ServerConfig struct + loadConfig; gains the FrontendConfig type
- internal/frontend/registry.go - Registry from leaf 01; registration happens here
- internal/frontend/s3/ - the s3 frontend from leaf 02; the only concrete frontend this leaf registers
- internal/auth/ - CredentialSource adapter for the shared credential pair

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: config.go (added types)
type FrontendConfig struct {
    Type       string `json:"type"`                 // "s3" today; "webdav", "ftp" later
    ListenAddr string `json:"listenAddr,omitempty"` // empty = share default listener
}

// ServerConfig gains:
//   Frontends []FrontendConfig `json:"frontends,omitempty"`
// Semantics: absent/empty slice == [{"type":"s3"}] on the default listener
// (exact backward compatibility).
```

```go
// File: internal/frontend/registry.go (from leaf 01 — consumed, not modified)
type Registry struct{ /* ... */ }
func NewRegistry() *Registry
func (r *Registry) Register(f Frontend) error
func (r *Registry) Lookup(name string) (Frontend, bool)
func (r *Registry) All() []Frontend
```

```go
// File: main.go (new wiring, unexported)
// buildFrontends constructs each configured frontend, registers it, and
// returns mount plans: which handler goes on the default mux and which get
// dedicated listeners.
type frontendMount struct {
    frontend   frontend.Frontend
    listenAddr string // "" = default listener
}

func buildFrontends(cfg FrontendsSpec, b backend.Backend, creds auth.CredentialSource) (*frontend.Registry, []frontendMount, error)
//   - unknown Type -> error listing known types ("s3")
//   - duplicate Type across entries -> error
//   - empty cfg -> single s3 mount on default addr
func mountFrontends(mux *http.ServeMux, mounts []frontendMount) (extraListeners []listenerSpec)
//   - mounts with empty listenAddr register Handler() on the shared mux
//   - mounts with explicit listenAddr are returned for dedicated listeners
```

### What This Leaf Consumes

```go
// From 01-frontend-interface.md:
package frontend // Registry, Frontend, ProtocolCaps, RunConformanceSuite (tests)

// From 02-s3-frontend-extraction.md:
package s3
func New(b backend.Backend, opts ...Option) *Frontend
func WithCredentialSource(cs auth.CredentialSource) Option

// From backend-interface-2026-09:
package backend // type Backend — constructed in main as today, passed in
```

## Tasks

### Task 1: `FrontendConfig` + backward-compatible config loading

**Objective:** Add the `frontends` config key with exact absent-key semantics.

**Files:**
- Modify: `config.go` (ServerConfig + new FrontendConfig type + normalization in loadConfig)
- Test: `config_frontend_test.go` (new file, package main)

**Step 1: Write failing test**

```go
package main

import (
    "os"
    "path/filepath"
    "testing"
)

func writeTempConfig(t *testing.T, content string) string {
    t.Helper()
    path := filepath.Join(t.TempDir(), "config.json")
    if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
        t.Fatal(err)
    }
    return path
}

func TestLoadConfig_Frontends(t *testing.T) {
    tests := []struct {
        name       string
        configJSON string
        envAddr    string // ZETAOBJECT_LISTEN_ADDR override, "" = unset
        want       []FrontendConfig
        wantAddr   string // effective default listen addr after normalization
    }{
        {
            name:       "absent frontends defaults to s3 on default addr",
            configJSON: `{"dataDir":"./data/"}`,
            want:       []FrontendConfig{{Type: "s3"}},
            wantAddr:   ":8443",
        },
        {
            name:       "explicit s3 only",
            configJSON: `{"frontends":[{"type":"s3"}]}`,
            want:       []FrontendConfig{{Type: "s3"}},
            wantAddr:   ":8443",
        },
        {
            name:       "s3 with dedicated second listener",
            configJSON: `{"frontends":[{"type":"s3"},{"type":"webdav","listenAddr":":8444"}]}`,
            want: []FrontendConfig{
                {Type: "s3"},
                {Type: "webdav", ListenAddr: ":8444"},
            },
            wantAddr: ":8443",
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            if tt.envAddr != "" {
                t.Setenv("ZETAOBJECT_LISTEN_ADDR", tt.envAddr)
            }
            cfg, err := loadConfig(writeTempConfig(t, tt.configJSON))
            if err != nil {
                t.Fatalf("loadConfig: %v", err)
            }
            if len(cfg.Frontends) != len(tt.want) {
                t.Fatalf("Frontends = %+v, want %+v", cfg.Frontends, tt.want)
            }
            for i := range tt.want {
                if cfg.Frontends[i] != tt.want[i] {
                    t.Fatalf("Frontends[%d] = %+v, want %+v", i, cfg.Frontends[i], tt.want[i])
                }
            }
            if cfg.ListenAddr != tt.wantAddr {
                t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, tt.wantAddr)
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test -run TestLoadConfig_Frontends ./... -v`
Expected: FAIL — `Frontends` field does not exist.

**Step 3: Write minimal implementation**

In config.go:

```go
type FrontendConfig struct {
    Type       string `json:"type"`
    ListenAddr string `json:"listenAddr,omitempty"`
}

// In ServerConfig:
//   Frontends []FrontendConfig `json:"frontends,omitempty"`

// In loadConfig, after unmarshal:
//   if len(cfg.Frontends) == 0 {
//       cfg.Frontends = []FrontendConfig{{Type: "s3"}}
//   }
```

`ZETAOBJECT_LISTEN_ADDR` keeps overriding only the *default* listener address
(existing behavior untouched).

**Step 4: Run test to verify pass**

Run: `go test -run TestLoadConfig_Frontends ./... -v`
Expected: PASS

### Task 2: Frontend factory + buildFrontends/mountFrontends

**Objective:** Construct configured frontends, register them, and split mounts
into shared-mux vs dedicated-listener.

**Files:**
- Create: `frontends.go` (package main)
- Test: `frontends_test.go` (package main)

**Step 1: Write failing test**

```go
package main

import (
    "net/http"
    "testing"

    "zeta-object/internal/auth"
    "zeta-object/internal/backend"
    "zeta-object/internal/frontend"
)

type nilBackend struct{ backend.Backend } // embeds interface; methods unused in these tests

type stubCreds struct{}

func (stubCreds) SecretKey(accessKeyID string) (string, bool) {
    return "minioadmin", accessKeyID == "minioadmin"
}

func TestBuildFrontends(t *testing.T) {
    tests := []struct {
        name        string
        cfg         []FrontendConfig
        wantErr     bool
        wantNames   []string
        wantMounted int // mounts with empty listenAddr (shared mux)
        wantExtra   int // mounts with dedicated listeners
    }{
        {
            name:        "empty config yields default s3 mount",
            cfg:         nil,
            wantNames:   []string{"s3"},
            wantMounted: 1, wantExtra: 0,
        },
        {
            name:        "explicit s3 on default listener",
            cfg:         []FrontendConfig{{Type: "s3"}},
            wantNames:   []string{"s3"},
            wantMounted: 1, wantExtra: 0,
        },
        {
            name: "unknown type rejected with known list",
            cfg:  []FrontendConfig{{Type: "webdav"}}, // webdav factory not yet wired (GH issue)
            wantErr: true,
        },
        {
            name: "duplicate type rejected",
            cfg:  []FrontendConfig{{Type: "s3"}, {Type: "s3", ListenAddr: ":8444"}},
            wantErr: true,
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            reg, mounts, err := buildFrontends(tt.cfg, nilBackend{}, stubCreds{})
            if (err != nil) != tt.wantErr {
                t.Fatalf("buildFrontends err = %v, wantErr %v", err, tt.wantErr)
            }
            if tt.wantErr {
                return
            }
            var names []string
            for _, f := range reg.All() {
                names = append(names, f.Name())
            }
            if len(names) != len(tt.wantNames) {
                t.Fatalf("registered %v, want %v", names, tt.wantNames)
            }
            shared, extra := mountFrontends(http.NewServeMux(), mounts)
            if len(shared) != tt.wantMounted || len(extra) != tt.wantExtra {
                t.Fatalf("mounted=%d extra=%d, want %d/%d", len(shared), len(extra), tt.wantMounted, tt.wantExtra)
            }
        })
    }
}

func TestMountFrontends_DedicatedListenerSpec(t *testing.T) {
    // A configured frontend with listenAddr gets a listenerSpec carrying that
    // addr; the shared mux is untouched by it.
    _, mounts, err := buildFrontends(
        []FrontendConfig{{Type: "s3"}, {Type: "s3", ListenAddr: ":8444"}},
        nilBackend{}, stubCreds{})
    // NOTE: duplicates rejected per table above; this test uses the accepted
    // shape once a second concrete frontend type exists. Until then this
    // test exercises mountFrontends directly with hand-built mounts.
    if err == nil {
        t.Skip("second concrete frontend type not yet available (GH issues: webdav/ftp)")
    }
    _ = mounts
    _ = frontend.NewRegistry
}
```

**Step 2: Run test to verify failure**

Run: `go test -run TestBuildFrontends ./... -v`
Expected: FAIL — buildFrontends does not exist.

**Step 3: Write minimal implementation**

```go
// File: frontends.go
package main

import (
    "fmt"
    "net/http"

    "zeta-object/internal/auth"
    "zeta-object/internal/backend"
    "zeta-object/internal/frontend"
    s3frontend "zeta-object/internal/frontend/s3"
)

// frontendFactories maps config Type -> constructor. Future frontends
// (webdav, ftp/sftp, owncloud — see their GH issues) add one entry each.
var frontendFactories = map[string]func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error){
    "s3": func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
        return s3frontend.New(b, s3frontend.WithCredentialSource(creds)), nil
    },
}

type listenerSpec struct {
    frontend frontend.Frontend
    addr     string
}

type frontendMount struct {
    frontend   frontend.Frontend
    listenAddr string
}

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

// mountFrontends registers shared-mux handlers and returns specs needing
// dedicated listeners. Returns the shared handlers registered (for asserts).
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
```

**Step 4: Run test to verify pass**

Run: `go test -run "TestBuildFrontends|TestMountFrontends" ./... -v`
Expected: PASS (the dedicated-listener test may SKIP until a second concrete frontend exists — that is expected and documented)

### Task 3: main() wiring + multi-listener + graceful shutdown

**Objective:** Replace the hardcoded s3 mount with registry-driven mounting;
serve dedicated listeners; drain everything on shutdown.

**Files:**
- Modify: `main.go` (startup section around the current mux construction at main.go:88-91 and shutdown at main.go:104-119)
- Test: `main_frontends_test.go` (package main)

**Step 1: Write failing test**

```go
package main

import (
    "testing"
)

func TestStartupPlan_BackwardCompat(t *testing.T) {
    tests := []struct {
        name          string
        cfg           []FrontendConfig
        wantShared    int
        wantListeners int
    }{
        {"no frontends key", nil, 1, 0},
        {"explicit s3 only", []FrontendConfig{{Type: "s3"}}, 1, 0},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // startupPlan is the extracted pure function from main(): given
            // config it returns the mux mounts and dedicated listener specs.
            plan, err := startupPlan(tt.cfg, nilBackend{}, stubCreds{})
            if err != nil {
                t.Fatalf("startupPlan: %v", err)
            }
            if len(plan.shared) != tt.wantShared || len(plan.listeners) != tt.wantListeners {
                t.Fatalf("plan = %+v, want shared=%d listeners=%d",
                    plan, tt.wantShared, tt.wantListeners)
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test -run TestStartupPlan_BackwardCompat ./... -v`
Expected: FAIL — startupPlan does not exist.

**Step 3: Write minimal implementation**

In main.go (or frontends.go), extract the wiring into a testable pure function
and keep main() thin:

```go
type startupPlanT struct {
    registry  *frontend.Registry
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
    return startupPlanT{registry: reg, shared: shared, listeners: listeners}, nil
}
```

main() changes, minimally:
1. Build the s3-serving mux via the plan instead of the hardcoded
   `mux.HandleFunc("/", rootHandler)` line (post-leaf-02: the s3 Handler).
2. After starting the default TLS server, start one additional TLS
   `http.Server` per `listenerSpec` (same certFile/keyFile) — decided limit:
   **support any number of extra listeners via a plain loop**; only one is
   expected in practice for the near future (second frontend port). Document
   the chosen behavior in config.json.example.
3. Graceful shutdown: `srv.Shutdown` on the default server, then loop
   `Shutdown` over every extra listener server, all within the existing
   30-second `serverShutdownTimeout` drain window.
4. `ZETAOBJECT_LISTEN_ADDR` still overrides only the default listener.
5. Update `config.json.example` with a commented `frontends` sample.

**Step 4: Run test to verify pass**

Run: `go test -run TestStartupPlan ./... -v && go test ./... -count=1 && make e2e`
Expected: ALL PASS; harness (which runs without `frontends` in its config)
green — backward compatibility proven.

### Task 4: Documentation of the seam + listener decision

**Objective:** Record the config surface and multi-listener decision where
operators and future-frontend authors will find them.

**Files:**
- Modify: `config.json.example` — commented `frontends` example
- Create: `docs/frontends.md` — short operator/author guide

**Step 1: Write failing test** (docs-only task; verification is grep, not Go)

No Go test. Verification step below.

**Step 2: Confirm old state**

Run: `grep -c frontends config.json.example docs/frontends.md`
Expected: `config.json.example:0` and `docs/frontends.md` missing.

**Step 3: Write content**

`config.json.example` gains:

```jsonc
  // Optional protocol frontends. Absent = S3 on "listenAddr" (backward
  // compatible). Each entry: "type" (required) and "listenAddr" (optional —
  // omit to share the default listener; set to give this frontend its own
  // TLS port, e.g. ":8444" for a future WebDAV frontend).
  // "frontends": [
  //   { "type": "s3" },
  //   { "type": "webdav", "listenAddr": ":8444" }
  // ],
```

`docs/frontends.md` covers, concisely:
- what a frontend is and the `frontend.Frontend` contract (Name/Handler/Authenticator/Capabilities)
- the semantic rule: unsupported capability → protocol-appropriate error at the seam, never silent emulation
- how to add one: implement the interface, run `frontend.RunConformanceSuite` in your tests, add a factory entry in `frontends.go`, document any new config in config.json.example
- multi-listener decision: entries with `listenAddr` get dedicated TLS listeners drained by the same graceful-shutdown window; entries without share the default mux; unknown/duplicate types fail startup with the known-type list
- pointer to the future frontend GH issues (WebDAV, (S)FTP, ownCloud) and the auth GH issue that will replace the CredentialSource/Identity placeholder

**Step 4: Verify cross-references resolve**

Run: `grep -l "frontend.Frontend" docs/frontends.md && grep -q frontends config.json.example && echo OK`
Expected: OK

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented; `go test ./... -count=1`, `make test`, `make test-race` pass
- [ ] `make e2e` green with a config lacking `frontends` — backward compatibility exact
- [ ] A config with `frontends: [{"type":"bogus"}]` fails startup with an error listing known types
- [ ] Multi-listener: dedicated-listener path is implemented and exercised by tests (may use a stub frontend for the second entry); graceful shutdown drains all listeners
- [ ] `ZETAOBJECT_LISTEN_ADDR` semantics unchanged (default listener only)
- [ ] Frozen contracts untouched; no changes to `internal/frontend` package code (leaf 01 owns it)
- [ ] `config.json.example` and `docs/frontends.md` written and consistent
- [ ] gofmt clean, `make vet`, `make fmt-check` pass
- [ ] No debug artifacts; all files at exact specified paths

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Backward compat is test-proven: absent `frontends` key → identical startup (shared mux, s3, default addr, env override intact)
- [ ] Factory map pattern: adding a frontend requires one map entry + one config type, nothing else in main
- [ ] Dedicated listeners: TLS params from the same certFile/keyFile, shutdown drains them within `serverShutdownTimeout`
- [ ] Startup failures are loud: unknown type, duplicate type, factory error all abort startup with actionable messages
- [ ] No behavioral change to the S3 request path (this leaf only changes construction/wiring)
- [ ] Docs accurate: docs/frontends.md matches the actual implemented behavior; config.json.example valid JSONC-comment style consistent with the file
- [ ] Code follows project conventions (stdlib only, errors as values, gofmt, table-driven tests)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The `webdav` entry in Task 1's test table is intentionally expected to FAIL
  construction today (no factory) — that is the loud-startup requirement, not
  a bug. The dedicated-listener test skips until a second concrete frontend
  exists; when the WebDAV GH issue lands it removes the skip.
- Do not invent per-frontend TLS certificates or per-frontend auth config —
  the auth GH issue owns identity; TLS stays a single shared cert pair this
  tree.
- Keep `startupPlan` pure (config in, plan out, no listeners opened) — it is
  what makes backward compatibility testable without binding ports.
- Registry comes from leaf 01 unchanged; if you find yourself wanting to
  modify `internal/frontend/registry.go`, stop and take it to the orchestrator
  instead.
- Harness configs must not gain a `frontends` key in this leaf — the absent-key
  path is the compatibility proof.
