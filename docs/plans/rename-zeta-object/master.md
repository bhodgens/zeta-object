# Rename: mini-s3 → zeta-object

> **For Hermes:** Execute leaf-by-leaf with the subagent-driven-development skill,
> or hand each leaf to a fresh implementation agent. Leaves are ordered; 01 gates
> the rest.

**Goal:** Rename the project, module, binaries, env-var prefix, and documentation
from mini-s3 / minis3 / MINIS3_* to zeta-object / zetaobject / ZETAOBJECT_*,
with zero behavior change.

**Architecture decision (recorded here so implementers do not relitigate it):**

- Go identifier and module path: `zetaobject` (no hyphen; Go packages drop hyphens).
  Module: `module github.com/bhodgens/zeta-object`. Import paths become
  `github.com/bhodgens/zeta-object/internal/...`.
- Shell, docs, prose, GitHub repo name: `zeta-object` (hyphenated).
- Env-var prefix: `ZETAOBJECT_` (env vars cannot contain hyphens). Backward
  compatibility: keep `MINIS3_*` as a deprecated fallback, checked only when the
  `ZETAOBJECT_*` counterpart is absent, for two minor releases. This preserves
  existing scripts, cron jobs, and deploy configs.
- Binary names: `zeta-object-server` (and `zeta-object` if the client binary is
  still used; check `Makefile` targets).

**Scale verified 2026-09-29:**

- `module mini-s3` in `go.mod`; 101 internal import statements across 8 internal
  packages reference `mini-s3/internal/...`.
- 123 files (excluding `vendor/` and `coverage.out`) contain `mini-s3`,
  `minis3`, or `MINIS3`; 31 of them are plan docs under `docs/plans/`.
- Env prefix `MINIS3_` appears in 12 Go files and 13 script files.
- `Makefile` builds `mini-s3-server` and runs `goimports -local mini-s3`.
- Git remote: `git@github.com:bhodgens/mini-s3.git`.

**Ordering rule:** rename code and scripts first (leaves 01-03), keep everything
green with `make check`; docs and plan trees last (leaves 04-05) so no leaf
edits docs that a later code leaf also touches.

---

## Leaf 01: Module, imports, binaries, env prefix (code)

**Scope:** All Go code, `go.mod`, `Makefile`, `scripts/`. No docs.

**Mechanical steps (run, then verify with the checks below):**

1. `go.mod`: `module mini-s3` → `module github.com/bhodgens/zeta-object`.
2. Sed across tracked `.go` files (excluding `vendor/`), in this order:
   - `"mini-s3/internal/` → `"github.com/bhodgens/zeta-object/internal/`
   - `MINIS3_` → `ZETAOBJECT_`
   - `minis3` → `zetaobject` (covers any stray identifiers)
3. Env backward-compat shim: in `config.go` (and any other direct
   `os.Getenv("ZETAOBJECT_...")` call sites — grep for them), wrap lookup in a
   helper:
   ```go
   // envOr returns the ZETAOBJECT_-prefixed variable, falling back to the
   // deprecated MINIS3_-prefixed variable. Remove the fallback in v0.next+2.
   func envOr(zetaKey, legacyKey string) string {
       if v := os.Getenv(zetaKey); v != "" {
           return v
       }
       return os.Getenv(legacyKey)
   }
   ```
4. `Makefile`: `BINARY_NAME := mini-s3-server` → `zeta-object-server`;
   both `goimports -local mini-s3` occurrences → `-local github.com/bhodgens/zeta-object`.
5. `scripts/`: `MINIS3_` → `ZETAOBJECT_` and `mini-s3-server` →
   `zeta-object-server` in all 13 files (list verified: `.gitleaks.toml`,
   `conformance/baseline.txt`, `conformance/run-conformance.sh`,
   `conformance/s3tests.conf`, `e2e/lib.sh`, `e2e/run-e2e.sh`,
   `e2e/cases/{11,14,15,16}-*.sh`, `install-hooks.sh`, `test-s3-full.sh`,
   `test-s3-operations.sh`). Keep one `MINIS3_` mention per file only where the
   script documents the fallback explicitly (e.g. a comment); otherwise drop.

**Verification (all must pass):**

- `grep -rn "mini-s3\|minis3\|MINIS3" --include='*.go' --include='*.sh' \
  --include='*.toml' --include='*.conf' Makefile scripts/ | grep -v vendor`
  → only intentional fallback comments remain.
- `go build ./...` clean. `make test` green. `make lint` green. `make check` green.
- `make e2e` green (harness builds `zeta-object-server`, all 17 cases pass).

**Commit:** `refactor: rename module, binaries, env prefix to zeta-object (MINIS3_ fallback kept)`

---

## Leaf 02: Repo-level config and tests that assert the old names

**Scope:** Test fixtures and any `config.json.example`, testdata, or hardcoded
expectations referencing the old binary/env names that Leaf 01's blanket sed
missed or that assert literals.

**Steps:**

1. `grep -rn "MINIS3\|mini-s3" --include='*_test.go' .` — for each hit, decide:
   - Test asserts the fallback works → keep, and add a mirror test asserting
     `ZETAOBJECT_` wins when both are set.
   - Test asserts the new name → Leaf 01 should have converted it; fix stragglers.
2. Update `config.json.example` and any testdata env names.
3. Add one focused test: `TestEnvPrefixFallback` in `config_frontend_test.go`
   style — set both `ZETAOBJECT_ACCESS_KEY` and `MINIS3_ACCESS_KEY`, assert the
   zeta value is used; set only `MINIS3_ACCESS_KEY`, assert it is used.

**Verification:** `go test ./...` green; the new fallback test passes both ways.

**Commit:** `test: cover ZETAOBJECT_ env prefix with MINIS3_ fallback`

---

## Leaf 03: GitHub repo rename + remote flip

**Scope:** Repository identity. Do AFTER leaves 01-02 are pushed, because the
module path (`github.com/bhodgens/zeta-object`) only resolves once the repo
renames.

**Steps (user-performed actions marked [USER]):**

1. [USER] GitHub → mini-s3 → Settings → rename to `zeta-object`. GitHub redirects
   the old URL automatically; CI badges and clones keep working during the
   transition window.
2. `git remote set-url origin git@github.com:bhodgens/zeta-object.git`
3. Verify: `git ls-remote origin` succeeds; `go build ./...` still clean
   (module path now matches canonical location).
4. Check `.github/workflows/*` for hardcoded `mini-s3` URLs; update.
5. Local dir rename is optional and user's call: `~/git/mini-s3` → `~/git/zeta-object`
   (Hermes session cwd will need re-pointing afterward).

**Verification:** `git ls-remote origin` OK; one CI run green on the renamed repo.

**Commit:** none required (remote/workflow edits only, commit as
`chore: point CI and remote at renamed repo`).

---

## Leaf 04: User-facing docs

**Scope:** `README.md`, `docs/frontends.md`, `docs/gap-closure.md`,
`docs/conformance/2026-09-28-matrix.md`, `config.json.example`,
`bucket-actions.sample.json5`, root `AGENTS.md`/`CLAUDE.md` if they name the
project.

**Steps:**

1. Blanket replace in docs: `mini-s3` → `zeta-object`, `MINIS3_` →
   `ZETAOBJECT_`, `mini-s3-server` → `zeta-object-server`.
2. Rewrite the README "Why mini-s3?" section header to "Why zeta-object?" and
   add one paragraph: the zeta prefix honors the OpenZFS event-history metadata
   capability; "object" names the neutral object model every protocol frontend
   shares.
3. Add a deprecation note table where env vars are documented:
   `MINIS3_*` accepted as fallback until two minor releases after the rename.

**Verification:** `grep -rn "mini-s3\|MINIS3" README.md docs/ | grep -v plans/`
→ only the intentional fallback/deprecation mentions remain.

**Commit:** `docs: rename project to zeta-object across user-facing docs`

---

## Leaf 05: Plan trees and internal history docs

**Scope:** 31 files under `docs/plans/` (hardening, conformance,
metadata-zfs, backend-interface, frontend-interface trees) plus
`docs/plans/issue-*.md`.

**Steps:**

1. Blanket replace `mini-s3` → `zeta-object`, `MINIS3_` → `ZETAOBJECT_`,
   `minis3` → `zetaobject` in all 31 plan files.
2. Do NOT rewrite history semantics: plan docs describe work done under the old
   name; add a one-line banner at the top of each tree's `master.md`:
   `> Names below refer to "mini-s3", the former project name (now zeta-object).`
   — then a blanket replace is unnecessary inside leaf bodies. Prefer the banner;
   replace only `master.md` headers and any doc an agent will execute later.
   Decision rule: if a plan leaf is COMPLETE (per its tree's status), banner it;
   if still actionable, rename in place.
3. `git log --oneline | head` sanity: history stays untouched.

**Verification:** `grep -rln "mini-s3" docs/plans/` returns only files carrying
the explicit old-name banner.

**Commit:** `docs(plans): mark former project name in plan trees`

---

## Out of scope (explicit)

- Docker/Helm artifacts: none exist in-repo today; revisit at first release.
- The `test-bucket/` and `data/` fixtures: no name references found; leave alone.
- Homebrew/release packaging: not yet published.

## Risk notes

- The sed order in Leaf 01 matters: import paths first, then env prefix, then
  bare identifiers. Doing bare `mini-s3` → `zeta-object` globally would corrupt
  import paths (hyphen is illegal in Go imports).
- The GitHub redirect makes Leaf 03 low-risk, but external links in issues/PRs
  should be spot-checked after rename.
- Coverage floors: rename touches no logic, but re-run `make test` with coverage
  and confirm floors still hold before pushing (AGENTS.md rule).
