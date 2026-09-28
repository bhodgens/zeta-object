# S3 Frontend Extraction into internal/frontend/s3 - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Move the existing S3 HTTP layer (rootHandler dispatch, SigV4 authentication, object/bucket/multipart handler routing, XML marshaling) into `internal/frontend/s3` implementing `frontend.Frontend`, calling storage only through `internal/backend.Backend`, with zero behavior change.
- **Dependencies:** 01-frontend-interface.md (defines `Frontend`, `ProtocolCaps`, registry, auth placeholder). External: object-model-2026-09 (neutral object model) and backend-interface-2026-09 (`internal/backend.Backend`) — if either has not landed this leaf is BLOCKED.
- **Estimated Context:** 110K
- **Concurrency Group:** B (after 01, before 03)

## Goal

Turn the S3 implementation that today lives directly in package `main` into
the first `frontend.Frontend`. After this leaf:

- `internal/frontend/s3` owns request dispatch (path → bucket/object parse),
  SigV4 authentication (header + presigned + aws-chunked), routing by
  method+query to operation handlers, and XML marshaling — i.e. everything
  wire-protocol-specific.
- All storage calls go through the `backend.Backend` interface. The s3
  frontend contains no `os.`/filesystem access; storage.go's atomic-write
  helpers become Backend implementation details, not frontend concerns.
- `package main` no longer contains any S3 handler logic: main.go wires
  `http.ServeMux` to the s3 frontend's `Handler()` (full mux cutover happens
  in leaf 03; this leaf leaves main.go mounting the new frontend where
  rootHandler used to be registered).
- The integration test harness (commits 7923cd5, a596b9e, dfce3bc) passes
  unchanged. This is the definition of "behavior-preserving" — if the harness
  disagrees with any extraction decision, the extraction is wrong.

## Context

mini-s3 is a single-binary Go S3-compatible server (module `mini-s3`,
package `main`, ~30 root .go files). Request flow today:

1. `main()` builds an `http.ServeMux` and registers `rootHandler` at `/`
   (main.go:89 registration; rootHandler defined at main.go:122).
2. `rootHandler` splits the path into bucket/object names
   (`strings.Split(strings.Trim(r.URL.Path, "/"), "/")`), then calls
   `authenticateRequest` (sigv4.go:438) for SigV4 validation (header or
   presigned; aws-chunked body decoding lives in sigv4.go too).
3. It routes by method + query params to handlers in object_handlers.go
   (1324 lines), bucket_handlers.go (364 lines), multipart_handlers.go
   (1051 lines).
4. Handlers read/write storage directly via storage.go helpers
   (`writeFileAtomic`, `writeFileAtomicJSON`, `lockObject`, per-key locks).
5. Errors are rendered by `errorToXML`/`writeS3Error` in xml.go; XML payload
   structs live in types.go; the S3 namespace
   `http://s3.amazonaws.com/doc/2006-03-01/` is pinned via `s3XMLNamespace`
   in xml.go.

Credentials are a single shared pair (config.go:109 `serverCredentials`; env
`MINIS3_ACCESS_KEY`/`MINIS3_SECRET_KEY`, default `minioadmin`). TLS on
`:8443` via config `listenAddr`/`certFile`/`keyFile`. Tests are stdlib
`testing`; the repo has an integration harness that must stay green.

Key files to understand before implementing:
- main.go - rootHandler definition (line ~122) and mux registration (line ~89); both change here
- sigv4.go (775 lines) - authenticateRequest, canonical requests, presigned parsing, aws-chunked decode; moves whole
- object_handlers.go (1324) / bucket_handlers.go (364) / multipart_handlers.go (1051) - operation handlers; move, then reroute storage calls through Backend
- xml.go / types.go - XML error/payload marshaling; move with the frontend
- config.go - serverCredentials consumed by the SigV4→auth adapter
- internal/frontend/ - created by leaf 01; the contract this leaf implements
- internal/backend/ - the storage seam (backend-interface-2026-09); every storage call reroutes through it

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/frontend/s3/frontend.go
package s3

import (
    "net/http"

    "mini-s3/internal/auth"
    "mini-s3/internal/backend"
    "mini-s3/internal/frontend"
)

// New constructs the S3 frontend over the storage seam. The SigV4 credential
// source is injected so package main's config stays the owner of credentials.
type Option func(*Frontend)

func WithCredentialSource(cs auth.CredentialSource) Option

func New(b backend.Backend, opts ...Option) *Frontend

// Frontend implements frontend.Frontend.
//   Name()          -> "s3"
//   Handler()       -> the former rootHandler pipeline
//   Authenticator() -> SigV4 adapter implementing auth.Authenticator
//   Capabilities()  -> ProtocolCaps{Buckets: true, ConditionalReads: true,
//                                    Multipart: true, PresignedURLs: true}
//                      (Versioning: false — not implemented; a versioning
//                      request degrades at the seam, matching today's
//                      "Not Implemented" behavior)
func (f *Frontend) Name() string
func (f *Frontend) Handler() http.Handler
func (f *Frontend) Authenticator() auth.Authenticator
func (f *Frontend) Capabilities() frontend.ProtocolCaps
```

```go
// File: internal/auth/credentials.go (small addition to leaf 01's package —
// the credential-source abstraction the SigV4 adapter needs)
package auth

// CredentialSource supplies the shared secret for an access key ID.
// v1 has exactly one pair (config.go serverCredentials); the auth GH issue
// replaces this with real identity lookup.
type CredentialSource interface {
    SecretKey(accessKeyID string) (string, bool)
}
```

```go
// Moved files (content moves from package main into internal/frontend/s3):
//   internal/frontend/s3/dispatch.go        <- rootHandler logic (main.go:122+)
//   internal/frontend/s3/sigv4.go           <- sigv4.go whole (775 lines)
//   internal/frontend/s3/object_handlers.go <- object_handlers.go whole
//   internal/frontend/s3/bucket_handlers.go <- bucket_handlers.go whole
//   internal/frontend/s3/multipart_handlers.go <- multipart_handlers.go whole
//   internal/frontend/s3/xml.go             <- xml.go whole (s3XMLNamespace)
//   internal/frontend/s3/types.go           <- XML payload structs from types.go
// Behavior-preserving move: same routes, same status codes, same XML bytes.
```

### What This Leaf Consumes

```go
// From 01-frontend-interface.md:
package frontend
type Frontend interface {
    Name() string
    Handler() http.Handler
    Authenticator() auth.Authenticator
    Capabilities() ProtocolCaps
}
type ProtocolCaps struct {
    Buckets, Versioning, ConditionalReads, Multipart, PresignedURLs bool
}

// From backend-interface-2026-09 (the ONLY storage access allowed):
package backend
// type Backend interface { ... } — object, bucket, and multipart operations.
// The exact method set is pinned by that tree's master; this leaf calls it,
// never os./ioutil/filepath filesystem primitives.
```

## Tasks

### Task 1: `auth.CredentialSource` + SigV4 adapter behind `auth.Authenticator`

**Objective:** Give the s3 frontend's authentication an interface boundary so
`authenticateRequest`'s credential lookup is injectable and the frontend can
return a real `Authenticator()`.

**Files:**
- Modify (create): `internal/auth/credentials.go`
- Create: `internal/frontend/s3/auth_adapter.go`
- Test: `internal/frontend/s3/auth_adapter_test.go`

**Step 1: Write failing test**

```go
package s3_test

import (
    "net/http"
    "testing"

    "mini-s3/internal/frontend/s3"
)

type staticCreds map[string]string

func (m staticCreds) SecretKey(accessKeyID string) (string, bool) {
    k, ok := m[accessKeyID]
    return k, ok
}

func TestS3Frontend_Authenticator(t *testing.T) {
    tests := []struct {
        name    string
        creds   staticCreds
        request *http.Request
        wantErr bool
    }{
        {
            name:    "unknown access key rejected",
            creds:   staticCreds{"minioadmin": "minioadmin"},
            request: mustSignRequest(t, "nobody", "nobody"), // helper below
            wantErr: true,
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            f := s3.New(nil /* backend unused by auth */, s3.WithCredentialSource(tt.creds))
            a := f.Authenticator()
            if a == nil {
                t.Fatal("Authenticator() = nil, want non-nil")
            }
            _, err := a.Authenticate(tt.request)
            if (err != nil) != tt.wantErr {
                t.Fatalf("Authenticate() err = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}

// mustSignRequest builds a SigV4-signed request using the same canonical
// construction the production code uses; see sigv4_test.go in package main
// for reference construction before the move.
func mustSignRequest(t *testing.T, accessKey, secret string) *http.Request {
    t.Helper()
    // (implementation copied from existing sigv4 test helpers during the move)
    return nil // TODO: replaced with real helper when migrating sigv4 tests
}
```

Note: the real `mustSignRequest` body is migrated from the existing SigV4
test helpers in package main (see sigv4_chunked_test.go, sigv4_presigned_test.go
for canonical-request construction to reuse). The TODO placeholder above must
not survive into the commit.

**Step 2: Run test to verify failure**

Run: `go test ./internal/frontend/s3/ -run TestS3Frontend_Authenticator -v`
Expected: FAIL — package does not exist.

**Step 3: Write minimal implementation**

`internal/auth/credentials.go`:

```go
package auth

// CredentialSource supplies the shared secret for an access key ID.
// v1 has exactly one pair; the auth GH issue
// ("auth: pluggable authentication architecture") replaces this with real
// identity lookup.
type CredentialSource interface {
    SecretKey(accessKeyID string) (string, bool)
}
```

`internal/frontend/s3/auth_adapter.go`:

```go
package s3

import (
    "net/http"

    "mini-s3/internal/auth"
)

// sigv4Authenticator adapts the existing SigV4 verification
// (former authenticateRequest) to auth.Authenticator. It resolves the
// signing secret through a CredentialSource instead of the package-level
// serverCredentials global.
type sigv4Authenticator struct {
    creds auth.CredentialSource
}

func (a *sigv4Authenticator) Authenticate(r *http.Request) (auth.Identity, error) {
    // Former authenticateRequest logic, restructured:
    //   - parse Authorization header or presigned query params
    //   - resolve secret via a.creds.SecretKey(accessKeyID)
    //   - derive signing key, compare signatures (constant-time)
    //   - on success return auth.Identity{AccessKeyID: accessKeyID}
    //   - on failure return the error mapped to S3 error codes by the caller
    // (body moved in Task 2; this task wires the adapter shape)
    return auth.Identity{}, errNotImplementedYet
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/frontend/s3/ -run TestS3Frontend_Authenticator -v`
Expected: PASS (adapter rejects unknown access keys using injected creds)

### Task 2: Move SigV4 machinery into the s3 frontend

**Objective:** Relocate sigv4.go wholesale into
`internal/frontend/s3/sigv4.go`, rewiring credential lookup through
`CredentialSource` and returning structured errors instead of writing HTTP
responses, so the adapter from Task 1 owns it.

**Files:**
- Move: `sigv4.go` → `internal/frontend/s3/sigv4.go`
- Modify: `internal/frontend/s3/auth_adapter.go` (body of `Authenticate` becomes the former `authenticateRequest` logic, refactored from `bool`-returns to `(Identity, error)`)
- Test: `internal/frontend/s3/sigv4_test.go` (migrate existing sigv4_chunked_test.go + sigv4_presigned_test.go cases)

**Step 1: Write failing test**

Migrate one representative case from each existing SigV4 test file into the
new package (table-driven, stdlib testing):

```go
package s3_test

import (
    "testing"

    "mini-s3/internal/frontend/s3"
)

func TestSigV4_HeaderAndPresigned(t *testing.T) {
    tests := []struct {
        name    string
        mode    string // "header" | "presigned"
        wantErr bool
    }{
        {"valid header signature", "header", false},
        {"valid presigned URL", "presigned", false},
        {"bad signature rejected", "header", true},
        {"expired presigned rejected", "presigned", true},
        {"wrong region rejected", "header", true}, // region pinned us-east-1
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
            req := buildSignedRequest(t, tt.mode, tt.wantErr) // migrated helper
            _, err := f.Authenticator().Authenticate(req)
            if (err != nil) != tt.wantErr {
                t.Fatalf("mode %s: err = %v, wantErr %v", tt.mode, err, tt.wantErr)
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/frontend/s3/ -run TestSigV4 -v`
Expected: FAIL — logic still in package main.

**Step 3: Write minimal implementation**

Move, do not redesign:
- canonical request construction, signing key derivation, signature
  comparison, presigned query parsing, aws-chunked body decoding,
  `x-amz-decoded-content-length` verification
- the only functional change: credential lookup goes through
  `auth.CredentialSource.SecretKey`, and failures return typed errors
  (`errAuth`, carrying the S3 error code string) instead of calling
  `writeS3Error` directly — dispatch (Task 3) maps errors to responses.

Verify region pinning (`us-east-1`) and the
`AuthorizationHeaderMalformed`/400 behavior survive the move unchanged.

**Step 4: Run test to verify pass**

Run: `go test ./internal/frontend/s3/ -run TestSigV4 -v && go test ./... -count=1`
Expected: PASS

### Task 3: Move dispatch and handler routing; reroute storage through Backend

**Objective:** Move rootHandler's parse+route logic and the three handler
files into `internal/frontend/s3`, replacing direct filesystem calls with
`backend.Backend` calls. This is the bulk of the leaf.

**Files:**
- Move: rootHandler logic (main.go:122 onward) → `internal/frontend/s3/dispatch.go`
- Move: `object_handlers.go` → `internal/frontend/s3/object_handlers.go`
- Move: `bucket_handlers.go` → `internal/frontend/s3/bucket_handlers.go`
- Move: `multipart_handlers.go` → `internal/frontend/s3/multipart_handlers.go`
- Move: `xml.go` → `internal/frontend/s3/xml.go`
- Move: XML payload structs from `types.go` → `internal/frontend/s3/types.go`
- Modify: `internal/frontend/s3/frontend.go` — `Handler()` returns the dispatch chain
- Modify: `main.go` — delete moved code; register `s3.New(...).Handler()` where rootHandler was registered (main.go:89 area)
- Test: `internal/frontend/s3/dispatch_test.go` + migrated handler tests

**Step 1: Write failing test**

```go
package s3_test

import (
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "mini-s3/internal/frontend"
    "mini-s3/internal/frontend/s3"
)

// stubBackend is the minimal backend.Backend double for dispatch tests;
// its full method set mirrors backend-interface-2026-09's pinned interface.
type stubBackend struct{ /* captured calls */ }

// ... stub methods returning fixed values, one per Backend method ...

func TestS3Frontend_ImplementsFrontend(t *testing.T) {
    tests := []struct {
        name string
        f    frontend.Frontend
    }{
        {"s3 frontend", s3.New(&stubBackend{}, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            if tt.f.Name() != "s3" {
                t.Fatalf("Name() = %q, want %q", tt.f.Name(), "s3")
            }
            caps := tt.f.Capabilities()
            if !caps.Buckets || !caps.Multipart || !caps.PresignedURLs || !caps.ConditionalReads {
                t.Fatalf("Capabilities() = %+v, want Buckets/Multipart/PresignedURLs/ConditionalReads true", caps)
            }
            if caps.Versioning {
                t.Fatalf("Capabilities().Versioning = true, want false (not implemented)")
            }
        })
    }
}

func TestS3Frontend_DispatchRouting(t *testing.T) {
    tests := []struct {
        name       string
        method     string
        path       string
        query      string
        wantStatus int
        wantBody   string // substring
    }{
        // Shapes preserved from existing routing_test.go cases; expected
        // values must match today's responses byte-for-byte on status and
        // XML shape (namespace pinned).
        {"PUT bucket", "PUT", "/testbucket", "", 200, ""},
        {"GET object missing", "GET", "/testbucket/missing", "", 404, "NoSuchKey"},
        {"unsupported method", "BREW", "/testbucket", "", 405, ""},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            f := s3.New(&stubBackend{}, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
            srv := httptest.NewServer(f.Handler())
            defer srv.Close()
            req, _ := http.NewRequest(tt.method, srv.URL+tt.path+"?"+tt.query, nil)
            resp, err := srv.Client().Do(req)
            if err != nil {
                t.Fatalf("request: %v", err)
            }
            defer resp.Body.Close()
            body, _ := io.ReadAll(resp.Body)
            if resp.StatusCode != tt.wantStatus {
                t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, tt.wantStatus, body)
            }
            if tt.wantBody != "" && !strings.Contains(string(body), tt.wantBody) {
                t.Fatalf("body %q missing %q", body, tt.wantBody)
            }
        })
    }
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/frontend/s3/ -run TestS3Frontend_Dispatch -v`
Expected: FAIL — Handler() not yet wired.

**Step 3: Write minimal implementation**

Mechanics (a move, not a rewrite):
1. Copy each file's content into `internal/frontend/s3/`, changing
   `package main` → `package s3`. Keep function bodies byte-identical at
   first; get it compiling.
2. Handlers that touched the filesystem now call the injected
   `backend.Backend` (field `b` on the `s3` struct). Mechanical mapping:
   object read/write/delete → Backend object ops; bucket list/create/delete →
   Backend bucket ops; multipart sessions → Backend multipart ops;
   `lockObject`/`getMultipartLock` semantics are owned by the Backend
   implementation per backend-interface-2026-09 — the frontend must not
   retain its own filesystem locks. `ObjectMetadata`-typed data crosses the
   seam as object-model types per object-model-2026-09; adapt at the boundary
   if names differ, in one place, documented.
3. `dispatch.go`'s rootHandler-equivalent becomes an unexported
   `func (f *Frontend) serveHTTP(w, r)` wrapped by `Handler()`. Same path
   parse, same method+query routing table, same order of checks.
4. `Handler()` wires: `sigv4Authenticator.Authenticate` → on error map to
   `writeS3Error` with the same status/code pairs as today → on success the
   existing routing.
5. main.go: remove moved code, keep only construction
   (`s3.New(backendInstance, s3.WithCredentialSource(credSourceFromConfig))`)
   and mount `f.Handler()` at `/` exactly where rootHandler was registered.
   Credentials still come from config.go/env (`MINIS3_ACCESS_KEY`/
   `MINIS3_SECRET_KEY`, default minioadmin) — wrap them in a
   `auth.CredentialSource` in package main.
6. Storage helpers that remain Backend-owned (storage.go) stay in package
   main for the Backend implementation to use; anything only the s3 frontend
   needed moves with it.

**Step 4: Run test to verify pass AND the full regression gate**

Run:
```
go test ./internal/frontend/s3/ -v
go test ./... -count=1
make test && make test-race
make e2e
```
Expected: ALL PASS. `make e2e` green is the behavior-preservation proof; any
harness delta means the move changed behavior — fix the move, not the test.

### Task 4: Conformance suite run + capability degradations

**Objective:** Prove the s3 frontend passes leaf 01's conformance suite and
that capability shortfalls surface as protocol-appropriate errors.

**Files:**
- Modify: `internal/frontend/s3/frontend_test.go`
- Modify: `internal/frontend/s3/bucket_handlers.go` (only if any request path
  currently emits a non-protocol error where `frontend.ErrCapability` should
  feed `writeS3Error` — e.g. versioning requests)

**Step 1: Write failing test**

```go
package s3_test

import (
    "testing"

    "mini-s3/internal/frontend"
    "mini-s3/internal/frontend/s3"
)

func TestS3Frontend_Conformance(t *testing.T) {
    f := s3.New(&stubBackend{}, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
    frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}

func TestS3Frontend_VersioningDegrades(t *testing.T) {
    tests := []struct {
        name       string
        method     string
        path       string
        query      string
        wantStatus int
        wantBody   string
    }{
        // Versioning is declared false in Capabilities; a versioning request
        // must be rejected at the seam with an S3-appropriate error — the
        // same response as today (Not Implemented family), never emulation.
        {"PUT bucket versioning", "PUT", "/testbucket", "versioning", 501, "NotImplemented"},
        {"GET bucket versioning", "GET", "/testbucket", "versioning", 501, "NotImplemented"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            resp := doSigned(t, tt.method, tt.path, tt.query)
            defer resp.Body.Close()
            if resp.StatusCode != tt.wantStatus {
                t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
            }
            // body contains tt.wantBody per S3 XML error shape
        })
    }
}
```

**Step 2: Run test to verify pass/failure**

Run: `go test ./internal/frontend/s3/ -run "TestS3Frontend_Conformance|TestS3Frontend_VersioningDegrades" -v`
Expected: conformance PASS. If versioning degradation is not yet
capability-error-driven, Step 3 fixes it.

**Step 3: Write minimal implementation**

Route unsupported-capability requests through the seam helper: dispatch
checks the routing table against `f.Capabilities()`; a request targeting an
undeclared capability (versioning subresource for s3) produces
`frontend.ErrCapability("Versioning")`, which `writeS3Error` maps to the
existing S3 response (501 `NotImplemented`) — identical wire output to
today, now seam-driven.

**Step 4: Verify full gate**

Run: `go test ./... -count=1 && make test && make e2e`
Expected: ALL PASS, harness unchanged.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented; `go test ./... -count=1`, `make test`, `make test-race` pass
- [ ] `make e2e` (integration harness) passes unchanged — behavior preserved
- [ ] `internal/frontend/s3` contains zero `os.`/`ioutil.`/`filepath` filesystem calls (grep and confirm; string/path manipulation is fine)
- [ ] `package main` contains no S3 handler/dispatch/SigV4/XML-marshaling logic anymore
- [ ] Frozen contracts intact: `frontend.Frontend`, `ProtocolCaps`, `auth.Authenticator` v1 shape untouched; `Name()` returns exactly `"s3"`
- [ ] Capabilities: Buckets/ConditionalReads/Multipart/PresignedURLs true, Versioning false, with versioning requests degrading at the seam
- [ ] Region pinning (`us-east-1`), `AuthorizationHeaderMalformed`/400, and XML namespace behavior identical pre/post move
- [ ] Conformance suite invoked from s3 tests (`frontend.RunConformanceSuite`)
- [ ] gofmt clean (`make fmt-check`), `make vet`, `make lint NEW_FROM_REV=<rev>` pass
- [ ] No debug artifacts, no leftover TODO placeholders from test scaffolds
- [ ] All files at exact specified paths

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] SigV4 moved whole: canonical requests, presigned parsing, aws-chunked decoding, region pinning all present under `internal/frontend/s3/`
- [ ] Every storage call in `internal/frontend/s3/` goes through `backend.Backend`; no direct filesystem access
- [ ] Wire behavior identical: same routes, status codes, XML bytes, error codes (spot-check against pre-move outputs; harness green)
- [ ] `auth.CredentialSource` injectable; no reads of `serverCredentials` from inside `internal/frontend/s3`
- [ ] main.go is wiring-only for the S3 path (construct frontend, mount handler, credentials adapter)
- [ ] Conformance suite runs against the s3 frontend in its tests
- [ ] Code follows project conventions (stdlib only, errors as values, gofmt)
- [ ] No scope creep: no config `frontends` key, no registry wiring in main (that is leaf 03), no WebDAV/FTP code

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- This is the riskiest leaf in the tree: ~3,500 lines move. The harness, not
  code review, is the authority on "behavior-preserving."
- Move-then-adapt, in that order: first get files compiling in the new
  package with minimal edits, then reroute storage through Backend. Mixing
  both steps makes regressions unattributable.
- Do not split or refactor signing logic during the move — canonical request
  construction, chunked decoding, and presigned handling move as one unit.
  Hardening trees own any SigV4 changes.
- If the backend-interface-2026-09 surface is missing a method a handler
  needs, STOP and report BLOCKED — do not add filesystem calls to work around
  it, and do not extend the Backend interface from this leaf.
- `xml.Header` prolog plus the pinned namespace (`s3XMLNamespace`) must appear
  on every response document exactly as before; byte-compare a sample of
  error and success bodies pre/post move if in doubt.
- Existing tests that lived in package main for moved code (sigv4 tests,
  routing tests, range/conditional tests) migrate to `internal/frontend/s3/`;
  lifecycle tests (main_test.go, main_lifecycle_test.go) stay in main.
