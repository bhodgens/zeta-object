# File Versions Endpoint (CONDITIONAL) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat.
>
> **CONDITIONAL LEAF:** Dispatch ONLY if leaf 01's decision.md §5 rules GO
> (the captured client actually probes versions endpoints). If NO-GO, the
> orchestrator marks this leaf SKIPPED and this file stays unread.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Serve the file-versions endpoints the desktop client probes,
  backed by `internal/metadata.MetadataProvider` (zfs-events) where
  available, degrading honestly (no versioning advertised) where not.
- **Dependencies:** leaf 02 (OCS router + capabilities wiring landed);
  `metadata-zfs-2026-09` OPTIONAL (provider interface + zfs-events consumer;
  absence = degrade path becomes the only path).
- **Estimated Context:** 80K
- **Concurrency Group:** C

## Goal

Classic ownCloud exposes per-file version history under WebDAV: the client
PROPFINDs a versions collection (or issues a versions REPORT) and restores
via GET on a version URI. zeta-object has no filesystem-native versioning,
but the metadata-zfs-2026-09 tree provides an optional
`MetadataProvider` whose zfs-events source can surface real per-object
history where ZFS is present. This leaf maps provider events to the
versions surface the client probes — and, critically, keeps the
capabilities document honest: no provider → no versioning capability →
client degrades gracefully instead of erroring.

## Context

- Provider seam (metadata-zfs-2026-09, landed shape): `internal/metadata/`
  exposes `MetadataProvider` with `Probe` + event/object-history queries; the
  zfs-events consumer shells out to `zfs events -j`. A nil provider is a
  normal, supported state.
- Parity rule from that tree: providers ENRICH, never alter core behavior.
  Versions endpoints here are additive; they must not change any WebDAV
  GET/PUT/PROPFIND response.
- Observed versions traffic (paths + verbs) comes from leaf 01's log — the
  classic client under `/remote.php/dav/meta/<fileid>/v/` (list) and
  restore via COPY/GET on version URIs, but VERIFY against the capture;
  implement exactly what was observed.
- If the client uses `oc:fileid` in PROPFIND responses to build versions
  URLs, serving fileid may be a prerequisite — leaf 01's subset list says
  whether fileid is required; if so, implement it in the versions task and
  note that it touches webdav PROPFIND property output (coordinate: property
  additions go through the webdav frontend's property registry, do not fork
  the PROPFIND encoder).

## Interface Contract (From Parent)

Master Contract 4: `VersionsHandler` serves observed versions endpoints,
backed by `MetadataProvider`; nil provider or negative probe →
protocol-appropriate error responses AND `Capabilities().Versioning` false so
the capabilities document never advertises unserved functionality. The
capability block added in leaf 02's presence-test flips on only when a
provider is attached AND this leaf's handlers are registered.

## Tasks

### Task 1: Versions surface over a fake provider (TDD)

**Objective:** The versions handlers work end-to-end against a fake
`MetadataProvider`, pinned before any ZFS-specific behavior.

**Files:**
- Create: `internal/frontend/owncloud/versions.go`
- Test: `internal/frontend/owncloud/versions_test.go`

**Step 1: Write failing tests** (fake provider in test)

- List versions for a known path (per the URL scheme leaf 01 observed) →
  multistatus XML listing versions derived from fake events, ordered newest
  first, with the properties the client requested in its PROPFIND body
  [observed set from log].
- GET a specific version → returns the versioned content bytes the fake
  provider reports (or 404 when the provider has no such version).
- Restore method observed in the log (COPY or MOVE onto the live path) →
  performs restore through the Backend seam (PUT of version content via the
  existing webdav data path — the handler composes, it does not bypass).
- Nil provider: all versions endpoints → 404 (WebDAV error body, not OCS —
  these are DAV-plane endpoints) [confirm expected wire shape from log];
  `Capabilities().Versioning` false.

**Step 2:** FAIL. **Step 3:** implement handlers + provider adapter. **Step
4:** PASS.

### Task 2: Capability wiring

**Objective:** The versioning capability block exists exactly when the
handler is active.

**Files:**
- Modify: `internal/frontend/owncloud/capabilities.go`
- Test: extend `internal/frontend/owncloud/capabilities_test.go`

**Step 1: Write failing tests** — with fake provider + versions handlers
registered, `BuildCapabilities` includes the versioning block (content per
classic schema, e.g. `files.versioning`); without provider, absent. **Step
2:** FAIL. **Step 3:** wire. **Step 4:** PASS.

### Task 3: Real-provider integration test (skippable)

**Objective:** Prove the zfs-events provider feeds versions where available.

**Files:**
- Test: `internal/frontend/owncloud/versions_zfs_test.go`

**Step 1: Write test with build/skip guard** — `exec.LookPath("zfs")` + probe
negative on non-ZFS hosts → `t.Skip`. On a ZFS host: seed events via the
provider's own test mechanism (per metadata-zfs-2026-09 conventions — its
record/replay fixtures are the model; reuse its fixture loader if exported),
list versions through the handler, assert mapping. **Step 2:** run → SKIP on
this machine (acceptable; the fake-provider tests are the gate) or PASS on
ZFS. **Step 3:** n/a (no production code expected). **Step 4:** confirm the
skip is graceful, not a failure.

### Task 4: Degradation documentation hook

**Objective:** The no-provider path is a documented, tested contract.

**Files:**
- Modify: `internal/frontend/owncloud/versions.go` (doc comments)
- Test: covered by Task 1's nil-provider cases

Verify (assert in Task 1 tests, re-check here): with no provider, the
capabilities document does not contain the versioning block AND a versions
probe returns the documented error. This pairing is the "degrade otherwise"
clause of issue #3.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] `go test ./internal/frontend/owncloud/... -v` passing (zfs test skips
      cleanly off-ZFS)
- [ ] Versions paths/methods exactly those observed in leaf 01's log
- [ ] Provider parity rule respected: no WebDAV core response changed
- [ ] Nil-provider degradation tested: no versioning advertised + error on probe
- [ ] Restore path composes through the Backend seam (no direct storage)
- [ ] `gofmt -l` empty; `go vet` clean; no debug artifacts

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Notes

- If leaf 01's log shows the client does NOT probe versions, this leaf is
  SKIPPED — record skip evidence in master.md's tracking table. Do not build
  "while we are here."
- Version metadata under zfs-events is event-log derived, so restored
  content accuracy depends on the provider's event completeness; document
  this limitation (versions are best-effort history, not a versioned
  filesystem) in the doc comments — leaf 06 carries it into the
  compatibility matrix.
