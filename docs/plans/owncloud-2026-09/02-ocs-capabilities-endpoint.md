# OCS Capabilities Endpoint - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Create `internal/frontend/owncloud`: the `owncloud` Frontend
  wrapping the webdav frontend, the OCS router (`/ocs/v1.php`, `/ocs/v2.php`),
  and the capabilities/user/config endpoints, with XML responses pinned by
  table tests and honest capability advertisement.
- **Dependencies:** leaf 01 (observed subset + client URL scheme), the landed
  `internal/frontend/webdav` package, the landed auth-2026-09 Basic adapter.
- **Estimated Context:** 90K
- **Concurrency Group:** B

## Goal

The desktop client's first contact with the server is OCS: it probes
`/status.php`-style config and `/ocs/v{1,2}.php/cloud/capabilities` to decide
what the server can do, then commits to WebDAV for data. This leaf builds
exactly that negotiation surface and nothing more.

Semantic rule (repo-wide): the capabilities document advertises ONLY what
zeta-object serves. A client that trusts a false capability flag will send
requests we cannot honor — silent emulation. Every flag in the document is
derived from real state (`ProtocolCaps`, provider probe, auth config), never
hardcoded.

## Context

Key facts before implementing:

- The observed request set from leaf 01 (`capture/request-log.md`,
  `decision.md` §3) is the SCOPE. Endpoints in this leaf's tasks marked
  "[observed: …]" assume the classic-client baseline; prune or extend to the
  log's actual set. An endpoint the log does not show is NOT implemented.
- OCS XML envelope (classic server):
  ```xml
  <?xml version="1.0"?>
  <ocs>
    <meta>
      <status>ok</status>
      <statuscode>200</statuscode>
      <message>OK</message>
    </meta>
    <data>
      <!-- endpoint-specific -->
    </meta>
  </ocs>
  ```
  (v2 failures use HTTP status mapping; v1 historically returned 200 with
  statuscode in the envelope — match what the log shows the client handling.)
- Content-Type: `text/xml; charset=UTF-8` (classic) — pin per log.
- `frontend.Frontend` seam (landed):
  `Name() string; Handler() http.Handler; Authenticator() auth.Authenticator;
  Capabilities() ProtocolCaps` with `ProtocolCaps{Buckets, Versioning,
  ConditionalReads, Multipart, PresignedURLs}`.
- Frontends call only `internal/backend.Backend` — this leaf adds no storage
  access; the webdav frontend already holds the Backend reference.
- Capability shortfalls: `frontend.ErrCapability` / `IsCapabilityError`
  (internal/frontend/caps.go).

Key files to understand before implementing:
- [master.md](../master.md) — Contracts 1–3 (FROZEN), Coding Conventions
- `decision.md` + `capture/request-log.md` from leaf 01 — the observed subset
- `internal/frontend/frontend.go`, `internal/frontend/conformance.go` — the
  seam and the conformance suite this frontend must pass (leaf 05 wires it;
  do not break it here)
- `internal/frontend/webdav/` (landed) — the wrapped data plane

## Interface Contracts (From Parent)

Implement master.md Contracts 1–3 verbatim: `CapabilitiesDoc` +
`BuildCapabilities(version, caps, opts)` (pure function), `OCSRouter`
(both `/ocs/v1.php` and `/ocs/v2.php` prefixes; documented OCS error envelope
for unimplemented endpoints), and `Frontend` with `Name() == "owncloud"`
wrapping the webdav frontend.

## Tasks

### Task 1: Package skeleton + Frontend composition

**Objective:** `internal/frontend/owncloud` with the Frontend type wrapping
webdav and routing OCS paths.

**Files:**
- Create: `internal/frontend/owncloud/owncloud.go`
- Test: `internal/frontend/owncloud/owncloud_test.go`

**Step 1: Write failing test**

- `New(wd, opts)` returns a Frontend with `Name() == "owncloud"`.
- `Handler()` serves `/ocs/v1.php/config` and `/ocs/v2.php/config` (after
  Task 2 exists — write the test asserting delegation now: a GET to a
  non-OCS path, e.g. `/remote.php/webdav/`, is served by the wrapped webdav
  handler; a stub webdav handler records that it received the request).
- `Capabilities()` returns the composed caps (stub webdav reports
  `ProtocolCaps{Buckets: false, Versioning: false, ConditionalReads: true}` →
  composed matches, `Versioning` forced false while no provider attached).

**Step 2:** `go test ./internal/frontend/owncloud/ -run TestFrontend` → FAIL
(package missing).

**Step 3: Write minimal implementation** — `Frontend` struct per Contract 3,
path-prefix switch in `Handler()`: `/ocs/` → OCSRouter, everything else →
wrapped webdav Handler. [observed: confirm from leaf 01 whether the client
uses `/remote.php/webdav/` or `/remote.php/dav/files/user/`; route both
prefixes if both appear in the log.]

**Step 4:** test → PASS.

### Task 2: OCS envelope + router + error responses

**Objective:** XML encoding helpers, request dispatch, and the documented
OCS error envelope for endpoints not implemented.

**Files:**
- Create: `internal/frontend/owncloud/ocs.go`
- Create: `internal/frontend/owncloud/xml.go`
- Test: `internal/frontend/owncloud/ocs_test.go`

**Step 1: Write failing tests**

- Envelope: encoding a response produces the exact `<ocs><meta>…` byte shape
  (golden-string compare; statuscode 200/404/997 cases).
- Routing: `GET /ocs/v1.php/config` and `GET /ocs/v2.php/config` both reach
  the config handler; `GET /ocs/v1.php/cloud/capabilities` likewise.
  Unknown endpoint (e.g. `/ocs/v2.php/apps/files_sharing/api/v1/shares`)
  → 404 with OCS error envelope whose message names the degradation.
- v2 status mapping: a not-found on v2 yields HTTP 404; on v1 yields HTTP 200
  with `<statuscode>404</statuscode>` [confirm against log; classic v1
  behavior is the default if the log is ambiguous].
- Method mismatch on a known path → 405 with OCS envelope.

**Step 2:** FAIL. **Step 3:** implement envelope writer (encoding/xml,
no reflection tricks), router table, error responses. **Step 4:** PASS.

### Task 3: Capabilities document

**Objective:** `BuildCapabilities` + the `/config` and `/cloud/capabilities`
handlers, deriving every flag from real state.

**Files:**
- Create: `internal/frontend/owncloud/capabilities.go`
- Test: `internal/frontend/owncloud/capabilities_test.go`

**Step 1: Write failing tests** (table-driven, golden XML)

- Baseline document (no provider, Basic auth) contains: `version` block
  (major/minor/micro/string pinned as constants — declare zeta-object's
  owncloud-compat version, e.g. major 1 mirroring classic 10.x line ONLY if
  leaf 01's log shows the client version-gating on it; otherwise omit), and
  capability blocks for what the webdav data plane truly provides
  (files: bigfilechunking false, versioning absent, etc. — the exact flag set
  comes from the log: emit what the client reads, omit the rest).
- With a metadata provider attached (fake), the versioning capability block
  appears; with nil, it must not. (Leaf 03 fills in the block's content;
  here it is a boolean presence test.)
- Flags can never lie: assert `BuildCapabilities` output for a frontend whose
  `Capabilities().Versioning` is false contains no `files.versioning` node.
- Both `/config` and `/cloud/capabilities` serve the same document
  [confirm from log; classic serves both].

**Step 2:** FAIL. **Step 3:** implement per Contract 1 as a pure function;
handlers marshal it. **Step 4:** PASS.

### Task 4: User metadata endpoint

**Objective:** `/ocs/v{1,2}.php/cloud/user` returning the minimal user shape.

**Files:**
- Create: `internal/frontend/owncloud/user.go`
- Test: `internal/frontend/owncloud/user_test.go`

**Step 1: Write failing tests**

- Authenticated request (Basic headers; auth adapter stubbed at the seam the
  webdav frontend already uses — do NOT invent auth here) returns XML with
  the user's identity fields the client needs [observed set from log:
  typically `id`, `display-name`, `email`; emit only observed fields].
- Unauthenticated → OCS 401 envelope [map per v1/v2 rule from Task 2].

**Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS.

### Task 5: Conformance suite instantiability (smoke only)

**Objective:** prove the owncloud frontend satisfies the seam.

**Files:**
- Test: `internal/frontend/owncloud/conformance_test.go`

**Step 1: Write failing test** — `frontend.RunConformanceSuite(t, frontend,
frontend.ConformanceOptions{})` over the owncloud frontend compiles and
passes. **Step 2:** FAIL (if composition wrong). **Step 3:** fix composition.
**Step 4:** PASS. (Deeper conformance + e2e is leaf 05; do not expand here.)

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] `go test ./internal/frontend/owncloud/... -v` fully passing
- [ ] Master Contracts 1–3 satisfied: names, file paths, `Name() == "owncloud"`
- [ ] OCS envelope byte-shape pinned by golden tests; v1/v2 status mapping
      per leaf 01's log
- [ ] Every capability flag derived from state — no hardcoded aspirational
      flags (grep for the flag names: they appear only in capabilities.go)
- [ ] Unimplemented OCS endpoints → documented OCS error envelope, tested
- [ ] Only endpoints from leaf 01's observed subset implemented
- [ ] No storage access: package touches only the webdav frontend + auth
      adapter + `internal/frontend` types
- [ ] `gofmt -l` empty; `go vet ./internal/frontend/owncloud/` clean
- [ ] No debug artifacts, no TODOs

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Notes

- The scope discipline here is the whole point: implement the observed
  subset, not the classic server. If you find yourself adding a shares
  endpoint "because it is easy," stop — that is exactly the speculative
  surface issue #3 excludes.
- The `version` block's values are a compatibility shim decision; document
  the chosen values and rationale in capabilities.go's comment so leaf 06 can
  mirror them in the compatibility matrix.
- If leaf 01 ruled `/status.php` observed, add it as a minimal handler in
  Task 3 (json: `{installed, maintenance, version, …}` per log) — it is OCS-
  adjacent but often the FIRST probe; skipping it fails account setup.
