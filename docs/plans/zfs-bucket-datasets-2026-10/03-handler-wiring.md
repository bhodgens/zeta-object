# ZFS Bucket Datasets: Handler Wiring - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you
> touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** createBucketHandler/deleteBucketHandler consult the leaf-02
  hooks; startup wiring installs them from the leaf-01 config; the 409
  BucketHasSnapshots error mapping.
- **Dependencies:** 01 (config keys, `zfsBucketsParentDataset` var,
  `validateZfsBucketDatasets`), 02 (`InstallZfsDatasetProvisioner`,
  hook vars, `ErrDatasetHasSnapshots`) - both MUST be committed before
  dispatch.
- **Estimated Context:** 60K
- **Concurrency Group:** B

## Goal

When the hooks are installed (feature on), CreateBucket makes the
dataset and DeleteBucket destroys it, per the master's Contract 3
behavior table. When hooks are nil (feature off - the default), the
existing mkdir/RemoveAll code runs byte-identically. Custom-bucket
create/delete behavior is UNCHANGED (user decision 3). Startup wiring:
main/s3_wiring passes the resolved parent dataset + binary into
`s3.InstallZfsDatasetProvisioner` when `cfg.ZfsBucketDatasets` is true.

## Context

- `internal/frontend/s3/bucket_handlers.go`:
  `createBucketHandler` (line ~149): validateBucketName -> custom-bucket
  guard -> getBucketPath -> `lockObject(bucketPath)` -> stat existence
  checks (existing dir = 409 BucketAlreadyOwnedByYou, file = 409
  BucketAlreadyExists) -> `os.MkdirAll(bucketPath)` ->
  `os.Mkdir(bucketPath/.metadata)` (rollback RemoveAll on failure) ->
  200.
  `deleteBucketHandler` (line ~235): validBucket -> custom guard 403 ->
  getBucketPath -> lock -> stat (404 NoSuchBucket) -> empty check
  excluding .metadata -> RemoveAll.
- Wiring precedent: `s3_wiring.go` (package main) installs frontend
  seams via `installS3Seams`; leaf 01 exposes
  `zfsBucketsParentDataset` (package main). Find the install call site
  with `grep -n "installS3Seams\|InstallZfsVersioningMode" s3_wiring.go
  internal/frontend/s3/versioning_seam.go`.
- Error mapping precedent: `errors_to_s3.go` maps sentinel errors to
  S3 codes; but the 409 here is written directly in the handler with
  `writeS3Error` (the count rides in the wrapped error's message -
  extract via a small helper or `fmt.Sprintf("%v", err)` after
  `errors.Is`).

## Interface Contracts (From Parent)

### What This Leaf Exposes

Modified handlers with this exact wire behavior (Contract 3 table):

| Situation | Result |
|---|---|
| hooks nil (feature off) | unchanged legacy path (existing tests must pass unmodified) |
| create: hook error | 500 InternalError, stderr in log, NO mkdir fallback, NO partial dir |
| create: dataset OK, `.metadata` mkdir fails | rollback `zfsBucketDestroy(ctx, dataset)` then 500 |
| create: existing-dataset/dir guards | UNCHANGED - the stat-based guards run BEFORE the hook; a pre-existing dataset directory hits the existing 409 BucketAlreadyOwnedByYou path |
| delete: path IS its own dataset, no snapshots | destroy hook, then existing success path (204 + RemoveAll is replaced by the destroy; do NOT RemoveAll a live mountpoint) |
| delete: dataset with snapshots | 409, code `BucketHasSnapshots`, message includes the snapshot count and the `zfs destroy <ds>@<snap>` hint |
| delete: exists hook false (pre-feature plain dir) | legacy RemoveAll path |
| delete: exists hook errors | 500 (fail loud; do not guess RemoveAll) |
| custom bucket create/delete | UNCHANGED (existing guards run first - do not touch them) |

- The `lockObject(bucketPath)` serialization stays and still wraps the
  hook calls.
- Delete ORDER is fixed: emptiness check (incl. `.uploads`) ->
  exists hook -> destroy hook -> 204. NEVER RemoveAll after a
  successful destroy (live mountpoint), and never destroy before the
  emptiness check - `zfs destroy` succeeds on a dataset holding
  objects. A host snapshot cron firing between the snapshot check
  and destroy turns the 409 into a 500: accepted, never `-r`.

### What This Leaf Consumes

```go
// from leaf 02 (package s3, file zfsdatasets.go):
var ErrDatasetHasSnapshots error
var zfsBucketCreate func(ctx context.Context, bucket string) (string, error)
var zfsBucketDestroy func(ctx context.Context, dataset string) error
var zfsBucketDatasetExists func(ctx context.Context, dataset string) (bool, error)
func InstallZfsDatasetProvisioner(parentDataset, zfsBinary string) error
func UninstallZfsDatasetProvisioner()

// from leaf 01 (package main):
var zfsBucketsParentDataset string   // "" when feature off
// ServerConfig.ZfsBucketDatasets bool; ServerConfig.ZfsBinary string
```

## Tasks

### Task 1: createBucketHandler dataset path

**Objective:** Dataset create replaces MkdirAll when hooks installed.

**Files:**
- Modify: `internal/frontend/s3/bucket_handlers.go` (createBucketHandler only)
- Test: `internal/frontend/s3/zfsdatasets_handlers_test.go` (NEW)

**Step 1: Write failing tests** (fake the three hook vars directly -
they are plain package vars; httptest requests like
moved_handlers_test.go's TestCreateBucketHandler_*):
- hooks installed, create succeeds -> 200; hook received the bucket
  name; `.metadata` exists under the fake-created dir (fake hook does
  the MkdirAll to simulate zfs making the mountpoint); assert
  `os.MkdirAll` path NOT taken by checking a marker the fake records.
- create hook error -> 500; body contains InternalError; assert NO
  directory was created (no fallback); log contains stderr text.
- dataset OK + `.metadata` mkdir fails (make bucketPath/.metadata a
  pre-existing FILE) -> rollback: fake destroy hook called with the
  dataset name returned by create; 500. Also: rollback destroy that
  hits ErrDatasetHasSnapshots (host snapshot cron raced the create)
  -> still 500, dataset LEFT in place, loud log - never `-r`.
- existing-dir guard: pre-create the dir -> 409
  BucketAlreadyOwnedByYou, hooks NOT called.
- custom bucket guard: config Buckets entry -> existing 409/200
  behavior, hooks NOT called.

**Step 2:** `go test ./internal/frontend/s3/ -run TestCreateBucketZfsDataset -count=1` -> FAIL.

**Step 3: Implement** - after the stat guards and lock, branch on
`zfsBucketCreate != nil`. Keep the legacy branch untouched.

**Step 4:** PASS + full package suite:
`go test ./internal/frontend/s3/ -count=1` (existing create tests must
stay green UNMODIFIED).

### Task 2: deleteBucketHandler dataset path

**Objective:** Destroy replaces RemoveAll for dataset-backed buckets;
409 on snapshots.

**Files:** same two as Task 1.

**Step 1: Write failing tests:**
- exists hook returns "pool/data/bkt", destroy hook OK -> existing
  success wire result (match current handler's status - read the code
  for 204 vs 200); assert RemoveAll NOT called (dir still exists in
  the test tree after handler returns, since the fake destroy does not
  remove it).
- destroy hook returns wrapped ErrDatasetHasSnapshots (count 2) ->
  409; body XML contains `BucketHasSnapshots` and "2".
- destroy hook other error -> 500.
- exists hook returns false (plain dir) -> legacy RemoveAll; dir gone.
- exists hook errors -> 500; dir NOT removed.
- non-empty dataset dir (an object file present) -> existing
  BucketNotEmpty 409 BEFORE any hook call.
- custom bucket -> existing 403, hooks NOT called.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS + full
package suite green.

### Task 3: Startup wiring (package main)

**Objective:** main installs the provisioner when the feature is on.

**Files:**
- Modify: `s3_wiring.go` (the installS3Seams path or adjacent startup
  wiring - match where InstallZfsVersioningMode is called from main)
- Test: `root_coverage_wiring_test.go` or a NEW `zfs_wiring_test.go`
  (package main)

**Step 1: Write failing test** - simulate cfg with
ZfsBucketDatasets=true + `zfsBucketsParentDataset="pool/data"` ->
after the wiring call, the s3 hooks are non-nil (export a tiny probe
in export_test_surface.go if needed:
`ZfsDatasetProvisionerInstalled() bool`); feature off -> hooks nil.
Call `s3.UninstallZfsDatasetProvisioner()` in test cleanup.

**Step 2:** FAIL. **Step 3: Implement** the wiring call (guarded by
`if serverConfig.ZfsBucketDatasets`). **Step 4:** PASS;
`go test . -count=1` green.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./internal/frontend/s3/ . -count=1` green
- [ ] ALL pre-existing create/delete bucket tests pass UNMODIFIED
      (if one needed changing, stop and report - that is a contract break)
- [ ] `errors.Is(err, ErrDatasetHasSnapshots)` is the only 409 mapping;
      every other hook error is 500
- [ ] No mkdir fallback after failed zfs create (grep your diff)
- [ ] Custom-bucket guards untouched
- [ ] `golangci-lint run ./internal/frontend/s3/ .` clean on changed files
- [ ] DO-NOT-TOUCH: `reflinkversions.go`, `versioning_handlers.go`,
      `run-zfs-validation.sh`, `go.sum`, `zfsdatasets.go` (consume, never edit)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 3 behavior table fully pinned by tests (every row)
- [ ] Legacy path byte-identical when hooks nil
- [ ] lockObject still wraps hook calls in both handlers
- [ ] Wiring aborts nothing when feature off; installs exactly once at startup

Output: APPROVED or specific gaps with file:line.

## Notes

- gocognit: createBucketHandler/deleteBucketHandler may trip the
  complexity gate after branching. Established fix: extract the
  dataset branch into a named helper (`createBucketDatasetPath` /
  `deleteBucketDatasetPath`) - cf. serveMultiRangeIfNeeded precedent.
  Run the scoped linter BEFORE reporting.
- The fake hooks in tests must also create/remove the real dirs where
  a subsequent handler step stats them (zfs create makes the
  mountpoint; simulate that or the .metadata mkdir fails spuriously).
