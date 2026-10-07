# Leaf 07 - scheduler, quota enforcer, eviction

## Goal

The three daemon loops and the quota/eviction policy engine: prompt
upload, periodic sync, eviction. All timing, backoff, and policy
decisions live here; the mechanisms they drive live in leaves 02/04.

## Requirements

- Scheduler (three loops, all ctx-cancellable, all jittered):
  - Prompt upload: filesystem events (FSEvents on macOS, inotify on
    linux - both via a pure-Go lib, no cgo) on the cache/staging dirs,
    ~2s debounce, triggers the upload path for dirty paths. Events that
    arrive while a sync is running are coalesced (no queue explosion).
  - Sync: on connect (immediately after a successful first probe) +
    every ~15min with exponential backoff + jitter on failures (cap the
    backoff at 1h; every failure logs and surfaces in status.state).
    SyncOnce from leaf 04.
  - Eviction: nightly + immediately when the high-water mark is
    crossed; macOS: defer when on battery AND not plugged in (battery
    status via a small pure-Go probe or IOKit bind - no cgo; if IOKit
    is cgo, poll `pmset -g batt` output instead and say so).
- Quota enforcer (locked policy): cache cap configured in bytes;
  high-water ~90% starts eviction, low-water ~70% is the eviction
  target, min-free-device-space is a HARD STOP (never evict below it -
  actually: never WRITE below it; eviction still runs). Policy knobs in
  config: `policy: size | size+age | lru`, `maxCacheBytes`,
  `minFreeDeviceBytes`.
- Eviction (locked rules): candidates = hydrated AND clean AND NOT
  pinned, from ONE SQL query (leaf 03's parameterized query). Order by
  the policy (size: biggest first; size+age: biggest-and-oldest score;
  lru: least-recently-accessed - requires an atime-ish column updated
  on hydration/read: add `lastAccess` in this leaf). Evict = delete the
  cache file (temp-safe: the file is clean, the remote copy is
  authoritative) + index update (hydrated=0) + journal row. NEVER evict
  dirty files. If the working set exceeds the cap: evict what qualifies,
  keep the rest, REPORT the overflow count via IPC status - eviction
  never fails a hydration.
- Deletion grace table: tombstoned paths (remote-deleted, local-clean)
  are retained as index rows for ~30 days (config) so the GUI can offer
  restore (the file content is gone unless still cached - restore =
  re-download if the server still has it via ?versions or a snapshot;
  if unrecoverable, the GUI says so honestly). A periodic pass prunes
  expired tombstones.
- Pin lists: config file `pins: [path...]` + IPC toggle; pinned paths
  are hard-filtered from eviction (the SQL query's NOT-pinned term) and
  always kept hydrated (hydration trigger when a pin is added).
- Interaction rules: sync running + eviction fires -> eviction proceeds
  (it only touches clean files); prompt-upload firing during sync ->
  coalesce; server unreachable -> upload retries back off, eviction
  continues (local-only operation), sync state shows error.

## Constraints

- No new server interaction beyond leaf 04/06 operations (eviction is
  local-only; hydration is a Get).
- Battery probing must not be cgo; a shelling-out impl is acceptable,
  documented.
- All loops must be race-clean and ctx-cancel-responsive (tests assert
  shutdown latency < 2s).

## Acceptance

1. `go test -race ./internal/scheduler/ ./internal/quota/` green:
   backoff/jitter math table tests, high/low-water transitions,
   clean-only eviction (dirty file survives eviction in every policy),
   pin filter, min-free hard stop, tombstone expiry, battery-defer
   (injected battery probe).
2. Deterministic clock: inject the clock; no real-time sleeps in tests.
3. Report: the state machine (loop x event -> action), the eviction
   SQL, and the config keys added to the example config.
