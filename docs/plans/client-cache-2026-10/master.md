# Client cache (zeta-cache) - FUSE sync daemon + status GUI

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 8 leaf documents (flat)
- **Scope:** zeta-cache - the two-way sync cache daemon that makes a remote
  zeta-object bucket look like local disk on a client device. Lives in the
  repo subdirectory `zeta-cache/` with its OWN Go module. Speaks WebDAV to
  the gateway (TCP or HTTP/3), keeps all cache/eviction/sync state on the
  client (charter-clean), and ships with a macOS-first menu-bar GUI.

## User decisions (FINAL - ask before reopening)

1. **Name and home.** The client tool is **zeta-cache** (never
   "zeta-sync"), a subdirectory of THIS repo (`zeta-cache/`, its own Go
   module) - the repo root stays clean. (quic-h3-2026-10 master
   decision 6.)
2. **Platform scope v1:** FUSE-only on Linux (go-fuse) and macOS
   (macFUSE). The iCloud-grade macOS FileProvider extension is FUTURE
   work tracked in `docs/plans/issue-fileprovider-macos-client.md`
   (zeta-object issue #14) - not a leaf here.
3. **Client protocol is WebDAV, not S3.** Real directories + true mtimes
   from the backing fs, MOVE is one atomic metadata op, webdav locking
   ships. S3 stays the interop surface (rclone/mc/boto3).
4. **Transport:** plain HTTPS (HTTP/1.1/2) for v1; the client MAY upgrade
   to HTTP/3 when the server advertises `alt-svc: h3=...` (leaf 02 of
   quic-h3-2026-10). h3 auth is mTLS only - there is no HTTP 401 over
   h3; a missing/wrong cert fails the TLS handshake. The client must
   implement BOTH auth paths: Basic for TCP, per-device client cert for
   h3.
5. **Two-way sync engine, not just a cache** (locked design shape in
   `references/client-cache-quic.md`): PROPFIND diff vs the index +
   append-only sync journal (client-side) + conflict-copy on divergence.
   Writes use If-Match (server enforces, 412 on stale ETag; MOVE
   enforces it on the destination).
6. **All cache/eviction/sync state is CLIENT-owned.** The server stores
   nothing about who cached what (gateway charter). The client-side
   SQLite index (modernc.org/sqlite - cgo banned repo-wide) + sync
   journal live at the per-OS app-data path.
7. **ZFS metadata is enrichment, never load-bearing.** The event cursor
   (zeta-object#15, upstream zfs-metadata#17) is an OPTIMIZATION; the
   ETag-diff PROPFIND scan is the universal path that must work on
   plain-dir buckets. No zeta-cache feature may REQUIRE zfs-metadata.
8. **Collection tokens are a skip hint, never a correctness input**
   (shipped: webdav `getetag` on collections, `dir-<hex>` derived token,
   e2e 19 part 19g). A token that says "unchanged" lets the client skip
   a subtree; every conflict decision still uses file ETags / If-Match.
9. **Eviction only ever removes CLEAN (uploaded, ETag-confirmed) files.**
   Dirty files always stay local. Quota pressure never fails a
   hydration; if the working set exceeds the cap, keep files and report
   the number in the GUI.
10. **GUI:** macOS-first menu-bar app (sync state, conflicts,
    pause/resume, usage vs quota, pins, recently-deleted restore); Linux
    tray app after the daemon stabilizes. The daemon owns all state; the
    GUI is a view over a local IPC socket.

## Server facts this tree builds on (parent-verified 2026-10-06)

All SHIPPED and stable - grep before relying on exact line numbers:

- WebDAV Range GET (206 + Content-Range, 416 semantics) - quic-h3
  leaf 01.
- HTTP/3 transport wrapping webdav, Alt-Svc discovery, mTLS cert auth -
  leaf 02; e2e 38.
- webdav versioning capture (PUT/DELETE join zfs_versioning) - leaf 05.
- `?events` / `?versions` JSON over webdav - leaf 06.
- JSON `?batch` (delete) + S3 DeleteObjects on both frontends - leaf 07.
- webdav locking (LOCK/UNLOCK, If-token, 423 enforcement) - e2e 31.
- Collection change tokens (`getetag` = `dir-<hex>` immediate-children
  token) - colltoken.go, e2e 19g.
- If-Match/If-None-Match enforced on webdav PUT (412) and MOVE
  destination.
- ETag = MD5 of body stored in the sidecar; Stat does NOT re-hash.
  **Out-of-band writes to the backing fs do NOT move the ETag** -
  the client's conflict detection assumes no out-of-band writes.
  Load-bearing, documented here; the client journal is what recovers
  from a crashed write.
- webdav PUT rejects chunked bodies (400) - the client always sends
  Content-Length. **No resumable upload server-side; v1 re-PUTs the
  whole file on failure** (decision: resumable upload is a future
  server capability, not a client workaround).

## Concurrency / file ownership (dispatch accordingly)

- `zeta-cache/go.mod` + `main.go` + `internal/` skeleton: leaf 01 owns.
- FUSE filesystem layer: leaf 02.
- Sync engine + index + journal: leaves 03, 04, 05 (03 owns index.go,
  04 owns scan.go, 05 owns cursor.go - disjoint).
- Transport/auth: leaf 06.
- Quota/eviction + scheduler: leaf 07 (disjoint from 03-05 files).
- GUI + IPC: leaf 08 (owns gui/ + ipc/).
- Every leaf documents its own paths; two leaves sharing a file go in
  different dispatch waves.

## Leaves

Flat, numbered, one file each. Read the leaf before dispatching it.

1. `01-skeleton-module.md` - `zeta-cache/` Go module skeleton: module
   decl, main.go (daemon lifecycle: mount, sync loop, IPC socket,
   signal handling), internal package layout, config file format, e2e
   harness extension (a `zeta-cache` e2e case shell), CI wiring.
2. `02-fuse-layer.md` - FUSE mount serving the local cache dir:
   lookup/getattr/readdir/read/write/flush/fsync mapping onto the cache
   + daemon, hydration on read miss (Range-aware), write-through
   staging, dirty tracking, mount lifecycle rules (mount anyway when
   server unreachable; unmount with dirty files = block and flush or
   refuse with the reason).
3. `03-index-journal.md` - client SQLite index (modernc): resources
   table (path, etag, mtime, size, hydrated, dirty), append-only
   journal, WAL mode; purge/eviction = one SQL query; daemon-start
   reconcile pass (journal rows vs disk; upload or discard).
4. `04-sync-scan.md` - the universal ETag-diff PROPFIND scan: full-tree
   walk using collection tokens to skip unchanged subtrees, diff vs
   index, conflict rules (clean/changed/dirty matrix from the reference
   doc), conflict-copy naming, remote-delete vs dirty-local = keep
   local + surface, upload with If-Match, MOVE atomicity, batch delete
   via JSON ?batch.
5. `05-event-cursor.md` - the OPTIMIZATION path: when the bucket
   capabilities say ZFS (via a probe; zeta-object#15 lands the
   `since-id` pass-through), track last-seen event id per bucket,
   reconnect with `?events&since-id=N`, fall back to the leaf-04 scan
   on provider 503, gap counters (recordsLost/ringSwaps) trigger full
   rescan. Built and shipped ONLY AFTER zeta-object#15 lands.
6. `06-transport-auth.md` - HTTP client: Basic over TCP; alt-Svc
   discovery + http3 + per-device mTLS cert (CN = device name,
   keychain/keyring storage from v1); h3 negative path = handshake
   failure handling; Range GET hydration; If-Match PUT; locking usage
   for multi-machine coherence.
7. `07-quota-eviction-scheduler.md` - the three daemon loops (prompt
   upload via FSEvents/inotify ~2s debounce; sync on connect + ~15min
   backoff+jitter; eviction nightly + at high-water), quota enforcer
   (high ~90% / low ~70% / min-free hard stop; clean-only eviction;
   pinned files hard-filter), battery-aware deferral on macOS.
8. `08-gui-ipc.md` - local IPC socket protocol (daemon owns state, GUI
   is a view), macOS menu-bar app (pause/resume, conflicts resolve,
   usage, pins, recently-deleted restore), Linux tray app deferred.

## Dispatch order

1 (skeleton, everything depends on it) -> 2 (FUSE) + 3 (index/journal)
concurrent after 1 -> 4 (sync-scan, needs 2+3) -> 5 (cursor, needs 4;
ALSO blocked on zeta-object#15 landing) -> 6 (transport) + 7
(quota/scheduler) concurrent after 4 -> 8 (GUI) last.

Leaf 05 was explicitly CONDITIONAL: zeta-object#15 (the since-id
pass-through) has now SHIPPED — the leaf is unparked (READY; see the
tracking table). Scan-only remains the behavior on plain-dir buckets
(decision 7).

## Acceptance (whole tree)

- e2e: a `zeta-cache` e2e case exercising mount -> write -> sync ->
  evict -> re-hydrate -> conflict -> resolve against the real server
  binary (graceful skip when macFUSE/fuse is absent, with the manual
  mount procedure documented like e2e 19's Finder/mount_webdav block).
- Race detector clean on the sync engine's tests.
- Charter audit: `git grep` the zeta-cache module for server-visible
  state (nothing client-owned may be sent to the server beyond standard
  protocol fields).
- Docs: zeta-cache README (mount quickstart, config reference),
  protocol-compatibility.md row for the client, and a pointer from the
  main README.

## Tracking table

| Leaf | Title | Status | Commit | Gate evidence |
|---|---|---|---|---|
| 01 | Module skeleton + daemon lifecycle | DONE | bda8c8e | e2e 40: 19 asserts; make e2e 900/0; race green; lint 0 |
| 02 | FUSE filesystem layer | DONE | 84b21fb | race green; lint 0; case 40 mount skip on FUSE-less hosts; live-mount activates with leaf 06 |
| 03 | Index DB + sync journal | DONE | 84b21fb | race green (WAL concurrency, reconcile, crash-sim); lint 0 |
| 04 | Sync engine (scan path) | DONE | b221669 (+a757d01 adapter, c7ffe92 empty-bucket) | race green all 5 pkgs; lint 0; 12 sync tests incl. full matrix + empty-bucket |
| 05 | Event cursor (CONDITIONAL on #15) | READY (code complete, unblocked by #15 — awaiting review/commit) | - | gateway #15 shipped: since-id pass-through on both frontends (e2e 18: 35 asserts); zeta-cache cursor ChangeFeed + transport Events seam (stub tests: redelivery idempotence, loss invalidation, 503 scan-only); race green all pkgs; lint 0 both modules |
| 06 | Transport + auth | DONE | ad9fd73 (+fixes) | race green; lint 0; case 40 23/23; h3 mTLS sync asserted |
| 07 | Scheduler + quota + eviction | DONE | ad9fd73 (+fixes) | race green; watermark/pin/battery tests; lint 0 |
| 08 | GUI + IPC | DONE | 27595d0 | swift build+test 11 conformance; Go race green; golden fixtures 16; case 40 23/23 |

Dispatch order: 1 -> (2 + 3) -> 4 -> 5/6/7 -> 8. Leaf 05 stays parked
until zeta-object#15 lands; v1 ships scan-only.

## Tree closure (2026-10-08)

- All leaves except 05 are DONE; 05 stays PARKED-PREREQ by decision 7
  (the cursor unblocks when gateway issue #15 lands).
- Final CI: run for 27595d0 - all jobs green (build incl. the
  zeta-cache module step, lint incl. the module's golangci-lint, e2e
  900+/0 with case 40's LIVE FUSE mount round-trip on the CI runner,
  27 test-matrix jobs).
- Integration bugs the live-FUSE CI runner caught that local testing
  could not: the fusefs adapter's invalid journal ops (every FUSE write
  silently rolled back - adapter.go, fixed with index constants +
  adapter_test.go), the empty-bucket root PROPFIND 404 (sync aborted on
  fresh buckets - engine.go root+ErrNotExist rule), the IPC re-Serve
  deleting the live socket (ReplaceHandler now swaps the handler on the
  live listener), and the FUSE availability probe (fusermount3, not
  just /dev/fuse).
- Charter audit: zeta-cache stores nothing server-side beyond standard
  webdav protocol fields; all index/journal/conflict state is
  client-owned (zeta-cache/internal/index).

## Open questions (not blocking leaves 1-4)

- GUI tech (Swift/SwiftUI vs Go+fyne for the menu bar) - decide at
  leaf 08 dispatch time; the IPC protocol is GUI-agnostic.
- Windows support: out of scope v1 (WinFSP is a future evaluation).
- Whether the eviction policy engine is shared with a future
  FileProvider extension (issue #14 open question) - the policy core
  should be an internal package with no FUSE dependency so it CAN be
  shared.

## Tracking

- Server-side prerequisite: zeta-object#15 (event cursor pass-through)
  - leaf 05 blocked on it.
- Upstream contract: zfs-metadata#17.
- Design record: `references/client-cache-quic.md` (the user-locked
  decisions live there and in this file; this tree is the implementation
  plan).
- Future work: FileProvider extension (issue #14), resumable upload
  (server-side), Windows client.
