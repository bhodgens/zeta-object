# Filesystem Backend (fsbackend) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** master.md (this directory)
- **Scope:** Extract the existing local-filesystem object storage into
  `internal/backend/fsbackend` preserving the on-disk layout byte-for-byte, then switch
  the S3 handlers to call through the Backend with behavior preserved (harness green).
- **Dependencies:** 01-backend-interface.md (Backend interface, error taxonomy,
  conformance suite) merged.
- **Estimated Context:** 70K (largest leaf: reads several handler files, writes extraction,
  migrates call sites, runs full gates)
- **Concurrency Group:** B (after A)

## Goal

Two deliverables, strictly in order:

1. **fsbackend package:** a `backend.Backend` implementation that reproduces today's
   local-filesystem behavior exactly — same paths, same `.metadata/<object>.meta` sidecar
   JSON, same locking, same atomic-write guarantees, same error mapping. It passes the
   leaf-01 conformance suite against a temp-root instance and passes an additional
   on-disk-compat test suite. Existing buckets keep working with zero migration.
2. **Handler flip:** `object_handlers.go` (and `bucket_handlers.go` listing paths) stop
   touching the fs directly for the seven data-plane operations and call through a
   package-level `backendFor(bucket)` indirection. The integration harness
   (`make e2e`, from commits 7923cd5/a596b9e/dfce3bc) must be green after the flip.

Multipart (`multipart_handlers.go`) is explicitly OUT of scope for the flip — it stays
direct-fs in v1 per parent Contract 4; this leaf documents the part-staging decision.

## Context

mini-s3 is a Go 1.27 single-binary S3 server (module `mini-s3`, package `main` at repo
root). Object data and metadata currently flow:

- **Write path** (`object_handlers.go` putObjectHandler, ~line 85–150): MD5 ETag, per-object
  + parent-dir locks, `os.MkdirAll`, `writeFileAtomic(objectDataPath, body, 0644)`, then
  `writeFileAtomicJSON(objectMetadataPath, meta, 0644)`. Two documented RESIDUAL WINDOW
  comments: on metadata-write failure the data file is intentionally LEFT IN PLACE
  (removing it was the historical PUT-overwrite data-loss bug; see CLAUDE.md /
  `docs/gap-closure.md`, fix in commit 6fceb5c). This leaf VERIFIES that current behavior
  with a test before extracting, and preserves it.
- **Read path** (getObjectHandler, ~line 382+): read sidecar JSON → unmarshal
  `ObjectMetadata` → `resolveObjectDataPath(bucketPath, objectName, &meta)` (line 173;
  honors meta.StoragePath with canonical-path fallback on corrupt StoragePath) → per-data-dir
  + per-meta-dir locks (leaf-4.8 stress fix — dropping these reintroduces spurious 500s
  under concurrent delete) → stat → stream file.
- **Sidecar struct** `types.go:51`: `ObjectMetadata{ContentType, ContentLength, ETag,
  CustomMetadata, LastModified, StoragePath}` with JSON keys `contentType`, `contentLength`,
  `eTag`, `customMetadata`, `lastModified`, `storagePath`. THIS IS THE FROZEN ON-DISK
  FORMAT — byte-for-byte.
- **Helpers** `storage.go` (72 lines): `writeFileAtomic` (temp file same dir, fsync,
  rename), `writeFileAtomicJSON`, `lockObject(path) func()` (sync.Map of per-path mutexes).
- **Key validation** `validateObjectKey` (object_handlers.go) rejects `..`/`.metadata`
  segments — the security boundary. fsbackend re-validates defensively but handlers keep
  first-line validation.
- **Buckets:** auto-discovered under `dataDir` or custom paths from config `buckets` map;
  `getBucketPath`/`bucketExists` in `bucket_handlers.go`; both follow symlinks.

Key files to understand before implementing:
- `object_handlers.go` — put/get/head/delete/copy object handlers; `resolveObjectDataPath`;
  `validateObjectKey`; `cleanupEmptyDirs`
- `storage.go` — atomic write + lock helpers (the techniques fsbackend reproduces)
- `types.go:51-58` — ObjectMetadata (frozen sidecar format)
- `bucket_handlers.go` — getBucketPath, bucketExists, validateBucketName
- `multipart_handlers.go` — read-only for this leaf (staging decision doc)
- `main.go` / `config.go` — where server wiring happens (flip touches call sites, not here)
- `internal/backend/` — leaf 01's interface, errors, conformance suite
- `internal/objectmodel/` — neutral types + ObjectMetadata↔objectmodel conversion target

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/backend/fsbackend/fsbackend.go
package fsbackend

import (
	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// FS is a Backend rooted at a filesystem directory containing bucket
// subdirectories (dataDir or a custom bucket path). Safe for concurrent use.
type FS struct{ /* unexported fields: root, custom bucket map if needed */ }

// New returns an FS backend rooted at root. Does not create root.
func New(root string) (*FS, error)

// Compile-time interface assertion (must appear in fsbackend.go):
var _ backend.Backend = (*FS)(nil)
```

```go
// File: backend_lookup.go (package main, repo root)
// backendFor resolves the Backend serving a bucket. Leaf 02 installs a default
// that serves every bucket from a single FS rooted at dataDir (custom-bucket
// paths resolved per-bucket, preserving today's layout); leaf 03 replaces the
// installer with registry/config-driven construction. Handlers must call
// backendFor and surface errors as S3 errors; they must not touch os.* for
// the seven data-plane ops after this leaf.
var backendFor = defaultBackendFor // func(bucket string) (backend.Backend, error)
```

```go
// Registration hook consumed by leaf 03 (MUST exist at end of this leaf):
// File: internal/backend/fsbackend/fsbackend.go
func init() { backend.Register("fs", func(cfg backend.BackendConfig) (backend.Backend, error) {
	return New(cfg.Root)
}) }
```

### What This Leaf Consumes

```go
// From 01-backend-interface.md (internal/backend):
type Backend interface { Get/Put/Delete/Stat/List/Buckets/Capabilities /* frozen */ }
func ToObjectModelError(err error) error
var ErrNotSupported, ErrUnknownBackend error
// Conformance suite — run against FS instances in this leaf's tests:
// (per leaf 01's Notes: either the exported test runner or
// internal/backend/conformance package — use whatever shape leaf 01 landed)
```

```go
// From object-model-2026-09 tree (internal/objectmodel):
// Object, GetOptions, PutOptions, ListParams, ListPage, BucketInfo, CapabilitySet, Error
// — plus whatever conversion helpers that tree shipped. Read the package first;
// code against what exists.
```

## Tasks

### Task 0: Pre-extraction behavior pin (verification, no new behavior)

**Objective:** Prove — with tests that exist BEFORE extraction — the exact current
behaviors that must survive: the PUT-overwrite residual window, corrupt-StoragePath
fallback, parent-dir lock behavior, sidecar byte format.

**Files:**
- Create: `internal/backend/fsbackend/compat_test.go` (written against the FS package
  skeleton, but its EXPECTATIONS are transcribed from current handler behavior verified by
  reading `object_handlers.go` first)
- Test reference: `handlers_current_behavior_test.go` (package main) — optional but
  recommended: pin the RESIDUAL WINDOW behavior at the handler level first; keep it green
  through the flip.

**Step 1: Write failing test** (against a minimal FS skeleton — Task 1's struct, no methods)

```go
package fsbackend

import (
	"path/filepath"
	"testing"
)

// The on-disk contract. These literals are the frozen format — do not "fix" them.
func TestSidecarFormatFrozen(t *testing.T) {
	dir := t.TempDir()
	// (after Task 2 lands Put) put "hello world" into bucket "b", key "k",
	// content-type "text/plain", one x-amz-meta-* custom header, then read
	// <dir>/b/.metadata/k.meta raw and assert:
	//   - unmarshals into main's ObjectMetadata field set
	//   - JSON keys are exactly: contentType, contentLength, eTag, customMetadata,
	//     lastModified, storagePath
	//   - eTag is lowercase-hex MD5 of the body
	//   - storagePath is the absolute data path (matching current handler behavior)
	_ = filepath.Join(dir, "b", ".metadata", "k.meta") // placeholder until Task 2
}
```

Also transcribe from `object_handlers.go` into `compat_test.go`: overwrite-residual-window
expectation (metadata-write failure ⇒ data file still present, old sidecar intact),
corrupt-StoragePath fallback (hand-edit sidecar's storagePath to junk ⇒ Get still serves
canonical path). Mark each with `// PIN: verified pre-extraction <date>`.

**Step 2: Run test to verify failure**

Run: `go test ./internal/backend/fsbackend/ -v`
Expected: FAIL — package is a skeleton.

**Step 3/4:** This task's tests stay RED until Task 2 implements Put/Get; that is expected.
Run the optional handler-level pin test in package main NOW (it should pass against
current handlers — if any pin FAILS against current code, STOP and report: the parent's
facts are wrong and contracts need reconciliation before extraction).

### Task 1: fsbackend skeleton + New + Capabilities + Buckets

**Objective:** Package shell, constructor, compile-time assertion, the two "easy" methods.

**Files:**
- Create: `internal/backend/fsbackend/fsbackend.go`
- Test: `internal/backend/fsbackend/fsbackend_test.go`

**Step 1: Write failing test**

```go
package fsbackend

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"mini-s3/internal/backend"
)

func TestNewRootedAndInterface(t *testing.T) {
	fs, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var _ backend.Backend = fs // compile-time assertion also in fsbackend.go
	_ = fs.Capabilities()      // must not panic; flags are backend-owned
}

func TestBucketsDiscoversDirs(t *testing.T) {
	root := t.TempDir()
	// mkdir root/alpha, root/beta (auto-discovered), plus a symlinked custom
	// dir per current bucket_handlers behavior; assert Buckets returns both,
	// sorted, with names matching today's discovery semantics.
	if _, err := New(root); err != nil {
		t.Fatal(err)
	}
	_ = errors.New // placeholder import guard until assertions written
	_ = context.Background
	_ = filepath.Join
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/backend/fsbackend/ -run 'TestNew|TestBuckets' -v`
Expected: FAIL — `undefined: New`.

**Step 3: Write minimal implementation**

`fsbackend.go`: package doc ("local-filesystem Backend; on-disk layout is FROZEN:
<root>/<bucket>/<object> + <root>/<bucket>/.metadata/<object>.meta; existing buckets work
unchanged"), `FS` struct, `New`, `init()` registering `"fs"` with
`backend.Register`, `Capabilities()` returning the fs capability set (per objectmodel's
CapabilitySet flags — set exactly the flags true of today's server: e.g. no range support,
no multipart-at-backend — read objectmodel's flag definitions first), `Buckets(ctx)` with
discovery logic extracted from `bucket_handlers.go` (symlink-following preserved).

**Step 4: Run test to verify pass**

Run: `go test ./internal/backend/fsbackend/ -v` — these subtests PASS; Task 0 pins still RED.

### Task 2: Put/Get/Stat/Delete — the extraction core

**Objective:** Port the write/read/delete paths from `object_handlers.go` into FS methods,
byte-identical on disk.

**Files:**
- Modify: `internal/backend/fsbackend/fsbackend.go` (Put/Get/Stat/Delete)
- Create: `internal/backend/fsbackend/paths.go` (path derivation: the
  `resolveObjectDataPath` logic incl. StoragePath-fallback, `.metadata` layout — extracted
  logic, not an import of package main)
- Create: `internal/backend/fsbackend/atomic.go` (writeFileAtomic / writeFileAtomicJSON /
  lockObject equivalents — copy the techniques from `storage.go`; do NOT import package
  main. Keep the leaf-4.8 parent-dir locking INSIDE these methods.)
- Test: `internal/backend/fsbackend/compat_test.go` (Task 0 pins go GREEN here)

**Step 1: Write failing test** — Task 0's `compat_test.go` pins + conformance suite:

```go
// compat_test.go additions
func TestConformanceFS(t *testing.T) {
	// Use leaf 01's runner in whatever shape it landed, e.g.:
	// backendtest.Run(t, "fs", func(t *testing.T) backend.Backend {
	// 	fs, err := New(t.TempDir()); if err != nil { t.Fatal(err) }; return fs
	// })
}

func TestPutGetRoundTripSidecarBytes(t *testing.T) {
	// Put body "hello world", ct "text/plain", custom meta {"x-amz-meta-owner":"caimlas"}
	// → read sidecar raw; assert field-for-field vs ObjectMetadata expectations,
	//   including lowercase-hex MD5 eTag and storagePath canonical path.
	// Put again with different body (overwrite) → data + sidecar both updated,
	//   old content gone, new eTag.
}

func TestOverwriteResidualWindowPinned(t *testing.T) {
	// PIN from object_handlers.go RESIDUAL WINDOW comments: when the metadata
	// write fails (simulate via unwritable .metadata dir), the data file must
	// STILL EXIST and the previous sidecar must be untouched. This is the
	// historical data-loss bug fix — do not "improve".
}

func TestCorruptStoragePathFallbackPinned(t *testing.T) {
	// Hand-write a sidecar whose storagePath is "/nonexistent/junk",
	// place real data at the canonical path → Get/Stat succeed via fallback
	// (resolveObjectDataPath behavior, object_handlers.go:173).
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/backend/fsbackend/ -v`
Expected: FAIL — Put/Get/Stat/Delete undefined on FS.

**Step 3: Write minimal implementation**

Port semantics, not code shape:
- `Put`: validate key defensively (mirror validateObjectKey rules — reject `..`/`.metadata`
  segments; return objectmodel-invalid-key error), MD5 ETag, per-object + parent-dir locks
  (leaf-4.8 pattern), MkdirAll data + metadata parents, writeFileAtomic data 0644, build
  `ObjectMetadata` with identical JSON, writeFileAtomicJSON. On metadata failure: data file
  STAYS (residual window), error returned. `size int64`: today's handlers buffer the body
  and use actual length; keep buffering semantics in v1 (Content-Length is enforced
  upstream by sigv4/aws-chunked decoding). PutOptions carries content-type + custom
  metadata from the handler.
- `Get`: read sidecar → ObjectMetadata → resolveObjectDataPath w/ fallback → dir locks →
  `os.Open` → return ReadCloser + converted objectmodel.Object. Missing sidecar or data
  file → objectmodel NoSuchKey.
- `Stat`: same as Get minus the open.
- `Delete`: today's deleteObjectCore semantics — remove data + sidecar, prune empty dirs
  (`cleanupEmptyDirs`), missing key per the leaf-01 conformance pin.
- Conversion `objectMetadataToObject(...)` + back: pure funcs in
  `internal/backend/fsbackend/convert.go`, table-tested (sidecar JSON keys frozen).

**Step 4: Run test to verify pass**

Run: `go test -race ./internal/backend/fsbackend/ -v`
Expected: PASS — Task 0 pins + conformance all green.

### Task 3: List

**Objective:** Port ListObjectsV2's storage-facing half (key enumeration, prefix/delimiter/
marker/maxKeys, common prefixes) onto `List(ListParams) ListPage`.

**Files:**
- Modify: `internal/backend/fsbackend/fsbackend.go`
- Test: `internal/backend/fsbackend/list_test.go`

**Step 1: Write failing test** — table-driven: keys flat + nested (`a/b/c` — today's layout
treats `/` in keys as real directories), prefix, delimiter=`/` grouping into common
prefixes, marker exclusion, maxKeys truncation + next marker, lexicographic order,
`encoding-type=url` stays HANDLER-side (note it in a comment).

**Step 2: Run test to verify failure** — `List` undefined.

**Step 3: Write minimal implementation** — extract the walk logic from the ListObjectsV2
section of `object_handlers.go` (verify actual line ranges when you get there), preserving
order and delimiter semantics exactly. Ctx-checked between directory entries.

**Step 4: Run test to verify pass** — `go test -race ./internal/backend/fsbackend/ -v` PASS.

### Task 4: Handler flip (behavior-preserving)

**Objective:** Handlers call `backendFor(bucket)` for the seven data-plane ops; direct
data-plane `os.*` calls leave `object_handlers.go` (multipart keeps its direct-fs staging
per Contract 4).

**Files:**
- Create: `backend_lookup.go` (package main) — `backendFor` default: one `fsbackend.New`
  rooted at dataDir + per-bucket FS for custom paths, memoized; nil-safe.
- Modify: `object_handlers.go` — put/get/head/delete/copy + list paths call the Backend;
  handlers keep: SigV4/auth, key+bucket validation, XML rendering, `encoding-type=url`,
  Range/If-* header handling (ranges stay unimplemented — no behavior change), error →
  writeS3Error mapping via objectmodel error codes.
- Modify: `bucket_handlers.go` — listing/create/delete bucket paths route through
  `Buckets`/FS where applicable; `validateBucketName` stays handler-side.
- Test: `handlers_backend_flip_test.go` (package main)

**Step 1: Write failing test**

```go
package main

import (
	"net/http/httptest"
	"testing"
)

func TestHandlersRouteThroughBackend(t *testing.T) {
	// Install a recording Backend wrapper via the backendFor indirection;
	// drive PUT/GET/DELETE through the root handler with httptest;
	// assert the wrapper observed the calls (proving handlers no longer
	// short-circuit to direct fs) AND responses are byte-identical to
	// pre-flip fixtures (status, ETag, Content-Type, body).
	_ = httptest.NewRequest
}
```

Plus: keep Task 0's optional handler-level pin test green through the flip.

**Step 2: Run test to verify failure**

Run: `go test . -run TestHandlersRouteThroughBackend -v`
Expected: FAIL — `backendFor` undefined.

**Step 3: Write minimal implementation**

The mechanical migration. Order: GET → HEAD → PUT → DELETE → COPY → LIST (objects) →
LIST (buckets), one commit-sized unit each, full suite between each. Every S3 error code
mapping table (fs err → code → status) lives in ONE place (`errors_to_s3.go` or inline in
xml.go's neighborhood — your call, one place only).

**Step 4: Run test to verify pass + full gates**

Run: `go build ./... && go vet ./... && go test -count=1 ./... && go test -race ./... && make e2e`
Expected: all green. `make e2e` is the authoritative regression gate.

### Task 5: Multipart staging decision (documentation, mandatory)

**Objective:** Document — with rationale — how the multipart flow relates to the seam.

**Files:**
- Modify: `internal/backend/fsbackend/fsbackend.go` (package doc section)
- Modify: `docs/plans/backend-interface-2026-09/master.md` — NO. Record the decision in
  fsbackend's package doc AND report it to the orchestrator for Contract 4 reconciliation.

**Decision to make and document (pick ONE, justify):**
1. **Put accepts streamed unknown-size writes** (`size <= 0` ⇒ buffer-to-temp then
   finalize): keeps the door open for main-side multipart to later push parts through Put.
2. **Parts stage fs-side and finalize through Put** (today's behavior, formalized):
   multipart never calls Put for parts; only `CompleteMultipart` produces a final
   assembled Put (or keeps direct-fs assembly in v1).

v1 default: option 2 (zero behavior change). Document in the package doc: "Multipart
staging is S3-orchestration above the seam (parent Contract 4); fsbackend.Put receives
only CompleteMultipart's assembled object in v1; option-1 extension is tracked for the
backend-extension tree." Rationale: option 1 changes part-write atomicity semantics
today for a future need.

**Verify:** doc comment exists; no code changes.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented; Task 0 pins GREEN; conformance suite green for fsbackend
- [ ] `go test -race ./...` and `make e2e` green after the flip
- [ ] On-disk layout byte-identical: sidecar JSON keys/values verified by compat_test
- [ ] Residual-window + corrupt-StoragePath pins preserved (tests prove it)
- [ ] Parent-dir lock pattern preserved inside fsbackend (leaf-4.8 race fix)
- [ ] `backendFor` indirection exists; handlers make no direct data-plane `os.*` calls
      (`grep -n 'os\.' object_handlers.go` — remaining hits are validation/lock-free paths
      only; report the exact residual list)
- [ ] multipart_handlers.go untouched by the flip
- [ ] init() registers `"fs"` in the backend registry
- [ ] Multipart staging decision documented with rationale
- [ ] gofmt clean; `go vet ./...` clean; coverage floor not diluted
      (`make test-cover-enforce`)
- [ ] No deviations from spec (or documented below)

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task implemented, including Task 0 (pins PRE-EXIST extraction) and Task 5 (doc)
- [ ] Conformance suite runs against fsbackend with zero duplicated assertions
- [ ] On-disk format: diff a pre-seam-written sidecar vs post-seam — byte-identical
- [ ] Locking: per-object AND parent-dir locks present in fsbackend write/read paths
- [ ] Error mapping: no `*os.PathError` reaches handlers; codes match objectmodel identity
- [ ] Handler flip is behavior-preserving: harness green; response fixtures identical
- [ ] validateObjectKey semantics not weakened (fsbackend re-validates defensively)
- [ ] No bugs, no security issues (path traversal via key, lock gaps, symlink handling)
- [ ] No scope creep (no registry, no config — leaf 03's job)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- **Read `object_handlers.go` in full before writing anything** — it is the behavioral
  spec. Line numbers in this leaf (85–150, 173, 382+) are from the 2026-09 tree and may
  have drifted; locate by symbol name, not line.
- The historical data-loss bug (PUT-overwrite deleting the old good object on metadata
  failure) is FIXED in the current tree (commit 6fceb5c + leaf-2.4 residual-window
  comments). VERIFY with Task 0's test before extraction. If the pin fails, STOP — do not
  extract a regression.
- `cleanupEmptyDirs` pruning races were fixed by holding the parent-dir lock (leaf-4.8).
  The same pattern must appear in fsbackend Put AND Delete. `-race` + the conformance
  concurrency subtest are your guard.
- Custom buckets (config `buckets` map) point at arbitrary dirs incl. symlinks; the
  default `backendFor` must preserve per-bucket root resolution exactly
  (`getBucketPath` semantics).
- Do NOT migrate multipart, do NOT add config/registry (leaf 03), do NOT implement Range
  (still a documented divergence), do NOT change error statuses the harness asserts.
- `storage.go` stays in package main for handler-era callers (multipart); fsbackend gets
  its own copies of the atomic/lock techniques. A later cleanup tree may deduplicate —
  not this one.
