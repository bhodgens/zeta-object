# Neutral Object Model (internal/objectmodel) - Implementation Orchestrator

> Note: this tree predates the project rename; "mini-s3" in the text below is now "zeta-object".

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 2 leaf documents under this node
- **Scope:** Define the backend-neutral object model package `internal/objectmodel/` that all future Backend implementations, Frontend protocol plugins, and MetadataProviders share.

## Goal

mini-s3 is growing a gateway initiative: alternate storage Backends (filesystem
today, ZFS and others later), alternate Frontend protocols (S3 today; WebDAV,
SFTP, OwnCloud later), and pluggable MetadataProviders. Every one of those
components needs a shared, protocol-neutral vocabulary for "what an object is":
its key, size, ETag, timestamps, user metadata, the shape of a list page, the
shape of conditional GET/PUT options, and a backend-neutral error taxonomy.

Today that vocabulary lives implicitly in `package main`: `ObjectMetadata`
(types.go:48) mixes protocol-visible fields with a filesystem-internal
`StoragePath`, error codes are S3 strings scattered through handlers, and
precondition evaluation is coupled to `*http.Request`. Any second Backend or
Frontend would have to re-parse S3-shaped structs, which is exactly the
coupling this tree removes.

This tree delivers `internal/objectmodel/`: a small, dependency-free Go package
defining the canonical types (leaf 01) and the canonical S3 metadata surface —
what is visible on the wire, how it serializes, and the parity rule that gates
all future Backends and MetadataProviders (leaf 02).

**Dependency direction (binding for sibling trees):** this tree must complete
BEFORE `backend-interface-2026-09` and `metadata-zfs-2026-09` start; both
consume these types directly. `frontend-interface-2026-09` also depends on this
tree (Frontends translate their protocol onto `Object`, `ListParams`,
`GetOptions`, `PutOptions`, and the `Error` taxonomy). Sibling trees may add
capability-typed extensions but MUST NOT rename, remove, or re-shape the pinned
contracts below.

## Architecture

One new Go package, `internal/objectmodel`, split into three files:
`model.go` (canonical types — pinned), `errors.go` (backend-neutral error
taxonomy with per-frontend-mappable HTTP status defaults), and
`metadata.go` (serialization between HTTP headers and `Object`, plus
`ObjectMetadata` conversion). A fourth file family is `_test.go` table-driven
tests using stdlib `testing` only.

Design principles:

- **Neutral core, protocol adapters at the edges.** `Object`,
  `BucketInfo`, `ListPage`/`ListParams`, `GetOptions`/`PutOptions`,
  `CapabilitySet`, and `Error` carry no `net/http`, no XML tags, no JSON tags.
  Serialization to S3 headers is an explicit helper in `metadata.go`, so a
  future WebDAV or SFTP frontend maps the same structs to different wire forms.
- **Prefix discipline.** User metadata is stored lower-case with NO
  `x-amz-meta-` prefix inside `Object.Metadata`; the prefix is added only on
  serialize and stripped on parse. This is the single most drift-prone
  convention, so it is tested exhaustively (leaf 01) and owned by the
  serialization helpers (leaf 02).
- **storagePath never escapes the backend.** The existing
  `ObjectMetadata.StoragePath` field is filesystem-internal; the conversion
  helpers deliberately do not map it onto the neutral model.
- **Errors as values.** `objectmodel.Error` is a concrete `error`
  implementation with stable `Code`, plus constructor helpers and S3 mappings;
  Frontends map codes to their protocol's error shapes.

## Interface Contracts

The following types are **pinned by the orchestrator**. Leaves refine
implementation detail but MUST NOT rename or remove fields; additions require
justification in the leaf's Notes and orchestrator review approval.

### Contract 1: Canonical types (pinned)

```go
// File: internal/objectmodel/model.go
package objectmodel

type Object struct {
	Key          string
	Size         int64
	ETag         string            // opaque strong validator, S3 quoted form
	LastModified time.Time
	ContentType  string
	Metadata     map[string]string // user metadata; lower-case keys, NO x-amz-meta- prefix
}

type BucketInfo struct {
	Name      string
	CreatedAt time.Time
}

type ListPage struct {
	Objects        []Object
	CommonPrefixes []string
	IsTruncated    bool
	NextToken      string
}

type ListParams struct {
	Prefix            string
	Delimiter         string
	StartAfter        string
	ContinuationToken string
	MaxKeys           int
}

type GetOptions struct {
	IfMatch            string
	IfNoneMatch        string
	IfModifiedSince    time.Time
	IfUnmodifiedSince  time.Time
	Range              string
}

type PutOptions struct {
	ContentType string
	Metadata    map[string]string
	IfMatch     string
	IfNoneMatch string
}

type CapabilitySet struct {
	Multipart          bool
	Versioning         bool
	Immutable          bool
	MetadataProviders  []string
}
```

- **Owner:** 01-canonical-types.md
- **Consumers:** 02-metadata-surface.md; sibling trees backend-interface-2026-09,
  frontend-interface-2026-09, metadata-zfs-2026-09

### Contract 2: Backend-neutral error taxonomy

```go
// File: internal/objectmodel/errors.go
package objectmodel

type Error struct {
	Code       string // e.g. "NoSuchKey", "NoSuchBucket", "PreconditionFailed"
	Message    string
	HTTPStatus int  // canonical S3 default; frontends may override per-protocol
}

func (e *Error) Error() string
func NewError(code, message string, status int) *Error
func ErrNoSuchKey(key string) *Error
func ErrNoSuchBucket(bucket string) *Error
func ErrPreconditionFailed() *Error
func ErrNotModified() *Error          // 304 semantics
func ErrBucketAlreadyExists(bucket string) *Error
func ErrNotImplemented(op string) *Error
// Codes are constants: CodeNoSuchKey, CodeNoSuchBucket, CodePreconditionFailed,
// CodeNotModified, CodeBucketAlreadyExists, CodeNotImplemented, CodeInternalError,
// CodeInvalidArgument, CodeAccessDenied. Mapping to S3 XML error bodies happens
// in package main (existing errorToXML), NOT in this package.
```

- **Owner:** 01-canonical-types.md
- **Consumers:** all sibling trees; `xml.go` errorToXML maps Code → S3 error doc

### Contract 3: Normalization helpers

```go
// File: internal/objectmodel/model.go (or metadata.go where noted)
package objectmodel

// ETag normalization
func NormalizeETag(etag string) string        // strips surrounding quotes -> bare hex/md5 form
func QuotedETag(etag string) string           // ensures S3 quoted wire form
func ETagsMatch(a, b string) bool             // quote-insensitive comparison; supports "If-Match: *" via caller

// Metadata key normalization (metadata.go)
func NormalizeMetadataKey(k string) string    // lower-case; strips "x-amz-meta-" prefix
func ParseMetadataHeaders(get func(string) string, keys []string) map[string]string
func MetadataHeaderName(key string) string    // "x-amz-meta-" + key
func ObjectToMetadataHeaders(o Object) map[string][]string // Content-Type/Length, ETag, Last-Modified, x-amz-meta-*
```

- **Owner:** 01-canonical-types.md (ETag + key normalization),
  02-metadata-surface.md (header serialization)
- **Consumers:** all sibling trees

### Contract 4: Canonical S3 metadata surface (pinned definition, owned by leaf 02)

> Copied verbatim into 02-metadata-surface.md. This definition is the parity
> gate consumed by the metadata-zfs-2026-09 tree.

The S3-visible metadata of an object is exactly: Content-Type header,
Content-Length, ETag header, Last-Modified header, and x-amz-meta-* headers
(from objectmodel.Object.Metadata, prefix added on serialize, stripped on
parse). Mapping from existing ObjectMetadata (types.go:48):
contentType↔ContentType, contentLength↔Size, eTag↔ETag,
customMetadata↔Metadata, lastModified↔LastModified; storagePath is
filesystem-internal and NEVER surfaced. PARITY RULE: a MetadataProvider or
alternate Backend may add capability endpoints but MUST NOT change
Get/Head/Put/List metadata behavior; the drift gate is a parity test asserting
identical headers+body for the same requests against FS-backed vs ZFS-backed
buckets.

### Contract 5: ObjectMetadata conversion

```go
// File: internal/objectmodel/metadata.go
package objectmodel

// FromLegacy converts package main's ObjectMetadata into the neutral model.
// StoragePath is intentionally dropped (filesystem-internal).
func FromLegacy(key string, m LegacyObjectMetadata) Object

// ToLegacy reconstructs the legacy struct for the FS backend adapter.
// StoragePath must be set by the caller (backend-internal concern).
type LegacyObjectMetadata struct { // shape mirrors types.go ObjectMetadata without importing main
	ContentType    string
	ContentLength  int64
	ETag           string
	CustomMetadata map[string]string
	LastModified   time.Time
}
func ToLegacy(o Object) LegacyObjectMetadata
```

- **Owner:** 02-metadata-surface.md
- **Consumers:** backend-interface-2026-09 (FS backend adapter), main-package
  migration wiring in later trees

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | docs/plans/object-model-2026-09/01-canonical-types.md | leaf | none | 45K | A |
| 02 | docs/plans/object-model-2026-09/02-metadata-surface.md | leaf | 01 (compiles against model.go) | 45K | B |

**Concurrency groups:** Group A runs alone; group B starts only after 01 is
REVIEWED because leaf 02's code and tests import leaf 01's types.

## Dispatch Protocol

For each concurrency group, in dependency order:

### Phase 1: Dispatch Concurrency Group [A]

Dispatch this child alone:

1. **Read** docs/plans/object-model-2026-09/01-canonical-types.md and dispatch
   via `delegate_task`:
   - Goal: "Implement all tasks from 01-canonical-types.md"
   - Context: Full leaf document text + Contracts 1-3 from this orchestrator
     + Coding Conventions block below + the pinned type definitions INLINED
     + the `ObjectMetadata` struct from types.go:48 INLINED
   - Include: "Do NOT commit. Do NOT run git add. Write code, run tests, report results only."
   - Include: "Do NOT use read_file on existing source files — explore with
     search_files or terminal cat instead. If you read a file, never feed its
     output into write_file."
   - Agent follows TDD per the leaf's instructions

### Phase 2: Dispatch Concurrency Group [B]

After 01 reaches REVIEWED:

1. **Read** docs/plans/object-model-2026-09/02-metadata-surface.md and dispatch
   via `delegate_task`:
   - Goal: "Implement all tasks from 02-metadata-surface.md"
   - Context: Full leaf document text + Contracts 1-5 from this orchestrator
     + Coding Conventions block + the canonical metadata surface definition
     (Contract 4) verbatim + the actual `internal/objectmodel/model.go` and
     `errors.go` produced by leaf 01 INLINED + the
     `x-amz-meta-*` collection loop from object_handlers.go:139 INLINED
   - Include: "Do NOT commit. Do NOT run git add."
   - Include: "Do NOT use read_file on existing source files — explore with
     search_files or terminal cat instead."
   - Include: "After writing a file, do NOT read it back to verify. Write once and stop."

### Phase 3: Review and Commit Each Child

After each implementation agent returns, the orchestrator reviews in-session
(the main model reviews directly, NOT a delegated subagent — delegate_task
children inherit delegation.model, so a delegated reviewer would use the
same subagent model that wrote the code):

1. **Orchestrator reviews in-session:**
   - Read the changed files (from the implementer's file list)
   - Check against leaf spec + pinned contracts + Review Checklist below
   - Run `make build && make test && make fmt-check`
   - Verify pinned structs byte-match Contract 1 (field names, order, comments)

2. **If review finds gaps:**
   - Re-dispatch implementation agent with specific feedback
   - Context: original leaf + review findings + "fix these gaps"
   - Re-review after return
   - Max 3 re-dispatch cycles; if still failing, escalate to parent

3. **If review passes:**
   - **Commit the leaf's files:** `git add internal/objectmodel/ && git commit -m "feat(objectmodel): implement [leaf name]"`
   - Update tracking table: status -> REVIEWED
   - Proceed to next child

### Phase 4: Integration Review

After ALL children reach REVIEWED:

1. **Orchestrator runs integration review in-session:**
   - Verify leaf 02's helpers compile against leaf 01's types with zero
     `package main` imports (the package must not depend on main)
   - Run `go test ./internal/objectmodel/... -v` and
     `go test ./... -count=1` (full suite must stay green)
   - Verify `go vet ./...` and `golangci-lint run ./internal/objectmodel/...` clean
   - Verify parity-test helpers from leaf 02 are importable from a `_test.go`
     in a different package (compile-check via a small throwaway test or
     doc-level assertion)

2. **If integration gaps found:**
   - Identify which child failed the contract
   - Re-dispatch that child with contract-fix instructions (do NOT commit)
   - Orchestrator re-reviews -> commits the fix -> re-runs integration checks

3. **If integration passes:**
   - **Normalize formatting:** run `make fmt` across changed files
   - **Verify no line-number corruption:** `grep -rcE '^\s+[0-9]+\|' --include='*.go' --include='*.md' .` returns zero
   - **Commit integration changes** (if any): `git add internal/objectmodel/ && git commit -m "feat(objectmodel): integrate neutral object model"`
   - Update tracking table: all children -> COMPLETE
   - Signal sibling trees (backend-interface-2026-09, metadata-zfs-2026-09,
     frontend-interface-2026-09) that this contract-owner tree is COMPLETE

## Review Checklist

The orchestrator (main model) verifies each child in-session:

- [ ] All tasks from the leaf document are implemented
- [ ] Interface contracts from this orchestrator are satisfied — pinned structs
      match Contract 1 field-for-field (names, types, order)
- [ ] All specified files created/modified at exact paths
- [ ] Tests written and passing (TDD followed, table-driven, stdlib testing only)
- [ ] Package imports: stdlib only; no `mini-s3` main-package imports
- [ ] Code follows project conventions (see Coding Conventions below)
- [ ] No scope creep (nothing beyond spec)
- [ ] No obvious bugs or security issues
- [ ] No debug artifacts: no print/stdout debugging, no TODOs, no placeholder values, no commented-out code
- [ ] No line-number corruption: no `     N|` prefixes baked into source files

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language/Framework:** Go (module `mini-s3`, go 1.25.x per go.mod), stdlib only — NO new dependencies in this tree, no testify
- **Naming:** exported = PascalCase, unexported = camelCase; error constructors prefixed `Err`
- **Imports:** stdlib single group, goimports with `-local mini-s3`; never import `package main`
- **Error handling:** errors as values (`*Error` implementing `error`), no panic in library code
- **Testing:** table-driven with stdlib `testing`; `_test.go` alongside source; no external assertion libs
- **Formatting tool:** `gofmt` / `make fmt` before reporting completion
- **File headers:** no boilerplate headers; package comment on one file only (model.go)
- **Comments:** pinned structs keep the orchestrator's comments verbatim (ETag semantics, metadata prefix rule)

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-canonical-types | COMPLETE    | 0 | |
| 02-metadata-surface | COMPLETE    | 0 | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. **Package-level:** `go test ./internal/objectmodel/... -v` — all leaf tests
   green. Expected: ETag normalization table, metadata-key normalization table,
   error taxonomy table, header round-trip table, legacy conversion table all pass.
2. **Full-suite regression:** `make test && make test-race` — the new package
   must not perturb existing `package main` tests; race detector clean (the
   package is immutable-by-value; no shared state).
3. **Isolation check:** `go list -deps ./internal/objectmodel` must show only
   stdlib packages. Any `mini-s3` main dependency is a contract violation.
4. **Cross-boundary (leaf 02 deliverable):** compile a parity-test helper from
   a foreign package (e.g. a temporary `internal/objectmodel/parity_test` usage
   in a scratch file during review, removed after) proving other trees can
   drive FS-vs-alternate-backend header comparisons without importing main.
5. **Coverage floor:** `make test-cover-enforce` still passes (new package
   should land near 100% coverage, raising the total).
6. **Downstream readiness (doc-level):** confirm sibling-tree plans can cite
   `objectmodel.Object`, `objectmodel.Error`, and the canonical metadata
   surface without amendment — contracts are frozen at this tree's COMPLETE.

## Structural Completeness Check (Before Dispatch)

After writing every document in this tree (this master + both leaves), run:

```
python3 /Users/caimlas/.hermes/skills/software-development/hierarchical-planning/scripts/check_template_compliance.py docs/plans/object-model-2026-09 --strict-leaves
```

Required in every orchestrator: `## Dispatch Protocol`, `## Interface Contracts`,
`## Review Checklist`, `## Coding Conventions`, `## Completion Tracking Table`,
`## Integration Test Plan`. Fix before dispatching any agent.

## Notes

- **This tree is the contract-owner for the gateway initiative.** If a sibling
  tree needs a change to a pinned contract, the change lands HERE first, in a
  new revision of this tree, before the consuming tree proceeds.
- The pinned `GetOptions.Range string` exists even though mini-s3 currently
  ignores Range (documented divergence in CLAUDE.md): the neutral model must
  be able to carry the request so a future backend can honor it without a
  contract revision.
- Versioning is NOT implemented in mini-s3 (501 on versionId,
  object_handlers.go:1120). `CapabilitySet.Versioning` lets backends declare
  support so the S3 frontend can decide between honoring and 501-ing.
- Existing precondition logic (object_handlers.go:306, `evaluatePreconditions`)
  is HTTP-coupled. This tree does NOT migrate it; the neutral
  `GetOptions`/`PutOptions` condition fields are the target shape a later tree
  can refactor onto. Do not touch `evaluatePreconditions` in this tree.
- `Error.HTTPStatus` carries the canonical S3 default so the S3 frontend needs
  no mapping table today; WebDAV/SFTP frontends map Code → their own status
  words later. Keep mapping logic OUT of internal/objectmodel.
- Repo fact check: `go.mod` says `go 1.25.6` (CLAUDE.md says 1.27 — trust go.mod).
  Leaf agents must not bump the go directive.
