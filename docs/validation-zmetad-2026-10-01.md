# Live Validation: zeta-object + zmetad on zfs-meta (2026-10-01)

Host: zfs-meta (Ubuntu 24.04, kernel 6.8, OpenZFS 2.4.1-1 with the
extended-metadata branch at `e5f75679e`: `events`/`events_size` dataset
properties, ring GUID, layout-6 zmetad). Deployment: linux/amd64 static
binary (repo HEAD `c4cc478` + the version-range fix) at `~/zeta-validate/`,
HTTPS :9707, `dataDir: /testpool/` (ZFS-backed), zmetad polling at 2s into
`~/zeta-validate/zmetad.db`.

Harness: `scripts/zfs-validate/run-zfs-validation.sh` — rewritten this run
for the zmetad path (the CLI `zfs events -j` transport no longer exists in
the server). **Result: 35/35 PASS.**

## PASS matrix (all over the real SigV4 wire)

| Area | Result |
|---|---|
| Deployment | Static linux binary runs; config loads (zmetad_db_path + zmetad_binary); TLS on :9707; zmetad co-resident, poll 2s |
| zmetad DB live | db_schema_version=6 accepted (consumer range 5..6); datasets/sync_state track the scratch dataset |
| Core S3 on ZFS bucket | PUT/GET/DELETE/HEAD round-trip, custom x-amz-meta, multipart (5MB+1MB) complete + GET integrity |
| ?events vs DB ground truth | Server events match the zmetad SQLite rows per-record; multipart rename (tmp→final) semantics preserved (F-live-4) |
| full_path fidelity (v5) | Server keys equal DB `full_path` verbatim; nested keys `edge/deep/leaf.txt` and `a1/a2/leaf2.txt` resolve EXACT — the F-live-1 failure mode is gone on the zmetad path (insert-time resolution, no window dependence) |
| Envelope contract | JSON keys exactly {dataset, recordsLost, ringSwaps, events}; ext XML carries IsLossy/RecordsLost |
| Loss accounting | wire recordsLost == gaps knownLost (SUM lost>0); wire ringSwaps == gaps -1 count; never folded (SCHEMA.md §4) |
| Freshness | SIGUSR1 forced collect makes a just-written event visible (SCHEMA.md §8); pre-collect lag is the documented poll bound |
| events=off | 503 NotImplemented — zmetad prunes untracked datasets rows (prune_stale_datasets), path resolution fails, probe degrades. SAME client-visible semantics as the old CLI path |
| events=on | Service restored on the next poll; capture resumes; writes during the off window are never captured (verified via DB count) |
| Bucket integrity | PUT '.' rejected (mux 307 canonicalize); no stray objects on the dataset |

## FINDINGS

**F-zmetad-1 (FIXED this run, mini-s3): exact-version gate 503'd everything.**
The leaf-01 accessor required `db_schema_version == 5` exactly. Upstream
`276fe5085` shipped layout 6 (additive: `sync_state.last_lost`,
`meta.purge_epoch` — neither read by this consumer), so every `?events`
request 503'd on the live host while all unit tests (fixture-pinned at 5)
passed. Fix: accept the additive range `[zmetadMinDBSchemaVersion=5,
zmetadMaxDBSchemaVersion=6]`, refuse newer (SCHEMA.md §1 evolution rule is
additive-only, so a range is the correct consumer posture). Tests updated
(v6 accepted, v7 refused). Lesson: pin the MINIMUM functional layout, not
an exact version, when the upstream contract promises additive evolution.

**F-zmetad-2 (harness, fixed): pkill self-match killed the ssh command
channel.** `pkill -f 'zmetad.*zeta-validate'` matched the remote
`bash -c` process running the pkill itself (its cmdline contains the
pattern), killing the session mid-script (exit 255, silent). Fixed with the
bracket trick (`zmeta[d]`) in all harness pkill sites; the server pattern
uses `[.]/zeta-serve[r]` for the same reason.

**F-zmetad-3 (INFO, semantics confirmed): events=off prunes tracking.**
zmetad skips events=off datasets during collection and prunes `datasets`
rows not refreshed in the cycle, so path resolution fails and the provider
degrades to 503 — identical client-visible behavior to the deleted CLI
path. The earlier plan assumption ("durable DB keeps serving history while
capture is off") is WRONG against current upstream; the harness check was
corrected to pin the actual semantics (503 during off, 200 after on).
Note: history ROWS survive in the DB (retention governs), only the
mountpoint tracking disappears; if upstream later keeps rows queryable
without the datasets row, this check should flip.

**F-zmetad-4 (INFO): ring_guid observed live.** sync_state carries non-null
ring_guid for freshly polled datasets (e.g. testpool/zval), NULL for rows
that predate the GUID stamp — matching SCHEMA.md §6 "identity unknown"
semantics. No consumer action needed (swap detection is zmetad's job;
consumers read the gaps -1 sentinel).

**F-zmetad-5 (DECIDED 2026-10-01): purge stays operator-only.** The
provider's Purge (execs `zmetad --purge <dataset>`) has no HTTP route and
deliberately keeps it that way: purge destroys audit-flavored data (event
history + the permanent gap record), and object-write credentials should
not grant erasure of that history. Operators purge on the host directly.
If an admin-tier grant lands later, an authenticated purge endpoint gated
on that tier is the right shape. README "Purge (operator-only...)"
documents the decision; the earlier README claim that purge was "reachable
only through authenticated request paths" was wrong (no route existed) and
was corrected.

## Cleanup

Server + zmetad stopped, `testpool/zval` destroyed (harness auto-cleanup).
The host's other datasets (rt2, g4, sig, ...) are prior-session scratch and
were left untouched; zmetad was left NOT running (was inactive before this
validation).
