# Docs: README, Compatibility Matrix, Config Keys - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Docs-only leaf. Do NOT commit — the orchestrator handles git operations
> after review.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** User-facing documentation for the ownCloud frontend: README
  section, compatibility matrix (supported / degraded / unsupported), config
  keys, and the decision-doc cross-link.
- **Dependencies:** leaves 01–05 landed (docs describe what ships, not what
  was planned).
- **Estimated Context:** 40K
- **Concurrency Group:** E (last)

## Goal

Issue #3's first acceptance criterion is "decision documented: which
client(s) are the compatibility target, which subset implemented." The
decision doc (leaf 01) holds the full version; user-facing docs must surface
the honest summary: what works, what degrades, what does not exist — so an
ownCloud user can decide in one screen whether zeta-object serves them.

## Context

- Repo docs convention: README carries a short protocol section per frontend
  with config examples; deeper rationale lives in `docs/plans/` — link, do
  not duplicate.
- The compatibility matrix is the centerpiece. Three honest columns:
  **Supported** (works with the tested client version), **Degraded** (client
  functions but a feature is absent — e.g. versioning without ZFS, no
  shares), **Unsupported** (endpoint rejected with a documented error).
- Follow the webdav frontend's README section structure (from
  webdav-2026-09) for consistency: intro, config, auth, usage, limitations.

## Tasks

### Task 1: README ownCloud section

**Files:**
- Modify: `README.md`

**Content:**
1. One-paragraph position statement including the honest-value framing:
   ownCloud protocol support exists to serve concrete ownCloud client
   deployments; it is a WebDAV superset, not a full ownCloud server.
2. Config example: `frontends: [{"type": "owncloud", "listenAddr": …}]`
   (exact shape per the landed registry/config wiring).
3. Auth: what credentials work (Basic; app-passwords only if leaf 04 ran),
   referencing auth-2026-09's config surface.
4. Quickstart: point an ownCloud desktop client at the listener; the four
   operations that work (connect, sync up, sync down, delete) and the tested
   client version(s) from decision.md.
5. Link to `docs/plans/owncloud-2026-09/decision.md` for the full subset
   rationale.

### Task 2: Compatibility matrix

**Files:**
- Create: `docs/owncloud-compatibility.md`

**Content:** a table with three sections (Supported / Degraded /
Unsupported). Rows come from decision.md's subset + degradation lists:
- Supported: OCS capabilities negotiation, user metadata, WebDAV data plane
  (PROPFIND/GET/PUT/DELETE/MKCOL/COPY/MOVE), Basic auth, versions (only if
  leaf 03 ran AND a metadata provider is attached — note the conditional).
- Degraded: versioning without ZFS provider (not advertised, probes error
  cleanly), any observed-but-partial client behaviors from the log.
- Unsupported: shares, provisioning/accounts, interactive login flow (if
  ruled out), anything else on decision.md's list — each with the wire
  behavior a user will see (404 + OCS envelope).
Include: tested client version(s), the "other versions untested" caveat, and
a dated last-verified line.

### Task 3: Config keys documentation

**Files:**
- Modify: wherever the repo documents config keys (README config section or
  `docs/` config reference — match where webdav-2026-09 documented its keys).

**Content:** every new key/env var this tree introduced (expected: only the
`owncloud` frontend type entry; auth keys belong to auth-2026-09's docs —
link them). AGENTS.md counts config surface as user-facing: confirm the e2e
cases from leaf 05 exercise exactly the documented keys.

### Task 4: Decision-doc cross-link + plan-tree bookkeeping

**Files:**
- Modify: `docs/plans/owncloud-2026-09/master.md` — move leaf statuses to
  COMPLETE only if the orchestrator has not already (check first; if the
  orchestrator owns the table, leave it).

If this leaf is executed by an agent, do NOT edit master.md — the
orchestrator updates the tracking table. Just verify the decision doc links
back from README and the matrix, and note any dead links found.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] README section: position statement, config example, auth, quickstart,
      decision-doc link
- [ ] `docs/owncloud-compatibility.md`: three honest sections, tested
      version(s), untested-version caveat, dated
- [ ] Every matrix row traceable to decision.md / the request log
- [ ] No doc promises a feature the code does not ship (cross-check each
      "Supported" row against landed handlers)
- [ ] Config keys documented match what e2e exercises
- [ ] Links resolve (relative paths correct from each file's location)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Notes

- The matrix is a trust artifact: an ownCloud admin reading "Degraded" must
  know exactly which client feature disappears. Vague rows ("mostly works")
  are a defect; cite the log.
- If leaf 03 was SKIPPED, versioning rows move to Unsupported with the
  "not probed by the tested client; revisit if a client demands it" note —
  that keeps the door visible without building.
