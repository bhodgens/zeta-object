# zmetad-Exclusive Metadata Provider - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 5 leaf documents under this node
- **Scope:** Replace the `zfs events -j` CLI transport in mini-s3's `zfs-events`
  MetadataProvider with exclusive reads from the zmetad SQLite export database
  (zfs-metadata `extended-metadata` at `8a839cd2e`, consumer contract
  `contrib/zmetad/SCHEMA.md`, **DB layout version 5** - events carry
  insert-time-resolved `full_path`/`old_full_path`; issue #11), preserving the
  frozen `MetadataProvider` interface and the existing `?events` wire surface.
  Driver dependency: `modernc.org/sqlite` (pure Go, static binary preserved;
  user-approved 2026-09-30).

## Goal

Today every `?events` request spawns a `zfs` process and re-reads the entire
kernel ring buffer (`internal/metadata/zfs_events.go`, `zfsExecSem` bound of
4). zmetad durably exports the same event log to SQLite and, since the
issue-#5..#10 resolution commits (`c46d69a57`..`999bef072`), now covers the
FULL consumer path with documented contracts:

- SCHEMA.md is the stable consumer contract (additive-only versioning,
  refuse-newer rule, NULL-never-fabricated).
- Layout v5: `events.full_path` is resolved AT INSERT TIME by the daemon
  (authoritative as of the row's own event); `full_path IS NULL` is exactly
  the PARTIAL case; directory renames never rewrite history
  (`old_full_path` links the before-path). Consumers serve full_path
  directly - no client-side graph reconstruction (SCHEMA.md section 7).
- The `datasets` table maps mountpoint -> dataset: path resolution needs
  ZERO kernel calls (SCHEMA.md section 5).
- `captured_at` gives true wall-clock ingest time; `timestamp` is monotonic
  hrtime (SCHEMA.md section 2.1).
- `gaps` has pinned loss semantics: knownLost / regressions / ring swaps as
  SEPARATE classes; swaps carry the `-1` sentinel and MUST NOT be summed
  into a record count (SCHEMA.md section 4).
- `sync_state.ring_guid` segments history into kernel-log epochs; a swap
  writes a `-1` gap row (SCHEMA.md section 6).
- `zmetad --purge <dataset>` is the coordinated wipe: DB rows (events,
  gaps, sync_state) AND the kernel ring (SCHEMA.md section 9).
- SIGUSR1 forces an out-of-band collect; the documented freshness bound is
  one poll interval (SCHEMA.md section 8).

This tree makes the zmetad database the ONLY read path for object event
history. The CLI `zfs events -j` transport is deleted from the production
path. The frozen `MetadataProvider` interface
(`internal/metadata/metadata.go:15`, master Contract 1 of
`docs/plans/metadata-zfs-2026-09/master.md`) MUST NOT change: providers
enrich, never alter core S3 metadata behavior.

Non-goals: no kernel-side changes, no zmetad changes (its contracts are
upstream-owned), no change to the `.meta` sidecar canonical metadata layer,
no auto-SIGUSR1 plumbing (documented as a future option; Open Question 2).

## Architecture

Read path: mini-s3 opens the zmetad SQLite database read-only (pure-Go
driver `modernc.org/sqlite` - no cgo, the repo's static-binary rule),
refuses `db_schema_version` outside 5..6 (SCHEMA.md refuse-newer rule;
versions evolve additively, so the consumer accepts a RANGE: >= 5 because
the v5 `full_path` columns are the read-path contract, <= the max known
layout - currently 6). Older DBs are refused with an upgrade hint (zmetad
migrates in place).

Dataset resolution: longest mountpoint-prefix match in the `datasets` table
(SCHEMA.md section 5) - NO `zfs get` exec anywhere. DetectZFS (statfs)
stays in Probe only as a fast-fail with a precise Reason; it is not
authoritative (a non-polled dataset is unavailable regardless of fs type).

History: rows ordered `(txg ASC, id ASC)` - the SCHEMA.md section 7
tie-break. Key resolution: non-NULL `full_path` is authoritative and is
served directly (leaf 02 is a thin mapper, NOT a graph walk); NULL
`full_path` = PARTIAL = conservative bare-name match. The Since filter
uses `captured_at` (wall clock) when present, falls back to
`plausibleWallClockTime(timestamp)` for legacy rows (NULL captured_at);
zero-time rows always pass (unknown is not old).

Purge: exec `zmetad --purge <dataset>` (runner seam, config-key binary
path, default `zmetad` on PATH). SQL-level purge in mini-s3 is FORBIDDEN:
only upstream purge deletes the right rows AND clears the kernel ring;
hand-rolled deletion that drops sync_state causes a full re-import of an
uncleared ring. Purge is authenticated-endpoint-only, never at startup.

Loss reporting (wire): `recordsLost` = knownLost
(`SUM(lost) WHERE lost > 0`); ring swaps surface as the NEW additive wire
field `ringSwaps` (count of `-1` gap rows) in the JSON envelope and
`RingSwaps` in the ext XML - SCHEMA.md section 4 forbids folding swaps into
a record count. `IsLossy` = recordsLost > 0 OR ringSwaps > 0. Epoch
segmentation: events before a `-1` gap row are still real file history and
ARE served; the boundary is visible through ringSwaps (documented choice,
Open Question 3).

Provider identity: registry name `"zfs-events"`; `LastDetail()
HistoryDetail` (the frontend's structural `detailReporter` seam) gains a
`RingSwaps` field. The frontend fallback shim, the wiring resolver in
`s3_wiring.go`, and the `?events` endpoints stay unchanged except the
constructor swap and the two additive wire fields.

## Interface Contracts

### Contract 1: EventRow + database accessor (FROZEN for this tree)

```go
// File: internal/metadata/zmetad_db.go
package metadata

// EventRow mirrors one zmetad `events` table row (SCHEMA.md section 2.1,
// layout version 5). Pointer fields distinguish "absent" from "present
// and zero" - the DB leaves absent fields NULL and consumers MUST NOT
// fabricate values. Mode is never written upstream (always NULL) and is
// intentionally omitted.
type EventRow struct {
    ID           int64
    Dataset      string
    Txg          uint64
    Timestamp    uint64  // kernel hrtime ns, MONOTONIC - not wall clock
    CapturedAt   *int64  // unix seconds at ingest; NULL in pre-v4 rows
    ObjectID     uint64
    Op           string  // schema enum NAME: CREATE, REMOVE, RENAME, LINK, SYMLINK, TRUNCATE, SETATTR, WRITE, READ (or UNKNOWN)
    Path         *string // bare name, dataset-relative at event time
    OldPath      *string // RENAME only
    FullPath     *string // RESOLVED dataset-relative path, authoritative as of this row's event (v5); NULL = PARTIAL
    OldFullPath  *string // RENAME: resolved path of old_path at insert time (v5)
    UID, GID     *uint64
    Size         *int64  // new size (TRUNCATE/SETATTR) or IO total
    IoOffset     *uint64
    IoBytes      *uint64
    Parent       *uint64 // parent dir object id (ground truth; not needed for reads on v5)
    OldParent    *uint64
    Target       *string // SYMLINK only
    OldSize      *int64
    Attrs        *uint64
}

// OpenZmetadDB opens the zmetad database READ-ONLY (modernc.org/sqlite,
// mode=ro, busy_timeout 2s; WAL lets readers run against the live daemon).
// Requires meta.db_schema_version == 5: a NEWER value is
// *ZmetadDBVersionError{Newer:true}; 1-4 is Newer:false with an upgrade
// hint in the message (zmetad migrates in place); a missing key is treated
// as pre-v1. A missing file is *ZmetadDBMissingError. Callers treat every
// open failure as "provider unavailable", never a hard error. Also
// verifies meta.events_schema_version == 2 (refuse condition per SCHEMA.md).
func OpenZmetadDB(ctx context.Context, path string) (*ZmetadDB, error)

// Events returns the dataset's rows ordered (txg ASC, id ASC) - the
// SCHEMA.md section 7 tie-break. max <= 0 means no LIMIT.
func (db *ZmetadDB) Events(dataset string, max int) ([]EventRow, error)

// GapStats returns the dataset's loss classes per SCHEMA.md section 4.
// Never folded: KnownLost sums rows with lost > 0 only.
func (db *ZmetadDB) GapStats(dataset string) (GapStats, error)

type GapStats struct {
    KnownLost   uint64 // SUM(lost) over rows with lost > 0
    Regressions uint64 // COUNT(lost = 0): watermark regressions, count unknown
    RingSwaps   uint64 // COUNT(lost = -1): kernel-log identity swaps
}

// ResolveDatasetByPath maps a symlink-resolved bucket path to its dataset
// via the `datasets` table ONLY (SCHEMA.md section 5): longest "/"-rooted
// mountpoint that is the path or an ancestor of it. Non-absolute
// mountpoints (verbatim "none"/"legacy") never match. No match:
// (*DatasetNotTrackedError), nil - an availability status, not a failure.
func (db *ZmetadDB) ResolveDatasetByPath(path string) (string, error)

// HasDataset reports whether sync_state has a row for the dataset.
func (db *ZmetadDB) HasDataset(dataset string) (bool, error)

func (db *ZmetadDB) Close() error
```

NO PurgeDataset method exists on the accessor: purge is `zmetad --purge`
(Contract 3). Owner: `01-db-accessor.md`. Consumers: `02`,
`03`, `05`.

### Contract 2: Row -> ObjectEvent mapping (full_path-first, thin mapper)

```go
// File: internal/metadata/zmetad_paths.go
package metadata

// rowsToEvents maps zmetad rows to ObjectEvents per SCHEMA.md section 7
// (layout >= 5): non-NULL full_path is AUTHORITATIVE and served directly
// (no graph reconstruction); NULL full_path = PARTIAL: Key stays the bare
// path name, resolved flag false. Op is lowercased. Timestamp: captured_at
// (wall clock) when present, else plausibleWallClockTime(timestamp), else
// zero time. Sizes: TRUNCATE/SETATTR -> SizeNew from size, SizeOld from
// old_size; WRITE/READ -> SizeNew from io_bytes. Never fabricate.
func rowsToEvents(rows []EventRow) *rowEventSet

type rowEventSet struct {
    events      []ObjectEvent
    resolved    []bool // true iff full_path was non-NULL for events[i].Key
    oldResolved []bool // true iff old_full_path was non-NULL for events[i].OldKey
}

// rowsMatchKey is eventMatchesKey over rowEventSet (exact when resolved,
// conservative bare-name/"/"+bare suffix match otherwise). Reuse
// bareMatchesKey; do not duplicate its logic.
func rowsMatchKey(set *rowEventSet, i int, key string) bool
```

Owner: `02-row-mapping.md`. Consumers: `03-provider.md`.

### Contract 3: The zmetad provider (implements the frozen interface)

```go
// File: internal/metadata/zmetad_provider.go
package metadata

// zmetadEventsProvider serves ?events from the zmetad SQLite DB.
// Registry name stays "zfs-events". Implements LastDetail()
// HistoryDetail (detailReporter seam); HistoryDetail gains RingSwaps.
func NewZmetadEventsProvider(dbPath string) MetadataProvider

// Probe (per bucket, no exec): EvalSymlinks -> DetectZFS (fast-fail
// Reason only) -> openDB -> ResolveDatasetByPath -> HasDataset. Dataset
// resolution result cached on the instance (positive results only).
// Any failure: ProbeResult{Available:false, Reason}, nil error.
// History: dataset -> openDB -> Events -> rowsToEvents -> key filter
// (rowsMatchKey) -> Since filter (captured_at primary,
// plausibleWallClockTime fallback, zero passes) -> MaxEvents cap.
// RecordsLost = GapStats.KnownLost; detail.RingSwaps = GapStats.RingSwaps.
// Purge: exec <zmetadBinary> --purge <dataset> via the zmetadRunner seam
// (context timeout, argv-only, stderr captured). Never SQL. Never at
// startup. Authenticated endpoint only.
var zmetadRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error)
```

Owner: `03-provider.md`. Consumers: `04-wiring-config.md`.

### Contract 4: Config + wiring

```go
// config.json: new optional keys (defaults shown)
//   "zmetad_db_path": "/var/lib/zfs/zmetad.db"
//   "zmetad_binary":  "zmetad"
// main.go config struct gains ZmetadDBPath, ZmetadBinary (defaults set
// at config load - one place owns defaults).
// s3_wiring.go registration becomes:
//   metadata.Register(metadata.NewZmetadEventsProvider(cfg.ZmetadDBPath))
// The CLI provider constructor NewZFSEventsProvider and the
// `zfs events` transport in zfs_events.go are DELETED (leaf 04);
// zfs_cmd.go ResolveDataset STAYS (dead code is removed in leaf 04 only
// if nothing references it - the fsdetect files stay regardless).
```

Owner: `04-wiring-config.md`. Consumers: `05-parity-e2e.md`.

### Contract 5: Loss semantics on the wire (additive, documented)

- `recordsLost` (JSON) / `RecordsLost` (XML) = GapStats.KnownLost for the
  dataset - lifetime, never rewritten by retention (gaps rows are never
  retention-deleted, SCHEMA.md section 9).
- NEW additive fields: `ringSwaps` (JSON) / `RingSwaps` (XML) =
  GapStats.RingSwaps. Swaps are NEVER summed into recordsLost.
- `IsLossy` (XML) = recordsLost > 0 OR ringSwaps > 0.
- The JSON envelope keys stay `dataset`,`recordsLost`,`ringSwaps`,
  `events` with per-event `op,key,oldKey,txg,timestamp,sizeOld,sizeNew`;
  per-event `timestamp` renders from `captured_at` when present
  (true wall clock), else `plausibleWallClockTime`, else zero-time
  RFC3339 ("0001-01-01T00:00:00Z" - never fabricated).
- README documents the freshness bound (one poll interval) and retention
  (events expire at `--retention`, default 90 days; gaps are lifetime).

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-db-accessor.md | leaf | none | ~50K | A |
| 02 | 02-row-mapping.md | leaf | 01 (EventRow type) | ~30K | B (after 01) |
| 03 | 03-provider.md | leaf | 01, 02 | ~55K | C (after 02) |
| 04 | 04-wiring-config.md | leaf | 03 | ~40K | D (after 03) |
| 05 | 05-parity-e2e.md | leaf | 03, 04 | ~55K | E (after 04) |

Sequential execution: each leaf compiles against the previous leaf's frozen
contracts. Leaves 01 and 02 can start together ONLY if 02 is given
Contracts 1-2 verbatim (it needs only the EventRow type, not the compiled
file).

## Dispatch Protocol

For each child document, in dependency order:

1. Read the leaf document fully.
2. Dispatch one implementation agent with the leaf's full text plus this
   context: repo root `/Users/caimlas/git/mini-s3`, Go module
   `github.com/bhodgens/zeta-object`, upstream contract at
   `/Users/caimlas/git/zfs-metadata/contrib/zmetad/SCHEMA.md` (read-only
   reference), run `make test` and `make lint` before reporting. Include:
   "Do NOT run git commit. Do NOT run git add. Write code, run tests,
   report results. The orchestrator handles all git operations."
3. Review the report in-session against the leaf's Review Checklist.
4. Re-dispatch only the failing parts if review fails (never the whole leaf
   blindly - audit `git status` for surviving partial work first).
5. After review passes, commit the leaf's files with a message referencing
   the leaf document (e.g. `feat(metadata): zmetad DB accessor (leaf 01,
   zmetad-provider-2026-09)`).
6. Update the Completion Tracking Table.

Dependencies are sequential; do not parallel-dispatch a leaf before its
dependency's review passes.

## Coding Conventions

- Go, stdlib-first; the ONLY new dependency allowed is
  `modernc.org/sqlite` (pure Go - no cgo, static binary preserved).
  Do not add mattn/go-sqlite3 (cgo) under any circumstance.
- Never fabricate metadata: NULL DB fields stay zero/absent in ObjectEvent
  - pointer-vs-zero discipline (SCHEMA.md stability policy).
- Provider unavailability is a ProbeResult status (`Available:false` +
  Reason), never a returned error - except true bugs.
- Timestamps: `timestamp` is monotonic hrtime - NEVER wall clock on the
  wire; wall clock comes from `captured_at` or
  `plausibleWallClockTime` (bughunt M2) with zero-time fallback.
- Loss classes are never folded: swaps are counts, lost records are sums
  (SCHEMA.md section 4).
- User-facing prose (README, docs): hyphens not em-dashes; plain language.
- Concurrency: the provider MUST be safe for concurrent use
  (MetadataProvider contract); guard the dataset cache with a mutex.
- Every error message starts with `metadata: ` (package convention).
- Test files sit beside the code (`_test.go`), table-driven where natural.
- Do not modify `internal/metadata/metadata.go` (frozen interface), the
  parity gate semantics, or the `.meta` sidecar path.

## Completion Tracking Table

| Leaf | State | Review | Commit | Notes |
|------|-------|--------|--------|-------|
| 01-db-accessor.md | done | pass | b58309b | 9/9 tests; make test green; lint NEW_FROM_REV 0 issues; metadata cov 87.9%; SCHEMA.md v5 nits reported upstream-pending |
| 02-row-mapping.md | done | pass | 0d72654 | 35 subtests pass; drift guard green on 4 fixtures; make test green (metadata 89.0%); lint NEW_FROM_REV 0; SCHEMA.md TEXT-typo drift = upstream #12 |
| 03-provider.md | done | pass | c0ff5ea | 36 tests pass incl -race; single permitted zfs_events.go edit (HistoryDetail.RingSwaps) verified by diff; make test 83.1%; lint NEW_FROM_REV 0; deviations 1-5 accepted (cache fast-path, probeDB split) |
| 04-wiring-config.md | done | pass | 1e2c992 | make test/lint/build green (82.8%); CLI transport fully deleted (rg 'zfs events' internal/ = 0); parser kept test-only for drift guard; ResolveDataset deleted; DEVIATION: no purge HTTP endpoint exists (pre-existing) - leaf 05 e2e purge task adjusted |
| 05-parity-e2e.md | done | pass | 4f7e8c9 | parity zmetad scenario + wire-shape pin PASS; e2e case 18 PASS 20/20; suite 428/11 (11 pre-existing webdav/owncloud failures proven); metadata cov 90.3%, total 82.4%, COVER_MIN unchanged (47); ZETAOBJECT_ASSUME_ZFS env gate added (statfs hint bypass only, unit-pinned + README) |

States: pending / dispatched / in-review / done / re-dispatched.

## Integration Test Plan

After leaf 05:

1. `make test` - full unit suite green including new zmetad tests.
2. `make lint` - golangci-lint clean with the new dependency.
3. `make e2e` - all cases pass including the new `18-zmetad-events.sh`
   (skips gracefully when no fixture can be built).
4. Parity gate: `internal/metadata/parity_test.go` scenarios still
   byte-identical between plain-FS and provider-enabled buckets.
5. Manual live check (deferred, needs zfs-meta host): zmetad running at
   layout v5, mini-s3 `?events` matches SCHEMA.md ground truth on a
   non-wrapped log; each gaps class surfaces correctly; `zmetad --purge`
   through the endpoint clears both stores. Recorded in
   `docs/validation-*.md` per repo convention.

## Review Checklist

- [ ] Every leaf's interface contract matches Contracts 1-5 verbatim
      (no signature drift between leaves).
- [ ] No cgo dependency anywhere (go.mod has no `mattn/go-sqlite3`).
- [ ] `internal/metadata/metadata.go` untouched (`git diff --stat`).
- [ ] CLI `zfs events` transport deleted; NO SQL-level purge; purge goes
      through `zmetad --purge` only.
- [ ] Dataset resolution is DB-only (no `zfs get` on read/probe paths).
- [ ] All wires covered: `?events` JSON (with ringSwaps),
      `?events&versions` XML (IsLossy/RingSwaps/RecordsLost per
      Contract 5), 503 on unavailable probe.
- [ ] Refuse-newer version gate present (db_schema_version AND
      events_schema_version).
- [ ] e2e case exists per the numbered-case convention; README documents
      config keys, freshness bound, retention, loss semantics.
- [ ] Coverage floors re-measured if code moved packages (AGENTS.md rule).

## Open Questions

1. **Dependency approval (BLOCKING for leaf 01):** adding
   `modernc.org/sqlite` is a new third-party dependency. User approval
   still outstanding - confirm at dispatch.
2. **Freshness:** upstream bound is one poll interval (default 30s);
   SIGUSR1 forces an out-of-band collect. This tree only documents the
   bound (README). A future enhancement could SIGUSR1 zmetad after PUTs -
   NOT in scope (signal-per-write is racy and unowned).
3. **Cross-epoch history:** events from before a ring swap are still real
   file history and are SERVED; the boundary is visible via `ringSwaps`
   (chosen alternative to SCHEMA.md section 6's offset-segmentation, which
   applies to offset-keyed queries). Revisit if a consumer needs strict
   per-epoch listing.
4. **WRITE/READ ops (wire schema v2):** passed through lowercased in event
   JSON, excluded from the `&versions` derivation (which maps only
   create/rename/truncate/remove). Revisit if IO auditing should surface.
