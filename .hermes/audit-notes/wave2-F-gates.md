# Wave-2 F — gate-integrity audit (CI / e2e / script gates). HEAD=7f4a544.

Read-only. Assigned findings H12, M28, M29, M30, M33, M34, M35, L36.
Assigned fix commits inspected: 99c2179, d366078, 7f4a544, abf547e.

## Method / evidence gathered

- `git show`/`git diff <hash>^ <hash>` on all four commits.
- `bash scripts/ci/test-ci-floor-guard.sh` (real) -> 13 passed / 0 failed, exit 0.
- Mutated copies run from /tmp (never the repo copy):
  - M29: copy of test-ci-floor-guard.sh with a `go` stub returning nothing
    -> `FAIL go list ./... returned NO packages` + `CI FLOOR GUARD: FAIL`, exit 1.
  - M30: the exact Makefile guard snippet under `sh -c` with a no-match pattern
    -> `FAIL ... parity gate gating nothing`, exit 1; with the real pattern
    -> `parity: 5 matched`, exit 0. (objectmodel pattern = 3 matches.)
- M28: reproduced run-one.sh lines 111-125 in /tmp (stub lib.sh + stub case
  setting E2E_PASS=7) -> generated `.case-body.sh` writes `/tmp/.../.case-tally`
  = `7 0 0`. The tally handoff works at HEAD.
- `cd zeta-cache && CGO_ENABLED=0 go test -count=1 -race ./...` (the exact new
  CI command) -> 8 test packages ok, exit 0, 5.6s.
- `bash -n` on all touched scripts: all OK.
- Case/trap cross-ref over 41 cases: only case 13 changed in this wave.
- gitleaks git-mode over the 16 unpushed commits (`--log-opts=HEAD~16..HEAD`,
  the CI push-range shape) -> no leaks, exit 0. (A `--no-git` working-tree scan
  finds 78 hits, but all in the UNTRACKED, non-shipped `vendor/s3-tests/**`.)

## New-gate-hole sweep

- New zeta-cache CI job DOES run `-race` and `-count=1` (check.yml:70). Good.
- Floor-guard derives `wf_floors` from the workflow's own case block
  (`grep -cE '\) +FLOOR=[0-9]+'` = 24) and compares against `go list` count
  (24) — parses the right list, root module only.
- No new e2e case was added; case 13 gained an EXIT trap (scoped: cases run in
  a nested `bash -c` at run-e2e.sh:277 / `bash "$CASE_SCRIPT"` at
  run-one.sh:125, so it does not clobber the suite trap).
- `e2e-report-test` / `ci-floor-test` are NOW called in CI (check.yml:235).

## Per-finding disposition

- H12 = PARTIAL. check.yml:61-70 adds a real test job and the exact command is
  verified green. But no coverage floor exists for zeta-cache anywhere and
  test-ci-floor-guard.sh is root-module-only, so the "floor + floor-guard case"
  half of the finding is unimplemented.
- M28 = CLOSED. run-one.sh:156-163 three-way verdict (SKIP when cp==0 or
  cs!=0); tally handoff proven to write `7 0 0` at HEAD. Residual: no pin.
- M29 = CLOSED. test-ci-floor-guard.sh:74-81 fails on an empty `go list`; proven
  by the /tmp mutation (exit 1).
- M30 = CLOSED. Makefile:257-271 counts matches via `go test -list`; proven to
  exit 1 on zero matches.
- M33 = CLOSED. case 13 TMPD now /tmp/zetaobject-e2e.13-mc.* + EXIT trap
  (13:55-57); s3req body now /tmp/zetaobject-e2e.s3req.* (lib.sh:66); both
  inside the /tmp/zetaobject-e2e.* sweep (e2e-cleanup.sh:94).
- M34 = PARTIAL. cases 03/05 gate on the CLI exit code, but the sentinel is a
  SEPARATE `aws s3api list-*` invocation from the one feeding the absence
  assert, so a transient failure of the captured call still yields a false pass.
- M35 = CLOSED. Teardown moved into the EXIT trap (run-zfs-validation.sh:71-89),
  registered before the first die() (:98), gated on KEEP_SERVER.
- L36 = CLOSED. check.yml:234-235 runs `make e2e-report-test ci-floor-test`.
- abf547e = CLOSED (test-only): the OCFileID determinism pin now compares two
  live calls through wrapOCFileID (propfind_test.go:319-335); still falsifiable.
- 7f4a544 = CLOSED (test-only): waitTimerSettled (loops_test.go:56-76) removes
  the -race flake; no gate hole introduced.

## Findings (detail)

F1 MEDIUM check.yml:61-70 — H12 half-closed; zeta-cache job has no coverage
  floor / floor-guard. Fix: -coverprofile + awk floor in the job; generalize
  test-ci-floor-guard.sh to modules.
F2 MEDIUM cases/03:59-74 (also :76-81) and cases/05:69-81 — M34 sentinel is a
  distinct invocation from the asserted capture. Fix: tie the guard to the
  captured call's exit status (`if LIST4=$(aws ...); then ...`).
F3 LOW cases/13:52-57 — new trap names only $TMPD; 5 sibling mktemp files
  (e2e13-mc/shimerr/put/range/big) rely on inline rm, contradicting the case's
  own comment + AGENTS.md rule 1. Backstopped by /tmp/e2e[0-9]*.
F4 LOW run-one.sh:156-163 — verdict/handoff unpinned; test-skip-reporting.sh
  pins only run-e2e.sh's duplicate fold_verdict.
F5 LOW test-ci-floor-guard.sh:46,61 — guard is root-module-only; a second
  module (zeta-cache) is invisible to it — the H12/G9 blind spot.
F6 LOW check.yml:69-70 — CI duplicates the Makefile command instead of calling
  `make zeta-cache-test-race`; drift risk between two definitions.
