# Conformance Instantiation + E2E Cases - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Owncloud frontend conformance instantiation, the AGENTS.md
  e2e/wire-level cases for the OCS surface (curl-based), and coverage-floor
  re-measurement if code moved packages.
- **Dependencies:** leaves 02 (mandatory) and 03/04 (whichever ran) landed.
- **Estimated Context:** 70K
- **Concurrency Group:** D

## Goal

Two gates, per the AGENTS.md hard rule ("every new user-facing feature MUST
ship e2e or wire-level test coverage in `scripts/e2e/` in the SAME change"):

1. **Conformance:** the owncloud frontend runs
   `frontend.RunConformanceSuite` and passes — proof it satisfies the
   frontend seam like every other frontend.
2. **Wire-level e2e:** curl-driven OCS + WebDAV round-trips prove the
   protocol surface works against a running server, not just unit fakes.

Real-client verification is explicitly a MANUAL gate documented by the
orchestrator (master.md Integration Test Plan §4) — the harness cannot run
the Qt client; do not fake it in CI.

## Context

- E2E harness: `make e2e` → `scripts/e2e/run-e2e.sh`; cases live in
  `scripts/e2e/cases/*.sh` (lexical order), helpers in `scripts/e2e/lib.sh`
  (`assert_eq`, `assert_contains`, fail counters, no `set -e`). Conventions:
  copy an existing numbered case, keep `BKT=` bucket convention and
  create/cleanup pairing, private-server pattern from case 16 for
  frontend-specific listeners.
- Case 16 (`16-frontends-config.sh`) is the model for: dedicated listener
  config, `frontends` JSON, fail-loud unknown-type assertions. The
  known-types list will contain `owncloud` after leaf 02/05 wiring — the
  fail-loud error text in case 16 greps `known: [s3]`; that assertion must
  be updated HERE if the registry list changed (check the landed state).
- Frontend registration + config key shape follow webdav-2026-09's leaf for
  listener mapping (master.md Contract 5); do not invent new config keys.
- Coverage floors: if any package move happened in this tree, re-measure and
  update floors in the SAME change (AGENTS.md rule) — set to measured value
  rounded down with a ratchet-back comment.

## Tasks

### Task 1: Conformance instantiation (TDD)

**Objective:** The owncloud frontend passes the shared suite.

**Files:**
- Modify: `internal/frontend/owncloud/conformance_test.go` (extends leaf 02's
  smoke test)

**Step 1: Write failing test** — `RunConformanceSuite` over the owncloud
frontend built from its real constructor + a stubbed/staging Backend, with
`ConformanceOptions` appropriate to its declared caps. If the suite's
capability-gated checks (mountability, conditional-read reachability) fail
against the composition, fix the composition (in leaf 02's files, noted in
deviations) — never weaken the suite. **Step 2:** FAIL if broken. **Step
3:** fix. **Step 4:** PASS. Full `go test ./internal/frontend/...` green.

### Task 2: E2E case — OCS capabilities wire surface

**Objective:** curl-only proof of the OCS negotiation surface.

**Files:**
- Create: `scripts/e2e/cases/18-owncloud-ocs.sh`

**Step 1: Write the case (it fails before the feature — but the feature
landed in leaf 02, so this case is written to pin behavior):**

- Private-server pattern (case 16): owncloud/webdav frontend on its own
  port, TLS cert in mktemp dir, cleanup trap, free-port helper.
- `curl -k -u user:pass https://…/ocs/v1.php/config` → HTTP 200, body
  contains `<status>ok</status>` and the versioning block matches the
  provider-less baseline (ABSENT — assert `<versioning>` not in body).
- Same for `/ocs/v2.php/cloud/capabilities`.
- Unauthenticated request → 401 (v2 mapping) per leaf 02's pinned rule.
- Unimplemented OCS endpoint from decision.md's degradation list → 404 with
  OCS error envelope (assert message substring).
- `assert_contains`/`assert_eq` helpers only; create/cleanup pairing; `BKT=`
  convention for any bucket used (reuse the webdav bucket mapping from the
  webdav-2026-09 doc for the data-plane part).

**Step 2:** run `make e2e` — new case green, ALL prior cases green.

### Task 3: E2E case — OCS + WebDAV sync round-trip

**Objective:** The full client workflow in wire form: negotiate via OCS,
then sync up/down/delete over WebDAV.

**Files:**
- Create: `scripts/e2e/cases/19-owncloud-sync-roundtrip.sh`

**Step 1: Write the case:**

- Capabilities probe first (mirrors the real client's first contact).
- MKCOL a directory; PUT two files with distinct bodies; PROPFIND Depth 1 —
  assert both names present.
- GET both files; byte-compare (`cmp -s`) against local copies (sync down).
- Overwrite one file (PUT new body), GET back, compare (sync up update).
- DELETE one file; PROPFIND again — assert absence (delete propagation).
- Negative: versioned probe (only if leaf 03 ran): versions list endpoint
  returns the documented no-provider error; with provider out of scope for
  e2e (no ZFS in harness), assert the DEGRADED wire behavior — that is the
  honest contract the harness can pin.
- Cleanup: DELETE the directory tree; assert 404 on final PROPFIND.

**Step 2:** run `make e2e` — green; prior cases untouched.

### Task 4: Coverage floors + gates

**Objective:** The repo's gates stay truthful.

**Files:**
- Modify (only if needed): coverage floor comments in the Makefile /
  CI workflow per AGENTS.md.

**Steps:**
1. `make test-cover-enforce` (or equivalent floor target) — if this tree's
   package additions moved aggregate coverage, re-measure, set floors to
   measured-rounded-down with the ratchet comment, in this change.
2. `make vet`, `make fmt-check`, `make lint` clean.
3. Re-run `make e2e` end-to-end one final time; record pass counts.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] `frontend.RunConformanceSuite` runs for the owncloud frontend and passes
- [ ] New e2e cases follow harness conventions (helpers, pairing, BKT=,
      private-server, cleanup traps)
- [ ] Both new cases green in `make e2e`; zero prior-case regressions
- [ ] Degradation behavior asserted on the wire (no versioning advertised;
      unimplemented endpoints → documented envelope)
- [ ] Coverage floors truthful (updated in this change iff moved)
- [ ] No debug artifacts in cases (no set -x residue, no hardcoded ports)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Notes

- Case numbering: `18`/`19` assume the current lexical tail is `17-…`;
  renumber to the next free slots if siblings took numbers — the order is
  the contract, not the digits.
- The real-client manual pass is NOT automatable here; its checklist lives
  in decision.md (leaf 01) and the orchestrator executes it at integration
  review. Say so in the case comments so nobody "fixes" the gap later.
