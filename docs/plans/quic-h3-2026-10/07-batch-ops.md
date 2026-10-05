# Batch Operations (server-side multi-file copy/move/delete) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** batch execution of copy/move/delete for remote files: the
  client sends ONE request carrying a manifest of operations, the
  server executes them, one response carries per-item results. Two
  surfaces: (a) the S3-STANDARD DeleteObjects (POST /{bucket}?delete)
  for interop, (b) a JSON batch extension (POST /{bucket}?batch)
  serving copy+move+delete on BOTH the s3 and webdav frontends (the
  h3 frontend inherits by wrapping webdav).
- **Dependencies:** 05 and 06 committed (shared-package placement
  outcomes decided)
- **Estimated Context:** 70K
- **Concurrency Group:** A4 (after 06 commits)

## Goal

"Copy these 400 files" is one round trip, not 400. The manifest is a
flat list of explicit operations; the server executes each through
its NORMAL single-file path and reports per-item status. Sources may
span any number of directories within the bucket - directories are
path prefixes, and nothing in the execution path cares.

## Context

- S3 DeleteObjects is a real standard (REST API: POST /{bucket}?delete,
  XML manifest of up to 1000 Object keys, optional Quiet mode, XML
  result with per-key Error/Deleted entries). boto3's
  `delete_objects` speaks it. Interop clients are an AGENTS.md e2e
  concern - check `scripts/e2e/cases/` for the boto3 case and extend
  it.
- There is NO S3 standard for batch copy/move (AWS S3 Batch is an
  async job system - wrong shape, do not imitate it). The JSON batch
  extension is this repo's own wire shape: synchronous, one request,
  per-item results, all operations within ONE bucket.
- Universality decision 7 applies to ZFS features; batch is a
  protocol capability. The rule this leaf follows is its sibling:
  ONE implementation in a shared package, mounted by both the s3
  and webdav frontends. Versioning capture (leaf 05) applies to
  batch items automatically because each item executes the normal
  write path - pin that with a test (a batch overwrite of a
  versioned bucket captures the old version per item).
- Charter: no new state. The manifest lives in the request; the
  results in the response. Nothing is written outside the normal
  data path.
- The frozen Backend seam MUST NOT gain methods. Batch is an
  ORCHESTRATION above the seam (the same layer as multipart): each
  item is a Get/Put/Delete/Stat call sequence the frontend already
  makes.

## Interface Contracts (From Parent)

### Contract 9: batch surfaces (you produce this)

```go
// internal/batchops (NEW package, shared by s3 and webdav frontends):
//
// Request (JSON, POST /{bucket}?batch):
// {
//   "operations": [
//     {"op":"copy",  "from":"a/old.txt", "to":"b/new.txt"},
//     {"op":"move",  "from":"a/x.bin",   "to":"archive/x.bin"},
//     {"op":"delete","from":"tmp/junk"}
//   ]
// }
// - "from" and "to" are object keys (validated by the SAME key
//   validator as single ops - .metadata/.zfs rules included).
// - Limits: max 1000 operations per request; the limit is a named
//   constant with a test, and exceeding it is a 400 that changes
//   nothing.
//
// Response (200, application/json) - per-item, manifest order:
// {
//   "results": [
//     {"index":0, "status":"ok"},
//     {"index":1, "status":"error", "code":"NoSuchKey",
//      "message":"..."},
//     {"index":2, "status":"conflict", "code":"PreconditionFailed"}
//   ]
// }
// - Item execution is SEQUENTIAL in manifest order (v1; parallel
//   execution is a later optimization - the RTT win is already the
//   point).
// - NO cross-item atomicity: a mid-manifest failure does not roll
//   back earlier items. The response says exactly which items
//   succeeded. This is documented, pinned in tests, and honest.
// - Each item runs the SAME code path as its single-op equivalent,
//   including versioning capture and the If-Match precondition if
//   the manifest carries "ifMatch" on a copy/move/delete item.
//
// DeleteObjects (XML, POST /{bucket}?delete): the S3 standard shape,
// mapped onto the same batchops execution core (per-key Deleted /
// Error entries, Quiet mode honored).
```

- Auth: the mounting frontend's existing authenticator (Basic on
  webdav, SigV4 on s3, mTLS cert on h3) gates the batch request as
  one request. Authorization is per-item conceptually but the
  frontend's bucket-level model already covers all items (same
  bucket).
- Audit: ONE audit record for the batch request (op value per
  frontend's normal op vocabulary; the record counts N operations in
  existing fields if a natural place exists - do NOT widen the
  8-key record shape).
- Errors: a malformed manifest (bad JSON, unknown op, invalid key,
  over-limit) is a 400 naming the problem, NOTHING executes. Item
  failures after execution starts are per-item results, not request
  failures.

## Tasks

### Task 1: the batchops core (TDD)

**Objective:** the execution engine, protocol-free.

**Files:**
- Create: `internal/batchops/batchops.go` (manifest parsing,
  validation, sequential execution against an injected op interface)
- Create: `internal/batchops/batchops_test.go`

**Step 1: Write failing tests** (the op interface is a package var
or constructor parameter - a test double executes items; do NOT let
batchops import a frontend):
- a 3-item manifest (copy, move, delete) executes in order;
- per-item results carry manifest order and the right status;
- item 2 failing does not stop item 3 (partial success);
- malformed JSON / unknown op / invalid key / >1000 items are 400s
  with NOTHING executed (assert the double saw zero calls);
- an item carrying ifMatch enforces the precondition;
- key validation rejects `.metadata`/`.zfs` segments (the same rule
  as the frontends' validators - call the shared validator or pin
  identical behavior).

**Step 2:** `go test ./internal/batchops/ -count=1` -> FAIL.
**Step 3: Implement.** **Step 4:** PASS.

### Task 2: DeleteObjects (S3 standard)

**Objective:** interop parity.

**Files:**
- Create: `internal/frontend/s3/deleteobjects.go` (XML parse,
  delegate to batchops, XML result)
- Test: `internal/frontend/s3/deleteobjects_test.go` (XML fixtures
  inline: manifest with 2 keys + quiet mode; result with one Deleted
  + one Error entry)

**Step 1: FAILING tests** pinning the wire shapes (manifest XML and
result XML against the AWS documented shapes). **Step 2: Implement**
mapping onto the Task 1 core. **Step 3:** PASS; the dispatch hook
(POST ?delete) slots beside the existing ?events/?versions hooks in
`internal/frontend/s3/dispatch.go:316-330`.

### Task 3: the JSON batch endpoint on both frontends

**Objective:** one handler, mounted twice.

**Files:**
- Create: `internal/frontend/s3/batch_endpoint.go` + webdav twin
  (thin: JSON decode, delegate to batchops, JSON encode)
- Modify: `internal/frontend/s3/dispatch.go` and
  `internal/frontend/webdav/dispatch.go` (the ?batch POST hook)
- Tests: both packages

**Step 1: FAILING tests:** POST ?batch via the s3 handler and via
the webdav handler execute identical manifests with identical
results (parity, mirroring leaf 06's pattern); unauthorized request
is the frontend's normal auth rejection; a GET ?batch is 405.
**Step 2: Implement.** **Step 3:** PASS. The versioning-capture
per-item test from Context lives here (a batch overwrite on a
versioned test bucket; List shows per-item captures).

### Task 4: gates

- `go test ./... -count=1` green; `make parity-test` green;
- `gofmt -l` clean on changed files; `make lint NEW_FROM_REV=HEAD`
  -> 0 findings;
- floors: new package = new coverage; if the root or frontend floors
  move, re-measure (AGENTS.md).

## Self-Verification Checklist

- [ ] Manifest is flat explicit ops; no recursive/prefix expansion
      server-side (the client expands from its index)
- [ ] Per-item results in manifest order; partial success honest
- [ ] Over-limit/malformed -> 400, zero execution (asserted)
- [ ] DeleteObjects wire shapes match the AWS documented forms
- [ ] Same manifest, identical results via s3 and webdav handlers
- [ ] Versioning capture fires per item (pinned)
- [ ] Backend seam untouched; no new methods
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings;
      `make parity-test` green
- [ ] DO-NOT-TOUCH respected: `internal/metadata/`,
      `internal/backend/`, leaf 01's `get.go`, leaf 05's
      put/delete/copymove, leaf 06's `zfssurface.go` + dispatch GET
      branch, `scripts/zfs-validate/run-zfs-validation.sh`,
      `go.mod`/`go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] The execution core is protocol-free (batchops imports no
      frontend)
- [ ] A batch move item is move semantics (no source copy left),
      not copy+leave
- [ ] The 400-before-any-execution rule is TESTED (double saw zero
      calls)
- [ ] Batch over h3 needs no code (wrapping) - stated or tested
- [ ] The RTT claim in docs (leaf 03 updates them) is honest: 1
      request for N ops, sequential server-side execution

Output: APPROVED or specific gaps with file:line.

## Notes

- **Why sequential v1:** the point is eliminating (N-1) round trips,
  not parallel IO. Server-side parallelism inside one batch invites
  ordering surprises (move a/x then copy a/ - order matters). If a
  workload later wants parallelism, the manifest gains an explicit
  parallel-safe flag - a NEW decision, not a silent default.
- **Why no cross-item atomicity:** the backing filesystem has no
  multi-file transaction; faking one needs a journal (server-owned
  state - charter violation). Per-item results ARE the honest
  contract, and the client's journal reconciles.
- **The two-directories question, answered:** sources spanning any
  set of directories within the bucket are just rows in the flat
  manifest; execution never inspects directory grouping. Cross-
  BUCKET operations are the v1 boundary (one request = one bucket =
  one auth + one dataset scope); cross-bucket batch is a future
  decision.
