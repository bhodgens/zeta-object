# Live Validation: zfs_bucket_datasets on zfs-meta (2026-10-03)

Leaf 05 of the `zfs-bucket-datasets-2026-10` tree. Repo HEAD `4cdab2d`
(leaf 04 committed). Harness:
`scripts/zfs-validate/run-zfs-validation.sh` extended with **section 12**
(dataset buckets) behind a **second server phase** (option (b)).

## Run result

| Sweep | Tally |
|---|---|
| Sections 0–11 (pre-existing, byte-identical) | **91/91 PASS** |
| Section 12 (zfs_bucket_datasets, new) | **23/33** (10 FAIL, all one cascade) |
| Run total | **114/124** — VALIDATION FAILED (exit 1) |

All 3 harness fix+rerun cycles were consumed (see Findings); the leaf's
acceptance bar (all green) is **NOT met this run**. Everything that
failed traces to ONE wire behavior (F-zbd-1 below); the zfs dataset
semantics themselves — create/nesting, round-trip, snapshot refusal,
snapshot-gated destroy, dotted names, cleanup — are all proven green.

## Host facts (verified before asserting)

- Host: zfs-meta, Ubuntu, OpenZFS **2.4.99-1** (`zfs-2.4.99-1`),
  extended-metadata branch. ssh user `caimlas`.
- Installed zmetad DB: `db_schema_version=8`, `events_schema_version=3`
  (binary at `/usr/local/sbin/zmetad`, built 2026-10-03).
- testpool exists (`testpool` mounted at `/testpool`); scratch dataset
  is `testpool/zval` (recreated fresh by the harness with
  `events=on`, `events_size=1M`).
- **`zfs create` permission:** `caimlas` has NO delegation on
  `testpool` (`zfs allow testpool` is empty; unprivileged create on the
  pool root → `permission denied`). Passwordless sudo IS available
  (`(ALL) NOPASSWD: ALL`), which is how the whole harness already runs
  its zfs ops. A delegated-subtree experiment (`zfs allow` +
  `create,destroy,mount`) still produced
  `filesystem successfully created, but it may only be mounted by root`
  — **on this host, only root can mount datasets**, so an
  unprivileged-dataDir server cannot provision usable bucket datasets.
- `/dev/zfs` is 0666 (zmetad runs unprivileged fine).
- `events` is `PROP_INHERIT` upstream
  (`module/zcommon/zfs_prop.c:803`) — child datasets inherit
  `events=on` from the scratch parent with no server-side `-o` flag
  (verified live: `zfs get events testpool/zval/zval-permtest/kid` →
  `on` after plain `zfs create`).

## Upstream contract check (zfs-metadata)

`git fetch --all`; `origin/extended-metadata` head is `19c157b7f`
(2026-10-02, "tests: ZFS_EV_PRINCIPAL coverage and schema 3/8 pins").
**zfs-metadata#15 (per-txg snapshot-on-write) has NOT landed** — no
per-txg / snapshot-on-write commit exists on any ref. No contract
movement to adapt to; the wire/layout contract the server consumes
(layout 8 / wire 3) is unchanged.

## Option (a)/(b) decision

**Option (b): a second server phase.** Rationale:
- With the harness's generated config (`dataDir: /testpool/`), the
  leaf-01 startup parent resolution (`zfsDatasetForMount`) resolves the
  PARENT to the **pool root** (`testpool`) — option (a) would provision
  bucket datasets directly under the pool root, which the shared-pool
  rule forbids and which `caimlas` cannot create on anyway. The phase-2
  config uses `dataDir: /testpool/zval/` so the parent is the scratch
  dataset itself.
- Option (a) would flip EVERY existing section's bucket creates into
  dataset creates (section 10 even snapshots the bucket dataset), a
  large unreviewed behavior change to sections 1–11. Option (b) keeps
  checks.py and sections 0–11 byte-identical (verified: 91/91 both
  baseline-equal).
- Phase 2 listens on **:9708** (distinct from :9707), starts AFTER the
  main sweep, and is killed by observed pid (pidfile `zbd-server.pid`
  + `pgrep -af 'zeta-serve[r]'` first). It runs under `sudo -n -E`
  (see F-zbd-3) with `ZETAOBJECT_CONFIG` pointing at a dedicated
  `config-zbd.json` (`zfs_bucket_datasets: true`).

## Section 12 check list (exact names, run 3)

PASS (23): s12 PUT dataset bucket -> 200 · s12 dataset
<parent>/zbd12 resolves on zfs list · s12 dataset PARENT is the
scratch dataset (nesting proof) · s12 .metadata dir exists inside the
new dataset · s12 PUT object on dataset bucket -> 200 · s12 GET object
round-trip exact bytes · s12 DELETE object -> 204 · s12 zmetad
datasets table picked up the bucket dataset · s12 zmetad sync_state
row present (bucket dataset fully polled) · s12 ?events on the dataset
bucket -> 200 · s12 ?events dataset field == the bucket's own dataset ·
s12 events DB rows carry the bucket dataset name · s12 re-create
bucket -> 200 (after manual dataset destroy) · s12 PUT object (pre-pin)
-> 200 · s12 DELETE object (bucket empty again) -> 204 · s12 pin
snapshot taken · s12 DELETE with snapshots -> 409 · s12 dataset STILL
exists after 409 refusal · s12 PUT dotted bucket zbd12.dot.bkt -> 200 ·
s12 dotted dataset <parent>/zbd12.dot.bkt resolves · s12 dotted bucket
object round-trip PUT -> 200 · s12 dotted bucket GET exact bytes · s12
dotted bucket object DELETE -> 204 · s12 no zbd12 dataset/snapshot
leaks under the scratch parent.

FAIL (10, single cascade from 12e): s12 DELETE empty dataset bucket ->
204 (got **409 BucketNotEmpty** despite the dataset containing only
`.metadata/`) · s12 dataset gone from zfs list after delete · s12
re-create bucket -> 200 (409 BucketAlreadyOwnedByYou cascade) · s12
409 body code BucketHasSnapshots · s12 409 body carries the snapshot
count (1 snapshot(s)) · s12 409 body names the dataset · s12 DELETE
after snapshot destroy -> 204 · s12 dataset gone after successful
delete · s12 DELETE dotted bucket -> 204 · s12 dotted dataset gone
after delete.

(Note: two of the section-12 names evolved between runs — run 1 had no
sync_state check and the leak check was counted inside the 32; run 3 is
33 checks. Names above are run 3, the recorded run.)

## Cross-repo observation (Task 3): events attribution — CONFIRMED

Events generated by writes to a bucket dataset carry the bucket's OWN
dataset name in the zmetad DB. Observed rows (run-2 diagnostics, live
DB, `SELECT dataset, path, event_type, full_path FROM events WHERE
dataset LIKE '%zbd12%'`):

```
dataset=testpool/zval/zbd12  path=.metadata            CREATE
dataset=testpool/zval/zbd12  path=obj.txt.tmp-9770…    CREATE
dataset=testpool/zval/zbd12  path=obj.txt              RENAME
dataset=testpool/zval/zbd12  path=obj.txt.meta.tmp-…   CREATE
dataset=testpool/zval/zbd12  path=obj.txt.meta         RENAME
dataset=testpool/zval/zbd12  path=obj.txt              REMOVE
dataset=testpool/zval/zbd12  path=obj.txt.meta         REMOVE
```

`datasets` table: `{'dataset': 'testpool/zval/zbd12', 'mountpoint':
'/testpool/zval/zbd12'}` alongside the parent row. The dotted bucket's
rows are equally well-attributed (`dataset=testpool/zval/zbd12.dot.bkt`).
This is the attribution win the design promised: per-bucket history is
segregated at the DB level with zero server-side mapping state. After
the sync_state poll guard landed (run 3), the WIRE envelope also
reported `dataset: testpool/zval/zbd12` with the bucket's events.

## FINDINGS

**F-zbd-1 (SERVER BUG, OPEN — blocks 12e–12g, the only live blocker):
DELETE of an empty dataset bucket returns 409 BucketNotEmpty when a
`?events` read on the bucket preceded it.**
Repro (live host, feature on, fresh dataset):
PUT bucket → 200; PUT obj → 200; GET obj → 200; DELETE obj → 204
(dataset verified to hold ONLY `.metadata/`); GET
`/zbd12/obj.txt?events` → 200; DELETE bucket → **409 BucketNotEmpty**
(server log: `Attempted to delete non-empty bucket: "zbd12"`), while
the dataset holds only `.metadata/`. The same sequence WITHOUT the
`?events` call deletes cleanly (204) — reproduced both in the harness
(run 3, checks 12e) and in isolated manual probes. Working hypothesis:
the `?events` path re-materializes a bucket registry/dir entry (or
resets an emptiness-tracking sidecar read) so the delete-time emptiness
walk sees a phantom entry; the sidecar dir itself is empty, so the
ghost entry is in-memory state, not a file. NOT patched (server code is
out of this leaf's scope per the leaf contract). Verbatim wire body:
`<Code>BucketNotEmpty</Code><Message>The bucket you tried to delete is
not empty.</Message>`; verbatim stderr line in zbd-server.log:
`Attempted to delete non-empty bucket: "zbd12"`. **Concern-split commit
candidate for the fix owner: the emptiness walk must ignore
provider/probe-induced state.**

**F-zbd-2 (SERVER BUG, OPEN — narrowed by harness fix, kept as a
finding): `?events` before the bucket dataset is fully polled resolves
to the PARENT dataset and is then positively cached.**
`ResolveDatasetByPath` is longest-mountpoint-prefix by design; while a
fresh child dataset has no `datasets` row yet, the parent's row
matches, so `?events` returns `200 {"dataset":"testpool/zval",
"events":[]}` — a silent wrong-dataset answer, not 503 — and
`probeDB` caches the positive (wrong) resolution for the process
lifetime. Harness mitigation landed (section 12 now waits for the
child's `sync_state` row before the first `?events`, within the
existing poll window), after which the wire `dataset` field is correct
(`testpool/zval/zbd12`). The server-side gap (positive-cache a
longest-prefix answer that can later be shadowed by a better prefix)
remains for the fix owner: re-resolve when a longer mountpoint appears,
or never positively cache a resolution that was not
`sync_state`-confirmed.

**F-zbd-3 (DEPLOYMENT REQUIREMENT, documented not worked around): on
this host only root can mount datasets, so `zfs_bucket_datasets`
servers must run with sufficient privilege to mount.**
Verified: unprivileged `zfs create` with `mount` delegated still yields
"may only be mounted by root"; the created dataset's mountpoint never
appears, so the server's `.metadata` mkdir inside it fails and bucket
creates 500. The harness therefore launches the phase-2 server under
`sudo -n -E` (the harness's zfs ops are already sudo-based). A README
note should state the requirement: the `zfs_bucket_datasets` server
needs effective root (or CAP_SYS_ADMIN + mount delegation) on the
parent subtree. Sibling note for leaf 04's docs: `zfs allow`-style
delegation alone is NOT sufficient on 2.4.99 hosts.

**F-zbd-4 (HARNESS, fixed in cycle 1): section ordering + pin
permissions.** (a) The zbd phase initially ran BEFORE the main sweep —
its starter kills all `zeta-server` pids and took the phase-1 server
down (checks.py then died with ConnectionRefused). Moved after the
sweep. (b) The phase-2 server runs as root, so ITS datasets are
root-owned and the harness's pin/destroy ops on them need `sudo zfs`
(consistent with section 10's existing `sudo zfs snapshot` pattern).

**F-zbd-5 (HARNESS, fixed in cycle 2): root-owned phase-2 server
survives unprivileged cleanup.** The phase-2 server's config rides in
`ZETAOBJECT_CONFIG` (invisible to `pkill -f`) and the process is
root-owned (unkillable by the ssh user): both prior runs leaked it, and
the leaked run-1 server then served run-2's section 12 (stale-state
noise). Now killed by pidfile (pid captured at spawn) plus a
sudo'd bracketed `pkill -f '[.]/zeta-serve[r]'`; the harness's final
cleanup runs the same sudo pkill BEFORE `zfs destroy -r` (a live
bucket-dataset server would resurrect datasets under the parent).
Run 3 teardown verified: no servers, `testpool/zval` destroyed, **no
e2e-pin snapshot leftovers** (`zfs list -t snapshot -r testpool/zval`
→ dataset does not exist).

**F-zbd-6 (HARNESS, fixed in cycle 3): RC aggregation.** The run's
exit code now folds `ZBD_RC` in (`VALIDATION FAILED` tallies both
sweeps); previously a section-12 failure could exit 0.

## Cleanup state (post-run, verified)

`pgrep -af 'zeta-serve[r]'` → no matches. `zfs list -H -o name -r
testpool/zval` → dataset does not exist (harness destroy succeeded).
`zfs list -t snapshot -r testpool/zval` → no e2e-pin leftovers. The
host's other datasets (actchk2, dbg, fpx, fs1, g4, ordcap, order-test,
rt, rt2, sig) were untouched.

## Deviations from spec

1. The 409-BucketHasSnapshots body checks (12f) never exercised the
   real refusal this run because 12e's delete fails first (F-zbd-1);
   the pin itself IS taken (s12 pin snapshot taken PASS) and the
   "dataset STILL exists after 409 refusal" check passes. The 409
   BODY shape remains covered by e2e case 34e only.
2. checks.py was not kept byte-identical to the letter: probe.py (the
   embedded client) was refactored (`sign()` → `_request(port,...)` +
   `sign_on()`); checks.py's own section bodies are untouched and
   section 0–11 results are unchanged (91/91).
3. Doc filename: `docs/validation-zfs-bucket-datasets-2026-10-06.md`
   is what leaf 04 cites, but the run date is 2026-10-03, so this file
   is named `...-2026-10-03.md` per the leaf's "use the date you
   actually ran" rule. **The cross-links in leaf 04's committed docs
   need a one-line fix to point here.**


## Final run (same day, after the F-zbd-1 fix landed)

The fix commit (`fix(s3): .zfs control dir no longer blocks DeleteBucket
on dataset buckets`) landed between runs; the harness rebuilt and the
full sweep was re-run:

| Sweep | Tally |
|---|---|
| Sections 0–11 (pre-existing) | **91/91 PASS** |
| Section 12 (zfs_bucket_datasets) | **33/33 PASS** |
| Run total | **124/124 PASS — exit 0** |

Post-run host state: no zeta-server/zmetad processes, no `testpool/zval`
scratch dataset, no e2e-pin snapshot leftovers. Events attribution
re-confirmed green (`s12 events DB rows carry the bucket dataset name`).
