# zmetad SQLite DB Accessor - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Pure-Go read-only accessor for the zmetad SQLite database
  (layout version 5 per SCHEMA.md): open + dual version gate, event
  queries (incl. full_path/old_full_path), gap stats, DB-only
  dataset-by-path resolution.
- **Dependencies:** none (first leaf; defines EventRow + GapStats).
- **Estimated Context:** ~50K (explore 10K + generate 22K + iterate 8K + overhead 10K)
- **Concurrency Group:** A
- **Upstream references:** zfs-metadata issues #3 #6 #7 #9 (all resolved at
  `999bef072`); contract: `/Users/caimlas/git/zfs-metadata/contrib/zmetad/SCHEMA.md`

## Goal

Create `internal/metadata/zmetad_db.go`: a typed, read-only Go API over the
zmetad SQLite database. This is the ONLY file in the tree that touches SQL
or the sqlite driver. Everything above it consumes EventRow, GapStats, and
the accessor methods - no SQL leaks upward. There is deliberately NO purge
method: purge is `zmetad --purge` (parent Contract 3), and hand-rolled SQL
deletion is forbidden (it drops sync_state, causing a full re-import of an
uncleared ring).

The upstream contract is `/Users/caimlas/git/zfs-metadata/contrib/zmetad/SCHEMA.md`
(layout v5). Read it FIRST - sections 1 (stability), 2 (tables, incl. 2.4
objmap - an implementation detail we do NOT read), 4 (loss formulas), 5
(path resolution), 7 (full_path authority). This leaf implements those
sections; where SCHEMA.md and this leaf disagree, SCHEMA.md wins and you
report the drift.

## Context

Repo: /Users/caimlas/git/mini-s3, module github.com/bhodgens/zeta-object,
Go, stdlib-first. Package `internal/metadata` holds the frozen
MetadataProvider interface (metadata.go - DO NOT EDIT), the CLI provider
(zfs_events.go - DO NOT modify in this leaf), typed errors (errors.go).
Error strings start with `metadata: `.

The driver is `modernc.org/sqlite` (pure Go, no cgo - keeps the static
binary property). `go get modernc.org/sqlite@latest`; confirm no
mattn/go-sqlite3 enters go.mod. Open read-only:
`file:<path>?mode=ro&busy_timeout=2000`. WAL mode (SCHEMA.md header) means
readers run safely against the live daemon - do NOT set journal_mode
yourself (a ro connection must not attempt writes, including pragma
writes).

Key files to understand before implementing:
- /Users/caimlas/git/zfs-metadata/contrib/zmetad/SCHEMA.md - THE contract
- internal/metadata/metadata.go - frozen interface + ObjectEvent
- internal/metadata/errors.go - typed-error conventions to match
- internal/metadata/zfs_cmd.go - mountpointContains (REUSE for the
  ancestor-prefix rule in ResolveDatasetByPath - same semantics: equal or
  path+"/" prefix; do not duplicate)

## Interface Contracts (From Parent)

Contract 1 of master.md, verbatim (EventRow, GapStats, OpenZmetadDB,
Events, GapStats method, ResolveDatasetByPath, HasDataset, Close - see
parent for the full block; signatures are FROZEN).

Binding notes:
- Op values are the schema enum NAMES as stored (`CREATE`, ...,
  `UNKNOWN`); lowercase mapping happens in leaf 02/03, not here.
- `Events` orders `(txg ASC, id ASC)` - SCHEMA.md section 7 tie-break.
  `max > 0` applies `LIMIT max`.
- `GapStats` per SCHEMA.md section 4, three separate queries or one
  conditional aggregation - but NEVER `SUM(lost)` unconditioned (the -1
  sentinels would corrupt the figure).
- `ResolveDatasetByPath`: load all (mountpoint, dataset) rows, filter to
  "/"-prefixed mountpoints, longest match via mountpointContains; no
  match -> `*DatasetNotTrackedError` (typed, exported - callers turn it
  into a ProbeResult Reason, not a log-and-die).
- Version gate: refuse `db_schema_version > 4`
  (*ZmetadDBVersionError{Newer:true}); missing key -> treat as v1,
  refuse with Newer:false; also refuse when `events_schema_version`
  exists and != 2 (*ZmetadDBVersionError with a Which field set to
  "events_schema_version" - additive field, does not break Contract 1).
- All statements bind parameters; zero string-formatted SQL inputs
  (dataset names come from DB content and zfs get output).

## Tasks

### 1. Add the driver dependency

1. `go get modernc.org/sqlite@latest`; `go mod tidy`.
2. Verify: `grep -c mattn go.mod go.sum` returns no hits.

### 2. TDD: version gate

1. Write `internal/metadata/zmetad_db_test.go`:
   - helper `writeTestDB(t, dbSchemaVersion string, eventsSchemaVersion string)`
     builds a minimal SQLite file with the exact SCHEMA.md v5 schema
     (events with captured_at + full_path + old_full_path, sync_state with
     ring_guid, datasets, objmap, gaps, meta rows). Use the driver itself
     (rw open) to build it.
   - `TestOpenZmetadDB_VersionGate`: v5 -> ok; v6 ->
     ZmetadDBVersionError{Newer:true}; v4 -> ZmetadDBVersionError{Newer:false}
     with an upgrade hint in the message (zmetad migrates in place);
     missing db key -> ZmetadDBVersionError{Newer:false};
     events_schema_version=3 -> refuse with Which="events_schema_version";
     missing file -> ZmetadDBMissingError.
2. Run, verify fail (no implementation yet).
3. Implement OpenZmetadDB + typed errors in `zmetad_db.go`:

```go
type ZmetadDBMissingError struct{ Path string }
type ZmetadDBVersionError struct {
    Path    string
    Version string // stored value, "" when absent
    Newer   bool
    Which   string // "db_schema_version" (default) or "events_schema_version"
}
type DatasetNotTrackedError struct{ Path string }
```

4. Run, verify pass.

### 3. TDD: Events query + NULL discipline

1. Tests: two rows differing txg -> (txg,id) order; `max=1` limits; NULL
   optional columns (incl. captured_at, full_path, old_full_path) decode
   as nil pointers; a fully-populated row decodes non-nil; unknown dataset
   -> empty slice, nil error.
2. Run (fail), implement `Events`, verify pass.

### 4. TDD: GapStats

1. Tests: fixture with lost=5, lost=3, lost=0, lost=-1 rows ->
   GapStats{KnownLost:8, Regressions:1, RingSwaps:1}; the -1 NEVER enters
   KnownLost (this test is the #9 sentinel-corruption guard); dataset
   with no gap rows -> all zeros, nil error.
2. Run (fail), implement, verify pass.

### 5. TDD: ResolveDatasetByPath (DB-only resolution, #6)

1. Tests: datasets table with `/testpool` -> `testpool`,
   `/testpool/scratch` -> `testpool/scratch`, plus verbatim `none` row;
   resolve `/testpool/a/b` -> `testpool` (longest-prefix wins over
   deeper-but-unrelated? NO - longest MATCHING ancestor: `/testpool/scratch/x`
   -> `testpool/scratch`); `/testpoolx` -> NOT testpool (separator guard);
   only `none`/`legacy` rows -> DatasetNotTrackedError; empty table ->
   DatasetNotTrackedError.
2. Run (fail), implement using mountpointContains (reuse, do not copy),
   verify pass.

### 6. HasDataset + gate check

1. Test: true only when sync_state has the row.
2. `make test && make lint` green; `go list -deps ./internal/metadata |
   grep -c mattn` == 0.

## Interface Contract (Exposed to Siblings)

Exactly Contract 1 of master.md. No SQL, no driver types in exported
signatures, no schema constants exported.

## Self-Verification Checklist

- [ ] `go test ./internal/metadata/ -run Zmetad` green.
- [ ] make test && make lint green repo-wide.
- [ ] go.mod: modernc.org/sqlite present, mattn absent.
- [ ] All SQL uses bound parameters.
- [ ] NULL columns decode to nil pointers (no fabricated 0/"").
- [ ] KnownLost never includes -1 or 0 sentinels (test proves).
- [ ] ResolveDatasetByPath rejects `/testpoolx` style prefix escapes.
- [ ] Every error string starts `metadata: `.
- [ ] metadata.go and zfs_events.go untouched; no purge method exists.

## Review Checklist (for review agent)

- [ ] Contract 1 signatures match master.md verbatim.
- [ ] Fixture schema matches SCHEMA.md section 2 column-for-column
      (incl. captured_at, full_path, old_full_path, ring_guid,
      datasets.mountpoint, objmap).
- [ ] Version gate: ==5 accepted, >5 refuse-newer, <5 refuse with
      upgrade hint, events_schema_version == 2 enforced.
- [ ] GapStats implements SCHEMA.md section 4 formulas exactly.
- [ ] ResolveDatasetByPath implements SCHEMA.md section 5 exactly
      (non-absolute filtered, longest ancestor match).
- [ ] Read path cannot create/modify the DB (mode=ro; no pragma writes).
- [ ] No cgo; static binary property preserved.

## Do NOT commit

The orchestrator stages and commits after review.
