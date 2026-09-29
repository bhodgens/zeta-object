# Backend Registry + Config Selection - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** master.md (this directory)
- **Scope:** Add the `Registry` (name → constructor) to `internal/backend`, per-bucket
  backend selection from `config.json` (backward compatible: absent = fs), and `main.go`
  startup wiring; update `config.json.example`.
- **Dependencies:** 01-backend-interface.md (interface, BackendConfig types location),
  02-filesystem-backend.md (fs backend registered; `backendFor` indirection to replace).
- **Estimated Context:** 40K
- **Concurrency Group:** C (after B)

## Goal

Make the seam selectable: a `backend.Registry` populated at `init()` time, and config keys

```json
{
  "backends": {
    "fs": { "root": "./data/" }
  },
  "buckets": {
    "photos": { "path": "/mnt/storage/photos", "backend": "fs" }
  }
}
```

so each bucket resolves to a named backend instance at startup. Backward compatibility is
the hard requirement: a config with none of the new keys must behave exactly like today
(all buckets → fs rooted at `dataDir` / custom paths). Unknown backend names fail startup
loudly — never a silent fs fallback (a silent fallback would put a distributed bucket's
data on local disk).

This leaf also future-proofs the registration point: the crush-lite ZFS ring
(`docs/plan-distributed-zfs-backing.md`) and S3-proxy backends land later as packages
with a single `init()` — no further core changes.

## Context

zeta-object loads `config.json` (path via `ZETAOBJECT_CONFIG`, default `config.json`) through
`loadConfig` in `config.go` (144 lines): `ServerConfig` struct with `dataDir`,
`listenAddr`, `certFile`, `keyFile`, `buckets map[string]string` (bucket → custom path).
`main.go` (256 lines) wires the http.Server. After leaf 02, package main has
`backendFor func(bucket string) (backend.Backend, error)` (default: all-fs) and
`internal/backend/fsbackend` registers `"fs"` via `init()`.

Key files to understand before implementing:
- `config.go` — ServerConfig, loadConfig, env overrides. You will extend ServerConfig.
- `main.go` — startup sequence; where the backendFor installer gets built.
- `config.json.example` — commented sample; must document the new keys.
- `internal/backend/registry.go` — created here per parent Contract 2.
- `internal/backend/fsbackend/` — the `"fs"` registration (exists; consume, don't modify
  beyond what a task explicitly requires).
- `backend_lookup.go` — leaf 02's indirection; this leaf replaces its default installer.
- `CLAUDE.md` Configuration section — update the key table at the end of this leaf.

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/backend/registry.go
package backend

import "sync"

type Registry map[string]func(cfg BackendConfig) (Backend, error)

type BackendConfig struct {
	Type    string
	Root    string
	Options map[string]string
}

var (
	regMu   sync.RWMutex
	registrations = Registry{}
)

// Register installs a constructor under name. Panics on duplicate
// registration (init-time programming error, same as database/sql).
func Register(name string, fn func(cfg BackendConfig) (Backend, error))

// Lookup returns the constructor for name; unknown names return
// ErrUnknownBackend (leaf 01's sentinel) — never a default.
func Lookup(name string) (func(cfg BackendConfig) (Backend, error), error)

// Names lists registered backend names (sorted; for startup diagnostics).
func Names() []string
```

```go
// File: config.go (package main) — extended ServerConfig (JSON keys frozen here):
type ServerConfig struct {
	// ... existing fields unchanged: dataDir, listenAddr, certFile, keyFile ...
	Buckets  map[string]string          `json:"buckets"`  // existing: name → custom path
	Backends map[string]BackendCfg      `json:"backends"` // NEW: name → backend config
}
type BackendCfg struct {
	Root    string            `json:"root"`
	Options map[string]string `json:"options"`
}
// Per-bucket selection: buckets values change from string to EITHER string
// (backward compat) OR object. Recommended encoding:
type BucketCfg struct {
	Path    string `json:"path"`
	Backend string `json:"backend"` // absent/"" ⇒ default backend (fs)
}
// with custom UnmarshalJSON accepting the legacy string form — see Task 2.
```

```go
// File: backend_lookup.go (package main) — leaf 03 rewires the installer:
// buildBackendLookup(cfg ServerConfig) (func(bucket string) (backend.Backend, error), error)
//   - resolves each bucket's backend name (explicit or default "fs")
//   - constructs one Backend instance per (backend-name, root) pair, memoized
//   - unknown name → startup error: `unknown backend type %q (registered: %s)`
```

### What This Leaf Consumes

```go
// From 01-backend-interface.md: Backend, BackendConfig (defined here in registry.go
// per parent Contract 2 — coordinate: if leaf 01 already placed BackendConfig in
// backend.go, keep THAT location and do not re-declare; the frozen shape is what matters),
// ErrUnknownBackend, Names/Lookup conventions.
// From 02-filesystem-backend.md: fsbackend's init() registration of "fs";
// the existing backendFor default installer being replaced.
```

## Tasks

### Task 1: Registry with Register/Lookup/Names

**Objective:** Thread-safe name → constructor registry in `internal/backend`.

**Files:**
- Create: `internal/backend/registry.go` (or extend backend.go if leaf 01 already placed
  BackendConfig there — do not duplicate the type)
- Test: `internal/backend/registry_test.go`

**Step 1: Write failing test**

```go
package backend

import (
	"errors"
	"testing"
)

func TestRegistryRegisterLookup(t *testing.T) {
	cases := []struct {
		name    string
		wantErr error
	}{
		{"testfs", nil},
		{"nope", ErrUnknownBackend},
	}
	Register("testfs", func(cfg BackendConfig) (Backend, error) {
		return nil, errors.New("not constructed in test")
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn, err := Lookup(tc.name)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil || fn == nil {
				t.Fatalf("want ctor, got %v", err)
			}
		})
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on duplicate registration")
		}
	}()
	Register("dupe", func(BackendConfig) (Backend, error) { return nil, nil })
	Register("dupe", func(BackendConfig) (Backend, error) { return nil, nil })
}

func TestNamesSorted(t *testing.T) {
	// register two fresh names with unique prefixes; assert Names() contains
	// both in sorted order (filter to the unique prefix so parallel tests
	// in this package don't flake).
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/backend/ -run 'TestRegistry|TestRegister|TestNames' -v`
Expected: FAIL — `undefined: Register` etc.

**Step 3: Write minimal implementation**

`registry.go` per the contract above. RWMutex-guarded; `Register` panics on duplicates
with a message naming the backend; `Lookup` wraps unknown names with `ErrUnknownBackend`
plus the sorted registered-name list in the message (startup diagnostics come free).

**Step 4: Run test to verify pass**

Run: `go test -race ./internal/backend/ -v` — PASS (registry subtests + everything prior).

### Task 2: Config schema — backends map + per-bucket backend (backward compatible)

**Objective:** Extend ServerConfig so both legacy and new bucket value forms parse, and
`backends` loads.

**Files:**
- Modify: `config.go`
- Test: `config_backend_test.go` (package main)

**Step 1: Write failing test**

```go
package main

import (
	"encoding/json"
	"testing"
)

func TestLoadConfigBackendKeysBackwardCompat(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		verify  func(t *testing.T, c ServerConfig)
		wantErr bool
	}{
		{
			name: "legacy string bucket value still parses",
			json: `{"dataDir":"./data/","buckets":{"photos":"/mnt/photos"}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Buckets["photos"] != "/mnt/photos" { /* via compat accessor */ }
			},
		},
		{
			name: "object bucket value with backend",
			json: `{"buckets":{"photos":{"path":"/mnt/photos","backend":"fs"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				// path == /mnt/photos, backendName == "fs"
			},
		},
		{
			name: "backends map loads",
			json: `{"backends":{"fs":{"root":"./data/"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Backends["fs"].Root != "./data/" { t.Fatal("root lost") }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c ServerConfig
			err := json.Unmarshal([]byte(tc.json), &c)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil {
				tc.verify(t, c)
			}
		})
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test . -run TestLoadConfigBackendKeysBackwardCompat -v`
Expected: FAIL — `Backends`/object-form undefined or misparsed.

**Step 3: Write minimal implementation**

Per the contract: `BackendCfg` type; `Backends` field; per-bucket selection via a
`BucketCfg` type with custom `UnmarshalJSON` accepting BOTH the legacy bare string
(`"photos": "/mnt/photos"`) and the new object form. Keep the existing `Buckets` external
behavior intact for every current caller (existing tests must not need edits — if they do,
the compat accessor is wrong).

**Step 4: Run test to verify pass**

Run: `go test . -count=1 -v` — PASS including all pre-existing config tests.

### Task 3: buildBackendLookup + main.go wiring

**Objective:** Startup builds the per-bucket Backend table; `backendFor` uses it.

**Files:**
- Modify: `backend_lookup.go`, `main.go`
- Test: `backend_lookup_test.go` (package main)

**Step 1: Write failing test**

```go
package main

import (
	"testing"

	"zeta-object/internal/backend"
)

func TestBuildBackendLookup(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ServerConfig
		verify  func(t *testing.T, lookup func(string) (backend.Backend, error))
		wantErr string // empty = expect success
	}{
		{
			name: "absent keys → all fs, default root",
			cfg:  ServerConfig{DataDir: t.TempDir()},
			verify: func(t *testing.T, lookup func(string) (backend.Backend, error)) {
				b, err := lookup("anybucket")
				if err != nil || b == nil {
					t.Fatalf("default fs lookup failed: %v", err)
				}
			},
		},
		{
			name: "explicit fs per bucket",
			cfg: func() ServerConfig {
				root := t.TempDir()
				return ServerConfig{
					DataDir: t.TempDir(),
					Buckets: map[string]string{"photos": root}, // legacy form
				}
			}(),
			verify: func(t *testing.T, lookup func(string) (backend.Backend, error)) {
				if _, err := lookup("photos"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:    "unknown backend type → startup error, no fallback",
			cfg:     ServerConfig{DataDir: t.TempDir()}, // + object-form bucket with backend:"nope"
			wantErr: `unknown backend type "nope"`,
		},
		{
			name: "two buckets different roots → separate FS instances",
			// assert objects written via lookup("a") land under rootA —
			// construct through the registry, not by type-asserting (backends
			// are consumed via the interface; prove placement by writing
			// through the Backend and stat-ing the expected path).
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup, err := buildBackendLookup(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !containsStr(err.Error(), tc.wantErr) {
					t.Fatalf("want err %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.verify(t, lookup)
		})
	}
}

func containsStr(s, sub string) bool { return len(s) >= len(sub) && strings.Contains(s, sub) }
```

**Step 2: Run test to verify failure**

Run: `go test . -run TestBuildBackendLookup -v`
Expected: FAIL — `undefined: buildBackendLookup`.

**Step 3: Write minimal implementation**

`buildBackendLookup(cfg)`: for every bucket (from the compat view of Buckets + any
backend selection), resolve name = explicit backend or "fs"; `backend.Lookup(name)` →
construct with `BackendConfig{Type: name, Root: resolvedRoot, Options: ...}` — Root
resolution preserves today's precedence: explicit bucket path > named backend's root >
dataDir. Memoize one instance per (name, root). Install via the existing `backendFor`
var in `main.go`'s startup path BEFORE the listener opens (after leaf 02 the default
already exists; this replaces it). Startup failure (unknown name) aborts with a clear
error — no silent fallback.

**Step 4: Run test to verify pass + gates**

Run: `go build ./... && go vet ./... && go test ./... -count=1 && make e2e`
Expected: all green — backward compat proven by the unchanged harness.

### Task 4: Docs — config.json.example + CLAUDE.md configuration section

**Objective:** The new keys are discoverable and documented.

**Files:**
- Modify: `config.json.example` — add commented `backends` map and object-form bucket
  example with `"backend": "fs"`, noting absent = fs.
- Modify: `CLAUDE.md` — Configuration key table: add `backends` (default `{}`) and the
  per-bucket `backend` key (default `fs`); one line noting unknown backend names abort
  startup.

**Step 1:** Read current content of both files (terminal cat).

**Step 2:** Confirm old state: no `backends` key documented.

**Step 3:** Write the additions (minimal; match each file's existing style).

**Step 4: Verify** — `grep -n backends config.json.example CLAUDE.md` shows the new
documentation; JSON in the example parses (`python3 -c "import json,sys; json.load(open('config.json.example'))"`.
If the example is JSON5-with-comments, verify with the repo's existing convention instead —
check how the current example handles comments first).

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and tests passing (`go test -race ./...`)
- [ ] Registry: Register/Lookup/Names per contract; duplicate registration panics;
      unknown name → ErrUnknownBackend (never default)
- [ ] Backward compat: legacy string bucket values AND no-new-keys configs behave
      exactly as before — full suite + `make e2e` green with a config file lacking the
      new keys
- [ ] Startup wiring: backendFor installed before listener; unknown backend type aborts
      startup with a message naming the type and registered names
- [ ] `config.json.example` + `CLAUDE.md` updated
- [ ] gofmt clean; `go vet ./...` clean; `make test-cover-enforce` green
- [ ] No deviations from spec (or documented below)
- [ ] No scope creep — no new backend implementations (the stub from leaf 01 may be
      used in registry tests, but nothing registers it outside tests)

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task implemented; registry/config/wiring tests present and passing
- [ ] Contract 2 shape exact (Registry map type, BackendConfig fields)
- [ ] Backward-compat matrix from the parent's Integration Test Plan §5 all pass
- [ ] No silent fallback anywhere: unknown name is an error at Lookup AND at startup
- [ ] Instance memoization: two buckets sharing (name, root) share one Backend; two
      roots get two instances
- [ ] Docs updated in both files, matching each file's style
- [ ] No bugs, no security issues
- [ ] No scope creep

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The critical design point is FAIL-LOUD on unknown backend types. A silent fallback to
  fs would silently place a future distributed bucket's data on local disk — the worst
  possible failure mode. `ErrUnknownBackend` from Lookup, and a startup abort from
  buildBackendLookup, are both required.
- BackendConfig.Options exists for future backends (e.g. an S3 proxy's endpoint/region
  strings). fs ignores it in v1 — fine; do not validate unknown option keys for fs.
- If leaf 01 already defined BackendConfig inside backend.go, add registry.go without
  re-declaring it — a duplicate declaration breaks the build and will be caught in review.
- Keep `Names()` cheap and allocation-light enough for startup logging; one
  `log.Printf("registered backends: %s", strings.Join(backend.Names(), ", "))` at startup
  is nice-to-have diagnostics (optional).
- Custom-bucket + backend-name resolution precedence must match `getBucketPath` exactly —
  read it before wiring, don't assume.
