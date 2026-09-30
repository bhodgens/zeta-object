# ownCloud Protocol Frontend (OCS + WebDAV extension) - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 6 leaf documents under this node (2 conditional — see Child Index)
- **Scope:** GitHub issue #3 — extend the WebDAV frontend with the ownCloud
  protocol surface (OCS API endpoints) so the official ownCloud desktop sync
  client can connect, sync up, sync down, and delete against zeta-object.
  WebDAV core data transfer is NOT re-implemented here; this tree owns only
  the OCS extensions and the discovery/verification that scopes them.
- **GitHub issue:** `docs/plans/issue-owncloud-frontend.md` (#3)

## Honest value note (read before dispatching anything)

**This frontend ranks below WebDAV and (S)FTP.** It is worth doing only if
there is a concrete ownCloud client deployment to serve — a real user or
environment that will run the official ownCloud desktop/mobile client against
zeta-object. If that consumer does not exist, close issue #3 rather than
building speculatively. The orchestrator MUST confirm the consumer with the
maintainer before dispatching leaf 02 onward. Leaf 01 is cheap enough to run
as a go/no-go evidence gatherer; everything after it is not.

## Dependency ordering (hard gate)

This tree sequences strictly **AFTER** two sibling trees land:

1. `webdav-2026-09` — the WebDAV frontend (`internal/frontend/webdav`,
   PROPFIND/GET/PUT/DELETE/MKCOL/COPY/MOVE, bucket-mapping doc). This tree
   EXTENDS that frontend; if it has not landed, every leaf here is BLOCKED —
   do not improvise a parallel WebDAV implementation.
2. `auth-2026-09` — the pluggable authentication architecture (Basic-auth
   adapter; app-password style tokens if required). Leaf 04 consumes its
   identity model directly.

Additionally: `metadata-zfs-2026-09` is an **optional** dependency for leaf 03
(file versions). Where the ZFS events provider is available, versions list real
history; otherwise the frontend degrades (advertises no versioning capability
over OCS). Leaf 03 is written to work with the provider absent.

## Design decision (encoded, to be confirmed by leaf 01)

**Target: the minimal WebDAV+OCS subset the official ownCloud desktop sync
client actually requires — NOT a full ownCloud server clone, NOT the classic
provisioning API, NOT the oCIS API surface.**

Rationale: the desktop client negotiates server capabilities over OCS
(`/ocs/v1.php`/`/ocs/v2.php` `config` endpoints) and then uses plain WebDAV for
all data transfer. If the client insists on capabilities zeta-object does not
implement (shares, accounts/app management, provisioning), the correct
behavior per the repo's semantic rule is: advertise honestly in OCS
capabilities, document the degradation, and stop — never silent emulation.

Leaf 01 validates this decision empirically against a real client and produces
the observed-request log and the degradation list that scope leaves 02–04.
Leaves 02–04 implement exactly the observed subset, no more.

## Architecture

The ownCloud protocol surface is mounted by extending the WebDAV frontend
(`internal/frontend/webdav`) with an OCS router:

```
client ──► zeta-object server (frontends config)
             ├─ listener: owncloud/webdav frontend
             │    ├─ /ocs/v1.php/**  ─┐
             │    ├─ /ocs/v2.php/**  ─┤─► internal/frontend/owncloud  (OCS router,
             │    └─ /remote.php/**  ─┘   XML encode/decode, capability registry)
             │            └─► internal/frontend/webdav (data plane, unchanged)
             │                    └─► internal/backend.Backend (storage seam)
             └─ (optional) internal/metadata.MetadataProvider (ZFS events) ──► versions
```

New package `internal/frontend/owncloud` implements the `frontend.Frontend`
seam (wrapping or embedding the webdav frontend for the data plane), or — if
the webdav-2026-09 landing shape permits — registers OCS handlers as a
sub-router of the webdav frontend. Leaf 02 pins the exact composition shape
after reading the landed webdav package; either shape must satisfy the same
contract: one `Frontend` named `owncloud`, `Handler()` serving both OCS and
WebDAV paths, calls only `backend.Backend`.

OCS responses are XML (`application/xml`, `<ocs><meta>…</meta><data>…</data></ocs>`
envelope). The capabilities document advertises **exactly** what zeta-object
supports — derived from `frontend.ProtocolCaps` and the metadata provider
probe, never hardcoded aspirational flags.

## Interface Contracts

### Contract 1: OCS capabilities document shape (FROZEN after leaf 01)

```go
// File: internal/frontend/owncloud/capabilities.go
package owncloud

// CapabilitiesDoc is the Go shape of the OCS capabilities XML document.
// Field-for-field it must serialize to:
//   <ocs><meta><status>ok</status><statuscode>200</statuscode>
//   </meta><data><version>…</version><capabilities>…</capabilities></data></ocs>
// Only capability blocks the discovery log (leaf 01) shows the client
// consuming may be populated; everything else is omitted, not emitted as
// false. Advertising a capability zeta-object cannot express violates the
// frontend semantic rule (no silent emulation).
type CapabilitiesDoc struct { /* version + capabilities blocks */ }

// BuildCapabilities derives the document from the frontend's true state:
// frontend.ProtocolCaps of the composed frontend + MetadataProvider probe
// result (versioning) + auth method actually configured. It is a pure
// function of its inputs so table tests can pin the XML byte-for-byte.
func BuildCapabilities(version Version, caps frontend.ProtocolCaps, opts Options) CapabilitiesDoc
```

### Contract 2: OCS request routing (FROZEN)

```go
// File: internal/frontend/owncloud/ocs.go
package owncloud

// OCSRouter dispatches /ocs/v{1,2}.php/** requests. Both version prefixes
// MUST answer identically except for the ocs-version response header and
// (v2) the JSON acceptance rule: a v2 request carrying an
// "OCS-APIRequest: true" header gets XML; a v2 request with Accept:
// application/json MAY get JSON — implement whichever the discovery log
// shows the desktop client sending, defaulting to XML for both.
type OCSRouter struct{ /* handlers by (endpoint, method) */ }

// Endpoints implemented (exactly the observed subset; start set):
//   GET  {v1,v2}.php/config                      → capabilities document
//   GET  {v1,v2}.php/cloud/capabilities          → capabilities document
//   GET  {v1,v2}.php/cloud/user                  → user metadata (subset the
//                                                  client needs: displayname;
//                                                  degrade per discovery)
// Shares/provisioning endpoints: 404 with an OCS error envelope documenting
// the degradation — never a silent empty-success.
```

### Contract 3: Frontend composition (FROZEN)

```go
// File: internal/frontend/owncloud/owncloud.go
package owncloud

// Frontend implements frontend.Frontend with Name() == "owncloud".
// It wraps the webdav frontend: non-/ocs/ and non-/remote.php/dav paths
// delegate to the webdav Handler() unchanged (data plane); OCS paths are
// handled by OCSRouter. Authenticator() returns the auth-2026-09 Basic
// adapter (leaf 04 may swap in the token-aware adapter if discovery
// requires app passwords). Capabilities() returns the composed caps —
// Versioning true ONLY when a MetadataProvider is attached.
type Frontend struct{ /* webdav frontend + OCSRouter + options */ }

func New(wd webdavFrontend, opts Options) *Frontend
```

### Contract 4: Versioning degradation (leaf 03, conditional)

```go
// File: internal/frontend/owncloud/versions.go
package owncloud

// VersionsHandler serves the file-versions endpoints the desktop client
// probes (paths pinned by leaf 01's capture — under /remote.php/dav/ the
// client lists <path> with a versions REPORT or a /versions URI scheme,
// per classic-server convention). Backed by internal/metadata.MetadataProvider
// (zfs-events). Provider nil or probe negative → handlers return the OCS/WebDAV
// error appropriate to the protocol AND Capabilities().Versioning stays false
// so the capabilities document never advertises what is not served.
```

### Contract 5: Registry + config (leaf 05)

```go
// frontend type "owncloud" registers in internal/frontend/registry.go's
// known-types list (same mechanism as "s3"; config key shape follows
// webdav-2026-09's leaf for listener mapping — this tree does not invent a
// new config schema):
//   "frontends": [{"type": "owncloud", "listenAddr": "127.0.0.1:PORT"}]
```

## Child Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | [01-decision-and-client-subset-discovery.md](01-decision-and-client-subset-discovery.md) | leaf | webdav-2026-09 landed, auth-2026-09 landed | 60K | A |
| 02 | [02-ocs-capabilities-endpoint.md](02-ocs-capabilities-endpoint.md) | leaf | 01, webdav-2026-09, auth-2026-09 | 90K | B |
| 03 | [03-versions-endpoint.md](03-versions-endpoint.md) | leaf (**conditional** — skip if 01's log shows the client does not probe versions) | 02, metadata-zfs-2026-09 (optional) | 80K | C |
| 04 | [04-app-password-auth.md](04-app-password-auth.md) | leaf (**conditional** — skip if 01's log shows Basic auth suffices) | 01, 02, auth-2026-09 | 60K | C |
| 05 | [05-conformance-and-e2e.md](05-conformance-and-e2e.md) | leaf | 02 (and 03/04 if dispatched) | 70K | D |
| 06 | [06-docs.md](06-docs.md) | leaf | 01–05 | 40K | E |

**Concurrency groups:** strictly sequential A → B → C → D → E. Leaf 01's
discovery output is the input contract for 02–04; 02 must land before the
conditional leaves (they extend its router); e2e coverage (05) ships with the
last code leaf per the AGENTS.md same-change rule; docs (06) close the tree.

**External dependencies (whole tree):** webdav-2026-09 and auth-2026-09 MUST
be landed and merged. If either has not landed, the entire tree is BLOCKED.
metadata-zfs-2026-09 is optional; its absence only degrades leaf 03.

## Dispatch Protocol

### Phase 0: Value gate (orchestrator, no dispatch)

1. Confirm with the maintainer: a concrete ownCloud client deployment exists
   to serve. If not: recommend closing issue #3 and STOP. (Honest value note.)

### Phase 1: Dispatch Leaf 01 (discovery)

1. Read [01-decision-and-client-subset-discovery.md](01-decision-and-client-subset-discovery.md), dispatch via `delegate_task`:
   - Goal: "Execute the client-subset discovery protocol from 01-decision-and-client-subset-discovery.md"
   - Context: full leaf text + the webdav frontend's launch/config shape
     (from the landed webdav-2026-09 README) + local paths for the ownCloud
     desktop client install. Note this leaf produces DOCS + a request log
     artifact, not Go code.
2. Review: the capture log exists, the subset/degradation list is concrete
   (endpoint paths + methods + headers, not impressions), and the decision
   doc names the target client + version.

### Phase 2: Dispatch Leaf 02 (OCS capabilities)

1. **Decide conditional leaves first** from 01's output: if no versions
   probes in the log, mark 03 SKIPPED in the tracking table with the log
   line as evidence; same for 04.
2. Read [02-ocs-capabilities-endpoint.md](02-ocs-capabilities-endpoint.md), dispatch with:
   - Context INLINED: 01's observed request list (exact paths/headers), the
     frozen Contracts 1–3 above, the landed `internal/frontend/webdav` API
     surface, and the Coding Conventions block below.
   - Include: "Do NOT commit. Do NOT run git add. Write code, run tests,
     report results only."
3. Orchestrator reviews in-session: contracts match exactly, XML pinned by
   table tests, unimplemented OCS endpoints return documented OCS errors,
   `go test ./internal/frontend/owncloud/...` green.

### Phase 3: Dispatch conditional leaves (03 and/or 04)

Dispatch each non-skipped conditional leaf with 02's landed router as
context. Same boilerplate. Review per each leaf's Self-Verification Checklist.

### Phase 4: Dispatch Leaf 05 (conformance + e2e)

1. Read [05-conformance-and-e2e.md](05-conformance-and-e2e.md), dispatch with the
   final set of shipped endpoints (from 01 + leaves 02–04) as context.
2. Review: `make e2e` fully green including the new ownCloud cases;
   `frontend.RunConformanceSuite` invoked for the owncloud frontend; coverage
   floors re-measured and updated in the SAME change if any file moved.

### Phase 5: Dispatch Leaf 06 (docs), then Integration Review

1. Read [06-docs.md](06-docs.md), dispatch; review README, compatibility
   matrix, and decision doc cross-links.
2. **Orchestrator integration review in-session:**
   - `make build`, `make vet`, `make fmt-check`, `make test`, `make test-race` all pass
   - `make e2e` green (pre-existing cases unchanged)
   - `grep -rn "os\." internal/frontend/owncloud/` → no direct filesystem
     access outside the Backend seam (tests/`os/exec` in metadata consumer
     excepted per metadata-zfs-2026-09)
   - `"owncloud"` in the registry known-types list; unknown-type fail-loud
     error text lists it
   - OCS capabilities document matches the degradation list in the decision
     doc (nothing advertised that is not served)
3. Re-dispatch owning leaves for gaps (max 3 cycles each), then commit per
   leaf with conventional messages (`feat(owncloud): ...`), update the
   tracking table.

## Review Checklist

The orchestrator verifies each child in-session:

- [ ] All tasks from the leaf document implemented (or leaf SKIPPED with
      evidence from 01's log)
- [ ] Interface Contracts from this master satisfied exactly (names, paths,
      XML envelope shape)
- [ ] Calls only `backend.Backend` — no direct storage access in
      `internal/frontend/owncloud`
- [ ] Capability shortfalls degrade at the seam with protocol-appropriate
      errors/OCS envelopes — no silent emulation, no aspirational capability
      flags
- [ ] Tests written first (TDD), table-driven, stdlib `testing`; all passing
- [ ] AGENTS.md e2e rule satisfied: wire-level cases in `scripts/e2e/cases/`
      landed in the same change as the code they cover
- [ ] Code follows Coding Conventions below; no debug artifacts, no TODOs
- [ ] No line-number corruption (`grep -rcE '^\s+[0-9]+\|' --include='*.go' .` → 0)

## Coding Conventions

- **Language:** Go (module `github.com/bhodgens/zeta-object`), stdlib only —
  no new dependencies (XML via `encoding/xml`, auth via the auth-2026-09
  adapter, metadata via `internal/metadata`)
- **Style:** gofmt + goimports with `-local github.com/bhodgens/zeta-object`
  (`make fmt` before reporting)
- **Errors:** errors as values; wrap with `%w`; reuse
  `frontend.ErrCapability`/`IsCapabilityError` for seam rejections; OCS error
  envelopes at the wire
- **Tests:** stdlib `testing`, table-driven, `_test.go` alongside source
- **Gates:** `make vet`, `make lint`, `make test` per leaf; `make e2e` per
  code-bearing leaf; coverage floors re-measured on any package move

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-decision-and-client-subset-discovery | PENDING | | |
| 02-ocs-capabilities-endpoint | PENDING | | |
| 03-versions-endpoint | PENDING (conditional) | | skip evidence: |
| 04-app-password-auth | PENDING (conditional) | | skip evidence: |
| 05-conformance-and-e2e | PENDING | | |
| 06-docs | PENDING | | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED | SKIPPED

## Integration Test Plan

1. **Unit + conformance:** `go test ./internal/frontend/owncloud/... -v` —
   XML envelope tests, router tests, versioning degradation tests, and
   `frontend.RunConformanceSuite(t, owncloudFrontend, opts)` passing.
2. **Full suite:** `make test`, `make test-race`, coverage floor held.
3. **Wire-level:** `make e2e` — new ownCloud cases (curl OCS capabilities,
   OCS+WebDAV round-trip, degradation 404s) green; all pre-existing cases
   unchanged.
4. **Real-client verification (MANUAL, documented not automated):** install
   the official ownCloud desktop client, point it at a local instance,
   connect → sync up → sync down → delete → confirm convergence; record the
   client version + result in the decision doc. This is deliberately a manual
   gate — the e2e harness covers the wire protocol; only a human with the
   real client can certify the acceptance criterion "official client can
   connect, sync up, sync down, delete."
5. **Static gates:** `make build`, `make vet`, `make fmt-check`, `make lint`,
   filesystem-access grep above.

## Structural Completeness Check (Before Dispatch)

Before dispatching any child, confirm this master contains every required
section: Meta, Goal, Architecture, Interface Contracts, Child Index, Dispatch
Protocol, Review Checklist, Coding Conventions, Completion Tracking Table,
Integration Test Plan, Notes. If any is missing, fill it in first.

## Notes

- **Sequencing is not negotiable.** If webdav-2026-09 or auth-2026-09 is not
  merged, this tree does not start. Leaf 01 (discovery against the webdav
  frontend) is the earliest permitted activity.
- **Discovery output is a contract, not a suggestion.** Leaves 02–04
  implement the observed request set; features the log does not show the
  client requesting are out of scope, however cheap they seem.
- **Conditional leaves may be SKIPPED** — that is a success outcome, recorded
  in the tracking table with the log evidence, not a failure.
- **metadata-zfs-2026-09 interplay:** leaf 03 must respect that tree's parity
  rule (providers enrich, never alter core behavior) and its nil-provider
  degradation semantics. On hosts without ZFS, versioning is simply not
  advertised.
- **Auth:** this tree never hardcodes credential checks; it consumes the
  auth-2026-09 identity model through `frontend.Frontend.Authenticator()`.
- Client-version pinning: record the exact desktop client version(s) tested
  in the decision doc; capability negotiation is version-sensitive and a
  future client bump may re-open leaf 01.
