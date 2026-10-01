# Config, Wiring, and CLI-Transport Removal - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Wire the zmetad provider into startup config + s3_wiring (two
  new config keys); add the additive wire fields (ringSwaps/RingSwaps);
  delete the CLI `zfs events` transport; update README.
- **Dependencies:** 03-provider.md.
- **Estimated Context:** ~40K (explore 12K + generate 14K + iterate 8K + overhead 6K)
- **Concurrency Group:** D

## Goal

Flip production to zmetad: config carries the DB path and the purge-binary
path, s3_wiring registers `NewZmetadEventsProvider`, the `?events` envelope
gains `ringSwaps` (JSON) / `RingSwaps` (XML) additively, and the CLI-based
`NewZFSEventsProvider` + its `zfs events -j`/`zfs events -c` transport are
deleted. `zfs_cmd.go` ResolveDataset and the fsdetect files: ResolveDataset
loses its last caller when zfs_events.go dies - delete it AND its tests in
this leaf (grep-verify no remaining callers first); fsdetect stays (Probe
uses it). The frozen interface file (metadata.go) stays untouched.

This leaf touches several files but each change is small and mechanical;
the risk is missing a call site, so the task list is delete-driven.

## Context

- main.go:22-26 blank-imports internal/metadata so the provider registers.
- s3_wiring.go:113-127: `metadata.Register(metadata.NewZFSEventsProvider())`
  + InstallMetadataProvider shim (per-request Probe). The shim logic is
  CORRECT for zmetad too - only the constructor changes.
- main_test.go, s3_wiring_test.go:15-20 reference
  `metadata.Lookup("zfs-events")` - keep working (name unchanged).
- capability_endpoints.go: ObjectEventHistory envelope
  (`dataset`,`recordsLost`,`events`) and ListObjectVersionsExt XML
  (`IsLossy`,`RecordsLost`) - ADD `ringSwaps` / `RingSwaps` (Contract 5).
  historyDetailFor (line ~101): its LastHistoryDetail fallback dies with
  the package global in task 3 - after deletion the fallback returns a
  zero HistoryDetail for providers without LastDetail (update the
  affected tests; the structural interface stays the primary path).
- config: find where config.json is parsed (search json.Unmarshal /
  dataDir in main.go) and add both keys there.
- README.md documents the ?events endpoints (search `?events`).
- Defaults: `zmetad_db_path` = `/var/lib/zfs/zmetad.db`,
  `zmetad_binary` = `zmetad` (SCHEMA.md / zmetad.h:28; PATH default per
  upstream install). Defaults set at config load - one place owns them.

## Interface Contracts (From Parent)

Contract 4 of master.md:

```go
// config.json (optional, defaults shown)
"zmetad_db_path": "/var/lib/zfs/zmetad.db"
"zmetad_binary":  "zmetad"
// main.go config struct:
ZmetadDBPath string `json:"zmetad_db_path"`
ZmetadBinary string `json:"zmetad_binary"`
// s3_wiring.go:
metadata.Register(metadata.NewZmetadEventsProvider(cfg.ZmetadDBPath))
```

Contract 5 (wire): envelope keys `dataset`,`recordsLost`,`ringSwaps`,
`events`; XML `IsLossy`,`RecordsLost`,`RingSwaps`; IsLossy =
recordsLost > 0 OR ringSwaps > 0; per-event `timestamp` renders from
captured_at (via the provider, leaf 02/03) - no frontend change needed
for that part.

## Tasks

### 1. Config keys

1. Test: both keys parsed when present; absent -> both defaults (set at
   config-load). Extend the existing config-parse test.
2. Run (fail), implement, run (pass). Wire cfg.ZmetadBinary through
   s3_wiring into the provider's binary field (leaf 03 documented the
   pass-through choice - follow whatever leaf 03 landed, or set it via
   the documented setter; report deviation if leaf 03's shape differs).

### 2. Swap the constructor in s3_wiring

1. Update s3_wiring.go:113 to register `NewZmetadEventsProvider(cfg.ZmetadDBPath)`.
   Trace how s3_wiring receives config today (dataDir flow through
   s3_wiring.go and main.go) and pass the new fields the same way.
   Update s3_wiring_test.go's fake-provider registration accordingly.
2. Additive wire fields: ObjectEventHistory gains
   `RingSwaps uint64 `json:"ringSwaps"``; ListObjectVersionsExt gains
   `RingSwaps uint64 `xml:"RingSwaps"``; both populated from
   historyDetailFor; IsLossy updated per Contract 5. Update/extend the
   capability-endpoint tests (17-events case assertions live in e2e -
   unit tests here pin the JSON/XML shapes).
3. `go build ./... && go test ./...` green.

### 3. Delete the CLI transport

1. Delete from zfs_events.go: `zfsEventsProvider` and methods,
   `NewZFSEventsProvider`, `zfsRunner`, `runZFS`, `zfsExecSem`,
   `parseEventsOutput`, `parseEventSet`, `rawEvent`,
   `reconstructPaths`/`resolveRecordAt`/`mapAt`/`anyObjIDMappedTwice`,
   `detectRoot`, `resolvePath`, `maxPathDepth`, `objEntry`, `res`,
   `eventSet`, `eventMatchesKey` - the v5 full_path mapper (leaf 02)
   replaces ALL graph-walk machinery; grep each symbol first (leaf 02
   reuses ONLY bareMatchesKey and plausibleWallClockTime - those STAY),
   `LastHistoryDetail`/`setHistoryDetail`/`lastDetail`/
   `detailMu` package global (after updating historyDetailFor per
   Context). NOTE: leaf 02's drift-guard test consumes parseEventsOutput
   and the testdata fixtures - keep parseEventsOutput/parseEventSet/rawEvent
   ALIVE if that test still references them; grep decides, report which
   side won.
2. Delete `ResolveDataset` + `parseZfsGetNameMountpoint` +
   `DatasetMountMismatchError` + `ZFSBinaryMissingError` from zfs_cmd.go
   AND their tests (zfs_cmd_test.go) - grep-verify zero remaining
   callers. KEEP mountpointContains (leaf 01's accessor uses it) and the
   fsdetect_* files (Probe uses DetectZFS). If mountpointContains is
   only used by accessor tests, move it to zmetad_db.go (same package,
   no import churn) - implementer's choice, report it.
3. Delete CLI-provider tests that lose their subject: zfs_events_live_test.go,
   zfs_events_test.go fixtures retained ONLY if leaf 02's cross-check
   still consumes them (it should - testdata stays); zfs_integration_test.go
   port or delete per symbol grep.
4. `go build ./...` clean; `rg -n 'zfsRunner|NewZFSEventsProvider|runZFS|
   ResolveDataset|zfsExecSem' --glob '*.go'` returns only leaf-01/02/03
   references or nothing.
5. `rg -c 'zfs events' internal/ --glob '*.go'` == 0 outside testdata
   fixtures.

### 4. README + docs

1. README, events section: (a) prerequisite - zmetad running on the ZFS
   host at DB layout v5 (SCHEMA.md consumer contract), poll interval
   note: events appear within one poll (default 30s; SIGUSR1 forces a
   collect); (b) both config keys with defaults; (c) loss semantics:
   recordsLost = lifetime known-lost records (survives retention),
   ringSwaps = kernel-log replacements (history before a swap is still
   served and is bounded by a swap record); retention: events expire at
   zmetad `--retention` (default 90 days), gap/swap counts are lifetime;
   (d) purge semantics: `?events` purge execs `zmetad --purge` - clears
   DB rows AND the kernel ring, resets loss history; authenticated only.
2. docs/plans/zmetad-provider-2026-09/master.md Completion Tracking:
   leaves 01-03 done (orchestrator may have updated; leave if set).

### 5. Gate check

`make test && make lint` green. `make build` produces the binary.
`go env CGO_ENABLED` untouched.

## Interface Contract (Exposed to Siblings)

Contract 4 (config keys + registration) and Contract 5 (additive wire
fields). After this leaf the production binary contains NO `zfs events`
exec path and NO SQL purge path; the only execs are `zmetad --purge`.

## Self-Verification Checklist

- [ ] make test && make lint && make build green.
- [ ] No `zfs events` exec anywhere in production code.
- [ ] Config defaults land at config-load; provider receives concrete values.
- [ ] ringSwaps/RingSwaps on the wire; IsLossy covers both loss classes.
- [ ] s3_wiring_test.go still passes (name "zfs-events" unchanged).
- [ ] README covers prerequisite, config keys, freshness bound, retention,
      loss semantics, purge semantics.
- [ ] metadata.go (frozen interface) untouched.

## Review Checklist (for review agent)

- [ ] Deleted-symbol sweep clean (task 3.4-3.5 greps; ResolveDataset
      deletion justified by grep).
- [ ] Retained-symbol justification: anything kept from zfs_events.go /
      zfs_cmd.go is referenced by leaves 01-03 or tests.
- [ ] historyDetailFor fallback decision implemented + tested (no stale
      global read on the production path).
- [ ] Wire fields additive (no renamed/removed JSON/XML keys - diff the
      envelope structs against Contract 5).
- [ ] The 503-on-unavailable path still works end to end (shim unchanged).
- [ ] README matches Contract 5 and SCHEMA.md sections 4, 8, 9.

## Do NOT commit

The orchestrator stages and commits after review.
