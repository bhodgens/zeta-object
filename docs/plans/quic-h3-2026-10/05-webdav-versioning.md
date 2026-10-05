# WebDAV Versioning Capture (universal per bucket) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** version capture (and delete markers where the mode has
  them) on the WebDAV PUT/MOVE/DELETE paths, so per-bucket
  `zfs_versioning` capture is UNIVERSAL - protocol-independent on the
  same bucket. S3 and WebDAV writes to one bucket produce the same
  version history.
- **Dependencies:** none (disjoint files from leaves 01/02 EXCEPT the
  possible package-move note below - coordinate through the
  orchestrator if both 01 and 05 propose moving shared code; 05 owns
  `versionstore.go` extraction if needed)
- **Estimated Context:** 60K
- **Concurrency Group:** A2 (dispatch after 01 and 02 commit; before
  03)

## Goal

With `zfs_versioning = sidecar` (or reflink) on a bucket, a WebDAV PUT
that overwrites an object captures the OLD version exactly as the S3
PUT does; a WebDAV DELETE records what its S3 counterpart records.
A bucket is versioned or it is not - the protocol that writes the
bytes must not change that.

## Context

- The capture invariant is PINNED in the repo skill and MUST hold on
  the webdav path too: the OLD version's DATA is captured BEFORE the
  plain overwrite, but the sidecar RECORD lands AFTER the backend Put
  (the backend Put rewrites the sidecar wholesale, so a pre-write
  record is destroyed by the write itself).
- The S3 machinery lives in `internal/frontend/s3/`:
  `versionstore.go` (the `versionStore` interface at line 47,
  `versionStoreFor(bucketPath, zdb, mode)` resolver at line 88, the
  `unimplementedVersionStore` safe-floor at 120, sidecar store below),
  `reflinkversions.go` (`captureReflinkObjectVersion` at 158,
  `recordCapturedReflinkObjectVersion` at 216), and the write-path
  branches in `object_handlers.go` (the enabled-state check at line
  80) and `versioning_handlers.go` (file header docs the shape).
  `versionStoreForBucket(bucketPath)` (versioning_handlers.go:51) is
  the per-request resolver.
- The webdav write paths: `internal/frontend/webdav/put.go` (PUT with
  the If-Match precondition at line 47), `copymove.go` (MOVE with the
  destination If-Match at 185), `delete.go`. The webdav handlers
  currently call the backend directly with NO versioning branch.
- Package placement: the versioning code is in package s3; webdav
  importing s3 is legal if no cycle (leaf 01 makes the same kind of
  decision for rangespan - CHECK ITS OUTCOME FIRST in the tracking
  table; if 01 already created `internal/rangespan`, prefer the same
  pattern: extract what webdav needs into a shared leaf package and
  re-point s3). Do NOT duplicate capture logic into webdav.
- Universal means: the versioning MODE is bucket-level config
  (`zfs_versioning`); nothing protocol-gated exists in the config and
  none may be added. The e2e parity test below is the teeth.

## Interface Contracts (From Parent)

### Contract 7: protocol-independent version capture (you produce this)

```go
// Behavior contract, not new exported API:
// For a bucket with versioning enabled (sidecar or reflink mode):
//   webdav PUT (overwrite of existing object):
//     1. capture old data BEFORE overwrite (reflink: FICLONE to
//        .versions-r/; sidecar: read old bytes into the version file)
//     2. backend Put proceeds (plain overwrite)
//     3. record lands AFTER Put succeeds (sidecar rewrite)
//   webdav DELETE: records the same entry type the S3 DELETE records
//     for the mode (sidecar: delete marker; snapshots mode: no-op -
//     history is the snapshot timeline)
//   webdav MOVE (rename): no version entry on a pure rename (the s3
//     CopyObject+DeleteObject path records what IT records; a webdav
//     MOVE is a metadata op on the fs and must NOT manufacture
//     versions - pin the no-op with a test)
// Version ids, sidecar layout, and the ?versions listing output are
// BYTE-IDENTICAL whether the write arrived via S3 or WebDAV.
```

## Tasks

### Task 1: parity test first (RED)

**Objective:** the test that fails today and pins the end state.

**Files:**
- Create: `internal/frontend/webdav/versioning_test.go` (or extend
  the webdav conformance test file - follow the package's test
  layout)

**Step 1: Write the failing parity test.** Build the test bucket with
versioning enabled using the SAME fixture helper the s3 versioning
tests use (find it with search_files; if it is package-private,
mirror its setup inline - the store constructors are exported enough
or made so). Drive BOTH wire paths in one test:
- PUT file via the s3 handler test double, then PUT the same file via
  the webdav handler; assert `?versions`-equivalent store state
  (List) shows BOTH old versions captured;
- DELETE via webdav; assert the recorded entry matches the s3 DELETE
  behavior for the mode;
- MOVE (rename) the file; assert NO new version entry appeared.
Run it -> FAIL (webdav captures nothing today).

**Step 2:** `go test ./internal/frontend/webdav/ -run Versioning
-count=1` -> FAIL.

### Task 2: the capture hook on PUT

**Objective:** webdav PUT captures old data before overwrite.

**Files:**
- Modify: `internal/frontend/webdav/put.go` (the versioning branch,
  mirroring the s3 object handler branch order)
- Possibly create: the shared package per the Context placement
  decision (extraction + s3 re-point in the SAME task, s3 tests
  unmodified and passing from the new location; re-measure coverage
  floors if code moved packages - AGENTS.md rule)

**Step 1: Implement.** Resolve the store per request the way s3 does
(`versionStoreFor` with the same config inputs - if those are
package-main seams, check how the webdav frontend receives config
today and thread the mode through the SAME install path; do not
build a second config view). Capture before overwrite; record after
the backend Put; on capture failure: follow whatever the s3 path
does (read it and mirror - do not invent a different failure mode;
if s3 fails open, webdav fails open identically, and a test pins
it).

**Step 2:** Task 1's PUT parity assertions PASS.

### Task 3: DELETE and MOVE branches

**Objective:** delete markers (sidecar mode) and the MOVE no-op.

**Files:**
- Modify: `internal/frontend/webdav/delete.go` (sidecar mode:
  marker/entry per s3 DELETE parity)
- Modify: `internal/frontend/webdav/copymove.go` (MOVE: explicit
  no-op - a comment + the pinning test from Task 1)

**Step 1: Implement** DELETE parity. **Step 2:** Task 1's DELETE and
MOVE assertions PASS.

### Task 4: snapshots and both modes

**Objective:** the other modes are honest.

- snapshots mode: capture is a no-op (history = the snapshot
  timeline); pin with a test that webdav writes in snapshots mode
  add no sidecar state.
- both mode: sidecar entries appear exactly as in sidecar mode (the
  merged listing tests in s3 are the reference - mirror one
  assertion).

### Task 5: gates

- `go test ./... -count=1` green (s3 versioning tests UNMODIFIED);
- `gofmt -l` clean on changed files;
- `make lint NEW_FROM_REV=HEAD` -> 0 findings;
- if code moved packages: floors re-measured, comments added.

## Self-Verification Checklist

- [ ] Task 1 parity test exists and was RED before the fix (TDD)
- [ ] Capture-before-overwrite, record-after-Put invariant holds on
      the webdav path (code order mirrors s3)
- [ ] MOVE manufactures no versions (pinned)
- [ ] snapshots mode adds no sidecar state on webdav writes (pinned)
- [ ] `?versions` listing is byte-identical for s3-written vs
      webdav-written history (parity test asserts the listing, not
      just the sidecar files)
- [ ] No capture logic duplicated (webdav delegates to the shared
      store code)
- [ ] Backend + metadata seams untouched; Frontend interface untouched
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH respected: `scripts/zfs-validate/run-zfs-validation.sh`,
      `go.mod`/`go.sum`, `main_server.go` (leaf 02 owns), the rangespan
      files if leaf 01 moved them (check the tracking table)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] The parity test drives BOTH wire paths against ONE bucket and
      asserts the merged history
- [ ] Config surface unchanged (mode is bucket-level; nothing
      protocol-gated)
- [ ] Failure-mode parity with s3 (fail-open or fail-closed - same on
      both paths, pinned)
- [ ] If extraction happened: s3 tests unmodified from the new
      location; floors updated

Output: APPROVED or specific gaps with file:line.

## Notes

- This leaf is what makes conflict handling in the zeta-cache client
  dependable: per-write history server-side, reachable through
  `?versions` regardless of which protocol wrote the bytes.
- The e2e case (leaf 03's case 37 or a case 38 - orchestrator's call)
  gains a two-protocol version check: PUT via boto3 (s3), PUT via the
  webdav probe, GET the version listing via either, assert both old
  versions present. State the chosen case number in the report.
- Live validation (leaf 04) already runs the metadata round-trip over
  h3; if your parity surface extends it, coordinate through the
  orchestrator - do not edit the harness in this leaf.
