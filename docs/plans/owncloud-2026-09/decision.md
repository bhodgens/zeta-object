# ownCloud Protocol Frontend — Decision Document (leaf 01, ADAPTED SCOPE)

> **Scope adaptation (binding, per the dispatching orchestrator):** leaf 01's
> client-subset discovery requires running the REAL ownCloud desktop sync
> client. That is not possible in this environment. This document therefore
> records the decision on the DOCUMENTED protocol surface instead of an
> observed capture, implements the WebDAV+OCS minimal subset (leaf 02), and
> documents the real-client verification as a MANUAL procedure (§6). Every
> claim a capture would have grounded is marked UNVERIFIED and routed to the
> manual procedure. Nothing here is presented as empirically observed.

## 1. Target clients

- **Compatibility target:** the official ownCloud desktop sync client in its
  **classic server account mode** (the Qt client's "ownCloud server" URL
  scheme, `/remote.php/webdav/` + OCS), versions 3.x line. EXACT VERSION:
  **untested — no real client ran against zeta-object.** Other versions
  (older 2.x classic clients, oCIS-mode 3.x+ against `/remote.php/dav/`,
  mobile clients) are **untested and out of scope.**
- The classic-API surface is targeted, NOT the oCIS API surface
  (`/remote.php/dav/files/<user>/`, `/meta/`, app-provisioning API). The
  oCIS surface is NOT implemented.

## 2. Decision

**Implemented: the minimal WebDAV+OCS subset** — OCS capabilities
negotiation (`/ocs/v{1,2}.php/config`, `/cloud/capabilities`), OCS user
metadata (`/cloud/user`), and the full landed WebDAV data plane reachable
through the owncloud listener (`/remote.php/webdav/**` and every other
non-OCS path delegate unchanged to the webdav frontend).

This confirms the master's encoded recommendation by construction: the
desktop client negotiates capabilities over OCS and then speaks plain
WebDAV for all data transfer, so the minimal viable surface is exactly
negotiation + data plane. What is NOT implemented (classic provisioning
API, shares, oCIS APIs, versions) is listed in §4.

Honest-value framing (from master.md, recorded here as directed): this
frontend ranks below WebDAV and (S)FTP. It is worth doing only if a
concrete ownCloud client deployment exists to serve; the maintainer
directed implementation (issue #3), and this document exists so a future
reviewer can re-evaluate that judgment against what actually ships.

## 3. Implemented subset

The subset below is implemented in `internal/frontend/owncloud` and pinned
by golden unit tests plus e2e cases 23/24. Without a capture, the entry
evidence is the leaf-02 implementation contract itself (the documented
classic-server shapes), not a log line — the manual procedure (§6) is what
converts these rows to observed.

### OCS endpoints (XML envelope `text/xml; charset=UTF-8`)

| Method + path (both `/ocs/v1.php` and `/ocs/v2.php`) | Response | Evidence |
|---|---|---|
| `GET /config` | 200, capabilities document | leaf 02 golden tests; e2e 23a |
| `GET /cloud/capabilities` | 200, same document | leaf 02 golden tests; e2e 23a |
| `GET /cloud/user` | 200, `<id>` = authenticated access key | leaf 02 tests; e2e 23b |
| any other endpoint | documented OCS 404 envelope naming the minimal-subset degradation | e2e 23d |
| wrong method on a known endpoint | OCS 405 envelope | e2e 23e |

Capabilities document contents (ALL derived from true state — no
aspirational flags): a `version` block declaring classic-line **10.11.0**
(a compatibility-shim identity so clients treat the server as a classic
API server; UNVERIFIED against a real client's version gating), and a
`files` capability block that is EMPTY by design — bigfilechunking,
undelete, and versioning are omitted entirely (never emitted false), so
the document cannot promise anything the data plane does not serve.
Versioning appears only if a metadata provider is later attached (leaf 03
contract, not built).

Envelope rules: `<status>` is `ok` iff `<statuscode>` is 200; **v1 maps
every response to HTTP 200** (statuscode lives only in the envelope —
classic v1 behavior), **v2 mirrors the statuscode** in the HTTP status
(401/404/405). Unauthenticated requests get OCS statuscode 997 ("no valid
credentials"). Both mapping decisions follow the documented classic
behavior; UNVERIFIED against the client's actual parsing.

### WebDAV data plane (unchanged, wrapped)

Everything the webdav-2026-09 frontend serves: `OPTIONS`, `PROPFIND`
(Depth 0/1), `GET`, `HEAD`, `PUT`, `DELETE`, `MKCOL`, `COPY`, `MOVE` —
reachable under `/remote.php/webdav/**` (and any other non-OCS path) on an
owncloud listener, with the same Basic auth (`zeta-object` realm), the
same bucket modes (`bucket` config key accepted like webdav), and the same
rejection table (LOCK/UNLOCK/PROPPATCH → 405, Depth-infinity → 403,
collection COPY/MOVE → 403). No extra DAV properties (oc:fileid,
oc:permissions) were added: UNVERIFIED whether the desktop client blocks
without them; if the manual procedure shows a hard dependency, that is a
new leaf, not silent scope growth.

### Auth

HTTP Basic only (accessKey as username, secretKey as password) via the
auth-2026-09 adapter over the identity registry. App passwords/tokens are
NOT implemented (§4, leaf 04 ruling). Whether a modern desktop client
accepts plain Basic for a classic server is UNVERIFIED — the login flow is
part of the manual procedure.

## 4. Degradation list

| Client behavior | Wire response | Expected client impact |
|---|---|---|
| Shares (any `/apps/files_sharing/**` OCS call) | 404 + OCS envelope naming the minimal subset | sharing UI empty/broken; sync itself unaffected — UNVERIFIED |
| Provisioning / user listing (`/cloud/users`, apps API) | 404 + OCS envelope | admin features absent; UNVERIFIED client tolerance |
| File versioning (versions REPORT, `/meta/` paths) | 405 on the data plane (unimplemented method); no versioning capability advertised | no restore-previous-version UI; sync unaffected |
| App-password / token login flow | not offered; Basic only | client must use the account password path — UNVERIFIED whether the tested client generation tolerates this |
| oCIS endpoints (`/remote.php/dav/**`, `/app/api/**`) | fall through to the webdav handler; paths resolve as ordinary DAV resources | oCIS-mode accounts will NOT work; use classic mode |
| Chunked upload (`bigfilechunking`) | capability omitted; no chunking endpoints | large-file behavior = plain PUT; UNVERIFIED size limits |

**STOP CONDITION check:** none of the degradations is known (from the
protocol documentation) to hard-block account setup or basic sync —
capabilities and cloud/user are served, and the data plane is complete
class-1 WebDAV. But WITHOUT a capture this cannot be certified: if the
manual procedure (§6) shows the client refusing to complete setup, that is
a STOP CONDITION per leaf 01 Task 4 and must be reported before any
further investment.

## 5. Conditional-leaf rulings (GO/NO-GO)

- **Leaf 03 (versions endpoint): NO-GO — SKIPPED.** Justification: there
  is no capture log line showing the client probing versions (the capture
  cannot exist in this environment), AND the backing
  `metadata-zfs-2026-09` provider is not attached in any supported
  deployment configuration. Both of the leaf's triggers are absent.
  Re-open when: a real-client run shows versions probes, or a ZFS-backed
  deployment wants versioning advertised. GO criteria at that point: (a)
  capture shows the specific versions requests the client sends (paths +
  REPORT bodies), (b) a MetadataProvider is attached and its probe
  returns positive, (c) the capabilities document gains the versioning
  block ONLY from that probe result.
- **Leaf 04 (app-password auth): NO-GO — SKIPPED.** Justification: no
  evidence any target client demands app passwords against a classic
  server (the classic flow is Basic). Re-open when: the manual procedure
  shows the client's login flow rejecting plain Basic or demanding a
  token grant flow. GO criteria: (a) capture shows the login-flow
  requests, (b) a token authenticator lands in `internal/auth` behind the
  same `Authenticator` seam, (c) `Authenticator()` swaps without touching
  the OCS router.

## 6. Manual real-client verification procedure

Run this on a machine that can install the official ownCloud desktop
client. Record the exact client version and every result in this
document's §7 table.

1. **Stand up the stack:** `make build && make certs`; config with
   `frontends: [{"type": "s3"}, {"type": "owncloud", "listenAddr":
   "127.0.0.1:8444", "bucket": "photos"}]` (drop `bucket` for
   multi-bucket mode), one readwrite identity; HTTPS cert as usual.
2. **Pre-flight with curl** (no client): the e2e cases 23/24 are exactly
   this — run `bash scripts/e2e/run-e2e.sh` and require both ownCloud
   cases green.
3. **Capture setup:** run mitmproxy in reverse mode
   (`mitmproxy --mode reverse:https://127.0.0.1:8444 -w oc.flow`), trust
   its CA, and point the client at the proxy; if the client refuses the
   proxy, front the server with a stdlib logging reverse proxy (throwaway
   file, not committed). Corroborate decisions with `owncloudcmd
   --logdebug http`.
4. **Account setup:** add the account (URL `https://127.0.0.1:8444`, user
   = accessKey, password = secretKey). Capture every request during
   setup: `/status.php`?, `/ocs/v1.php` vs `v2.php`?, which capabilities
   fields the client reads, whether Basic is accepted.
5. **Sync up:** create a directory with a few files client-side; capture
   all PROPFIND/PUT/MKCOL and any interleaved OCS calls.
6. **Sync down:** add a file server-side via curl; let the client sync.
7. **Delete:** delete a file client-side; confirm the server-side delete.
8. **Idle:** 10+ minutes; record poll interval and etag probes.
9. **Reduce:** dedupe the capture into a request inventory
   (method, path template, notable headers, response code, client
   reaction) into `capture/request-log.md`, then update §3/§4/§5 here —
   prune subset rows the log does not support, re-rule the conditional
   leaves. Raw `.flow`/pcap files stay OUT of the repo.

## 7. Manual verification log

| Date | Client version | Setup | Sync up | Sync down | Delete | Verdict |
|---|---|---|---|---|---|---|
| — | not yet run | — | — | — | — | **PENDING — acceptance criterion "official client can connect, sync up, sync down, delete" is NOT yet certified** |

Until a row is filled in, the honest status of this frontend is: the wire
protocol is implemented and machine-verified (unit + e2e); real-client
compatibility is UNKNOWN.
