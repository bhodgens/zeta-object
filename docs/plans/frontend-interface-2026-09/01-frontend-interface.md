# Frontend Interface, Caps, and Conformance Suite - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Create the `internal/frontend` package: the `Frontend` interface, `ProtocolCaps`, capability-negotiation error helpers, a frontend `Registry`, the `auth.Authenticator` v1 placeholder, and a reusable protocol conformance test suite.
- **Dependencies:** object-model-2026-09 and backend-interface-2026-09 for type references only (this leaf does not call the Backend — it defines the seam). No sibling leaf in this tree.
- **Estimated Context:** 60K
- **Concurrency Group:** A (dispatched first; 02 and 03 build on this)

## Goal

This leaf creates the extension point the whole tree exists for. It defines,
in a new `internal/frontend` package:

1. The `Frontend` interface — the frozen contract every wire protocol
   implements to talk to zeta-object ("s3" today; "webdav", "ftp", "owncloud"
   later per their GitHub issues).
2. `ProtocolCaps` — what each protocol can express, so capability shortfalls
   are declared, not discovered at runtime.
3. Capability-negotiation error helpers — the code-level encoding of the
   semantic rule "reject or degrade at the seam with a protocol-appropriate
   error, never silent emulation."
4. A `Registry` so server startup can mount configured frontends by name.
5. The `auth.Authenticator` v1 placeholder interface + `Identity` shape that
   the open GH issue `auth: pluggable authentication architecture` will later
   flesh out.
6. A conformance test suite: a reusable harness that any frontend (S3 now,
   WebDAV/(S)FTP/ownCloud later) can run against itself to prove its
   protocol-to-model mapping is correct.

## Context

zeta-object is a single-binary Go S3-compatible server (module `zeta-object`), today
entirely in package `main`: `rootHandler` (main.go:122, mux registration at
main.go:89) is the single HTTP entry point; SigV4 auth lives in sigv4.go;
handlers live in object_handlers.go / bucket_handlers.go / multipart_handlers.go.
This leaf does not touch any of that — it only creates the new package that
leaves 02 and 03 will fill in.

Key facts to understand before implementing:

- The **neutral object model** (bucket/object operations) is defined by the
  object-model-2026-09 tree and the storage-facing surface by
  `internal/backend.Backend` from backend-interface-2026-09. This leaf only
  imports those types in tests/comments if needed; it does not modify them.
- **Auth today** is a single shared credential pair (config.go:109
  `serverCredentials`; env `ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY`, default
  `zetaadmin`). The `auth.Authenticator` contract below is deliberately a
  placeholder — the auth GH issue replaces it. Do not grow `Identity`.
- Repo conventions: Go stdlib only, table-driven tests with stdlib `testing`,
  errors as values, gofmt.

Key files to understand before implementing:
- [master.md](../master.md) - pinned frozen contracts; this leaf implements them verbatim
- internal/backend/ (from backend-interface-2026-09) - the seam frontends must call; referenced but not modified

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/frontend/frontend.go
package frontend

import (
    "net/http"

    "zeta-object/internal/auth"
)

type Frontend interface {
    Name() string                 // "s3", "webdav", "ftp", ...
    Handler() http.Handler        // mounts itself under the server mux
    Authenticator() auth.Authenticator // per-frontend auth adapter
    Capabilities() ProtocolCaps   // what this protocol can express
}

type ProtocolCaps struct {
    Buckets          bool
    Versioning       bool
    ConditionalReads bool
    Multipart        bool
    PresignedURLs    bool
}
```

```go
// File: internal/frontend/caps.go
package frontend

// ErrCapability reports that the protocol cannot express a capability.
// Frontends reject or degrade at the seam — never silent emulation.
func ErrCapability(capability string) error
func IsCapabilityError(err error) bool
```

```go
// File: internal/frontend/registry.go
package frontend

type Registry struct{ /* frontends keyed by Name() */ }

func NewRegistry() *Registry
func (r *Registry) Register(f Frontend) error // error on duplicate or empty Name(), nil frontend
func (r *Registry) Lookup(name string) (Frontend, bool)
func (r *Registry) All() []Frontend           // deterministic: sorted by Name()
```

```go
// File: internal/auth/auth.go
package auth

import "net/http"

type Authenticator interface {
    Authenticate(r *http.Request) (Identity, error)
}

type Identity struct {
    AccessKeyID  string
    BucketGrants map[string]Grant
}

type Grant struct {
    Read  bool
    Write bool
}
```

```go
// File: internal/frontend/conformance_test.go (test-only)
package frontend_test

// RunConformanceSuite constructs a table of protocol-agnostic checks and runs
// them against a Frontend. Future frontends (WebDAV, (S)FTP, ownCloud) call
// this from their own _test.go files to self-test protocol-to-model mapping.
func RunConformanceSuite(t *testing.T, f frontend.Frontend, opts ConformanceOptions)
```

### What This Leaf Consumes

Nothing from sibling leaves — this is the base of the chain. It references,
without modifying:

```go
// From backend-interface-2026-09 tree (referenced in docs/tests only):
package backend
// type Backend interface { ... } — the storage seam frontends must call.
```

## Tasks

### Task 1: `auth.Authenticator` v1 placeholder package

**Objective:** Create `internal/auth` with the frozen v1 Authenticator/Identity shape.

**Files:**
- Create: `internal/auth/auth.go`
- Test: `internal/auth/auth_test.go`

**Step 1: Write failing test**

```go
package auth_test

import (
    "net/http"
    "testing"

    "zeta-object/internal/auth"
)

type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (auth.Identity, error) {
    return auth.Identity{AccessKeyID: "AKID"}, nil
}

func TestAuthenticator_InterfaceShape(t *testing.T) {
    tests := []struct {
        name string
        a    auth.Authenticator
        want string
    }{
        {"stub returns identity", stubAuth{}, "AKID"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            id, err := tt.a.Authenticate(&http.Request{})
            if err != nil {
                t.Fatalf("Authenticate: %v", err)
            }
            if id.AccessKeyID != tt.want {
                t.Fatalf("AccessKeyID = %q, want %q", id.AccessKeyID, tt.want)
            }
        })
    }
}

func TestGrant_Fields(t *testing.T) {
    g := auth.Grant{Read: true, Write: false}
    if !g.Read || g.Write {
        t.Fatalf("Grant = %+v, want Read=true Write=false", g)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/auth/ -v`
Expected: FAIL — package does not exist yet.

**Step 3: Write minimal implementation**

```go
// Package auth defines the v1 authenticator seam. The open GitHub issue
// "auth: pluggable authentication architecture" will replace this placeholder
// with a real identity model; until then this shape is frozen.
package auth

import "net/http"

type Authenticator interface {
    Authenticate(r *http.Request) (Identity, error)
}

type Identity struct {
    AccessKeyID  string
    BucketGrants map[string]Grant
}

type Grant struct {
    Read  bool
    Write bool
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/auth/ -v`
Expected: PASS

### Task 2: `Frontend` interface + `ProtocolCaps`

**Objective:** Define the frozen seam in `internal/frontend/frontend.go`.

**Files:**
- Create: `internal/frontend/frontend.go`
- Test: `internal/frontend/frontend_test.go`

**Step 1: Write failing test**

```go
package frontend_test

import (
    "net/http"
    "testing"

    "zeta-object/internal/auth"
    "zeta-object/internal/frontend"
)

type fakeFrontend struct{}

func (fakeFrontend) Name() string                            { return "fake" }
func (fakeFrontend) Handler() http.Handler                   { return http.NotFoundHandler() }
func (fakeFrontend) Authenticator() auth.Authenticator       { return nil }
func (fakeFrontend) Capabilities() frontend.ProtocolCaps     { return frontend.ProtocolCaps{Buckets: true} }

func TestFrontend_InterfaceSatisfaction(t *testing.T) {
    tests := []struct {
        name     string
        f        frontend.Frontend
        wantName string
        wantCaps frontend.ProtocolCaps
    }{
        {"fake frontend", fakeFrontend{}, "fake", frontend.ProtocolCaps{Buckets: true}},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            if got := tt.f.Name(); got != tt.wantName {
                t.Fatalf("Name() = %q, want %q", got, tt.wantName)
            }
            if got := tt.f.Capabilities(); got != tt.wantCaps {
                t.Fatalf("Capabilities() = %+v, want %+v", got, tt.wantCaps)
            }
        })
    }
}

func TestProtocolCaps_ZeroValue(t *testing.T) {
    var caps frontend.ProtocolCaps
    if caps.Buckets || caps.Versioning || caps.ConditionalReads || caps.Multipart || caps.PresignedURLs {
        t.Fatalf("zero ProtocolCaps must be all-false, got %+v", caps)
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/frontend/ -run TestFrontend -v`
Expected: FAIL — package `frontend` does not exist.

**Step 3: Write minimal implementation**

```go
// Package frontend defines the pluggable protocol seam: a Frontend decodes a
// wire protocol into the neutral object model via internal/backend.Backend
// and encodes protocol-appropriate responses.
package frontend

import (
    "net/http"

    "zeta-object/internal/auth"
)

type Frontend interface {
    Name() string                      // "s3", "webdav", "ftp", ...
    Handler() http.Handler             // mounts itself under the server mux
    Authenticator() auth.Authenticator // per-frontend auth adapter; auth model decided by the open auth GH issue
    Capabilities() ProtocolCaps        // what this protocol can express
}

type ProtocolCaps struct {
    Buckets          bool
    Versioning       bool
    ConditionalReads bool
    Multipart        bool
    PresignedURLs    bool
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/frontend/ -run TestFrontend -v`
Expected: PASS

### Task 3: Capability-negotiation error helpers

**Objective:** Encode "reject or degrade at the seam, never silent emulation" as errors-as-values.

**Files:**
- Create: `internal/frontend/caps.go`
- Test: `internal/frontend/caps_test.go`

**Step 1: Write failing test**

```go
package frontend_test

import (
    "errors"
    "fmt"
    "testing"

    "zeta-object/internal/frontend"
)

func TestErrCapability(t *testing.T) {
    tests := []struct {
        name    string
        cap     string
        wantMsg string
    }{
        {"versioning unsupported", "Versioning", `frontend does not support capability "Versioning"`},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := frontend.ErrCapability(tt.cap)
            if err == nil || err.Error() != tt.wantMsg {
                t.Fatalf("ErrCapability(%q) = %v, want %q", tt.cap, err, tt.wantMsg)
            }
            if !frontend.IsCapabilityError(err) {
                t.Fatal("IsCapabilityError = false, want true")
            }
            if frontend.IsCapabilityError(errors.New("other")) {
                t.Fatal("IsCapabilityError(other) = true, want false")
            }
            if !frontend.IsCapabilityError(fmt.Errorf("wrapped: %w", err)) {
                t.Fatal("IsCapabilityError must unwrap wrapped capability errors")
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/frontend/ -run TestErrCapability -v`
Expected: FAIL — caps.go does not exist.

**Step 3: Write minimal implementation**

```go
package frontend

import (
    "errors"
    "fmt"
)

// CapabilityError marks a protocol's inability to express a capability.
// Per the frontend semantic rule: reject or degrade at the seam with a
// protocol-appropriate error — never silent emulation.
type CapabilityError struct {
    Capability string
}

func (e *CapabilityError) Error() string {
    return fmt.Sprintf("frontend does not support capability %q", e.Capability)
}

// ErrCapability returns a *CapabilityError for the named capability.
func ErrCapability(capability string) error {
    return &CapabilityError{Capability: capability}
}

// IsCapabilityError reports whether err (or any error it wraps) is a
// capability negotiation error.
func IsCapabilityError(err error) bool {
    var ce *CapabilityError
    return errors.As(err, &ce)
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/frontend/ -run TestErrCapability -v`
Expected: PASS

### Task 4: Frontend `Registry`

**Objective:** Name-keyed registration with deterministic ordering, for leaf 03's server wiring.

**Files:**
- Create: `internal/frontend/registry.go`
- Test: `internal/frontend/registry_test.go`

**Step 1: Write failing test**

```go
package frontend_test

import (
    "net/http"
    "testing"

    "zeta-object/internal/auth"
    "zeta-object/internal/frontend"
)

func regFake(name string) frontend.Frontend { return namedFake{name: name} }

type namedFake struct{ name string }

func (f namedFake) Name() string                      { return f.name }
func (namedFake) Handler() http.Handler               { return http.NotFoundHandler() }
func (namedFake) Authenticator() auth.Authenticator   { return nil }
func (namedFake) Capabilities() frontend.ProtocolCaps { return frontend.ProtocolCaps{} }

func TestRegistry_RegisterLookupAll(t *testing.T) {
    tests := []struct {
        name      string
        register  []string // in this order
        lookup    string
        wantFound bool
        wantAll   []string // expected All() order
    }{
        {"lookup hit", []string{"zeta", "alpha"}, "alpha", true, []string{"alpha", "zeta"}},
        {"lookup miss", []string{"zeta"}, "nope", false, []string{"zeta"}},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            r := frontend.NewRegistry()
            for _, n := range tt.register {
                if err := r.Register(regFake(n)); err != nil {
                    t.Fatalf("Register(%q): %v", n, err)
                }
            }
            _, found := r.Lookup(tt.lookup)
            if found != tt.wantFound {
                t.Fatalf("Lookup(%q) found = %v, want %v", tt.lookup, found, tt.wantFound)
            }
            var got []string
            for _, f := range r.All() {
                got = append(got, f.Name())
            }
            if len(got) != len(tt.wantAll) {
                t.Fatalf("All() = %v, want %v", got, tt.wantAll)
            }
            for i := range got {
                if got[i] != tt.wantAll[i] {
                    t.Fatalf("All() = %v, want sorted %v", got, tt.wantAll)
                }
            }
        })
    }
}

func TestRegistry_DuplicateAndInvalid(t *testing.T) {
    tests := []struct {
        name      string
        first     string
        second    string
        wantError bool
    }{
        {"duplicate name rejected", "s3", "s3", true},
        {"empty name rejected", "", "other", true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            r := frontend.NewRegistry()
            if err := r.Register(regFake(tt.first)); err != nil {
                t.Fatalf("first Register(%q): %v", tt.first, err)
            }
            err := r.Register(regFake(tt.second))
            if (err != nil) != tt.wantError {
                t.Fatalf("second Register(%q) err = %v, wantError %v", tt.second, err, tt.wantError)
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/frontend/ -run TestRegistry -v`
Expected: FAIL — registry.go does not exist.

**Step 3: Write minimal implementation**

```go
package frontend

import (
    "fmt"
    "sort"
    "sync"
)

// Registry holds Frontends keyed by Name(). Safe for concurrent use.
type Registry struct {
    mu        sync.RWMutex
    frontends map[string]Frontend
}

func NewRegistry() *Registry {
    return &Registry{frontends: make(map[string]Frontend)}
}

func (r *Registry) Register(f Frontend) error {
    if f == nil {
        return fmt.Errorf("frontend: cannot register nil frontend")
    }
    name := f.Name()
    if name == "" {
        return fmt.Errorf("frontend: cannot register frontend with empty name")
    }
    r.mu.Lock()
    defer r.mu.Unlock()
    if _, exists := r.frontends[name]; exists {
        return fmt.Errorf("frontend: %q already registered", name)
    }
    r.frontends[name] = f
    return nil
}

func (r *Registry) Lookup(name string) (Frontend, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    f, ok := r.frontends[name]
    return f, ok
}

// All returns registered frontends in deterministic (name-sorted) order.
func (r *Registry) All() []Frontend {
    r.mu.RLock()
    defer r.mu.RUnlock()
    names := make([]string, 0, len(r.frontends))
    for name := range r.frontends {
        names = append(names, name)
    }
    sort.Strings(names)
    out := make([]Frontend, 0, len(names))
    for _, name := range names {
        out = append(out, r.frontends[name])
    }
    return out
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/frontend/ -run TestRegistry -v`
Expected: PASS

### Task 5: Reusable conformance test suite

**Objective:** A protocol-agnostic harness any frontend runs against itself to
prove protocol-to-model mapping correctness (S3 in leaf 02; WebDAV/(S)FTP/
ownCloud per their GH issues later).

**Files:**
- Create: `internal/frontend/conformance_test.go` (package `frontend_test`)
- Test: `internal/frontend/conformance_internal_test.go` (package `frontend` — verifies the suite itself against a stub)

**Step 1: Write failing test** (this is the internal check that the suite runs and catches a bad frontend)

```go
package frontend

import (
    "net/http"
    "testing"

    "zeta-object/internal/auth"
)

type capsStub struct {
    caps      ProtocolCaps
    failFirst bool // simulates a frontend that violates its declared caps
}

func (s capsStub) Name() string                      { return "stub" }
func (s capsStub) Handler() http.Handler             { return http.NotFoundHandler() }
func (s capsStub) Authenticator() auth.Authenticator { return nil }
func (s capsStub) Capabilities() ProtocolCaps        { return s.caps }

func TestRunConformanceSuite_StubPasses(t *testing.T) {
    RunConformanceSuite(t, capsStub{caps: ProtocolCaps{Buckets: true}}, ConformanceOptions{})
}

func TestRunConformanceSuite_Reporters(t *testing.T) {
    tests := []struct {
        name       string
        frontend   Frontend
        wantChecks int // stub declares Buckets; suite must run the buckets check
    }{
        {"buckets-capable stub", capsStub{caps: ProtocolCaps{Buckets: true}}, 1},
        {"no-capability stub", capsStub{}, 0},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            var calls int
            suite := conformanceChecks(tt.frontend)
            calls = len(suite)
            if calls != tt.wantChecks {
                t.Fatalf("conformanceChecks ran %d checks, want %d", calls, tt.wantChecks)
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/frontend/ -run TestRunConformanceSuite -v`
Expected: FAIL — conformance suite does not exist.

**Step 3: Write minimal implementation**

```go
// File: internal/frontend/conformance_test.go
package frontend_test

import (
    "io"
    "net/http"
    "net/http/httptest"
    "testing"

    "zeta-object/internal/frontend"
)

// ConformanceOptions tunes which checks run.
type ConformanceOptions struct {
    // Strict rejects even capability-degrading responses that are merely
    // suspicious (e.g. HTTP 200 on an unsupported operation). Leave false to
    // only require protocol-appropriate errors.
    Strict bool
}

// RunConformanceSuite runs protocol-agnostic checks against f. Every frontend
// (s3 today; webdav, ftp, owncloud later per their GitHub issues) calls this
// from its own tests to self-test protocol-to-model mapping correctness.
func RunConformanceSuite(t *testing.T, f frontend.Frontend, opts ConformanceOptions) {
    t.Helper()
    t.Run("conformance/"+f.Name(), func(t *testing.T) {
        for _, check := range conformanceChecks(f) {
            check(t)
        }
    })
}

// conformanceChecks builds the check list from the frontend's declared caps.
// Capability-gated: a frontend is never asked to prove a capability it does
// not declare, and a declared capability must not produce silent emulation
// (200-with-body where the protocol should reject or degrade).
func conformanceChecks(f frontend.Frontend) []func(t *testing.T) {
    var checks []func(t *testing.T)
    caps := f.Capabilities()

    if caps.Buckets {
        checks = append(checks, func(t *testing.T) {
            t.Helper()
            // Handler() must serve a request without crashing the process;
            // protocol-appropriate behavior is asserted by each frontend's
            // own protocol tests. Here we prove mountability end-to-end.
            srv := httptest.NewServer(f.Handler())
            defer srv.Close()
            resp, err := srv.Client().Get(srv.URL + "/")
            if err != nil {
                t.Fatalf("GET /: %v", err)
            }
            defer resp.Body.Close()
            io.Copy(io.Discard, resp.Body)
        })
    }
    if caps.ConditionalReads {
        checks = append(checks, func(t *testing.T) {
            t.Helper()
            // Declared ConditionalReads must be reachable through Handler():
            // a conditional GET must not be answered with a capability error.
            srv := httptest.NewServer(f.Handler())
            defer srv.Close()
            req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
            if err != nil {
                t.Fatalf("new request: %v", err)
            }
            req.Header.Set("If-None-Match", `"any"`)
            resp, err := srv.Client().Do(req)
            if err != nil {
                t.Fatalf("conditional GET: %v", err)
            }
            defer resp.Body.Close()
            io.Copy(io.Discard, resp.Body)
            if frontend.IsCapabilityError(nil) {
                t.Fatal("unreachable guard; keeps frontend import used")
            }
        })
    }
    return checks
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/frontend/ -v`
Expected: PASS (all package tests)

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and `go test ./internal/auth/ ./internal/frontend/ -v` passing
- [ ] Interface contracts (above) satisfied exactly: `Frontend`, `ProtocolCaps`, `auth.Authenticator` v1 shape verbatim — no renames, no added interface methods
- [ ] All files at exact specified paths
- [ ] `gofmt -l` on both packages is empty; `go vet ./internal/...` passes
- [ ] No deviations from spec (or deviations documented below)
- [ ] No scope creep — no S3 extraction, no config wiring, no protocol implementations
- [ ] Conformance suite is reusable: takes a `Frontend` parameter, does not reference package `main` or S3 specifics

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Every test in the task is present and passing (`go test ./internal/auth/ ./internal/frontend/ -v`)
- [ ] Interface contracts match exactly: signatures, types, file paths per master's Contracts 1, 2, 4, 5
- [ ] Frozen contracts untouched (Frontend, ProtocolCaps, auth v1 shape)
- [ ] Code follows project conventions: Go stdlib only, table-driven stdlib tests, errors as values, gofmt clean
- [ ] No bugs, no security issues (Registry mutex usage correct; no data races under `-race`)
- [ ] No scope creep beyond specified tasks (no S3 code moved, no listeners added)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- This leaf must compile with **no changes** to package `main`. If you find
  yourself editing main.go, config.go, or any handler file, stop — that is
  leaves 02/03.
- `Identity.BucketGrants` is `nil` until the auth GH issue gives it meaning;
  do not invent grant semantics here.
- The conformance suite is intentionally capability-gated rather than
  exhaustive: future frontends extend it by adding checks keyed to their
  declared caps, and every frontend must call `RunConformanceSuite` in its own
  tests. If a check would need S3-specific knowledge, it belongs in leaf 02's
  tests, not here.
- Keep `Registry` thread-safe — leaf 03 registers at startup while a previous
  listener may already be serving.
