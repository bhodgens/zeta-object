# Leaf 05 - event cursor (CONDITIONAL: blocked on zeta-object#15)

## Status

**CONDITIONAL LEAF.** Blocked on gateway issue zeta-object#15 (the
`since-id` pass-through on `?events`, both frontends) being LANDED. If
#15 is not merged when leaf 04 closes, PARK this leaf (mark it parked in
the master tracking table with a one-line reason) and ship v1 scan-only
- decision 7 makes the cursor an optimization; v1 is complete without it.

## Goal

Replace the always-fullScan ChangeFeed default (leaf 04's interface
point) with a zmetad-backed cursor: reconnect in O(changes) by asking
the server for events since the last-seen id.

## Requirements

- Capability discovery FIRST: the engine must not assume ZFS. Probe via
  the events surface (GET `<bucket>?events` over webdav): 503
  NotImplemented = plain bucket -> cursor disabled, scan-only (this is
  the permanent behavior for non-ZFS buckets); 200 with an envelope =
  cursor available. The probe result is cached per bucket with a
  re-probe interval (server config can change).
- Cursor state: `meta` key per bucket `cursor:<bucket>` = last-seen
  event id (the monotonic `id` from the events JSON - EXACT field name
  TBD by #15's implementation; if the wire form carries no id, this
  leaf is BLOCKED and stays parked - a timestamp cursor is FORBIDDEN:
  gethrtime semantics reset on reboot, second-resolution captured_at
  loses same-second events; see zeta-object#15 rationale).
- Delta(ctx): fetch `?events&since-id=N` (max-events bounded), map
  event keys -> changed paths (op create/rename/remove/truncate;
  rename yields BOTH old and new path), feed leaf 04's diff engine with
  changedPaths + fullScan=false. Every event PAGE advances the stored
  cursor ONLY after the diff-apply for that page commits (at-least-once
  redelivery on crash is correct; at-most-once is not).
- Loss honesty: envelope `recordsLost`/`ringSwaps` counters - if either
  advances vs the last-seen values, the cursor is INVALIDATED: full
  rescan, reset cursor to the newest id AFTER the rescan completes.
  Ring wrap = the ring is a bounded kernel buffer; offline-past-
  retention MUST fall back to a full scan (document in the package doc).
- Events are the change DETECTION optimization only: conflict detection
  stays file-ETag/If-Match based (leaf 04 rules) - events never decide
  conflicts.
- Multi-device: cursors are per-device (client-owned). Another device's
  writes appear as events; ours appear too (self-events filtered by
  comparing event principal/timestamp vs journal upload-ok rows where
  possible; when not distinguishable, redelivery is harmless - the diff
  engine sees unchanged ETags and no-ops).
- Tests: stub transport serving a canned events JSON - delta feeding,
  cursor persistence across restart, loss-counter invalidation -> full
  scan, 503 -> permanent scan-only, redelivery idempotence (same event
  twice = one diff action).

## Acceptance

1. `go test -race ./internal/sync/` green including the new cursor table
   tests.
2. The scan-only path still green (regression: cursor disabled must
   behave EXACTLY as leaf 04).
3. Report: the events-JSON field names consumed (with a pointer to #15's
   implementation), the invalidation rules, and the probe caching
   behavior.
4. e2e: extend case 40's sync section (FUSE-gated) with a cursor-path
   exercise IF the test server runs ZFS; otherwise the stub tests carry
   it - state which in the report.
