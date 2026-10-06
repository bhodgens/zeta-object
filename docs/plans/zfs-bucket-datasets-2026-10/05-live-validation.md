# ZFS Bucket Datasets: Live Validation on zfs-meta - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> This leaf runs on a LIVE HOST (zfs-meta). Do NOT commit until the
> orchestrator reviews the harness diff and the full-sweep tally. Do
> NOT use read_file on existing source files - explore with
> search_files or terminal cat. After completing, report the EXACT
> check tally, every new check name, and any harness fixes.

## Meta

- **Parent:** ./master.md
- **Scope:** Extend `scripts/zfs-validate/run-zfs-validation.sh` with a
  dataset-bucket section; run the full sweep on zfs-meta; write
  `docs/validation-zfs-bucket-datasets-<date>.md`.
- **Dependencies:** 03 + 04 committed. The harness file is the repo's
  highest-collision file - BEFORE starting, run `git status` and
  `git log -3 -- scripts/zfs-validate/run-zfs-validation.sh`; if a
  sibling session has uncommitted hunks there, STOP and report to the
  orchestrator (do not stash or edit around sibling WIP).
- **Estimated Context:** 40K
- **Concurrency Group:** C (last; live host)

## Goal

Charter rule 2: fixtures alone never close a ZFS data-plane change.
Prove on real ZFS that: an S3 CreateBucket with the feature on creates
a real child dataset; the bucket is fully usable (object round-trip);
DeleteBucket destroys the dataset; a snapshot blocks delete with 409
BucketHasSnapshots and the dataset survives; after destroying the
snapshot, delete succeeds. Then run the WHOLE sweep and report the
exact tally (all pre-existing sections must stay green - max 3
fix+rerun cycles, then report).

## Context

- Harness shape: `run-zfs-validation.sh` builds the server binary
  locally, deploys to zfs-meta over ssh, creates a FRESH scratch
  dataset on the shared testpool, launches zmetad + zeta-server with a
  generated config, then runs an embedded python3 check script with
  numbered sections (`# ---- N. title ----`, `check(name, cond,
  detail)` calls). Latest section at authoring time: 11 (reflink
  versioning). Your section is the NEXT number - verify by reading the
  file at dispatch time (a sibling may have landed 12).
- The scratch dataset is recreated every run (shared testpool rule:
  sibling sessions churn it constantly). Your section must create its
  bucket UNDER the scratch dataset's mountpoint via the S3 API, and
  clean up its own datasets even on failure (the run's final teardown
  destroys the scratch parent, which takes children with it - but
  snapshot-holding children block the parent destroy: your cleanup
  MUST destroy the snapshot it creates).
- The launch config the harness generates must gain
  `"zfs_bucket_datasets": true` for your section's server phase. Two
  options - pick after reading the harness's config+starter section:
  (a) add the key to the EXISTING generated config (then every
  section's bucket creates become datasets - check that sections 1-11
  do not assert plain-dir semantics anywhere; if any do, use (b));
  (b) a second server phase: after the existing checks, relaunch the
  server with the feature-on config on a different port, run section
  N, kill it. Option (b) is safer and keeps pre-existing sections
  byte-identical; prefer it unless the harness structure makes (a)
  trivial.
- Remote-pkill discipline (skill pitfall): bracket one char in every
  pkill pattern (`pkill -f 'zeta-serve[r]'`) - an unbracketed pattern
  kills the ssh channel itself (exit 255, silent).
- Host facts to VERIFY before asserting (skill rule): the installed
  zmetad DB layout (`db_schema_version`), the testpool name, whether
  the running user may `zfs create` (the harness's ssh user needs
  dataset-create permission on the scratch subtree - if it does not,
  that is a FINDING to report, not something to work around with
  sudo).

## Interface Contracts (From Parent)

Contract 3 behavior table (master.md) is the wire truth:
- create OK -> 200 + dataset exists at `<scratchParent>/<bkt>`
- delete empty dataset bucket -> same success status the legacy path
  returns (read it from the handler/case 34)
- delete with snapshots -> 409, body contains `BucketHasSnapshots`
  AND the snapshot count; dataset still exists
- after snapshot destroy -> delete succeeds, dataset gone

## Tasks

### Task 1: Harness section (edit on the repo side first)

**Objective:** Add the dataset-bucket section + feature-on server phase.

**Files:**
- Modify: `scripts/zfs-validate/run-zfs-validation.sh` (append the new
  numbered section; add the second launch/teardown if option (b))
- Create (after the run): `docs/validation-zfs-bucket-datasets-<YYYY-MM-DD>.md`

**Steps:**
1. Read the harness end-to-end (terminal cat, it is ~975 lines; read
   the config+starter section and the LAST check section carefully).
2. Write the section: checks per the Contract list above, in the
   established `check(...)` style with detail strings that surface the
   wire body on failure. Include a check that the dataset's
   `zfs list -o name` PARENT is the scratch dataset (proves nesting),
   and a check that zmetad's datasets table picks up the new dataset
   within the poll-lag window the harness already uses for dataset
   tracking (copy that poll helper; do not invent a new sleep).
3. Cleanup: destroy the pin snapshot and any leftover bucket dataset
   in the section's finally path (or the harness's teardown
   convention) so a failed run does not wedge the scratch parent
   destroy.
4. `bash -n scripts/zfs-validate/run-zfs-validation.sh` clean.

### Task 2: Live run on zfs-meta

**Objective:** Full sweep green including the new section.

**Steps:**
1. `git fetch --all` in ~/git/zfs-metadata first; check
   `git log origin/extended-metadata -5` for upstream movement since
   the master's notes (esp. zfs-metadata#15 per-txg snapshots). If
   upstream changed the contract, report before adapting.
2. Run `./scripts/zfs-validate/run-zfs-validation.sh` per its usage
   header. Max 3 fix+rerun cycles on harness bugs (first live runs of
   new probes routinely expose them: missing host tools, poll-lag
   timing, stale gate versions). Fixing the harness itself is allowed
   and expected; fixing SERVER code found broken is a FINDING - report
   it to the orchestrator, do not silently patch server behavior in
   this leaf unless it is a one-line obvious bug (then report it as a
   separate concern-split commit candidate).
3. Record the exact final tally (e.g. "48/48") and every new check
   name into `docs/validation-zfs-bucket-datasets-<date>.md`, in the
   style of `docs/validation-zmetad-2026-10-01.md` (read it for the
   format: run metadata, host facts, check list, findings).
4. If a run fails and you need the live state for inspection: re-run
   with the keep flag if present and clean up MANUALLY afterward (the
   keep flag does not preserve state on failure - harness semantics).

### Task 3: Cross-repo observation (report only)

**Objective:** Note what dataset-per-bucket means for zmetad
attribution.

In the validation doc, record: events generated by writes to the new
bucket dataset carry the bucket's own dataset name in the zmetad DB
(`SELECT DISTINCT dataset FROM events ORDER BY 1` style probe, or the
datasets table). This is the attribution win the design promised -
cite the actual rows observed.

## Self-Verification Checklist

- [ ] New section numbered after the last existing section at DISPATCH time
- [ ] All new checks green AND every pre-existing check still green; exact tally recorded
- [ ] Snapshot cleanup verified: after the run, `zfs list -t snapshot -r <scratchParent>` shows no e2e-pin leftovers
- [ ] Validation doc written in the established format with host facts
- [ ] pkill patterns bracketed; no ssh-channel suicides (exit 255)
- [ ] `git diff --cached` (when the orchestrator stages) contains ONLY
      the harness + validation doc - never sibling WIP

**DO NOT COMMIT** until orchestrator review.

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Section asserts every Contract-3 row relevant on the live host
- [ ] Option (a)/(b) server-phase decision documented with rationale
- [ ] Tally in the report matches the run log (spot-check the log tail)
- [ ] The NEW section's own tally is reported as a hard number (the
      dev-host e2e SKIPs mean this section is the ONLY real coverage)
- [ ] A dotted bucket name (e.g. `zbd34.dot.bkt`) round-trips create+
      delete as a dataset on the live host
- [ ] Cleanup cannot wedge the shared testpool for sibling sessions
- [ ] Validation doc cites real observed output, not restated design

Output: APPROVED or specific gaps.

## Notes

- `ZETAOBJECT_ASSUME_ZFS=1` is NOT needed here (real ZFS host) and
  does not bypass the leaf-01 startup validation anyway.
- If the ssh user lacks `zfs create` permission on the scratch
  subtree, the create check fails with a permission stderr - capture
  it verbatim in the doc as a deployment requirement finding (README
  should then mention the permission need; report back so the
  orchestrator can fold a README line into leaf 04's committed docs).
- The second-server-phase relaunch must reuse a DIFFERENT port from
  every other listener (dual-listener self-collision pitfall) and be
  killed by its exact argv (`pgrep -af` first, kill by observed pid).
