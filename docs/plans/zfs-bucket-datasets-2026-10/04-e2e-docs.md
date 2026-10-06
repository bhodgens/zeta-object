# ZFS Bucket Datasets: E2E Case 34 + Docs - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you
> touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** Wire-level e2e case 34 (graceful skip off-ZFS) + README
  section + docs/protocol-compatibility.md rows.
- **Dependencies:** 03 committed (the e2e asserts real wire behavior).
- **Estimated Context:** 50K
- **Concurrency Group:** B

## Goal

Ship the e2e coverage the AGENTS.md hard rule requires in the same
change as the feature, plus honest docs: README explains the opt-in,
the destroy policy (409 on snapshots, never recursive destroy), and
the per-bucket snapshots/quota benefits; protocol-compatibility.md
gains CreateBucket/DeleteBucket dataset rows.

## Context

- e2e suite: `scripts/e2e/cases/*.sh` in lexical order; shared helpers
  in `scripts/e2e/lib.sh`; convention: `BKT=` bucket variable,
  create/cleanup pairing, graceful skip when the environment lacks a
  prerequisite (see case 32/33 versioning skips and case 28 xattr for
  the ZFS-conditional skip pattern). The suite runner is
  `scripts/e2e/run-e2e.sh`; server launch config is built there -
  check how case 32/33 get a ZFS-mode server or whether the runner
  launches ONE server for all cases (if one server: case 34 must skip
  unless the running server has the feature on - probe with
  `zfs list <datadir>` from the case, same as other ZFS-conditional
  cases).
- Numbering: 33 is taken (reflink-versioning). This case is
  `34-zfs-bucket-datasets.sh`.
- README has an "S3 Versioning" section and a feature-highlights list;
  docs/protocol-compatibility.md is the per-operation honest matrix
  (implemented / degrades / absent + wire status + proof link per
  row). Add rows in THIS change (repo hard rule).

## Interface Contracts (From Parent)

### What This Leaf Exposes

```
scripts/e2e/cases/34-zfs-bucket-datasets.sh   (NEW, executable)
README.md                                     (MODIFY: new section + highlight bullet)
docs/protocol-compatibility.md                (MODIFY: S3 CreateBucket/DeleteBucket rows or a new subsection)
```

### Case 34 asserts (Contract 4)

Skip guard first: feature probe - if the server's dataDir is not a ZFS
mountpoint OR the server was launched without `zfs_bucket_datasets`,
print the standard SKIP line (match existing cases' skip format) and
exit 0. The orchestrator's live run (leaf 05) provides the real
execution; on the dev host this case SKIPS.

When live:
1. `PUT /zbd34-bkt` -> 200. `zfs list -H -o name <parent>/zbd34-bkt`
   resolves (the case gets `<parent>` from
   `zfs list -H -o name -t filesystem <dataDir>`).
2. GET / (ListBuckets) contains `zbd34-bkt`.
3. PUT an object, GET it back, DELETE the object -> existing-case
   conventions (curl/aws-cli style from lib.sh).
4. `DELETE /zbd34-bkt` (empty bucket) -> success status (match what
   case 32/33 pin for empty-bucket delete); `zfs list` no longer
   resolves the dataset; the directory is gone.
5. Snapshot refusal: re-create the bucket, PUT+DELETE an object,
   `zfs snapshot <parent>/zbd34-bkt@e2e-pin`, `DELETE /zbd34-bkt`
   -> 409 with `BucketHasSnapshots` in the body; the dataset STILL
   exists; `zfs destroy <parent>/zbd34-bkt@e2e-pin`; DELETE -> 204;
   dataset gone.
6. Dotted bucket name: PUT `zbd34.dot.bkt` -> 200; dataset
   `<parent>/zbd34.dot.bkt` resolves (dots are legal dataset
   components); DELETE it clean.
7. Re-create the bucket via S3 after destroy (round-trip idempotence
   of the provisioning path), then final cleanup pairing: destroy any
   leftover dataset, delete any leftover dir - the case must not leak
   datasets into the shared testpool (harness rule: sibling sessions
   reuse it).

## Tasks

### Task 1: e2e case 34

**Objective:** The case file above, executable, skip-guarded.

**Files:**
- Create: `scripts/e2e/cases/34-zfs-bucket-datasets.sh`

**Step 1:** Read case 32 (versioning) and case 28 (xattr) with
terminal cat to copy the skip-guard shape, lib.sh sourcing, BKT
convention, and assert helpers exactly.

**Step 2:** Write the case with all six assert groups. Use lib.sh's
assert helpers (status asserts AND body asserts - status-only checks
are a known weak-assert finding in this repo).

**Step 3: Verify** `bash -n scripts/e2e/cases/34-zfs-bucket-datasets.sh`
(syntax) and `make e2e` on the dev host: case 34 SKIPS (verify the
SKIP line appears in output; the rest of the suite stays green).

### Task 2: README section + highlight bullet

**Objective:** Document the feature where users look.

**Files:** Modify: `README.md`.

Content (match README voice - plain, honest, mechanism-first):
- New section "ZFS bucket datasets" near "S3 Versioning": config keys
  `zfs_bucket_datasets` / `zfs_binary`; startup validation (aborts
  unless dataDir is ZFS); CreateBucket makes
  `<dataDirDataset>/<bucket>`, DeleteBucket destroys it.
- Destroy policy paragraph: a dataset with snapshots answers 409
  `BucketHasSnapshots`; the server NEVER destroys recursively and
  never removes snapshots - the operator decides (honest-data rule).
- Pre-feature plain directories on the same dataDir keep legacy
  delete semantics; custom-path buckets are unaffected (never
  creatable/deletable via API).
- Synergy note: a per-bucket dataset gets its own `.zfs/snapshot/`
  history, which `zfs_versioning: snapshots` reads - faux versioning
  becomes per-bucket-correct.
- Feature-highlights bullet mentioning per-bucket datasets.
- Grep README for any CreateBucket/DeleteBucket claims that are now
  stale (e.g. "buckets are plain directories") and fix them in this
  commit.

### Task 3: protocol-compatibility.md rows

**Objective:** The honest matrix stays current (repo hard rule).

**Files:** Modify: `docs/protocol-compatibility.md`.

- Find the S3 CreateBucket/DeleteBucket rows (or subsection) and
  update: dataset mode "implemented (opt-in)", wire status 200/204,
  proof link = e2e case 34 + the leaf-05 validation doc (reference
  `docs/validation-zfs-bucket-datasets-<date>.md` - leaf 05 creates
  it; cite the path even before it exists, matching how existing rows
  cite validation docs).
- Add the 409 BucketHasSnapshots behavior to the DeleteBucket row's
  notes column.

## Self-Verification Checklist

- [ ] `bash -n` clean on case 34; `make e2e` green with case 34 SKIPPING on this host
- [ ] Skip guard matches the existing cases' exact SKIP output format
- [ ] Body asserts present (409 body contains BucketHasSnapshots), not status-only
- [ ] README section + highlight bullet + stale-claim sweep done
- [ ] protocol-compatibility.md rows updated with proof links
- [ ] DO-NOT-TOUCH: `reflinkversions.go`, `versioning_handlers.go`,
      `run-zfs-validation.sh` (leaf 05 owns the harness), `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Case 34 covers all six assert groups incl. snapshot-refusal 409 and round-trip re-create
- [ ] Cleanup pairing: no dataset or dir leaks (explicit destroy/rm in cleanup)
- [ ] README claims match the Contract 3 behavior table exactly
- [ ] Compatibility matrix rows present with proof links

Output: APPROVED or specific gaps with file:line.

## Notes

- The shared testpool on zfs-meta is churned by sibling sessions -
  case 34 must resolve `<parent>` at RUNTIME from the live dataDir,
  never hardcode a dataset name.
- If run-e2e.sh launches one server for all cases without the feature
  flag, case 34 skips everywhere except a feature-flagged launch;
  leaf 05's harness supplies that launch. Note this in the case
  header comment.
