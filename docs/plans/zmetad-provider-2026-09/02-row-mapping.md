# Row -> ObjectEvent Mapping (full_path-first) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Map zmetad v5 EventRows to ObjectEvents: full_path-first key
  resolution (NO graph reconstruction), op lowercasing, wall-clock
  timestamp policy, size mapping, conservative match for PARTIAL rows.
- **Dependencies:** 01-db-accessor.md (EventRow type; read its Interface
  Contract - no need to explore the DB code).
- **Estimated Context:** ~30K (explore 6K + generate 10K + iterate 8K + overhead 6K)
- **Concurrency Group:** B
- **Upstream references:** SCHEMA.md section 7 (layout >= 5: non-NULL
  full_path is authoritative; NULL = PARTIAL); zfs-metadata #11 (resolved
  at `8a839cd2e`)

## Goal

Create `internal/metadata/zmetad_paths.go`: `rowsToEvents` + `rowsMatchKey`
(Contract 2 of master.md). This is a THIN MAPPER, not a graph walk: layout
v5 stores `full_path` resolved at insert time by zmetad, so mini-s3 serves
it directly. The old CLI-path reconstruction code (`reconstructPaths`,
`detectRoot`, `resolvePath` and their txg-scoped map machinery) is NOT
ported - leaf 04 deletes whatever of it loses its last caller. What IS
reused: `bareMatchesKey` (conservative match semantics),
`plausibleWallClockTime` (hrtime guard, bughunt M2), and the ObjectEvent
field conventions from zfs_events.go.

## Context

Package internal/metadata. Read before writing:
- /Users/caimlas/git/zfs-metadata/contrib/zmetad/SCHEMA.md sections 2.1
  (wire-field map, op-constrained columns) and 7 (authoritative
  full_path; PARTIAL = NULL full_path; conservative match; never
  fabricate; never hide) - THE contract. Where SCHEMA.md and this leaf
  disagree, SCHEMA.md wins; report the drift.
- internal/metadata/zfs_events.go: `plausibleWallClockTime`,
  `bareMatchesKey`, `eventMatchesKey` (the match pattern to mirror),
  the size/timestamp mapping in reconstructPaths pass 2 (lines ~370-397)
  for ObjectEvent field conventions.
- internal/metadata/metadata.go: ObjectEvent shape (frozen).

Field mapping (row -> ObjectEvent):
- Op: strings.ToLower(row.Op) - DB stores enum NAMES (CREATE -> create).
- Key: *row.FullPath when non-NULL (resolved=true); else *row.Path (bare,
  resolved=false). Never fabricate: a NULL full_path stays bare.
- OldKey (RENAME): *row.OldFullPath when non-NULL (oldResolved=true);
  else *row.OldPath (oldResolved=false).
- Txg: row.Txg.
- Timestamp: captured_at non-NULL -> time.Unix(*CapturedAt, 0).UTC();
  else plausibleWallClockTime(row.Timestamp); else zero time. Never both.
- Sizes: TRUNCATE/SETATTR -> SizeNew from *row.Size when present, SizeOld
  from *row.OldSize when present; WRITE/READ -> SizeNew from *row.IoBytes
  when present (master Open Question 4 - passthrough policy). All other
  ops: sizes stay zero (print_event parity).
- UID/GID: uint32(*row.UID) when present, else 0, with the same
  truncation comment as zfs_events.go:395.

## Interface Contracts (From Parent)

Contract 2 of master.md, verbatim (rowsToEvents, rowEventSet,
rowsMatchKey - signatures FROZEN).

## Tasks

### 1. TDD: full_path-first mapping

1. Write `zmetad_paths_test.go` with a row-builder helper
   (`row(op, txg, objid, path, fullPath) EventRow` + option mutators for
   every pointer field).
2. Tests: CREATE with full_path `edge/deep.txt` -> Key resolved, exact
   `edge/deep.txt`; RENAME with both full paths -> Key+OldKey resolved;
   CREATE with NULL full_path (gap-lost ancestor) -> Key = bare `deep.txt`,
   resolved=false; op casing CREATE->create, UNKNOWN->unknown; captured_at
   set -> Timestamp = that unix second UTC; captured_at NULL + plausible
   hrtime -> hrtime wall clock; both NULL/implausible -> zero time.
3. Run (fail), implement rowsToEvents, run (pass).

### 2. TDD: size mapping

1. Tests: TRUNCATE size=0/old_size=100 -> SizeOld=100, SizeNew=0 (zero
   new-size is honest, not absent - pointer semantics); SETATTR size=50 ->
   SizeNew=50; WRITE io_bytes=4096 -> SizeNew=4096; CREATE with size set ->
   sizes stay zero (print_event parity: sizes meaningful for truncate/io
   ops only); all-NULL sizes -> zero, nil pointers never fabricated.
2. Run (fail), implement, run (pass).

### 3. TDD: conservative match (rowsMatchKey)

1. Tests: resolved row matches ONLY exact key; partial row matches bare
   equality AND `/bare` suffix (key `edge/deep.txt` vs bare `deep.txt`);
   resolved row does NOT match a different key sharing the bare name
   (exactness is a filter, not a broadener); OldKey participates with its
   own resolved/partial rule.
2. Run (fail), implement rowsMatchKey reusing bareMatchesKey, run (pass).

### 4. TDD: drift guard against the CLI parser

1. Test: rows built from the recorded testdata fixtures (test-only
   converter rawEvent->EventRow: full_path = whatever parseEventsOutput
   resolved, captured_at NULL) produce IDENTICAL ObjectEvent slices to
   `parseEventsOutput` on the same fixture - op casing, keys, sizes,
   timestamps. This pins that the v5 fast path and the legacy parser agree
   on every fixture we have; if upstream resolution ever diverges from
   our old reconstruction on recorded data, this test fails.
2. Run (fail), fix until pass.

### 5. Gate check

`make test && make lint` green.

## Interface Contract (Exposed to Siblings)

Exactly Contract 2 of master.md. The rawEvent->EventRow test converter
stays in the test file (not exported).

## Self-Verification Checklist

- [ ] `go test ./internal/metadata/ -run 'RowsToEvents|RowsMatch'` green.
- [ ] Drift guard passes on every testdata fixture.
- [ ] No graph-walk code exists in this file (no map build, no parent
      chain, no root detection).
- [ ] Timestamp policy: captured_at > plausibleWallClockTime > zero.
- [ ] No changes to zfs_events.go, zfs_cmd.go, metadata.go.
- [ ] make test && make lint green.

## Review Checklist (for review agent)

- [ ] Contract 2 signatures verbatim.
- [ ] SCHEMA.md section 7 rules honored: full_path authoritative, NULL =
      PARTIAL bare name, never fabricated, never hidden; comments cite it.
- [ ] bareMatchesKey/plausibleWallClockTime REUSED, not duplicated.
- [ ] Resolved rows match exact-only (no broadening); partial rows match
      conservatively (tests prove both directions).
- [ ] Size mapping matches the op-constrained column table (SCHEMA.md 2.1).
- [ ] Drift guard converter is test-only.

## Do NOT commit

The orchestrator stages and commits after review.
