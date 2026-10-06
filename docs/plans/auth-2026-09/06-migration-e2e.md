# Migration, Back-Compat Tests + E2E Case - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** The integration layer: migration/back-compat proof (env-only
  deployments unchanged), plus `scripts/e2e/cases/18-auth-identities.sh` —
  the AGENTS.md-mandated e2e coverage for this tree's user-facing config
  surface (`identities`, `auth`), proving multi-identity + grant enforcement
  over the real wire from the S3 frontend.
- **Dependencies:** leaves 01, 02, 03, 05 (all merged). This leaf adds no new
  product behavior — it locks in what exists.
- **Estimated Context:** 70K
- **Concurrency Group:** C (after all B leaves)

## Goal

Two acceptance criteria close here:

1. **Migration path** (issue criterion): an existing deployment with only
   `ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY` (or nothing set →
   zetaadmin) keeps working with zero config change and zero behavioral
   drift. Proven at unit level (leaf 01 tests) AND end-to-end here: launch
   the real binary with env-only credentials and run a representative
   request set that must succeed exactly as before, plus unknown-key
   rejection that must produce the same S3 error as before.
2. **AGENTS.md e2e rule**: this tree ships a new user-facing config surface,
   so `scripts/e2e/cases/18-auth-identities.sh` lands in the SAME change,
   covering: multi-identity auth (two keys → two identities), per-identity
   grants (readonly denial on write), and the dev-mode loud banner.

## Context

Key files to understand before implementing:

- `scripts/e2e/run-e2e.sh` + `Makefile` `e2e` target: harness builds the
  server, generates temp certs, starts on a free port, runs every
  `cases/*.sh` in lexical order. New case must be self-contained (private
  server pattern) so case-boundary relaunch logic keeps working.
- `scripts/e2e/lib.sh` helpers: `assert_eq`, `assert_contains`,
  `assert_status`, `aws_ok`, `aws_fail_status`, `aws_cap`,
  `launch_expect_fail`, `wait_for_port`. Signed requests go through
  `aws_ok`-style curl `--aws-sigv4 "aws:amz:us-east-1:s3" --user "AK:SK"`.
- Precedent for a case that launches its OWN server with custom config/env:
  `cases/15-backends-config.sh` (mktemp dirs, own port via python3 socket,
  own certs via openssl, cleanup trap, TERM-then-KILL shutdown). COPY that
  skeleton — including the `BKT=` bucket convention and create/cleanup
  pairing.
- `cases/16-frontends-config.sh` — the config-surface e2e precedent.

## Interface Contracts (From Parent)

No new Go surface. This leaf's "contract" is the e2e case inventory:

| Scenario (case 18) | Client | Must |
|---|---|---|
| 18a env-only back-compat | curl `--aws-sigv4` with `$ZETAOBJECT_ACCESS_KEY` pair | full object round-trip OK; behavior identical to case 02 slice |
| 18b multi-identity | same server, config `identities`: `ak-rw` (wildcard readwrite) + `ak-ro` (`readonly` on the case bucket) | both keys authenticate; object round-trip under `ak-rw` OK |
| 18c grant denial | `ak-ro` signs a PUT | HTTP 403 with `AccessDenied` in body (`aws_fail_status`); GET (read) still OK |
| 18d unknown key | curl signs with unregistered key | 403 `InvalidAccessKeyId` (unchanged error) |
| 18e dev mode loud | second private server, config `{"auth":{"mode":"none"}}` | unsigned request succeeds; server log greps "AUTHENTICATION DISABLED" and per-request WARNING line |
| 18f invalid config fails loud | `launch_expect_fail` with duplicate accessKey in identities | startup aborts, error names the duplicate |

Unit-level back-compat additions (Task 1) pin the same guarantees below e2e
so failures localize.

## Tasks

### Task 1: Unit-level migration/back-compat locks

**Files:**
- Test: `config_migration_test.go` (new, package main) — only if leaf 01's
  `config_identities_test.go` leaves gaps; extend it instead of duplicating.
  Gaps to verify exist and close:
  - full startup with env-only + config WITHOUT `identities` produces a
    registry of exactly one `"env"` wildcard identity (no extra identities).
  - `ZETAOBJECT_*` and `MINIS3_*` both resolve to the same env identity
    (Z wins when both set — existing rule preserved through EnvPair).
- Test: `internal/frontend/s3/migration_test.go` — golden assertion that the
  S3 error bytes for unknown-key and bad-signature are UNCHANGED vs the
  literal strings in this leaf's Context of the pre-tree code
  (`InvalidAccessKeyId` / `SignatureDoesNotMatch` bodies). If leaves 02/03
  already covered these, reference, don't duplicate.

**Steps:** write tests first (they should PASS if B leaves are correct — this
task is a lock, not new behavior; a failure means a B leaf regressed the
contract — STOP and report, do not patch product code beyond this leaf's
scope). Run, record green.

### Task 2: E2E case `18-auth-identities.sh`

**Files:**
- Create: `scripts/e2e/cases/18-auth-identities.sh`

**Step 1: Write the case FIRST** (it is the failing spec), following the
case-15 private-server skeleton:

1. mktemp root/work/cert dirs; free port via python3; openssl self-signed cert.
2. Generate server config JSON in the work dir with: `dataDir` at mktemp
   root, `identities`: `ak-rw` (wildcard readwrite) + `ak-ro`
   (readonly on `B18_BKT`), fake-but-realistic secret keys (obviously fake,
   e.g. `secret-18-ro-not-real`).
3. **18a env-only leg:** launch server with ONLY env credentials (no config
   identities), `curl --aws-sigv4` PUT/GET/DELETE object round-trip →
   `assert_status` 200/204. Kill server.
4. **18b–18d identities leg:** relaunch with the identities config. `ak-rw`
   round-trip OK; `ak-ro` GET OK and PUT → `assert_status` 403 +
   `assert_contains "AccessDenied"`; unregistered key → 403 +
   `InvalidAccessKeyId`.
5. **18e dev-mode leg:** relaunch with `auth.mode: "none"`, capture server
   log to file; UNSIGNED request (plain curl -k) succeeds; `grep -q
   "AUTHENTICATION DISABLED" "$LOG"` and a per-request WARNING line.
6. **18f fail-loud leg:** `launch_expect_fail` with a config containing a
   duplicate accessKey across identities → expect the named-duplicate error.
7. Cleanup trap removes all temp dirs and children; keep create/cleanup
   paired per the harness convention.

**Step 2:** `make e2e` → the new case FAILS or the product behaves wrong
(legit — B leaves may have integration gaps). Triage each failure to its
owning leaf, report to the orchestrator; fix product code ONLY if the bug is
in this leaf's own case script.

**Step 3:** iterate until `make e2e` is fully green (all 18 cases).

### Task 3: Coverage floors final re-measure

**Files:**
- Modify: `Makefile` COVER_MIN and/or `.github/workflows/check.yml` floor
  comments — final numbers after ALL tree code landed.

**Steps:** `make test-cover-enforce`; set the floor to measured-rounded-down
with the ratchet comment naming this tree. This is the tree's last chance to
satisfy the AGENTS.md floors-move-with-code rule; state the before/after
numbers in the report.

## Self-Verification Checklist

- [ ] `make e2e` fully green INCLUDING `18-auth-identities.sh`
- [ ] `make check` green (build, vet, fmt, lint, test, cover floor)
- [ ] Back-compat proven twice: unit (Task 1) + e2e leg 18a
- [ ] The ≥2-frontends criterion: leg 18b–18c (SigV4/S3) + leaf 03's
      neutrality test both reference the same registry — state in the report
      which proofs ran
- [ ] Case follows harness conventions: `BKT=` bucket var, create/cleanup
      pairing, private-server pattern, no hardcoded ports/paths
- [ ] No real credentials anywhere; fake secrets in config fixtures
- [ ] Floor changes have measured before/after numbers + ratchet comments

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] All six e2e legs present and passing (18a–18f)
- [ ] Unit locks added where leaf 01/02 tests had gaps (no duplication)
- [ ] Floors re-measured with comments; no unexplained floor drops
- [ ] Case script is hermetic (own dirs/port/certs, cleanup on failure paths)
- [ ] AGENTS.md e2e rule satisfied for BOTH new config keys
      (`identities`, `auth.mode`) — 18e covers `auth`

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- This leaf owns the tree's e2e obligation. If a B leaf "shipped a feature
  without e2e", the fix is here, not a new leaf.
- If the harness reveals an ordering dependency (e.g. config file path env
  `ZETAOBJECT_CONFIG` needed for the private server), use what cases 14–16
  use — do not invent new harness mechanics.
