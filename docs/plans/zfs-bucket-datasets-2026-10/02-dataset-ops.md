# ZFS Bucket Datasets: Dataset Ops Seam - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you
> touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** New file `internal/frontend/s3/zfsdatasets.go`: argv-only
  zfs CLI exec helpers for create/destroy/snapshot-check/mountpoint-
  resolution, with the repo's runner-var + semaphore exec discipline.
- **Dependencies:** none (leaf 01 owns config; this leaf owns only the
  s3-package ops layer)
- **Estimated Context:** 60K
- **Concurrency Group:** A

## Goal

Provide the dataset operations leaf 03 wires into the bucket handlers:
create a child dataset under a resolved parent, refuse destroy when
snapshots exist (sentinel error), destroy without `-r` ever, and map a
bucket directory path back to its dataset (empty when the path is not
its own mountpoint - the pre-feature plain-dir case). All exec goes
through a package-level runner var tests replace.

## Context

zeta-object's s3 frontend already execs the zfs CLI in
`internal/frontend/s3/zfssnapshots.go` (lines ~40-90): a
`zfsSnapshotRunner` package var wrapping `exec.CommandContext`
(argv-only, 5s timeout, stdout/stderr captured), a dedicated bounded
semaphore `zfsSnapExecSem` (cap 4), and a `runZfsSnapshot` wrapper that
honors ctx cancellation while queueing. COPY that pattern into a new
file with its own runner var and its own semaphore - do not share the
snapshots store's semaphore (independent bound, same rationale as the
comment at zfssnapshots.go:53-56).

Key files to understand before implementing:
- `internal/frontend/s3/zfssnapshots.go` - exec pattern to copy.
- `internal/frontend/s3/versioning_seam.go` - Install* seam style
  (package-level hook vars installed at startup).
- `internal/frontend/s3/errors_to_s3.go` - how sentinel errors map to
  S3 wire codes (leaf 03 does the mapping; this leaf only defines the
  sentinel).

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/frontend/s3/zfsdatasets.go
package s3

// ErrDatasetHasSnapshots: destroy refused - `zfs list -H -t snapshot`
// on the dataset returned >= 1 row. Leaf 03 maps this to 409
// BucketHasSnapshots. NEVER destroy -r; NEVER auto-remove snapshots.
var ErrDatasetHasSnapshots = errors.New("s3: dataset has snapshots")

// InstallZfsDatasetProvisioner wires the feature ON. parentDataset is
// the dataDir dataset resolved at startup (leaf 01); zfsBinary is the
// configured binary name. It installs the three package-level hook
// vars leaf 03 reads:
//   zfsBucketCreate func(ctx context.Context, bucket string) (string, error)
//   zfsBucketDestroy func(ctx context.Context, dataset string) error
//   zfsBucketDatasetExists func(ctx context.Context, dataset string) (bool, error)
// Returns an error only on invalid arguments (empty parentDataset or
// zfsBinary); it does NOT exec (startup already validated ZFS).
func InstallZfsDatasetProvisioner(parentDataset, zfsBinary string) error

// UninstallZfsDatasetProvisioner restores nil hooks (tests).
func UninstallZfsDatasetProvisioner()
```

Internal ops (unexported, driven through the hooks and tested directly
via the fake runner):

```go
// zfsDatasetCreate(ctx, binary, parent, bucket) (dataset string, err error)
//   - validates bucket: non-empty, no "/", no "@", no leading "."
//     (defense in depth - handler already ran validateBucketName);
//     reject with a plain error BEFORE any exec.
//   - exec: <binary> create <parent>/<bucket>
//   - returns "<parent>/<bucket>" on success; on failure wraps err and
//     includes stderr.

// zfsDatasetDestroy(ctx, binary, dataset) error
//   - exec: <binary> list -H -o name -t snapshot -d 1 <dataset>
//     (10s timeout via the shared runner; parse: any non-blank line =
//     has snapshots)
//   - snapshots exist -> return fmt.Errorf("%w (%d): ...",
//     ErrDatasetHasSnapshots, n) - errors.Is must match, and the
//     COUNT rides in the message for the 409 body (leaf 03 formats).
//   - none -> exec: <binary> destroy <dataset>   // NEVER "-r"
//   - validate dataset: non-empty, contains no "@" (a snapshot arg is
//     a programming error here), no leading "-".

// zfsDatasetExists(ctx, binary, dataset) (bool, error)
//   - exec: <binary> list -H -o name <dataset>
//   - exit 0 + non-empty stdout -> true. Any non-zero exit -> false
//     (NOT an error): the name is deterministic (<parent>/<bucket>),
//     so "does not exist" simply means the bucket dir is a pre-feature
//     plain dir; only runner/timeout failures return an error.
//     NO stderr string matching - zfs's wording is not a contract.
//
// Exec discipline (copy zfssnapshots.go):
//   var zfsDatasetRunner = func(ctx, binary string, args ...string)
//       ([]byte, string, error) { ... exec.CommandContext, 10s
//       timeout constant zfsDatasetCmdTimeout, argv-only, stderr
//       captured ... }   // #nosec G204 with inline justification:
//       binary is config-controlled; args are argv-only; bucket names
//       are handler-validated and re-checked here.
//   var zfsDatasetExecSem = make(chan struct{}, 4)  // dedicated bound
//   func runZfsDataset(ctx, binary, args...) - semaphore + ctx-cancel
//     aware, same shape as runZfsSnapshot.
```

### What This Leaf Consumes

Nothing from sibling leaves at compile time. Leaf 01's startup passes
`parentDataset`/`zfsBinary` INTO `InstallZfsDatasetProvisioner` through
leaf 03's wiring - this leaf only defines the signatures above.

## Tasks

### Task 1: Runner + semaphore scaffold

**Objective:** The exec plumbing, pinned by a fake-runner test.

**Files:**
- Create: `internal/frontend/s3/zfsdatasets.go`
- Test: `internal/frontend/s3/zfsdatasets_test.go`

**Step 1: Write failing test** - replace `zfsDatasetRunner` with a
recording fake; call `runZfsDataset`; assert argv passthrough,
timeout constant applied (assert the ctx deadline the fake observes is
~10s), and semaphore bounding (fire 8 concurrent calls with a
blocking fake; assert max in-flight == 4).

**Step 2:** `go test ./internal/frontend/s3/ -run TestZfsDataset -count=1`
-> FAIL (no symbols).

**Step 3: Implement** runner + sem + wrapper per Contract (copy
zfssnapshots.go shape).

**Step 4:** PASS.

### Task 2: create / destroy ops

**Objective:** Dataset create with name validation; destroy with the
snapshots refusal.

**Files:** modify the two files from Task 1.

**Step 1: Write failing tests** (fake runner, table-driven):
- create: argv == `create <parent>/<bucket>`; returns
  `<parent>/<bucket>`; runner error -> wrapped with stderr; invalid
  bucket names (`a/b`, `a@b`, `.hidden`, ``) rejected WITHOUT the fake
  runner being called (assert zero calls).
- destroy, snapshots exist: fake returns "ds@snap1\nds@snap2" for the
  list exec -> `errors.Is(err, ErrDatasetHasSnapshots)` true, message
  contains "2"; assert NO destroy exec happened (fake records argvs).
- destroy, no snapshots: list returns "" -> destroy argv
  `destroy <dataset>`; assert argv NEVER contains "-r" across ALL
  destroy tests.
- destroy argv validation: dataset "" / containing "@" / leading "-"
  rejected pre-exec.
- runner failure on the list exec -> error (not the snapshots path).

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 3: mountpoint resolution + Install/Uninstall

**Objective:** `zfsDatasetForMount` and the hook installation.

**Files:** modify the two files from Task 1.

**Step 1: Write failing tests:**
- exists: exit 0 + "pool/data/<bkt>" -> true; "does not exist"
  failure -> false, nil (plain-dir case); runner/timeout failure ->
  error.
- Install with empty parentDataset or empty binary -> error, hooks
  stay nil. Install valid -> all three hook vars non-nil; calling
  `zfsBucketCreate(ctx, "bkt")` drives the fake runner with
  `create <parent>/bkt`; `zfsBucketDatasetExists` drives exists.
- Uninstall -> hooks nil again.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS; then run
the WHOLE package: `go test ./internal/frontend/s3/ -count=1`.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./internal/frontend/s3/ -count=1` green
- [ ] `ErrDatasetHasSnapshots` exported exactly as in the Contract
- [ ] No `destroy -r` anywhere; grep your diff for `"-r"` -> zero hits
- [ ] Hooks are package vars in this file (leaf 03 reads them)
- [ ] `golangci-lint run ./internal/frontend/s3/` clean on new files
- [ ] DO-NOT-TOUCH respected: `reflinkversions.go`,
      `versioning_handlers.go`, `zfssnapshots.go` (read, never edit),
      `run-zfs-validation.sh`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Task implemented; every test present and passing
- [ ] Signatures match Contract 2 in master.md exactly
- [ ] Exec discipline: argv-only, timeout, dedicated semaphore cap 4,
      ctx-cancel honored while queueing
- [ ] Invalid names rejected PRE-exec (zero runner calls)
- [ ] exists maps any zfs non-zero exit to (false, nil) - no stderr
      string matching; only runner/timeout failures error

Output: APPROVED or specific gaps with file:line.

## Notes

- The dataset name is DETERMINISTIC (<parent>/<bucket>), never
  resolved from a path: `zfs list <plaindir>` would return the
  PARENT dataset, and reading that as the bucket's dataset is the
  destroy-the-wrong-dataset hazard this contract exists to prevent.
- Keep the hook vars in THIS file so leaf 03's diff is handler-only;
  leaf 03 may read them but must not move them.
