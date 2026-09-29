# Frontend Protocol Seam (internal/frontend) - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 3 leaf documents under this node
- **Scope:** Introduce a pluggable `Frontend` protocol seam in `internal/frontend` that decodes wire protocols into the neutral object model via `internal/backend.Backend`, and extract the existing S3 HTTP layer as the first frontend without behavior change.

## Goal

Today mini-s3 speaks exactly one protocol: S3 over HTTPS, wired directly in
package `main` — `rootHandler` (main.go:122) parses paths, `authenticateRequest`
(sigv4.go:438) verifies SigV4, and handlers in `object_handlers.go`,
`bucket_handlers.go`, and `multipart_handlers.go` touch storage directly. There
is no seam where another wire protocol could attach.

This tree introduces that seam. A `frontend.Frontend` interface becomes the
single extension point for "how clients talk to mini-s3": it decodes an
incoming wire protocol into the neutral object model, calls only the
`internal/backend.Backend` interface, and encodes protocol-appropriate
responses. The existing S3 layer is extracted into
`internal/frontend/s3` as the first frontend with zero behavior change — the
existing integration test harness (commits 7923cd5, a596b9e, dfce3bc) is the
regression gate.

The payoff is the future frontend roadmap, each already a filed GitHub issue
and a consumer of this seam:

- GH issue `frontend: WebDAV` (issue-webdav-frontend.md) — HTTP-based, no bucket concept
- GH issue `frontend: (S)FTP` (issue-sftp-ftp-frontend.md) — non-HTTP protocol, no SigV4
- GH issue `frontend: ownCloud` (issue-owncloud-frontend.md) — WebDAV superset with metadata endpoints

Semantic rule binding on every leaf: **where a protocol cannot express a
capability (e.g. WebDAV has no bucket concept, S3 has no versioning), the
frontend rejects or degrades at the seam with a protocol-appropriate error —
never silent emulation.** `Capabilities()` advertises what a frontend can
express so routing and conformance tests can reason about it.

## Architecture

Three leaves build one vertical slice: contract, extraction, wiring. Leaf 01
creates the `internal/frontend` package — the `Frontend` interface,
`ProtocolCaps`, capability-negotiation error helpers, and a reusable
conformance test suite. Leaf 02 moves the existing S3 HTTP layer (rootHandler
dispatch, SigV4 authentication, object/bucket/multipart handler routing, XML
marshaling from types.go/xml.go) into `internal/frontend/s3` as the first
implementation, rerouting storage calls through the `Backend` interface from
the sibling backend-interface-2026-09 tree, behavior-preserving. Leaf 03 makes
server startup mount registered frontends: config gains a backward-compatible
`frontends` list (absent = S3 only on the existing listenAddr), and multi-listener
support (a second port for a second frontend) is decided and documented here.

Control flow after this tree: `main.go` → frontend registry (leaf 03) → one or
more `Frontend.Handler()` http.Handlers → each frontend decodes its protocol →
`backend.Backend` interface → storage. Auth at the seam goes through
`Authenticator()` returning an `auth.Authenticator` placeholder per the pinned
contract; the open GH issue `auth: pluggable authentication architecture`
will replace the placeholder with a real identity model, so nothing in this
tree may hardcode credential checks outside the s3 frontend's SigV4 adapter.

## Interface Contracts

### Contract 1: `Frontend` interface and `ProtocolCaps` (FROZEN)

```go
// File: internal/frontend/frontend.go
package frontend

import (
    "net/http"

    "mini-s3/internal/auth"
)

type Frontend interface {
    Name() string                 // "s3", "webdav", "ftp", ...
    Handler() http.Handler        // mounts itself under the server mux
    Authenticator() auth.Authenticator // per-frontend auth adapter; auth model decided by the open auth GH issue
    Capabilities() ProtocolCaps   // what this protocol can express
}

type ProtocolCaps struct {
    Buckets           bool
    Versioning        bool
    ConditionalReads  bool
    Multipart         bool
    PresignedURLs     bool
}

// Owner: 01-frontend-interface.md
// Consumers: 02-s3-frontend-extraction.md, 03-router-registration.md,
// and all future frontend GH issues (WebDAV, (S)FTP, ownCloud).
```

### Contract 2: `auth.Authenticator` v1 shape (FROZEN)

```go
// File: internal/auth/auth.go (package created here, fleshed out by the
// "auth: pluggable authentication architecture" GH issue later)
package auth

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

// Owner: 01-frontend-interface.md
// Consumers: 02-s3-frontend-extraction.md (SigV4 adapter implements it),
// 03-router-registration.md (wires Authenticator per frontend).
```

The open GitHub issue `auth: pluggable authentication architecture` will
replace this placeholder with a real identity model. Leaves refine around it
but MUST NOT rename or remove it, and MUST NOT invent a deeper identity model.

### Contract 3: `Backend` seam (owned by sibling tree)

```go
// File: internal/backend/backend.go (sibling tree backend-interface-2026-09)
package backend

// Frontends MUST call only this interface — no direct os./filesystem access.
// The exact surface (PutObject, GetObject, ListBuckets, multipart lifecycle,
// ...) is pinned by the backend-interface-2026-09 master. This tree treats it
// as a dependency, not a deliverable.

// Owner: backend-interface-2026-09 tree
// Consumers: 02-s3-frontend-extraction.md (all handler storage calls rerouted)
```

### Contract 4: Frontend registration

```go
// File: internal/frontend/registry.go (from 01) used by 03
package frontend

// Registry holds Frontends by Name(). main.go iterates registered frontends
// and mounts each Handler() per the config mapping decided in 03.
type Registry struct{ /* ... */ }

func NewRegistry() *Registry
func (r *Registry) Register(f Frontend) error // error on duplicate Name()
func (r *Registry) Lookup(name string) (Frontend, bool)
func (r *Registry) All() []Frontend           // deterministic order (sorted by Name)

// Owner: 01-frontend-interface.md
// Consumer: 03-router-registration.md
```

### Contract 5: Capability negotiation error helpers

```go
// File: internal/frontend/caps.go (from 01)
package frontend

// ErrCapability expresses "this protocol cannot express X" so frontends
// render a protocol-appropriate error at the seam. Never silent emulation.
func ErrCapability(cap string) error
func IsCapabilityError(err error) bool

// Owner: 01-frontend-interface.md
// Consumers: 02-s3-frontend-extraction.md, all future frontends (GH issues)
```

## Child Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | [01-frontend-interface.md](01-frontend-interface.md) | leaf | object-model-2026-09, backend-interface-2026-09 (types only) | 60K | A |
| 02 | [02-s3-frontend-extraction.md](02-s3-frontend-extraction.md) | leaf | 01 | 110K | B |
| 03 | [03-router-registration.md](03-router-registration.md) | leaf | 01, 02 | 70K | C |

**Concurrency groups:** strictly sequential (A → B → C): 02 implements
contract 1 and needs 01's package to exist; 03 mounts 02's frontend and needs
both.

**External dependencies (whole tree):** depends on the object-model-2026-09
tree (neutral object model types) and the backend-interface-2026-09 tree
(`internal/backend.Backend`). If either has not landed, 02 blocks — do not
improvise a parallel backend surface.

## Dispatch Protocol

Single dependency chain — dispatch strictly one at a time.

### Phase 1: Dispatch Leaf 01

1. **Read** [01-frontend-interface.md](01-frontend-interface.md) and dispatch via `delegate_task`:
   - Goal: "Implement all tasks from 01-frontend-interface.md"
   - Context: full leaf document text + Interface Contracts from this master
     + Coding Conventions block + internal/frontend pinned contract INLINED
   - Include: "Do NOT commit. Do NOT run git add. Write code, run tests, report results only."
   - Include: "Do NOT use read_file on existing source files — explore with search_files
     or terminal cat instead. If you read a file, never feed its output into write_file."

### Phase 2: Review Leaf 01, Dispatch Leaf 02

1. **Orchestrator reviews leaf 01 in-session** (main model, NOT a delegated
   reviewer): contracts match exactly, conformance suite compiles and passes,
   `gofmt` clean. Max 3 re-dispatch cycles, then escalate.
2. **Read** [02-s3-frontend-extraction.md](02-s3-frontend-extraction.md) and dispatch via `delegate_task` with the same boilerplate as Phase 1, plus:
   - Context additionally INLINES: current signatures of `rootHandler`,
     `authenticateRequest`, handler entry points in object/bucket/multipart
     handler files, and the `Backend` interface from backend-interface-2026-09.
   - Include: "The integration test harness is the regression gate — `make test`
     must pass with no behavior change before you report."

### Phase 3: Review Leaf 02, Dispatch Leaf 03

1. **Orchestrator reviews leaf 02 in-session:** package `main` no longer
   contains HTTP S3 handler logic; harness green; no `os.`/filesystem calls in
   `internal/frontend/s3` outside the seam.
2. **Read** [03-router-registration.md](03-router-registration.md) and dispatch via `delegate_task` with the same boilerplate.

### Phase 4: Integration Review

1. **Orchestrator runs integration review in-session:**
   - `make build`, `make vet`, `make fmt-check`, `make test`, `make test-race` all pass
   - Harness (`make e2e`) green — S3 behavior byte-identical to pre-refactor
   - `grep -rn "os\.\|ioutil\." internal/frontend/` returns no filesystem
     access outside the Backend seam (errors.As/tests excepted)
   - Config without `frontends` key starts exactly as before (backward compat)
   - No import cycle `internal/frontend` → `main`
2. If gaps: re-dispatch the owning leaf with contract-fix instructions, re-review.
3. If pass: run `gofmt` over changed files, verify no line-number corruption
   (`grep -rcE '^\s+[0-9]+\|' --include='*.go' .` returns zero), commit per leaf
   with conventional messages (`feat(frontend): ...`), update the tracking
   table to COMPLETE.

## Review Checklist

The orchestrator (main model) verifies each child in-session:

- [ ] All tasks from the leaf document are implemented
- [ ] Interface contracts from this master are satisfied exactly (names, signatures, file paths)
- [ ] Frozen contracts untouched: `Frontend`, `ProtocolCaps`, `auth.Authenticator` v1 shape
- [ ] All specified files created/modified at exact paths
- [ ] Tests written and passing (TDD followed); conformance suite reusable, not S3-specific
- [ ] Frontends call only `backend.Backend` — no direct `os.`/filesystem access
- [ ] Capability shortfalls produce protocol-appropriate errors at the seam — no silent emulation
- [ ] Code follows project conventions (see Coding Conventions below)
- [ ] No scope creep (no WebDAV/FTP implementation — those are future GH issues)
- [ ] No debug artifacts: no print debugging, no TODOs, no placeholder values, no commented-out code
- [ ] No line-number corruption: no `     N|` prefixes baked into source files

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go (module `mini-s3`, see go.mod for toolchain), stdlib only — no new dependencies
- **Style:** gofmt + goimports with `-local mini-s3` (`make fmt` before reporting)
- **Naming:** exported = PascalCase with doc comments; unexported = camelCase; packages lowercase single words (`frontend`, `s3`)
- **Imports:** stdlib group, then module-internal; no unused imports
- **Errors:** errors as values; wrap with `%w`; sentinel errors for capability negotiation; no panic in library code
- **Tests:** stdlib `testing`, table-driven, `_test.go` alongside source; no testify
- **Gates:** `make vet`, `make lint NEW_FROM_REV=<rev>`, `make test` must pass per leaf; `make test-cover-enforce` floor holds

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-frontend-interface | COMPLETE    | 0 | |
| 02-s3-frontend-extraction | COMPLETE    | 0 | |
| 03-router-registration | COMPLETE    | 0 | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

The existing integration test harness (commits 7923cd5, a596b9e, dfce3bc) is
the regression gate for the behavior-preserving extraction.

1. **Unit + conformance:** `go test ./internal/frontend/... -v` — interface
   tests, registry tests, and the S3 frontend passing the conformance suite.
2. **Full suite:** `make test` then `make test-race` — zero failures; coverage
   floor (`make test-cover-enforce`) not regressed.
3. **Harness:** `make e2e` — all pre-existing S3 integration scenarios pass
   unchanged (this proves extraction was behavior-preserving).
4. **Cross-boundary contract checks:**
   - Registry round-trip: register a stub frontend in a test, `Lookup` by name,
     mount `Handler()` on an `httptest.Server`, exercise one request end-to-end
     to `Backend` (stubbed) — proves Frontend→Registry→Handler→Backend wiring.
   - Backward compat: integration run with a config lacking `frontends` behaves
     identically to today (S3 on `listenAddr`).
   - Multi-listener (if leaf 03 adopts it): second listener serves its frontend;
     first listener unaffected; shutdown drains both.
5. **Static gates:** `make build`, `make vet`, `make fmt-check`, `make lint`,
   plus the filesystem-access grep from Phase 4.

## Structural Completeness Check (Before Dispatch)

Before dispatching any child, confirm this master contains every required
section: Meta, Goal, Architecture, Interface Contracts, Child Index, Dispatch
Protocol, Review Checklist, Coding Conventions, Completion Tracking Table,
Integration Test Plan, Notes. If any is missing, fill it in first.

## Notes

- **Pinned contracts are frozen.** Leaves refine (add methods to concrete
  types, add helpers) but MUST NOT rename or remove `Frontend`, `ProtocolCaps`,
  or the `auth.Authenticator` v1 shape. The auth GH issue
  (`auth: pluggable authentication architecture`) owns the future identity
  model — do not grow `Identity` in this tree.
- **The object model and Backend are external.** object-model-2026-09 and
  backend-interface-2026-09 own those surfaces. Leaf 02 is the consumer; if
  those trees have not landed, leaf 02 is BLOCKED, not improvisable.
- **Future frontends are out of scope.** GH issues `frontend: WebDAV`,
  `frontend: (S)FTP`, `frontend: ownCloud` (issue-*.md files in the planning
  scripts directory) are consumers of this seam, not part of this tree. Build
  the seam so their needs (non-HTTP protocols for (S)FTP, no-bucket model for
  WebDAV) are answerable by `Capabilities()` + capability error helpers.
- **Riskiest leaf is 02.** It moves ~3,500 lines of handler code. The
  behavior-preserving constraint is enforced by the harness, not by inspection:
  if the harness disagrees, the extraction is wrong, full stop.
- SigV4 stays whole: canonical requests, chunked decoding, presigned URLs move
  as one unit into `internal/frontend/s3` — do not split or refactor signing
  logic during the move (separate hardening trees own that).
