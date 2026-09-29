# Coverage-100 Plan — 2026-09-28

Mandate: raise test coverage so every package ships with a meaningful,
near-100%-of-REACHABLE coverage floor and a 100% test pass rate. CI floors
per package raised to measured-actual minus small margin, then ratcheted.

## Reality check on "100%"

Go statement coverage of TRUE 100% is reachable only by testing every line,
including Go error branches that cannot be forced without fault injection
(e.g. `rand.Read` failure, `Sync` on a closed fd). Mandate interpreted as:
**cover every REACHABLE line; provably-dead/unreachable branches documented
with why; floors set at the achieved per-package number minus 2.**

## Current state (measured 2026-09-28)

| Package | Coverage | Notes |
|---|---|---|
| mini-s3 (root) | 78.2% | 8 funcs < 50% |
| internal/auth | 0.0% | 29 lines total, NO tests |
| internal/backend | 94.1% | |
| internal/backend/conformance | 77.8% | |
| internal/backend/fsbackend | 85.6% | |
| internal/frontend | 71.1% | |
| internal/frontend/s3 | 28.9% | leaf-02 extraction, tests copied thin |
| internal/metadata | 86.4% | |
| internal/objectmodel | 92.7% | |

Note: another session is actively migrating root handlers into
internal/frontend/s3 (leaf-02 extraction). Root coverage work is confined
to the REMAINING root-only code (actions, bench, root-only glue) — do not
duplicate tests for handlers that now live in s3/ (their home is there).

## Leaves (disjoint scopes, 3 waves)

- 6.1-auth.md — internal/auth 0→100% (29 lines; the priority package)
- 6.2-s3-frontend.md — internal/frontend/s3 28.9→85%+ (the big one; use the
  copied root tests as seeds — they test the same code)
- 6.3-frontend-backend.md — internal/frontend 71→90%+, internal/backend/
  conformance 78→95%
- 6.4-root-misc.md — root package 78→85%+ (actions paths, remaining glue)
- 6.5-floors.md — set every floor to achieved-2, verify, done

## Wave order

W1: 6.1, 6.2 (parallel)
W2: 6.3, 6.4 (parallel)
W3: 6.5 + integration gate

## Rules

- Cover REACHABLE lines; unreachable branches get `//nolint` nothing —
  document in the leaf report why no test can reach them.
- Tests assert BEHAVIOR (status, bytes, error codes), not line execution.
- Reuse existing test helpers per package (setupTestEnv, signFn, etc).
- Any RED (test proves a bug) → STOP-clause finding, report, fix if trivial.
- Do NOT commit; orchestrator commits per leaf.
- Gates per leaf: go build/vet/test green, -race green on new tests,
  make lint 0, coverage delta reported via go tool cover -func.

## Tracking

| Leaf | Status | Commit | Coverage after |
|---|---|---|---|
| 6.1 | PENDING | | |
| 6.2 | PENDING | | |
| 6.3 | PENDING | | |
| 6.4 | PENDING | | |
| 6.5 | PENDING | | |
