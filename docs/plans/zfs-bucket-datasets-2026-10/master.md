# ZFS Bucket Datasets - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 5 leaf documents (flat)
- **Scope:** When enabled by config, S3 CreateBucket in a ZFS-backed
  dataDir creates a child ZFS DATASET (`<dataDirDataset>/<bucket>`)
  instead of a plain directory, and DeleteBucket destroys it. User
  decisions 2026-10-02: (1) refuse DeleteBucket with 409 when the
  dataset has snapshots - never `destroy -r`; (2) any zfs failure =
  fail-loud 500 on the request, never a silent mkdir fallback; (3)
  custom-path buckets keep the existing behavior - not creatable and
  not deletable via the API (403 on delete, already shipped).

## Goal

Each auto-discovered bucket under dataDir can be its own ZFS dataset.
Operators gain per-bucket snapshots, quotas, compression, and clean
zmetad attribution (events carry the bucket's own dataset name). The
gateway stays a dumb proxy: the dataset IS the bucket directory, the
filesystem remains the source of truth, no server-owned state is added
(charter rule 1 holds - a dataset is backing-store structure, like the
directories themselves).

Default behavior is UNCHANGED: without `zfs_bucket_datasets: true` in
config, CreateBucket is `mkdir` and DeleteBucket is `RemoveAll`, on ZFS
and non-ZFS hosts alike.

## Architecture

- Config (leaf 01): `zfs_bucket_datasets` (bool, default false) and
  `zfs_binary` (string, default `"zfs"`, PATH lookup - same convention
  as `zmetad_binary`). Config is fail-loud (DisallowUnknownFields);
  when the feature is enabled, startup ABORTS unless dataDir is a ZFS
  mountpoint (DetectZFS statfs probe) and its dataset resolves. No
  lazy first-request failures for a misconfigured server.
- Dataset ops (leaf 02): a new file
  `internal/frontend/s3/zfsdatasets.go` with argv-only exec of the zfs
  CLI (the `zfsSnapshotRunner` pattern from `zfssnapshots.go` - context
  timeout, semaphore bound, stderr captured, a package-level runner var
  tests replace). Parent-dataset resolution for dataDir is cached at
  install time.
- Handler wiring (leaf 03): `createBucketHandler` /
  `deleteBucketHandler` consult an installed provisioner hook (nil =
  legacy mkdir/RemoveAll path). The Backend interface stays FROZEN -
  no CreateBucket method; bucket lifecycle lives above the seam, as
  today.
- Wire behavior (leaf 03): create failure = 500 with the zfs stderr in
  the server log. Delete of a dataset WITH snapshots = 409, code
  `BucketHasSnapshots`, message telling the operator to remove
  snapshots first. Pre-feature plain directories on a ZFS dataDir
  still delete via RemoveAll (mountpoint-vs-dataset check at delete).
- e2e + docs (leaf 04): case 34 with graceful skip on non-ZFS hosts;
  README + docs/protocol-compatibility.md rows in the same change
  (repo hard rule).
- Live validation (leaf 05): the zfs-validate harness gains a
  dataset-bucket section on the zfs-meta host (charter rule 2 -
  fixtures alone never close a ZFS data-plane change).

## Interface Contracts

### Contract 1: Config keys (leaf 01, consumed by 03)

```go
// config.go ServerConfig additions (frozen JSON keys):
ZfsBucketDatasets bool   `json:"zfs_bucket_datasets"` // default false
ZfsBinary         string `json:"zfs_binary"`          // default "zfs"
```

- Defaults filled in the same loader pass as `defaultZfsVersioning` /
  `defaultZmetadBinary` (config.go ~lines 28-47 own the defaults).
- Startup validation when `ZfsBucketDatasets == true`:
  `metadata.DetectZFS(abs(DataDir))` must report ZFS AND
  `zfs list -H -o name -t filesystem <absDataDir>` must resolve a
  dataset name; either failing ABORTS startup with a message naming
  the check. ALSO: `exec.LookPath(ZfsBinary)` at startup when the
  feature is on - a missing binary ABORTS startup (no lazy
  first-request 500s). `ZETAOBJECT_ASSUME_ZFS=1` does NOT bypass this (that env
  gate is zmetad-DB-probe-only); dev hosts leave the feature off.
- config.json.example gains both keys with comments, in the SAME
  commit (fail-loud config rule: docs and struct move together).

### Contract 2: Dataset ops seam (leaf 02, consumed by 03)

```go
// File: internal/frontend/s3/zfsdatasets.go
package s3

// ErrDatasetHasSnapshots: destroy refused because `zfs list -t snapshot`
// on the dataset returned >= 1 row. Maps to 409 BucketHasSnapshots.
var ErrDatasetHasSnapshots = errors.New("s3: dataset has snapshots")

// InstallZfsDatasetProvisioner wires the feature ON: resolves the
// dataDir parent dataset once (argv-only `zfs list -H -o name
// -t filesystem <dataDir>`), stores the zfs binary name, and installs
// the create/delete hooks leaf 03 reads. Returns an error naming the
// failed check; main() aborts startup on error.
func InstallZfsDatasetProvisioner(dataDir, zfsBinary string) error

// zfsDatasetCreate(ctx, bucket) (string, error) - `zfs create
// <parent>/<bucket>`; returns the created dataset name.
// zfsDatasetDestroy(ctx, dataset) error - refuses with
// fmt.Errorf("...: %d snapshot(s): %w", n, ErrDatasetHasSnapshots)
// when snapshots exist (errors.Is matches; the count rides in the
// message for the 409 body); else `zfs destroy <dataset>` (NEVER -r).
// zfsDatasetExists(ctx, dataset) (bool, error) - `zfs list -H -o name
// <parent>/<bucket>` (the name is DETERMINISTIC, never resolved from a
// path: zfs list on a plain dir returns the PARENT dataset, which must
// never be read as the bucket's dataset). True when zfs prints the
// name; false (not an error) on "dataset does not exist" stderr +
// non-zero exit; other failures are errors. Used at delete to
// distinguish feature-created datasets from pre-feature plain dirs.
```

- Exec discipline (copy `zfssnapshots.go`): package-level
  `zfsDatasetRunner` var (tests replace), argv-only (no shell),
  `exec.CommandContext` with a 10s timeout constant, a dedicated
  bounded semaphore (cap 4), stderr captured and included in errors.
- Bucket names reaching `zfs create` are already
  `validateBucketName`-checked by the handler (leaf 03 keeps that
  ordering); the ops layer additionally rejects any name containing
  `/`, `@`, or leading `.` before exec (defense in depth).

### Contract 3: Handler wiring (leaf 03)

```go
// Hook vars are DECLARED in internal/frontend/s3/zfsdatasets.go
// (leaf 02) and INSTALLED by InstallZfsDatasetProvisioner; nil =
// legacy path. Leaf 03 MODIFIES bucket_handlers.go to consume them:
//   zfsBucketCreate func(ctx, bucket) (string, error)
//   zfsBucketDestroy func(ctx, dataset) error
//   zfsBucketDatasetExists func(ctx, path) (string, error)
```

Behavior table (pins the user decisions):

| Situation | Result |
|---|---|
| feature off | unchanged: mkdir / RemoveAll |
| create, `zfs create` fails | 500 InternalError; stderr in server log; NO mkdir fallback; no partial dir left (zfs create is atomic w.r.t. the mountpoint) |
| create, zfs OK but `.metadata` mkdir fails | rollback via `zfs destroy <dataset>`, then 500. RARE RACE: a host snapshot cron can snapshot the empty dataset first -> rollback hits the 409 sentinel -> still 500, LEAVE the empty dataset, log loudly (never `-r`) |
| delete, path is its own dataset, no snapshots | empty-check (incl. `.uploads`) FIRST -> `zfs destroy` -> 204; NO RemoveAll on a live mountpoint |
| delete, path is its own dataset, snapshots exist | 409, code `BucketHasSnapshots`, message: "Bucket dataset has N snapshot(s); remove them (zfs destroy <ds>@<snap>) before deleting the bucket." |
| delete, plain dir on ZFS dataDir (pre-feature) | legacy RemoveAll path |
| delete, exists hook errors | 500 fail-loud; dir NOT removed (never guess RemoveAll) |
| TOCTOU (any delete path) | snapshot check and destroy are two execs; a host snapshot cron firing between them turns the 409 into a 500. Accepted; never `-r`. |
| delete, custom-path bucket | 403 AccessDenied (EXISTING behavior - unchanged, pinned by test) |
| create, custom-path bucket | EXISTING behavior - unchanged (409/200 idempotent), pinned by test |

- The existing `lockObject(bucketPath)` create/delete serialization is
  KEPT and still taken before any zfs exec.
- `errors.Is(err, ErrDatasetHasSnapshots)` at the handler is the ONLY
  mapping to 409; every other ops error is 500.

### Contract 4: e2e case 34 (leaf 04)

```
scripts/e2e/cases/34-zfs-bucket-datasets.sh
```

- Graceful skip unless the server was launched with the feature on AND
  dataDir is ZFS (probe: `zfs list <dataDir>` succeeds) - same skip
  convention as the versioning cases.
- Asserts (with `ZETAOBJECT_...` env conventions from lib.sh):
  PUT /zbd34-bkt -> 200; `zfs list -H -o name <parent>/zbd34-bkt`
  resolves; ListBuckets contains it; PUT object then DELETE object;
  DELETE bucket -> 204 and the dataset is gone; create a snapshot,
  DELETE bucket -> 409 with `BucketHasSnapshots` in the body; destroy
  the snapshot, DELETE -> 204. Cleanup pairing per e2e rules.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-config-keys.md | leaf | none | 40K | A |
| 02 | 02-dataset-ops.md | leaf | none | 60K | A |
| 03 | 03-handler-wiring.md | leaf | 01, 02 | 60K | B |
| 04 | 04-e2e-docs.md | leaf | 03 (behavior), none (code) | 50K | B |
| 05 | 05-live-validation.md | leaf | 03, 04 | 40K | C (live host) |

**Concurrency groups:** A: 01+02 simultaneously (disjoint files). B: 03
then 04 (04's e2e asserts 03's wire codes; can run parallel to 03 only
with the Contract 3 table pasted inline). C: 05 runs last, on zfs-meta.

## Dispatch Protocol

For each concurrency group, in dependency order:

### Phase 1: Dispatch Concurrency Group A

Dispatch 01 and 02 simultaneously via `delegate_task`:

1. **Read** 01-config-keys.md, paste the FULL leaf text + Contracts 1
   and 3 into the dispatch context. Include the repo Coding Conventions
   block below. Include: "Do NOT commit. Do NOT run git add."
   DO-NOT-TOUCH (sibling WIP, currently dirty in the worktree):
   `internal/frontend/s3/reflinkversions.go`,
   `internal/frontend/s3/versioning_handlers.go`,
   `scripts/zfs-validate/run-zfs-validation.sh`, `go.sum`.
2. **Read** 02-dataset-ops.md, paste the FULL leaf text + Contract 2.
   Same includes and DO-NOT-TOUCH list. Point the child at
   `internal/frontend/s3/zfssnapshots.go` lines 40-90 as the exec
   pattern to copy.

### Phase 2: Review and Commit Each Child

After each agent returns, the orchestrator reviews in-session:

1. Read the changed files; check against leaf spec + contracts.
2. RE-RUN the gates in the parent (subagent gate reports are
   self-claims): `make test`, `make lint NEW_FROM_REV=HEAD` (0
   findings), `gofmt -l` clean on changed files.
3. Commit explicit paths only (never `git add -A` - sibling WIP files
   are dirty): `git add <leaf files> && git commit`.
4. Gaps -> re-dispatch with findings; max 3 cycles, then escalate.

### Phase 3: Dispatch Group B, then C

- Leaf 03 after 01+02 are COMMITTED (it wires their symbols).
- Leaf 04 after 03 is committed (e2e runs against real behavior).
- Leaf 05 last: live host. Before dispatching, check sibling state of
  `run-zfs-validation.sh` (`git status`, `git log -3 -- <file>`); if a
  sibling session still owns uncommitted hunks there, WAIT or do leaf
  05 in-session. The harness file is the single highest-collision file
  in this repo.

### Phase 4: Integration Review

1. Full gates: `make test && make lint NEW_FROM_REV=HEAD && make e2e`
   (e2e case 34 skips on this dev host - verify the SKIP line, not
   silence), `make parity-test` (metadata parity must be untouched).
2. `grep -rcE '^\s+[0-9]+\|' --include='*.go' .` returns zero.
3. Commit any integration fixups; update the tracking table; report.

## Review Checklist

- [ ] All tasks from the leaf document are implemented
- [ ] Contracts 1-4 satisfied exactly (JSON keys, signatures, status
      codes, error code string `BucketHasSnapshots`)
- [ ] Backend interface (`internal/backend/backend.go`) UNTOUCHED -
      frozen seam; `git diff --stat` must not list it
- [ ] `internal/metadata/metadata.go` UNTOUCHED - frozen seam
- [ ] No `destroy -r` anywhere in the diff (user decision 1)
- [ ] No mkdir fallback after a failed `zfs create` (user decision 2)
- [ ] Custom-bucket create/delete behavior unchanged (user decision 3),
      pinned by the existing tests still passing
- [ ] Tests written and passing (TDD followed)
- [ ] No scope creep, no debug artifacts, no line-number corruption

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go (repo go.mod version); exported PascalCase,
  unexported camelCase; imports grouped stdlib / third-party / local.
- **Errors:** wrap with `%w`; sentinel vars for wire-mapped conditions
  (ErrDatasetHasSnapshots pattern); no `panic` in library paths;
  ignored-error and empty-branch forms are HOOK-BANNED (`_ = err`,
  `if err != nil {}`) - route degraded reads through existing helpers.
- **Go 1.26 modernize forms:** `errors.AsType[T]` where applicable,
  `new(expr)` composite pointers, `for i := range n`, `wg.Go`.
- **Exec:** argv-only, never shell strings; `#nosec G204` comment
  justified inline like zfssnapshots.go:65.
- **Testing:** table-driven; runner-var seams for exec fakes; tests in
  the package under test.
- **Formatting:** `gofmt` before reporting; `make lint
  NEW_FROM_REV=HEAD` must be 0 (bare `make lint` shows sibling
  findings).
- **Static binary:** NO cgo, ever. zfs CLI exec only.

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-config-keys | COMPLETE | 1 | 91d4732; LookPath gate + parent-dataset var verified by parent |
| 02-dataset-ops | COMPLETE | 1 | 211bf41; hook site `_ = stdout` fixed by parent (positional discard); symbols re-verified pre-03 dispatch |
| 03-handler-wiring | COMPLETE | 1 | 3a33209; parent re-ran gates; delete-order + no-fallback hunks reviewed in-parent |
| 04-e2e-docs | COMPLETE | 1 | 4cdab2d; parent re-ran make e2e (723/0, SKIP verified) |
| 05-live-validation | COMPLETE | 2 | 23af6b4; first run 114/124 exposed the .zfs readdir bug -> fix 213f485; final sweep 124/124 exit 0 |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. `make test` - full unit suite green (new: config decode/validation
   tests, dataset-ops fake-runner tests, handler wiring tests).
2. `make lint NEW_FROM_REV=HEAD` - 0 findings.
3. `make e2e` - full suite green; case 34 SKIPS on non-ZFS dev hosts
   (verify the skip message appears).
4. `make parity-test` - metadata parity untouched.
5. Live: leaf 05's harness section on zfs-meta - create bucket via S3,
   confirm `zfs list` shows the dataset, snapshot-blocked delete 409,
   clean delete destroys; ALL checks green including pre-existing
   sections; report the exact tally.

## Structural Completeness Check (Before Dispatch)

Run: `python3 ~/.hermes/skills/software-development/hierarchical-planning/scripts/check_template_compliance.py docs/plans --strict-leaves`
(against `docs/plans` - the PARENT; a flat-leaf tree scanned alone is
misread as a forest root).

## Open Questions

- None blocking. All three design decisions were made by the user
  2026-10-02: (1) snapshots on delete -> 409 refusal, never
  `destroy -r`; (2) zfs exec failure -> fail-loud 500, no mkdir
  fallback; (3) custom-path buckets stay non-creatable/non-deletable
  via the API.
- Deferred (not in scope): per-bucket dataset properties at create
  time (quota/compression passthrough), dataset-per-bucket for
  custom-path buckets, and automatic snapshot policy for bucket
  datasets (host-owned, per charter).

## Notes

- **Sibling WIP is live in this worktree.** `git status` at authoring
  time shows dirty: `go.sum`, `internal/frontend/s3/reflinkversions.go`,
  `internal/frontend/s3/versioning_handlers.go`,
  `scripts/zfs-validate/run-zfs-validation.sh`. Branch `main` is ahead
  of origin by 19. Stage explicit paths; verify `git diff --cached`
  hunk-by-hunk before every commit; never sweep sibling files.
- **zfs-metadata upstream:** dataset-per-bucket makes zmetad events
  carry the bucket's dataset name directly. Re-check zfs-metadata#15
  (per-txg snapshot-on-write) before leaf 05 - if it landed, the
  harness section can also assert event attribution per dataset. Read
  the FETCHED contract (`git show origin/extended-metadata:contrib/zmetad/SCHEMA.md`),
  never the stale working tree.
- **Snapshots-mode versioning interaction:** a bucket that is its own
  dataset gets its OWN `.zfs/snapshot/` dir, which snapshots-mode
  versioning (`zfs_versioning: snapshots`) reads. Dataset-per-bucket
  makes faux versioning per-bucket-correct instead of whole-pool. This
  is a synergy, not a conflict - note it in README (leaf 04).
- **Destroy rollback in create:** a host snapshot cron (sanoid,
  zfs-auto-snapshot) CAN fire a snapshot between `zfs create` and the
  `.metadata` mkdir, so the rollback destroy CAN hit
  ErrDatasetHasSnapshots. Pin the behavior: still 500, LEAVE the empty
  dataset in place, log loudly - never `-r`, never force.
