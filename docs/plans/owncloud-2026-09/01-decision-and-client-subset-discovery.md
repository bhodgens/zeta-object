# Decision + Client-Subset Discovery - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> This leaf is unusual: it produces DOCS and a capture artifact, not Go code.
> Do NOT commit — the orchestrator handles git operations after review.
> After completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Empirically determine which ownCloud protocol subset the official
  desktop sync client actually requires, by running the real client against
  the landed webdav frontend with request logging, and write the decision
  document that scopes leaves 02–04.
- **Dependencies:** webdav-2026-09 LANDED (provides the data plane under
  test); auth-2026-09 LANDED (Basic auth for the client session). A machine
  that can run the official ownCloud desktop client (macOS/Windows/Linux).
- **Estimated Context:** 60K
- **Concurrency Group:** A (first)

## Goal

Issue #3's open design decision — classic ownCloud server API vs oCIS API vs
minimal subset — is settled here by observation, not opinion. The output is:

1. A **decision document** pinning target client(s) + versions and the exact
   implemented subset.
2. A **captured request log** of everything the real client sends during
   connect → first sync → steady state → delete.
3. A **degradation list**: client behaviors zeta-object will not serve, each
   with the planned wire-level response and the observed consequence.

Leaves 02–04 implement exactly the observed subset. This document is their
input contract.

## Context

The ownCloud desktop client (github.com/owncloud/client, Qt-based; also the
oCIS-capable "ownCloud desktop client ≥ 3.x" with the `ocis`/classic
account-mode split) discovers the server as follows (verify all of it — do
not trust this sketch):

1. Account setup hits `/status.php` (classic) and/or `/ocs/v1.php|v2.php/config`
   plus `/ocs/v2.php/cloud/capabilities` to negotiate.
2. It then issues PROPFINDs against `/remote.php/webdav/` or
   `/remote.php/dav/files/<user>/` (classic vs oCIS URL schemes).
3. Depending on capabilities, it may probe versioning paths
   (`/remote.php/dav/meta/...`, versions REPORTs) and OCS endpoints
   (`cloud/user`, share APIs for "resharing" indicators).
4. Auth: Basic; newer clients prefer app tokens/app passwords when the
   capabilities document or login flow indicates them.

zeta-object's webdav frontend (from webdav-2026-09) serves PROPFIND/GET/PUT/
DELETE/MKCOL/COPY/MOVE and knows nothing of OCS. This leaf measures the gap:
what does the client actually send, what does it tolerate failing, and where
does it hard-stop.

Key files to understand before starting:
- [master.md](../master.md) — Architecture, Contracts 1–3, honest-value note
- `docs/plans/issue-owncloud-frontend.md` — the issue this tree implements
- The landed webdav frontend's README/config section (from webdav-2026-09)
- `docs/plans/metadata-zfs-2026-09/master.md` — provider probe semantics
  (context for the versions question only)

## Tasks

### Task 1: Stand up the target stack

**Objective:** A locally running zeta-object with the webdav frontend + Basic
auth that the desktop client can reach over HTTPS.

**Steps:**
1. `make build`; configure `frontends: [{"type": "webdav", "listenAddr": …}]`
   per the webdav-2026-09 README on a free port with a local cert.
2. Verify with curl before any client is involved:
   - `curl -k -u user:pass -X PROPFIND https://127.0.0.1:PORT/remote.php/webdav/ -H 'Depth: 0'`
   - PUT/GET/DELETE round-trip on a scratch path.
3. Record the exact config + curl transcript in the decision doc appendix.

**Done-when:** curl round-trip against the webdav listener succeeds.

### Task 2: Capture the client's wire behavior

**Objective:** A complete request log of the desktop client against the
server, with full method/path/headers/body for OCS and PROPFIND/REPORT
requests.

**Method (preferred order):**
1. **mitmproxy** (if the client honors system CA / proxy env): run
   `mitmproxy --mode reverse:https://127.0.0.1:PORT -w oc-flow` and point the
   client at the proxy; trust mitmproxy's CA. Full fidelity including bodies.
2. **Server-side logging:** if the client cannot be proxied, enable the
   server's request logging (or front it with a tiny logging reverse proxy in
   Go stdlib — a throwaway file, NOT committed without orchestrator approval)
   and capture method/path/headers there. Bodies of OCS requests are small;
   capture them if the mechanism allows.
3. **Client-side debug log:** `owncloudcmd --logdebug http` (or the GUI's
   debug channel) as corroboration — useful for what the client *decided*
   after each response, not authoritative for the wire.

**Protocol:**
1. Install the official client; record exact version
   (`owncloudcmd --version`) in the decision doc.
2. Add the account (URL, user, password). Capture: status/capabilities/
   user probes during account setup.
3. First sync: create a directory with a few files client-side, let it sync
   up. Capture all PROPFIND/PUT/MKCOL traffic + any OCS calls interleaved.
4. Server-side change: add a file via curl through the webdav listener, let
   the client sync down.
5. Delete a file client-side; confirm server-side delete.
6. Let the client idle 10+ minutes; capture periodic probes (poll interval,
   etag checks).
7. Stop. Export the capture; reduce it to a deduplicated request inventory
   table (method, path template, notable headers, response code observed,
   client's reaction).

**Done-when:** `docs/plans/owncloud-2026-09/capture/request-log.md` contains
the full inventory with the raw capture referenced (raw `.flow`/pcap/log
files stay OUT of the repo — commit only the reduced markdown; note the raw
artifact's local path in the doc).

### Task 3: Write the decision + subset + degradation documents

**Objective:** The scoping documents for leaves 02–04.

**Files:**
- Create: `docs/plans/owncloud-2026-09/decision.md`
- Create: `docs/plans/owncloud-2026-09/capture/request-log.md`

**decision.md must contain (sections in order):**
1. **Target clients:** exact client(s) + version(s) tested; the statement
   "compatibility target is these versions; other versions untested."
2. **Decision:** minimal WebDAV+OCS subset (confirm or refute the master's
   encoded recommendation with log evidence). Explicitly state classic-API
   and oCIS API surfaces NOT implemented.
3. **Implemented subset:** the endpoint list (method + path + response shape)
   lifted from the request inventory — this is the input contract for leaves
   02–04. Separate into: OCS endpoints, WebDAV modifications (if the client
   requires PROPFIND properties the webdav frontend does not return — e.g.
   `oc:fileid`, `oc:permissions` — list them; extra DAV properties are this
   tree's scope only when the client blocks without them), and auth
   requirements (Basic suffices? app tokens demanded?).
4. **Degradation list:** every client request outside the subset, with the
   planned wire response (OCS 404 envelope, 503, etc.) and the expected
   client impact (feature lost vs client refuses to work — the latter is a
   stop condition; flag it loudly).
5. **Conditional-leaf rulings:** explicit GO/NO-GO for leaf 03 (versions)
   and leaf 04 (app-password), each with the log line that justifies it.
6. **Manual verification plan** for the real client (used again by the
   orchestrator's integration review).

**Step 0 (before Task 3): write the failing check.** The "test" here is the
decision doc's reviewability: create `capture/request-log.md` first with the
inventory table, and verify every claim in decision.md's subset list cites a
log line. A subset entry with no log evidence is a bug — delete it.

**Done-when:** decision.md names clients+versions, pins the subset, rules on
both conditional leaves with evidence, and includes the degradation list;
request-log.md contains the full inventory.

### Task 4: Stop-condition check

**Objective:** Honest early exit if the discovery says the client cannot work.

If the log shows a hard blocker (e.g. client refuses to complete account
setup without a capability zeta-object will not implement — shares-provisioning,
specific oCIS endpoints), write the blocker into decision.md §4 marked
`STOP CONDITION`, report to the orchestrator immediately, and do NOT proceed
to recommendations that assume the blocker away. The orchestrator decides:
close the issue, or document partial degradation and continue only with
maintainer sign-off.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] Real desktop client ran against the webdav frontend; versions recorded
- [ ] Capture covers: account setup, sync up, sync down, delete, idle probes
- [ ] `capture/request-log.md` inventory is deduplicated and cites response codes
- [ ] `decision.md` has all 6 sections; every subset entry has log evidence
- [ ] GO/NO-GO rulings made for leaves 03 and 04 with log citations
- [ ] Stop conditions (if any) flagged in §4
- [ ] No raw capture binaries added to the repo
- [ ] No code changes anywhere in the repo

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Notes

- This leaf is deliberately cheap relative to the rest of the tree; it is
  also the tree's real go/no-go. A vague capture defeats the purpose — if the
  proxy method fails, fall through to server-side logging; do not skip.
- If the desktop client cannot be installed in this environment, STOP and
  report: this leaf cannot be satisfactorily completed from documentation
  alone, and guessing the subset is exactly the speculative build issue #3
  warns against.
- The client's OCS version headers matter: record whether requests go to
  `/ocs/v1.php`, `/ocs/v2.php`, or both — leaf 02's router must match.
