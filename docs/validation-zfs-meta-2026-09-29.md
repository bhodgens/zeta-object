# Live Validation: mini-s3 + ZFS events on zfs-meta (2026-09-29)

Host: zfs-meta (Ubuntu 24.04, kernel 6.8, OpenZFS 2.4.1-1 with the
extended-metadata branch: `events` + `events_size` dataset properties,
pool feature `feature@events`). Deployment: linux/amd64 static binary at
`~/s3test/`, HTTPS :9443, `dataDir: /testpool/` (ZFS-backed).

## PASS matrix (all over real SigV4 wire, not unit doubles)

| Area | Result |
|---|---|
| Deployment | Static linux binary runs; config loads; fs backend registers; TLS on :9443 |
| Core S3 on ZFS bucket | PUT/GET/DELETE/HEAD, multipart (5MB+1MB, complete, GET integrity), 8MB md5 round-trip, delete-idempotent 204 |
| Custom metadata | x-amz-meta-* survives to sidecar (original casing) |
| Provider attach | `?events` returns 200 JSON with `dataset: testpool/...` on events=on buckets |
| Event fidelity | mini-s3 events == `zfs events -j` ground truth EXACTLY (3248 events, 19638 lost, per-record match) after wraparound |
| recordsLost | Plaintext trailer parsed and surfaced; `IsLossy`/`RecordsLost` in ext XML |
| events=off | Clean 503 NotImplemented; flipping back on restores service (probe-per-request works) |
| Wire hardening | unsigned 403, unknown bucket 404 NoSuchBucket, POST ?events 405, HEAD passes through, max-events cap + junk tolerated, no uid/gid on wire, dataset name only (no absolute paths) |
| Routing precedence | ?events&versions resolves to extension; plain ?versions untouched |
| events -c | Clear via CLI works; mini-s3 sees fresh log immediately (probe-per-request) |

## FINDINGS

**F-live-1 (HIGH, mini-s3): nested-key event history is empty.**
`GET /bucket/edge/deep.txt?events` returns `[]` while the ZFS log records
the CREATE (name=deep.txt, parent=<objid of edge>). Root cause:
`internal/metadata/zfs_events.go:158` sets `e.Key = *r.Name` (the ZFS
bare name); `History` filters `e.Key == key` against the FULL S3 key, so
events for keys under any prefix never match. Affects every non-root
key. Fix design: reconstruct paths from the event graph - the log emits
`object` + `parent` object ids on every record, so objid->name resolution
yields full paths without upstream changes (verify print_event emits
object id for dirs; confirmed in zfs_main.c:8340).

**F-live-2 (HIGH, zfs-metadata KERNEL branch): event log delivery freezes
after heavy wraparound.** On a 128K ring after ~30K events: `records lost`
counter kept growing (19638 -> 23499 across minutes) while NO new records
became readable - newest visible event stayed txg 31962 even as writes
( multipart complete, adhoc files) landed on the dataset. `zfs events -c`
(clear) restored normal operation immediately. mini-s3 handled the frozen
state correctly (honest IsLossy=true + growing RecordsLost), so this is
kernel-side: suspected wrap/realloc state bug in the ring-buffer hardening
(zfs_events.c). Repro: events_size=128K, generate >20K events rapidly,
query repeatedly while writing.

**F-live-3 (INFO, design confirmation): object-level ?events&versions**
returns object event JSON (not the ext XML) - the versions-extension
surface is bucket-level only, per the leaf-04 plan. Tested intentionally;
not a bug. Worth documenting in the README endpoint list.

**F-live-4 (INFO, semantics): multipart final object produces exactly one
create/rename pair** (tmp assembly file + rename inside the dataset) - the
wire-visible history shows the atomic-finalize design working on ZFS.

## Cleanup

Test datasets destroyed (s3test, s3fresh, s3noev). Server stopped; binary,
certs, and config left at zfs-meta:~/s3test/ for reuse.
