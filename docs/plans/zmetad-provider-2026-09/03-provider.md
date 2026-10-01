# zmetad Events Provider - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** The `zmetadEventsProvider` implementing the frozen
  MetadataProvider interface: DB-only Probe (cached dataset), DB-backed
  History with epoch-aware loss reporting, `zmetad --purge` execution.
- **Dependencies:** 01-db-accessor.md, 02-row-mapping.md.
- **Estimated Context:** ~55K (explore 15K + generate 20K + iterate 10K + overhead 10K)
- **Concurrency Group:** C
- **Upstream references:** SCHEMA.md sections 4, 5, 8, 9; zfs-metadata #5 #8 #9 (resolved)

## Goal

Create `internal/metadata/zmetad_provider.go`: the provider the wiring
leaf registers under the name `"zfs-events"`. It owns: DB-only dataset
resolution cached per bucket path (ZERO kernel execs), DB open per History
call, row -> ObjectEvent conversion via rowsToEvents (full_path-first,
no graph walk), key/Since/
MaxEvents filtering, GapStats-backed HistoryDetail, and Purge via the
`zmetad --purge` binary (the ONLY purge mechanism; SQL purge is forbidden).

This leaf does NOT delete the CLI provider (leaf 04) and does NOT touch
wiring (leaf 04). Tests stub the DB accessor and the purge runner via
seams.

## Context

Read for the interface and idioms (do not modify):
- internal/metadata/metadata.go - frozen MetadataProvider, ProbeResult,
  HistoryQuery, ObjectEvent
- internal/metadata/zfs_events.go - `zfsEventsProvider` (the pattern:
  per-instance detail mutex, `var _ MetadataProvider = (*T)(nil)` compile
  assertion), plus the filter semantics to replicate exactly: key filter
  BEFORE Since filter (index desync comment at zfs_events.go:642),
  zero-timestamp events pass Since (unknown is not old), MaxEvents caps
  post-filter. NOTE: after leaf 02, row events carry wall-clock Timestamp
  from captured_at; the zero-passes-Since rule applies to rows with NULL
  captured_at AND implausible hrtime.
- internal/frontend/s3/capability_endpoints.go:93 - detailReporter (the
  provider must satisfy LastDetail() HistoryDetail structurally)
- internal/metadata/zfs_cmd.go - mountpointContains (already reused in
  leaf 01's accessor; not needed here)

HistoryDetail gains a field (additive, Contract 5): `RingSwaps uint64`.
Update metadata.go? NO - HistoryDetail lives in zfs_events.go today;
declare the extended detail in THIS file as the zmetad provider's own
type? NO - the frontend reads `metadata.HistoryDetail`. The field goes on
`HistoryDetail` in zfs_events.go. This is the ONE permitted edit to
zfs_events.go in the whole tree: add `RingSwaps uint64` to HistoryDetail
with a comment citing SCHEMA.md section 4 (swaps never fold into
RecordsLost). Everything else in that file is leaf 04's deletion problem.

DB seam for tests (same pattern as zfsRunner/resolveDatasetFn):

```go
type dbHandle interface {
    Events(dataset string, max int) ([]EventRow, error)
    GapStats(dataset string) (GapStats, error)
    ResolveDatasetByPath(path string) (string, error)
    HasDataset(dataset string) (bool, error)
    Close() error
}
var openDB = func(ctx context.Context, path string) (dbHandle, error) {
    return OpenZmetadDB(ctx, path) // *ZmetadDB implements dbHandle
}
```

Purge runner seam (parent Contract 3):

```go
var zmetadRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error)
// production: exec.CommandContext with a 10s timeout, argv-only (dataset
// name from the DB, never user input), stdout/stderr captured - mirror
// the zfsRunner discipline it replaces.
```

Dataset cache: mutex-guarded map bucketPath -> dataset on the provider
instance; positive results only (a dataset can appear in `datasets` after
the first poll). DatasetNotTrackedError and all Probe failures are NOT
cached.

## Interface Contracts (From Parent)

Contract 3 of master.md, verbatim:

```go
func NewZmetadEventsProvider(dbPath string) MetadataProvider
```

Registry name: `Name() string { return "zfs-events" }` - the frontend
constant and wiring depend on it. Must satisfy `LastDetail()
HistoryDetail`.

Probe (NO exec, NO DB write): EvalSymlinks -> DetectZFS (fast-fail Reason
`filesystem is not ZFS` only - statfs is a hint, not authoritative) ->
openDB -> ResolveDatasetByPath -> HasDataset -> close DB. Available=true
only when every step succeeds. Reason prefixes: `path resolve:`,
`statfs:`, `db:`, `not tracked by zmetad` (DatasetNotTrackedError),
`not polled by zmetad yet` (no sync_state row). Probe returns nil error
for unavailable - a status, not a failure.

History: dataset (cache) -> openDB -> Events(dataset, 0) ->
rowsToEvents -> key filter (rowsMatchKey) -> Since filter ->
MaxEvents cap -> detail{Dataset, RecordsLost: stats.KnownLost,
RingSwaps: stats.RingSwaps}. The CLI path's MaxEvents*3 fetch-headroom
hack is GONE - SQL filtering needs no headroom.

Purge: dataset (cache) -> zmetadRunner(ctx, p.binary, "--purge", dataset)
-> non-zero exit or timeout = wrapped `metadata: ` error with dataset
name and stderr. Purge NEVER runs SQL and NEVER runs at startup;
authenticated endpoint only (the endpoint wiring from leaf 04 already
enforces auth). Document on the method: purges BOTH the DB copy and the
kernel ring (SCHEMA.md section 9); gaps rows are removed too - loss
history resets.

## Tasks

### 1. TDD: Probe matrix

1. Write `zmetad_provider_test.go` with stubbed openDB.
2. Tests: happy path (fs ok + tracked + polled) -> Available true,
   Dataset set, NO zfs exec (assert via a zfsRunner-free package: the
   probe path must not touch any exec); DetectZFS false -> Available
   false with the statfs Reason; DatasetNotTrackedError -> `not tracked
   by zmetad`; HasDataset false -> `not polled by zmetad yet`; openDB
   error -> `db: ...`. Assert resolution is called ONCE across two
   Probes (cache works) and ZERO times on a subsequent History with a
   warm cache.
3. Run (fail), implement Probe + cache, run (pass).

### 2. TDD: History filtering chain

1. Tests with stubbed rows (reuse leaf 02's row-builder): key filter
   exact-match on resolved keys; conservative match for partial rows;
   Since drops only older-than-Since and passes zero-timestamp rows
   (NULL captured_at + implausible hrtime); MaxEvents caps post-filter;
   detail exposes KnownLost as RecordsLost and RingSwaps separately
   (fixture: lost=5, lost=-1 -> RecordsLost=5, RingSwaps=1 - the
   never-fold guard at the provider level); empty DB -> empty slice,
   nil error.
2. Run (fail), implement History, run (pass).

### 3. TDD: concurrency

1. Test: 8 goroutines doing History on two bucket paths simultaneously
   (two datasets, two stub DBs) with -race; assert no cross-attribution
   of Dataset/RecordsLost/RingSwaps detail (the bughunt A2/C2 failure
   mode). Also concurrent Probe+History (cache race).
2. Run, fix with the per-instance mutex pattern, pass.

### 4. TDD: Purge via zmetad binary

1. Tests: stub zmetadRunner - success path passes ("--purge", dataset)
   with the configured binary; non-zero exit propagates a wrapped error
   carrying dataset + stderr; runner NEVER called at Probe/History
   (assert invocation count stays 0); timeout -> context error wrapped.
2. Run (fail), implement, run (pass).

### 5. Compile assertions + gate

`var _ MetadataProvider = (*zmetadEventsProvider)(nil)` and the
LastDetail assertion. `make test && make lint` green (with -race for the
package).

## Interface Contract (Exposed to Siblings)

Contract 3: `NewZmetadEventsProvider(dbPath string) MetadataProvider`
named `"zfs-events"` with `LastDetail() HistoryDetail` (HistoryDetail
now carries RingSwaps). Unexported seams stay unexported. The binary
path is provider config (leaf 04 wires `zmetad_binary`; default `zmetad`
on PATH - constructor keeps the single-arg signature per Contract 3, the
binary field defaults internally and is settable via an unexported field
for leaf 04's config pass-through IF needed; document the choice).

## Self-Verification Checklist

- [ ] go test ./internal/metadata/ -run ZmetadProvider green; -race green.
- [ ] make test && make lint green.
- [ ] Registry name is exactly "zfs-events".
- [ ] Probe and History perform ZERO process execs; only Purge execs.
- [ ] Filter order: key, then Since, then MaxEvents.
- [ ] RecordsLost = KnownLost; RingSwaps separate; never folded (tests).
- [ ] Purge only via zmetad --purge; never SQL; never at startup.
- [ ] Only permitted zfs_events.go edit: HistoryDetail.RingSwaps field.
- [ ] metadata.go (frozen interface) untouched.

## Review Checklist (for review agent)

- [ ] Contract 3 verbatim; Name() == "zfs-events"; LastDetail present.
- [ ] Probe failure modes all degrade to Available:false + Reason, nil
      error; statfs treated as a hint with its own Reason.
- [ ] Detail per-instance (race test passes, A2/C2 pattern).
- [ ] Filter semantics match zfs_events.go History (order, zero-pass,
      cap) with line citations in review.
- [ ] Purge semantics documented (DB + kernel ring; gaps history resets).
- [ ] DB handle lifecycle: opened per call, closed (defer Close).
- [ ] HistoryDetail.RingSwaps comment cites SCHEMA.md section 4.

## Do NOT commit

The orchestrator stages and commits after review.
