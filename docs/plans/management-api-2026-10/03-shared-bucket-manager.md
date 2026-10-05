# Management API: Shared Bucket Manager - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** extract bucket lifecycle (create/delete/exists/list) and the
  per-path lock into shared packages, rewire the S3 frontend onto them,
  and add the "do not delete a dataset" policy for management callers.
- **Dependencies:** none
- **Estimated Context:** 70K
- **Concurrency Group:** A

## Goal

One implementation of bucket lifecycle, callable by both the S3 frontend
and (in leaf 04) the management API, with the S3 wire behavior unchanged
and one process-wide lock table so an S3 create and a management delete
of the same bucket cannot interleave.

## Context

- `internal/frontend/s3/bucket_handlers.go:149` `createBucketHandler`,
  `:235` `deleteBucketHandler`. The custom-bucket guards are at
  `:163-175` (create) and `:243-250` (delete, 403 by design). The
  emptiness scan is `:268-296` and already excludes `.metadata`,
  `.bucket-actions` and `.zfs` (`:279-292`). The serialization is
  `lockObject(bucketPath)` (`internal/frontend/s3/seam.go:129`).
- `internal/frontend/s3/zfsdatasets_handlers.go` holds
  `createBucketDatasetPath`, `deleteBucketDatasetPath` and the
  `zfsBucketDatasetParent` variable; `internal/frontend/s3/zfsdatasets.go`
  holds the provisioning hook vars (`zfsBucketCreate`, `zfsBucketDestroy`,
  `zfsBucketDatasetExists`) and `InstallZfsDatasetProvisioner`.
- `seam.go:120-150` holds the lock implementation: a package-level hook
  plus `defaultLockObject` keyed by path in a `sync.Map`.
- AGENTS.md rule: moving code between packages REQUIRES re-measuring and
  updating the coverage floors in the SAME change.

## Interface Contracts (From Parent)

### Contract 4 (restated, binding)

```go
// internal/bucketmanager (NEW package):
var ErrDatasetBucketNotDeletable = errors.New("bucketmanager: bucket is a ZFS dataset; destroy it on the host")

type DeleteOptions struct {
	AllowDatasetDestroy bool
}

func Install(env Env)
func Create(ctx context.Context, name string) error
func Delete(ctx context.Context, name string, opts DeleteOptions) error
func Exists(ctx context.Context, name string) (bool, error)
func List(ctx context.Context) ([]BucketInfo, error)

type Env struct {
	Locks       Locker                                  // the ONE process lock table
	BucketPath  func(bucket string) string               // getBucketPath semantics
	Custom      func(bucket string) (path string, ok bool)
	Provisioner Provisioner                              // nil = plain dirs only
}

type Provisioner interface {
	Create(ctx context.Context, bucket string) (dataset string, err error)
	Destroy(ctx context.Context, dataset string) error
	Exists(ctx context.Context, dataset string) (bool, error)
	Parent() string
}
```

- S3 behavior preserved EXACTLY: statuses, error codes, messages, the
  custom-bucket guards, the empty check, and the lock ordering.
- The management path passes `AllowDatasetDestroy: false`, which turns a
  dataset-backed delete into `ErrDatasetBucketNotDeletable` (409 in leaf
  04). The S3 path passes `true`, keeping `zfs destroy` plus the
  existing `409 BucketHasSnapshots` refusal.
- Frozen seams untouched: `internal/backend/backend.go`,
  `internal/metadata/metadata.go`.

## Tasks

### Task 1: extract the per-path lock

**Objective:** one lock table for the whole process.

**Files:**
- Create: `internal/fslock/fslock.go` (move the implementation from
  `internal/frontend/s3/seam.go:120-150`: the installed hook, the
  default `sync.Map` implementation, and the `Lock(path)` entry)
- Modify: `internal/frontend/s3/seam.go` (its `lockObject` delegates to
  `fslock`; the s3-installed hook API is preserved so existing wiring and
  tests keep working)
- Test: `internal/fslock/fslock_test.go`

**Step 1: Write failing tests:** the same path locks out a second
caller; different paths do not block; an installed hook is used when
present; the default is used when not.

**Step 2:** `go test ./internal/fslock/ -count=1` -> FAIL.
**Step 3: Implement.** **Step 4:** PASS; the whole s3 package still green
(the existing concurrency tests are the real gate here).

### Task 2: the bucketmanager package

**Objective:** lifecycle logic in one place.

**Files:**
- Create: `internal/bucketmanager/bucketmanager.go` (Install/env,
  Create, Delete, Exists, List)
- Create: `internal/bucketmanager/delete.go` if the file gets long
  (keep every function under the gocognit gate)
- Test: `internal/bucketmanager/bucketmanager_test.go`

**Step 1: Write failing tests** (fake env: a temp dir for paths, a real
`fslock`, a fake provisioner recording calls):
- create on a fresh name creates the directory and the `.metadata`
  child;
- create when the directory exists -> the SAME error the S3 handler
  produces today (`BucketAlreadyOwnedByYou` semantics: assert the
  concrete returned error/typed value the extraction preserves);
- create when a FILE occupies the path -> the existing conflict error;
- create for a custom bucket -> the existing 409 semantics, and the
  provisioner is NOT called;
- delete refuses a non-empty bucket (an object file present) BEFORE any
  provisioner call;
- delete refuses in-flight multipart uploads (`.metadata/.uploads`
  containing a `.json`) BEFORE any provisioner call;
- delete ignores `.metadata`, `.bucket-actions` and `.zfs` (regression
  guard for the live bug fixed in 213f485);
- delete on a custom bucket -> the existing 403 semantics;
- delete with `AllowDatasetDestroy: false` on a dataset-backed bucket
  -> `errors.Is(err, ErrDatasetBucketNotDeletable)` and the provisioner's
  Destroy is NOT called;
- delete with `AllowDatasetDestroy: true` on a dataset-backed bucket ->
  Destroy IS called with the dataset name;
- delete of a plain directory -> the directory is removed, no
  provisioner call;
- a create that fails at the provisioner -> the error is returned and
  NO directory is created (no fallback);
- create with the provisioner OK but `.metadata` failing -> the
  provisioner's Destroy is called (rollback), matching today's behavior;
- CONCURRENCY: a goroutine holding the lock for a bucket path blocks a
  concurrent Create/Delete of the same bucket (proves both callers share
  one lock table).

**Step 2:** `go test ./internal/bucketmanager/ -count=1` -> FAIL.
**Step 3: Implement** by moving the logic verbatim, not by rewriting it.
**Step 4:** PASS.

### Task 3: the provisioner adapter in package s3

**Objective:** the existing dataset hooks are reachable without moving
the frozen provisioner API.

**Files:**
- Create: `internal/frontend/s3/dataset_provisioner.go` (package s3:
  adapt `zfsBucketCreate`/`zfsBucketDestroy`/`zfsBucketDatasetExists` and
  `zfsBucketDatasetParent` to `bucketmanager.Provisioner`; return a nil
  Provisioner when the hooks are not installed)
- Test: `internal/frontend/s3/dataset_provisioner_test.go`

**Step 1: Write failing tests:** with hooks installed the adapter's
methods drive the hooks; with no install the adapter is nil and
`Create`/`Destroy` are never called (assert via a fake hook).

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 4: rewire the S3 handlers

**Objective:** the S3 frontend calls the shared manager.

**Files:**
- Modify: `internal/frontend/s3/bucket_handlers.go` (create and delete
  bodies delegate to bucketmanager; the guards that must stay
  handler-side, if any, keep their ordering)
- Modify/delete: `internal/frontend/s3/zfsdatasets_handlers.go` (the
  moved helpers leave; keep only what is still s3-specific)
- Modify: `main.go` or a new main-side `bucketmanager_wiring.go` that
  calls `bucketmanager.Install` with the process lock, `getBucketPath`,
  the custom-bucket map, and the provisioner adapter (this leaf owns
  `main.go`)

**Step 1: Write failing tests:** the EXISTING s3 create/delete handler
tests must pass UNMODIFIED (they are the contract). Add one test
asserting the handler delegates (for example a marker recorded by the
installed manager env).

**Step 2:** `go test ./internal/frontend/s3/ . -count=1` -> the new test
FAILS. **Step 3: Implement** the delegation. **Step 4:** PASS with
every pre-existing test unmodified.

### Task 5: coverage floors

**Objective:** the AGENTS.md rule is satisfied in the same change.

**Files:**
- Modify: the floor table in `.github/workflows/check.yml` (and any
  floor in the Makefile) for every package whose measured coverage
  changed

**Step 1:** measure with
`go test ./internal/... . -count=1 -coverprofile=/tmp/c.out` and
`go tool cover -func=/tmp/c.out`.
**Step 2:** set each floor to the measured value rounded DOWN, with the
comment style already used in that file.
**Step 3:** no floor may be lowered to hide a regression: a drop caused
by moved code must be matched by the moved tests arriving in the same
change.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./... -count=1` green
- [ ] ALL pre-existing s3 create/delete tests pass UNMODIFIED (a needed
      change is a contract break - stop and report)
- [ ] The `.zfs` emptiness exclusion is preserved (regression test present)
- [ ] One lock table: the concurrency test proves S3 and manager callers serialize
- [ ] No `zfs destroy` on the management path (`AllowDatasetDestroy` false)
- [ ] `internal/backend/backend.go`, `internal/metadata/metadata.go`,
      `internal/frontend/s3/zfsdatasets.go` UNTOUCHED (consumed only)
- [ ] Coverage floors re-measured and updated in the same change
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH: `internal/frontend/s3/bucket_handlers.go` is YOURS;
      `frontends.go`, `main_server.go`, `s3_wiring.go`,
      `scripts/zfs-validate/run-zfs-validation.sh`, `go.sum` are not

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 4 satisfied: package name, function signatures,
      `DeleteOptions`, sentinel error text
- [ ] Moved logic is verbatim (diff the moved block against the original;
      a rewrite that changes a status code or message is a defect)
- [ ] The management delete path cannot reach `zfs destroy`
- [ ] Fault ordering preserved: guards, then lock, then emptiness, then
      provisioner
- [ ] Coverage floors updated with measured values

Output: APPROVED or specific gaps with file:line.

## Notes

- This leaf MOVES code, so the risk is silent behavior drift. The
  existing handler tests are the strongest available guard: they must
  pass unmodified, and the reviewer should confirm the test files were
  not edited.
- Do not add new bucket features here (quota, snapshot policy): this
  leaf is a pure extraction plus the one new delete policy.
