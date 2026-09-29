# Test-Gap Closure Plan — 2026-09-28

> Note: this tree predates the project rename; "mini-s3" in the text below is now "zeta-object".

Source: coverage audit after the hardening campaign (72.7%, race-clean).
Goal: close all 11 identified gaps, take coverage to ~85%+, bump COVER_MIN.

## Module facts (unchanged from hardening campaign)

- Module `mini-s3`, repo root, single package main. Go 1.27. No new deps.
- Baseline gates at dispatch: 210 unit tests + 101 e2e asserts green,
  make lint 0, race clean, staticcheck clean. Do not regress any.

## Frozen test seams (declared once; do not re-declare)

```go
// actions.go gets ONE test seam: a package-level command runner var.
// runCommand keeps its signature; executeAction/executeInactivityAction call
// runCommandVia(action) which defaults to runCommand. Tests swap
// actionCommandRunner = func(name, cmd string, timeout int, workDir string)
// and restore with defer. No interface types.
var actionCommandRunner = runCommand
```
```go
// multipart_handlers.go: sweeper decomposition
func sweepAllBucketsOnce() int  // one pass over dataDir + custom buckets; testable
// startMultipartExpirySweeper stays the goroutine wrapper (interval const,
// default 1h; tests drive sweepAllBucketsOnce directly)
```

## Conventions (all leaves)

1. TDD: RED evidence before GREEN where the gap is behavioral; pure
   coverage-completion tests just need GREEN + proof they exercise the target
   (report the coverage delta from `go tool cover -func`).
2. Table-driven; `t.Run` subtests; `t.Setenv`/`t.TempDir`/`t.Cleanup`.
3. Any seam change to production code is the MINIMUM listed above — no
   refactors beyond it.
4. Do NOT commit. Report files, coverage delta for your scope, gate output.
5. STOP clause: a gap requiring production changes beyond the frozen seams —
   stop and report.

## Tree

- 4.1-actions-runtime.md — gap 1 (executeAction/triggerActions/inactivity)
- 4.2-sweeper.md — gap 2 (sweepAllBucketsOnce seam + tests)
- 4.3-routing.md — gap 3 (dispatch matrix through rootHandler)
- 4.4-multipart-errors.md — gap 4+5 (multipart/initiate/complete error paths)
- 4.5-copy-delete-edges.md — gap 6 (copy/delete edge matrix)
- 4.6-storage-atomic.md — gap 7 (writeFileAtomic cleanup paths)
- 4.7-fuzz.md — gap 8 (4 fuzz targets + make fuzz)
- 4.8-concurrency-stress.md — gap 9 (race stress suite)
- 4.9-auth-negative.md — gap 10 (auth negative-fuzz corpus)
- 4.10-floor.md — gap 11 lite: bench for listObjectsFromKeys + COVER_MIN bump

## Dispatch order

W1 (parallel): 4.1, 4.2, 4.3  (disjoint: actions.go / multipart_handlers.go / dispatch tests)
W2 (parallel): 4.4, 4.5, 4.6  (4.4 after 4.2 lands — same file)
W3 (parallel): 4.7, 4.8, 4.9  (new test files + Makefile)
W4: 4.10 + integration gate

## File ownership (one writer per file per wave)

| Leaf | Owns |
|---|---|
| 4.1 | actions.go (seam), actions_test.go, inactivity_runtime_test.go (NEW) |
| 4.2 | multipart_handlers.go (seam only), multipart_sweeper_test.go (NEW) |
| 4.3 | routing_test.go (NEW) — no production edits |
| 4.4 | multipart_handlers_test.go, multipart_handlers.go (test-only fixes if a RED proves a bug) |
| 4.5 | copy_batch_test.go, object_handlers.go (test-only fixes if RED proves a bug) |
| 4.6 | storage_atomic_test.go (NEW), storage.go (test-only fixes if RED proves a bug) |
| 4.7 | fuzz_test.go (NEW), Makefile (fuzz target) |
| 4.8 | concurrency_stress_test.go (NEW) |
| 4.9 | auth_negative_test.go (NEW) |
| 4.10 | bench_test.go (NEW), Makefile (COVER_MIN), docs/plans (this file) |

## Tracking table

| Leaf | Status | Commit | Notes |
|---|---|---|---|
| 4.1 | COMPLETE | 933aa25 | executeAction 0->100%; timer drain pinned |
| 4.2 | COMPLETE | 933aa25 | sweepAllBucketsOnce 0->91.7% |
| 4.3 | COMPLETE | 933aa25 | dispatch trio + handleACL 100%; zero findings |
| 4.4 | COMPLETE | daf60a8 | multipart error matrix; key symbols +10-20pts |
| 4.5 | COMPLETE | daf60a8 | copy/delete edges; deleteObjectHandler 100% |
| 4.6 | COMPLETE | daf60a8 | rename-cleanup without euid skip; +race fix in actions_test |
| 4.7 | COMPLETE | 28dfecd | 4 fuzz targets + make fuzz; 1 unreachable-domain crasher pinned |
| 4.8 | COMPLETE | 28dfecd | 2 REAL races found and fixed (delete-vs-PUT prune, create-vs-delete bucket) |
| 4.9 | COMPLETE | 28dfecd | 121 mutations, zero accepted |
| 4.10 | COMPLETE | (uncommitted) | bench_test.go (4 benches) + COVER_MIN 50->70 (actual 83.6%) + make bench; all gates green |


## Completion

All 10 leaves COMPLETE as of 2026-09-28. Final: 306 unit tests + 101 e2e
asserts, coverage 83.6% (floor raised 50 -> 70), make check/lint/e2e green,
2 production races found by the stress suite and fixed.
