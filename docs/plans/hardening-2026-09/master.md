# mini-s3 Hardening & Feature Plan — master

> Note: this tree predates the project rename; "mini-s3" in the text below is now "zeta-object".

Created: 2026-09-27. Source: `.hermes/audits/2026-09-27-bughunt-report.md` (12 HIGH,
~27 MED, ~17 LOW) + feature-gap analysis. Mandate: fix all findings, port meept-grade
Go tooling, implement feature gaps, e2e for all features.

## Module facts (frozen)

- Module path: `mini-s3`. Root-level module. Single binary. Go 1.27.
- NO new third-party dependencies without orchestrator approval (stdlib only today).
- Single package `main`. Test files stay package main at repo root.

## File ownership map (post-split, frozen — one writer per file per wave)

| File | Contents | Owned by wave |
|---|---|---|
| main.go | main(), signal handling, http.Server wiring | 2.6 |
| config.go | ServerConfig, loadConfig, credentials, constants, getEnvOrDefault | 2.1 moves; 2.6 edits |
| types.go | ObjectMetadata, MultipartUpload, PartMetadata, XML structs | 2.1 moves; per-leaf edits per contract |
| sigv4.go | canonical URI/query/headers, payload hash, authenticateRequest, decodeAWSChunked, hmac helpers | 2.1 moves; 2.2 owns; 3.2/3.4 own later |
| bucket_handlers.go | listBuckets/create/delete/location/head, getBucketPath, bucketExists, validateBucketName | 2.1 moves; 2.4 owns |
| object_handlers.go | get/put/head/delete object, validateObjectKey, cleanupEmptyDirs | 2.1 moves; 2.4 owns; 3.1 owns later |
| multipart_handlers.go | initiate/uploadPart/complete/abort, getMultipartLock | 2.1 moves; 2.3 owns; 3.3 owns later |
| xml.go | errorToXML, writeS3Error, XML response structs | 2.1 moves; 2.4 owns |
| actions.go | bucket actions subsystem | 2.5 owns |
| storage.go | NEW: atomic-write helpers (contract below) | 2.3 creates+owns |
| main_test.go / actions_test.go / main_handler_test.go | existing tests | 2.2 owns main_test.go; 2.4 owns main_handler_test.go; 2.5 owns actions_test.go |
| e2e/ | NEW: e2e suite | 3.6 owns |

Concurrency contract (frozen): per-key write serialization via
`var objectLocks sync.Map` + `func lockObject(path string) func()` in storage.go
(LoadOrStore *sync.Mutex, lock, return unlock func). Multipart uses existing
getMultipartLock.

## Frozen helper contracts (declared once, all leaves code against them)

```go
// storage.go (leaf 2.3)
func writeFileAtomic(path string, data []byte, perm os.FileMode) error
// temp file in same dir (path+".tmp-<rand>"), write, Sync, Close, Rename.
func writeFileAtomicJSON(path string, v any, perm os.FileMode) error // MarshalIndent+writeFileAtomic
func lockObject(path string) func() // returns unlock func
```

Leaves must use these instead of os.WriteFile for ALL object-data and metadata JSON
writes. Leaf 2.4 codes against these names in wave 2b while 2.3 lands them in wave
2a — do not re-declare, do not stub.

## Conventions (all leaves)

1. Table-driven tests; new behavior gets a test that fails before the fix (RED) and
   passes after (GREEN). Report RED/GREEN evidence in the leaf report.
2. Error responses use writeS3Error with real S3 error codes and statuses.
3. XML responses: `xml.Header` + `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`
   on every document (leaf 2.4 establishes the pattern in xml.go).
4. No drive-by refactors beyond the assigned contract. No renames of exported test
   names. gofmt clean.
5. Leaves do NOT commit. Report: files changed, RED/GREEN evidence, gate output
   (go build/vet/test -count=1), deviations from contract.
6. STOP clause: any integration bug outside your owned files that you cannot work
   around — stop and report with the exact unblock condition.

## Gates

- Per-leaf: `go build ./...`, `go vet ./...`, `go test ./... -count=1` green.
- Orchestrator integration gate after each wave: those + `make lint` (new code only
  blocks; pre-existing backlog tracked separately) + `make check` once tooling lands.
- Final: `make check` + `make test-cover-enforce` + full e2e suite green.

## Tree

- 1.1-tooling.md — meept-grade tooling port (Makefile, .golangci.yml, hooks, gitleaks)
- 1.2-lint-sweep.md — full lint backlog to zero (after all code waves)
- 2.1-file-split.md — mechanical main.go split (prerequisite for 2.2-2.6)
- 2.2-sigv4-fixes.md — auth security + canonical fixes
- 2.3-storage-atomicity.md — storage.go helpers + multipart fixes
- 2.4-handler-semantics.md — object/bucket handler fixes + XML conformance
- 2.5-actions-fixes.md — actions.go injection + glob/JSON5/timer fixes
- 2.6-server-lifecycle.md — timeouts, graceful shutdown, config, scripts
- 2.7-docs.md — CLAUDE.md/README/gap-closure reality sync
- 3.1-range-conditional.md — Range + If-* requests
- 3.2-presigned.md — presigned URL auth
- 3.3-multipart-lifecycle.md — ListUploads/ListParts/expiry/min-part-size
- 3.4-streaming-integrity.md — chunk-signature chain + trailers
- 3.5-copy-batch-delete.md — CopyObject + DeleteObjects
- 3.6-e2e-suite.md — full-feature e2e harness + gate

## Dispatch order

W1 (parallel): 1.1, 2.1
W2a (parallel): 2.2, 2.3, 2.5, 2.6
W2b (parallel): 2.4
W3 (parallel): 2.7, 1.2, then integration gate
W4 (parallel): 3.1, 3.2, 3.3
W5 (parallel): 3.4, 3.5
W6: 3.6, final gates

## Tracking table

| Leaf | Status | Commit | Notes |
|---|---|---|---|
| 1.1 | COMPLETE | 6d43ddb | tooling stack landed; make lint backlog=44 (leaf 1.2) |
| 1.2 | COMPLETE | cd938cd | 39→0; +pre-commit-errors w.Write exemption & blocking-exit fix |
| 2.1 | COMPLETE | 6d43ddb | split verified 60/60 identical; +staticcheck cleanups |
| 2.2 | COMPLETE | 14574d8 | +orchestrator: getSigningKey region-for-service regression fixed w/ golden test |
| 2.3 | COMPLETE | 14574d8 | storage.go frozen contract landed |
| 2.4 | COMPLETE | 9ed81b2 | +orchestrator: uploadPart VerifyDecodedLength wiring; xmlns-constant pin test |
| 2.5 | COMPLETE | 14574d8 | |
| 2.6 | COMPLETE | 14574d8 | live smoke: TLS1.2 floor, SIGTERM clean, PORT wiring proven |
| 2.7 | COMPLETE | 2b79f1e | all code claims grep-verified |
| 3.1 | COMPLETE | be7e862 | 29 tests; multi-range→200 documented |
| 3.2 | COMPLETE | be7e862 | live CLI presign verified; scope duplication fixed |
| 3.3 | COMPLETE | be7e862 | sweeper + EntityTooSmall; marker semantics |
| 3.4 | COMPLETE | 4d5143a | AWS golden vectors pinned; scope fix in main_test.go by orchestrator |
| 3.5 | COMPLETE | 4d5143a | live CLI copy/batch verified |
| 3.6 | COMPLETE | d3eb35e | 101 e2e asserts green; 3 product bugs found+fixed |


## Completion

All 16 leaves COMPLETE as of 2026-09-28. Final gates: 210 unit tests +
101 e2e asserts green, make lint 0, race clean, staticcheck clean,
live AWS CLI smokes green (basic, presign, copy/batch).

## Test Gate

<!-- Tests-first gate (hierarchical-planning skill v1.3.0, patched 2026-10-24).
     Coverage matrix derived mechanically from leaf task headers; the control
     agent (orchestrator, in-session) refines rows and fills test names at
     Phase 0 dispatch. No implementation leaf dispatches until every row is
     VALIDATED. Delta protocol: lesser of plan/tests updates to the greater.
     Recipe: skill hierarchical-planning -> references/tests-first-gate.md -->

| # | Feature (from plan) | Source | Unit tests | E2E tests | Status |
|---|---------------------|--------|-----------|-----------|--------|
| F1 | Leaf 1.1 — Tooling port (meept-grade) | 1.1-tooling.md | (pending) | (pending) | PENDING |
| F2 | Leaf 1.2 — Lint backlog paydown to zero | 1.2-lint-sweep.md | (pending) | (pending) | PENDING |
| F3 | Leaf 2.1 — Mechanical file split | 2.1-file-split.md | (pending) | (pending) | PENDING |
| F4 | Leaf 2.2 — SigV4 security + canonical fixes | 2.2-sigv4-fixes.md | (pending) | (pending) | PENDING |
| F5 | Leaf 2.3 — Storage atomicity + multipart data-integrity fixes | 2.3-storage-atomicity.md | (pending) | (pending) | PENDING |
| F6 | Leaf 2.4 — Object/bucket handler semantics + XML conformance | 2.4-handler-semantics.md | (pending) | (pending) | PENDING |
| F7 | Leaf 2.5 — Actions subsystem fixes | 2.5-actions-fixes.md | (pending) | (pending) | PENDING |
| F8 | Leaf 2.6 — Server lifecycle + config + scripts | 2.6-server-lifecycle.md | (pending) | (pending) | PENDING |
| F9 | Leaf 2.7 — Docs reality sync | 2.7-docs.md | (pending) | (pending) | PENDING |
| F10 | Leaf 3.1 — Range + conditional requests | 3.1-range-conditional.md | (pending) | (pending) | PENDING |
| F11 | Leaf 3.2 — Presigned URL authentication | 3.2-presigned.md | (pending) | (pending) | PENDING |
| F12 | Leaf 3.3 — Multipart lifecycle: ListUploads, ListParts, expiry, part rules | 3.3-multipart-lifecycle.md | (pending) | (pending) | PENDING |
| F13 | Leaf 3.4 — Streaming integrity: chunk-signature chain + trailers | 3.4-streaming-integrity.md | (pending) | (pending) | PENDING |
| F14 | Leaf 3.5 — CopyObject + DeleteObjects (batch) | 3.5-copy-batch-delete.md | (pending) | (pending) | PENDING |
| F15 | Leaf 3.6 — E2E suite (all features, AWS CLI) | 3.6-e2e-suite.md | (pending) | (pending) | PENDING |

Statuses: PENDING | DELTA | VALIDATED

Delta log: (none yet)
