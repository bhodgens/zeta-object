# Backend Interface (Pluggable Data-Plane Seam) — Implementation Orchestrator

> Note: this tree predates the project rename; "mini-s3" in the text below is now "zeta-object".

Created: 2026-09-28. Sibling tree dependency: `object-model-2026-09` (internal/objectmodel
package — MUST complete first). Downstream consumers: `frontend-interface` tree (WebDAV/SFTP
frontends route through this seam) and `metadata-zfs` tree (ZFS metadata backend registers here).

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 3 leaves under `docs/plans/backend-interface-2026-09/`
- **Scope:** Introduce a pluggable `Backend` interface over the neutral object model, a
  local-filesystem implementation extracted from the existing handlers, and a registry/config
  layer that selects per-bucket backends from `config.json`.

## Goal

mini-s3 currently stores objects directly from its S3 handlers: `object_handlers.go` (1324
lines) and `multipart_handlers.go` (1051 lines) map S3 requests straight onto filesystem paths
(97 direct `os.*` calls across the four storage-touching files) with `.metadata/<object>.meta`
JSON sidecars written via the atomic helpers in `storage.go`. This couples the S3 protocol
layer to one storage implementation and blocks two roadmap items: distributed ZFS-ring backing
(`docs/plan-distributed-zfs-backing.md`) and non-S3 protocol frontends.

This tree inserts a single seam: the `Backend` interface in a new `internal/backend` package,
defined over the neutral object model types from `internal/objectmodel` (Get/Put/Delete/Stat/
List/Buckets/Capabilities). Leaf 01 authors the interface, an error taxonomy bridging
`objectmodel.Error` to backend errors, and a reusable conformance test suite. Leaf 02 extracts
the existing local-filesystem behavior into `internal/backend/fsbackend` preserving the exact
on-disk layout (so existing buckets keep working with zero migration) and flips the handlers to
call through the Backend, gated by the existing integration test harness. Leaf 03 adds the
registry (`name → constructor`), `config.json` `backends` + per-bucket `backend` keys
(backward compatible: absent = fs), and `main.go` startup wiring.

User-visible outcome: none. Behavior is preserved byte-for-byte; the payoff is that a future
`zfsring` or `s3proxy` backend is one package + one `init()` registration + a config line.

## Architecture

Three packages, layered strictly downward with no import cycles:

```
S3 handlers (package main, object_handlers.go / multipart_handlers.go)
        │  call through
        ▼
internal/backend          Backend interface + Registry + BackendConfig
        │  consumes types from
        ▼
internal/objectmodel      neutral object model (sibling tree, pre-existing)
        ▲
internal/backend/fsbackend  local-filesystem Backend (extracted from handlers)
```

- `internal/backend` depends only on stdlib + `internal/objectmodel`.
- `internal/backend/fsbackend` depends on `internal/backend` (+ objectmodel); it reuses the
  lock/atomic-write *techniques* from `storage.go` (re-implemented in-package or accepted via
  narrow hooks — leaf 02 decides, see its Context) but must NOT import package `main`.
- The registry lives in `internal/backend` (`Registry map[string]func(BackendConfig)
  (Backend, error)`), populated by `init()` in each backend package; `main.go` builds a
  per-bucket constructor table at startup and injects a single lookup func into the handlers,
  so handlers never touch config or registry internals.
- Concurrency: fsbackend carries the per-object and per-directory locking behavior that
  `object_handlers.go` implements today (`lockObject` pattern, including the leaf-4.8
  parent-dir lock that fixed the stress-suite races). This is preserved exactly.
- Multipart stays ABOVE the seam in v1: `multipart_handlers.go` remains S3-specific
  orchestration. See Contract 4 and leaf 02's Task on part staging for the frozen decision.

## Interface Contracts

### Contract 1: Backend interface (frozen — leaves MUST NOT rename or reshape)

```go
// File: internal/backend/backend.go
package backend

import (
	"context"
	"io"

	"mini-s3/internal/objectmodel"
)

type Backend interface {
	Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error)
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error)
	Delete(ctx context.Context, bucket, key string) error
	Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error)
	List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error)
	Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error)
	Capabilities() objectmodel.CapabilitySet
}
```

- **Owner:** 01-backend-interface.md
- **Consumers:** 02-filesystem-backend.md (implements), 03-registry-config.md (constructs),
  downstream frontend-interface and metadata-zfs trees.

### Contract 2: Registry + BackendConfig (frozen)

```go
// File: internal/backend/registry.go
package backend

type Registry map[string]func(cfg BackendConfig) (Backend, error)

type BackendConfig struct {
	Type    string
	Root    string
	Options map[string]string
}
```

- Constructors registered at package `init()`: `fsbackend` registers `"fs"`.
- **Owner:** 03-registry-config.md (registry.go), 02 (its `init()` registration).
- Lookup semantics: unknown type → error (never silent fs fallback); absent per-bucket
  backend key → fs (backward compatibility, leaf 03).

### Contract 3: Error taxonomy (owner 01, consumed by 02/03 and handlers)

Backend errors are sentinel/typed values in `internal/backend` that wrap or map to
`objectmodel.Error` codes (NoSuchBucket, NoSuchKey, BucketAlreadyExists, etc. — exact
codes per the objectmodel tree's pinned API). The seam rule: a Backend NEVER returns an
`*os.PathError` upward; handlers switch on `objectmodel` error identity only.

```go
// File: internal/backend/errors.go (shape; leaf 01 finalizes against objectmodel)
package backend

// ErrNotSupported is returned for operations a backend cannot serve;
// Capabilities() is the advisory path, errors are the authoritative one.
var ErrNotSupported = errors.New("backend: operation not supported")
```

### Contract 4: Multipart-above-the-seam decision (frozen rationale, leaf 02 documents)

The existing multipart flow (initiate/uploadPart/complete/abort in
`multipart_handlers.go`) remains in package `main`, above the Backend seam, in v1. It is
S3-specific orchestration: parts are staged by direct fs writes today, and `CompleteMultipart`
assembles the final object. v1 policy:

- Multipart part staging and completion continue to use the existing direct-fs path via the
  bucket's resolved *fs* root when the bucket's backend is `fs`.
- For non-fs backends in the future, this is a documented limitation until a
  `BeginPart/CompleteParts` extension or streamed-part contract exists. Leaf 02 MUST document
  with rationale whether Backend.Put's `data io.Reader` + `size int64` signature is sufficient
  for streamed unknown-size part writes (signature allows `-1`/unknown handling — leaf 02
  decides: either Put accepts size<=0 streamed writes with buffered staging, or parts are
  staged fs-side and finalized through Put; whichever it picks must be written into
  fsbackend's doc comment and this tree's Integration Test Plan is unaffected since multipart
  remains main-side).

### Contract 5: Handler injection point (owner 02, consumed by 03)

Handlers stop touching the fs directly and call a package-level indirection installed at
startup:

```go
// File: backend_lookup.go (package main) — created by leaf 02, wired by leaf 03
package main

// backendFor returns the Backend serving bucket. Installed by main.go at
// startup (leaf 03); nil in leaf 02's intermediate state, in which case the
// default fs backend (one instance rooted at dataDir + custom-bucket map) is used.
var backendFor func(bucket string) (backend.Backend, error)
```

- Leaf 02 defines the var + default behavior and migrates handlers through it.
- Leaf 03 replaces the default with registry/config-driven construction. After leaf 03,
  `backendFor` is always non-nil before the server accepts connections.

### Contract 6: On-disk layout (frozen, owner 02 — byte-for-byte preservation)

```
<root>/<bucket>/<object-file>
<root>/<bucket>/.metadata/<object>.meta     # ObjectMetadata JSON, field names unchanged
```

ObjectMetadata JSON keys (`contentType`, `contentLength`, `eTag`, `customMetadata`,
`lastModified`, `storagePath` — types.go:51) are produced identically. Existing buckets work
with NO migration. The historical PUT-overwrite data-loss bug (documented in CLAUDE.md /
gap-closure.md; fix verified in `object_handlers.go` RESIDUAL WINDOW comments — leaf 02
verifies current behavior with a test BEFORE extracting, then preserves it).

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-backend-interface.md | leaf | objectmodel tree complete | 45K | A |
| 02 | 02-filesystem-backend.md | leaf | 01 | 70K | B |
| 03 | 03-registry-config.md | leaf | 01, 02 | 40K | C |

**Concurrency groups:** strictly sequential (A→B→C); each leaf builds on the previous
leaf's landed code.

## Dispatch Protocol

For each leaf, in order (01 → 02 → 03):

### Phase 1: Dispatch Leaf

1. **Read** the leaf document and dispatch via `delegate_task`:
   - Goal: "Implement all tasks from 01-backend-interface.md" (substitute leaf name)
   - Context: Full leaf document text + Contracts 1–6 from this orchestrator + the Coding
     Conventions block below + relevant existing source INLINED:
     - leaf 01: nothing from main (objectmodel types only — pinned by sibling tree)
     - leaf 02: `object_handlers.go` (put/get/head/delete/copy paths),
       `storage.go`, `types.go:48-58`, `bucket_handlers.go` (getBucketPath,
       validateBucketName), `multipart_handlers.go` staging sections
     - leaf 03: `config.go`, `main.go`, `config.json.example`
   - Include: "Do NOT commit. Do NOT run git add. Write code, run tests, report results only."
   - Include: "Do NOT use read_file on existing source files — explore with search_files or
     terminal cat. Never feed read_file output into write_file. After writing a file, do NOT
     read it back to verify."
   - Include: "STOP clause: any blocker outside your assigned files — stop and report with
     the exact unblock condition."
2. Agent follows TDD per the leaf's instructions.

### Phase 2: Review and Commit Each Leaf

After each implementation agent returns, the orchestrator reviews **in-session** (NOT via
delegated subagent — children inherit the same model):

1. Read the changed files listed by the implementer.
2. Check against leaf spec + Contracts 1–6 + Review Checklist below.
3. Run gates: `go build ./...`, `go vet ./...`, `go test ./... -count=1`; for leaf 02 also
   `make e2e` (integration harness from commits 7923cd5/a596b9e/dfce3bc is the regression
   gate) and `go test -race ./...`.
4. If gaps: re-dispatch with specific feedback (max 3 cycles, then escalate).
5. If pass: `git add <exact leaf file list> && git commit -m "feat(backend): <leaf>"`;
   update tracking table to REVIEWED.

### Phase 3: Integration Review

After all leaves are REVIEWED:

1. Verify Contracts 1–6 hold across packages (interface shape, registry lookup, error
   identity, on-disk layout unchanged — diff a pre/post PUT'd object's sidecar bytes).
2. Run full suite: `make check` + `make test-cover-enforce` + `make e2e`.
3. Round-trip check: create bucket/put/list/get/delete against a data dir populated by the
   PRE-seam binary — must succeed unchanged (no migration).
4. Normalize: `make fmt`; verify no line-number corruption:
   `grep -rcE '^\s+[0-9]+\|' --include='*.go' --include='*.md' .` returns zero.
5. Commit integration fixes if any; update tracking table to COMPLETE.

## Review Checklist

The orchestrator verifies each leaf in-session:

- [ ] All tasks from the leaf document implemented
- [ ] Interface contracts satisfied exactly (frozen names/signatures untouched)
- [ ] All specified files created/modified at exact paths
- [ ] Tests written and passing (TDD; RED/GREEN evidence in report)
- [ ] Code follows project conventions (below)
- [ ] No scope creep beyond spec
- [ ] No obvious bugs or security issues (path traversal, lock gaps, error swallowing)
- [ ] No debug artifacts: no print debugging, TODOs, placeholders, commented-out code
- [ ] No line-number corruption (` N|` prefixes) in source or docs
- [ ] Leaf 02 specific: on-disk layout byte-identical; harness green; pre-extraction
      overwrite-bug verification test present
- [ ] Leaf 03 specific: config absent-key = fs verified by test; unknown backend type errors

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go 1.27, module `mini-s3`, stdlib ONLY (no new deps without orchestrator
  approval). New packages under `internal/`; existing server code stays package `main`.
- **Naming:** exported = PascalCase; unexported = camelCase; package names lowercase,
  no underscores (`fsbackend`).
- **Imports:** stdlib group, then `mini-s3/internal/...` group; goimports
  `-local mini-s3`; gofmt clean.
- **Errors:** errors as values; wrap with `%w`; sentinel errors in `internal/backend`;
  a Backend never leaks `*os.PathError` upward; no panics in library packages.
- **Testing:** stdlib `testing` only, table-driven; `_test.go` alongside; conformance suite
  is a plain exported test func other packages call on themselves.
- **Context:** every Backend method honors ctx cancellation (check at loop/IO boundaries).
- **Formatting:** `make fmt` before reporting; gates: `go build ./... && go vet ./... &&
  go test ./... -count=1`.
- **Commits:** leaves NEVER commit — orchestrator only, after review.

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-backend-interface.md | COMPLETE    | 0 | |
| 02-filesystem-backend.md | COMPLETE    | 0 | |
| 03-registry-config.md | COMPLETE    | 0 | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. **Unit + conformance:** `go test ./internal/... -count=1` — includes fsbackend running
   the shared conformance suite (leaf 01) against itself.
2. **Race:** `go test -race ./...` — fsbackend locking under concurrent Get/Put/Delete of
   the same key and same directory.
3. **Behavior-preservation harness:** `make e2e` — the existing integration suite
   (commits 7923cd5/a596b9e/dfce3bc; 101 asserts per hardening leaf 3.6) must be green
   with handlers calling through the Backend. This is the regression gate for leaf 02.
4. **On-disk compatibility (manual-but-scripted):** run pre-seam binary, PUT 3 objects
   (incl. one with `x-amz-meta-*` headers and one multipart-complete); stop; run post-seam
   binary against the same dataDir; GET all three → identical bytes, Content-Type,
   ETag, custom metadata. Sidecar JSON byte-diff must be empty (modulo `lastModified`
   on newly written objects only).
5. **Config matrix (leaf 03):** (a) config with no `backends`/`backend` keys → all-fs,
   harness green; (b) bucket with `"backend": "fs"` → same; (c) bucket with
   `"backend": "nope"` → startup error naming the type; (d) two buckets, different
   `backends` roots → objects land in the right roots.
6. **Coverage floor:** `make test-cover-enforce` stays green (new packages raise, not
   dilute, coverage).

## Structural Completeness Check (Before Dispatch)

Required orchestrator sections present in this document: Dispatch Protocol ✓, Interface
Contracts ✓, Child Index ✓, Review Checklist ✓, Coding Conventions ✓, Completion Tracking
Table ✓, Integration Test Plan ✓. Run
`python3 ~/.hermes/skills/software-development/hierarchical-planning/scripts/check_template_compliance.py docs/plans --strict-leaves`
before dispatching any agent; fix until `ALL TREES COMPLIANT: True`.

## Notes

- **Dependency ordering:** `internal/objectmodel` (tree `object-model-2026-09`) must be
  merged FIRST. Its types (Object, GetOptions, PutOptions, ListParams, ListPage,
  BucketInfo, CapabilitySet, Error) are pinned by that tree; this tree consumes them and
  must not re-declare. If objectmodel drifted from what leaves assume, the orchestrator
  reconciles Contracts 1–3 before dispatch, not the leaves.
- **Frontend/metadata trees:** the `frontend-interface` tree (WebDAV/SFTP) and
  `metadata-zfs` tree both build on this seam — `Capabilities()` exists precisely so
  frontends can degrade (e.g. no multipart on ZFS ring) and so the metadata backend can
  advertise restrictions. Do not trim CapabilitySet to fs-only needs.
- **Known risky areas:** (a) the leaf-4.8 parent-dir lock pattern — dropping it during
  extraction reintroduces stress-suite races (spurious PUT/GET 500s); (b) the
  PUT-overwrite residual-window semantics (do not "improve" to os.Remove — that was the
  historical data-loss bug); (c) `resolveObjectDataPath` (object_handlers.go:173) honors
  StoragePath fallback — extraction must keep the corrupt-StoragePath fallback;
  (d) multipart stays main-side; do not let leaf 02 "helpfully" migrate it.
- **ObjectMetadata vs objectmodel.Object:** the sidecar JSON stays `ObjectMetadata`
  (frozen on-disk format); `objectmodel.Object` is the in-memory neutral view. fsbackend
  converts between them; the conversion is pure and table-testable.
- **No third-party deps.** The eventual distributed backend (crush-lite ZFS ring) and any
  S3-proxy backend land in their own trees against the same Registry; nothing in this tree
  should anticipate their internals beyond the constructor signature.
