# App-Password / Token Auth (CONDITIONAL) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat.
>
> **CONDITIONAL LEAF:** Dispatch ONLY if leaf 01's decision.md §5 rules GO
> (the captured login flow demands app-password/token auth beyond Basic).
> If NO-GO (Basic suffices), the orchestrator marks this leaf SKIPPED.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** If the desktop client requires app-password style tokens (as
  newer ownCloud clients do during account setup), extend the auth adapter
  per the auth-2026-09 identity model so the owncloud frontend accepts them.
  This leaf CONSUMES the auth-2026-09 identity/token model; it does not
  invent one.
- **Dependencies:** leaf 01 (ruling + observed login flow), leaf 02
  (frontend composition landed), auth-2026-09 LANDED (token model + Basic
  adapter).
- **Estimated Context:** 60K
- **Concurrency Group:** C

## Goal

Issue #3: "app-password style tokens if the client requires them." The
condition is decided by discovery, not preference. When GO: the client's
account-setup flow (typically a login URL → token exchange dance, or
username + app-password over Basic) must succeed against zeta-object using
the auth-2026-09 token machinery. When the client is satisfied with plain
Basic against the shared credentials, this leaf never runs and the Basic
adapter from leaf 02 stands.

## Context

- auth-2026-09 delivers the pluggable authenticator registry, the Basic
  adapter, and (per its tree scope) app-password style tokens "if required."
  Its identity model replaces the v1 placeholder `auth.Identity`. This leaf
  wires the owncloud frontend's `Authenticator()` to the token-aware adapter
  and, if the observed flow requires it, the OCS-side token endpoints.
- The frontend never validates credentials itself — all auth flows through
  `frontend.Frontend.Authenticator()` (frontend-interface seam rule).
- Observed login flow from leaf 01 decides which of these is needed:
  1. Client sends Basic with user + app-password → token-aware adapter
     validation only; no new endpoints.
  2. Client drives an interactive login flow (opens
     `/index.php/login/flow`-style URLs, polls for a token) → token
     endpoints in the OCS router. This is a bigger surface; if the log shows
     it, flag it to the orchestrator BEFORE implementing — it may change the
     honest-value calculus (interactive login flow is server-clone
     territory; the degradation list must rule on it).

## Interface Contract (From Parent)

Master Contract 3 amendment (this leaf): `Frontend.Authenticator()` returns
the auth-2026-09 token-aware adapter instead of (or wrapping) the Basic
adapter. The swap must be invisible to the rest of the frontend — requests
that authenticated under Basic before this leaf must still authenticate
after (regression-gated by leaf 02's existing tests).

## Tasks

### Task 1: Token-aware adapter wiring (TDD)

**Objective:** The owncloud frontend accepts app-password tokens via the
auth-2026-09 adapter.

**Files:**
- Modify: `internal/frontend/owncloud/owncloud.go` (Authenticator wiring)
- Test: `internal/frontend/owncloud/auth_test.go`

**Step 1: Write failing tests**

- A request with `Authorization: Basic base64(user:APP-TOKEN)` authenticates
  against the token-aware adapter (use the auth-2026-09 test fixtures for a
  valid token; assert Identity flows through to a handler context as leaf 02
  already does for Basic).
- Regression: valid Basic with the server's regular credentials still
  authenticates (both credential classes accepted, per auth-2026-09's
  adapter semantics; if its adapter is token-OR-credential, pin that
  behavior in the test).
- Invalid token → 401 with the OCS envelope (wire shape per leaf 02's
  Task 4 pattern).

**Step 2:** FAIL. **Step 3:** wire the adapter (constructor option, no
hardcoded checks). **Step 4:** PASS.

### Task 2: Interactive token flow endpoints (ONLY if observed)

**Objective:** If leaf 01's log shows the client polling token endpoints
during setup, serve exactly those endpoints.

**Files:**
- Create: `internal/frontend/owncloud/logintoken.go`
- Test: `internal/frontend/owncloud/logintoken_test.go`

**Step 1: Write failing tests** per the observed flow (endpoint paths,
polling semantics, JSON payload shapes from the capture; the flow's
"initiate" half typically requires rendering a page the user approves in a
browser — if the capture shows that, STOP and report to the orchestrator:
the approval UX is outside an unattended server's scope, and the degradation
ruling must be re-made). **Step 2:** FAIL. **Step 3:** implement the minimal
observed surface against auth-2026-09's token service. **Step 4:** PASS.
Skip this task entirely (document in deviations) if the log shows no
interactive flow.

### Task 3: Credentials doc contract

**Objective:** What authenticates is documented in code, not tribal.

**Files:**
- Modify: doc comments in `owncloud.go` / `logintoken.go`

List the accepted credential classes and their source (config keys from
auth-2026-09). Leaf 06 mirrors this into the README config section.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] `go test ./internal/frontend/owncloud/... -v` passing, including
      pre-leaf-02 tests unchanged (Basic regression)
- [ ] No credential checks in this package — only adapter wiring
- [ ] Token endpoints (if any) exactly the observed set
- [ ] `gofmt -l` empty; `go vet` clean; no debug artifacts

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Notes

- If the client works with plain Basic (the common case for self-hosted
  classic targets), this leaf is SKIPPED — that is the expected outcome and
  the simplest one. Do not add token support speculatively.
- Interactive login flows (browser-approval) are a stop-and-escalate: they
  imply server-rendered UX the repo does not own. Report before building.
