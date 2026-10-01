# Parity Gate + E2E Coverage - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Extend the parity gate to the zmetad provider; add the e2e
  case for ?events served from a fixture zmetad DB (AGENTS.md e2e rule);
  pin the v5 loss/swap wire semantics and full_path-first key serving.
- **Dependencies:** 03-provider.md, 04-wiring-config.md.
- **Estimated Context:** ~55K (explore 15K + generate 20K + iterate 10K + overhead 10K)
- **Concurrency Group:** E
- **Upstream references:** SCHEMA.md sections 4, 6, 9; zfs-metadata #5 #8 #9 #10 (resolved)

## Goal

Two guarantees, both test-backed:

1. Parity: a ZFS bucket served by the zmetad provider returns IDENTICAL
   core S3 metadata responses as a plain-FS bucket (providers never alter
   canonical behavior), and the `?events` envelope shape matches
   Contract 5 exactly (dataset, recordsLost, ringSwaps, events[]).
2. E2E: `scripts/e2e/cases/18-zmetad-events.sh` exercises ?events on a
   bucket whose provider reads a real (fixture-built) SQLite database -
   built by a tiny Go helper before server start, since e2e hosts lack
   ZFS and zmetad. The purge test doubles as the `zmetad --purge`
   contract check: when the binary is absent, the endpoint fails
   cleanly (502-class error), never silently no-ops.

## Context

- internal/metadata/parity_test.go - the existing gate; read its scenario
  shape and extend, do not restructure.
- internal/backend/conformance/conformance.go - shared request-set runner.
- scripts/e2e/lib.sh - assert helpers; 17-events-endpoints.sh is the
  direct template (BKT convention, create/cleanup pairing).
- scripts/e2e/cases/ numbered lexically - 18 is next.
- The e2e harness builds the server with production wiring; the fixture
  DB must exist BEFORE server start and its path rides in config.json
  (zmetad_db_path from leaf 04).
- Fixture builder: scripts/e2e/fixtures/zmetad-fixture/main.go - creates
  the SCHEMA.md v5 layout and inserts a canned set using
  modernc.org/sqlite (already a dependency after leaf 01). Canned set
  MUST be the row-form of an existing testdata fixture so expected
  outputs are copy-pasteable from unit tests - including full_path values
  matching what leaf 02's drift guard derives - PLUS: one gaps row
  lost=7 (known loss), one gaps row lost=-1 (ring swap), one row with
  NULL captured_at (legacy style), one nested create with NULL full_path
  whose ancestor is ABSENT (conservative-match case), and a datasets row
  mapping the e2e bucket dir (mountpoint = the bucket path inside the
  temp dataDir).

## Interface Contracts (From Parent)

Contract 5 (pin in tests): JSON envelope keys exactly
`dataset`,`recordsLost`,`ringSwaps`,`events`; per-event
`op,key,oldKey,txg,timestamp,sizeOld,sizeNew`; XML
`IsLossy`,`RecordsLost`,`RingSwaps`; recordsLost = knownLost ONLY
(never folded with swaps); IsLossy = recordsLost > 0 OR ringSwaps > 0.

## Tasks

### 1. TDD: parity extension

1. Extend parity_test.go: scenario "zmetad-provider bucket" - register
   the zmetad provider (stub openDB via leaf 03's seam) against a bucket
   path; run the SAME request set as the plain-FS scenario; diff
   complete responses (headers + body). Core metadata responses MUST be
   byte-identical; ?events exists only on the provider side (503 on
   plain-FS, 200 on provider bucket).
2. Run. A failure here is a REAL regression from leaves 01-04 - report
   it, fix the CODE, never the parity test.
3. Wire-shape pin: JSON-decode an ?events response; assert the exact
   Contract 5 key set (including ringSwaps); decode the ext XML;
   assert IsLossy/RecordsLost/RingSwaps with the canned fixture's
   values (RecordsLost=7, RingSwaps=1, IsLossy=true).

### 2. Fixture builder program

1. Write scripts/e2e/fixtures/zmetad-fixture/main.go: output path arg;
   v5 schema; canned set above; exit 0. stdlib + driver only.
2. Smoke test: runs it on t.TempDir, opens with OpenZmetadDB, asserts
   GapStats{7,0,1} and the nested-key conservative case - green.

### 3. TDD: e2e case 18

1. Copy 17-events-endpoints.sh to 18-zmetad-events.sh; adapt:
   before server start, `go run scripts/e2e/fixtures/zmetad-fixture
   $TMP/zmetad.db`; write zmetad_db_path into the temp config.json;
   assert: ?events 200 with dataset + recordsLost=7 + ringSwaps=1;
   key-scoped ?events on the nested key returns the create via
   conservative match (bare name); ?events&versions XML carries
   IsLossy=true, RecordsLost=7, RingSwaps=1; unsigned request 403.
2. Purge-path assertion: POST/GET the purge endpoint with
   zmetad_binary pointing at a stub script (a 3-line shell script in
   $TMP that logs its argv and exits 0 - NO real zmetad on e2e hosts)
   -> endpoint succeeds and the stub received `--purge <dataset>`.
   Then a negative case: zmetad_binary=/nonexistent -> endpoint error,
   server stays healthy (subsequent ?events still 200).
3. If `go run` cannot prime the driver on the host, the case SKIPS
   (exit 0, skip message) - graceful-skip convention; never fail the
   suite on environment limits.
4. `make e2e` green (long first build).

### 4. Coverage floors

Re-measure `go test ./internal/metadata/ -cover` after all leaves; update
COVER_MIN in the Makefile per the AGENTS.md rule (measured value rounded
down, ratchet comment) if and only if the aggregate moved.

### 5. Full gate

`make check` green. Report numbers: unit test count, coverage %, e2e
case count.

## Interface Contract (Exposed to Siblings)

- scripts/e2e/fixtures/zmetad-fixture/ - fixture builder (path arg, v5
  schema, canned set documented in its header comment).
- scripts/e2e/cases/18-zmetad-events.sh - the e2e case.
- parity_test.go scenario "zmetad-provider bucket".

## Self-Verification Checklist

- [ ] make check green (unit + lint + e2e).
- [ ] Parity scenario proves core-S3 byte-identity with zmetad attached.
- [ ] Wire-shape pin asserts the exact Contract 5 key set incl. ringSwaps.
- [ ] Case 18: fixture values (7/1/true) asserted on the wire; purge stub
      and negative case present; graceful skip verified.
- [ ] COVER_MIN updated only if the floor moved, with ratchet comment.

## Review Checklist (for review agent)

- [ ] AGENTS.md e2e rule satisfied: config surface (zmetad_db_path,
      zmetad_binary) and provider behavior both covered by case 18.
- [ ] Fixture rows traceable to a recorded testdata fixture (not
      invented) plus the explicitly listed synthetic loss rows.
- [ ] Case 18 keeps the BKT convention and create/cleanup pairing.
- [ ] No parity weakening: diff set still covers complete responses.
- [ ] Purge stub proves argv shape (--purge <dataset>) without a real
      zmetad; negative case proves clean failure + server health.
- [ ] Numbers reported: unit count, coverage %, e2e case count.

## Do NOT commit

The orchestrator stages and commits after review.
