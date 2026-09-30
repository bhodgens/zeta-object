# ownCloud Client Compatibility

Status of the `owncloud` protocol frontend against the official ownCloud
desktop sync client. Full rationale and the adapted discovery scope:
[`docs/plans/owncloud-2026-09/decision.md`](../plans/owncloud-2026-09/decision.md).

**Tested client versions: NONE — the real-client pass has not been run.**
The wire protocol below is machine-verified (unit golden tests + e2e cases
23/24); compatibility with any specific client build is **unverified**.
Other client versions are untested. Last verified: never (see the pending
log in decision.md §7).

## Supported

| Feature | Notes |
|---|---|
| OCS capabilities negotiation | `GET /ocs/v{1,2}.php/config` and `/cloud/capabilities`; XML envelope `text/xml; charset=UTF-8`; v1 always HTTP 200 (statuscode in the envelope), v2 mirrors the statuscode |
| OCS user metadata | `GET /ocs/v{1,2}.php/cloud/user`; `<id>` is the authenticated access key (no display-name/email — the auth seam has no such fields, and none are invented) |
| WebDAV data plane | PROPFIND (Depth 0/1), GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE under `/remote.php/webdav/**` (any non-OCS path) on the owncloud listener — the identical webdav frontend, wrapped |
| Bucket modes | `"bucket"` config key behaves exactly like the webdav frontend's (single-bucket `/` = the bucket; absent = top-level collections are buckets) |
| HTTP Basic auth | accessKey / secretKey from the `identities` config or the environment pair; realm `zeta-object`; 401 + OCS 997 envelope for anonymous OCS requests |

## Degraded

| Feature | Client impact |
|---|---|
| No versioning | The capabilities document carries NO versioning block (provider-less build) — restore-previous-version UI will not appear; sync is unaffected. A versions REPORT on the data plane is rejected 405. |
| Server identity is a compatibility shim | The document declares classic-line `10.11.0` so clients treat the server as a classic API server; it is not a feature claim. A client gating features on `>10.x` version checks may misbehave (unverified). |
| No chunked upload | `bigfilechunking` is emitted explicitly **false** in the capabilities (owncloud/client#7862: the client treats a missing flag as chunking-enabled, so `false` is the off switch); large files transfer as plain PUTs. |
| User metadata is minimal | Only `<id>`; clients showing a display name will show the access key. |

## Unsupported

| Feature | Wire behavior |
|---|---|
| Shares (`/ocs/*/apps/files_sharing/**`) | `404` + OCS envelope naming the minimal-subset degradation |
| Provisioning / users / apps API | `404` + OCS envelope |
| App-password / token login flow | not offered; Basic only |
| oCIS API surface (`/remote.php/dav/**`, app API) | not implemented; such paths fall through to the plain WebDAV handler — oCIS-mode accounts will not work |
| LOCK/UNLOCK/PROPPATCH, Depth-infinity PROPFIND, collection COPY/MOVE | same rejections as the webdav frontend (405 / 403 / 403) |

## Re-opening scope

Conditional leaves (versions endpoint, app-password auth) are SKIPPED with
explicit GO/NO-GO criteria in
[decision.md §5](../plans/owncloud-2026-09/decision.md). When a real-client
capture shows the client probing versions or rejecting Basic auth, those
leaves re-open with their criteria — the door is documented, not built.


---

## Real-client validation (2026-09-30, owncloudcmd 2.5.4 via docker owncloud/client)

OwnCloud single-bucket mode, owncloudcmd against /remote.php/webdav/<bucket>:

CONFIRMED WORKING:
- OCS v1/v2 capabilities + cloud/user negotiation (client accepts our XML shim)
- HTTP Basic auth with identity accessKey/secretKey
- PROPFIND with trailing slash (207 multistatus)
- Wire-level flow reaches file discovery

FOUND + FIXED THIS SESSION:
- F-oc-1a: slash-less collection PROPFIND returned 404 (owncloudcmd strips the
  trailing slash) - PROPFIND-only relaxation in propfindEntries; GET/PUT/DELETE
  keep exact-key semantics
- F-oc-1b: content-type charset="utf-8" (quoted) - ownCloud client rejects;
  unquoted now

FOUND + FILED AS ISSUE #5 (blocker):
- F-oc-2: the client's file-discovery requires oc: namespace properties
  (oc:fileid, oc:permissions, oc:size) that we do not emit; sync aborts with
  "The server file discovery reply is missing data."

CONCLUSION: the ownCloud frontend passes OCS negotiation and auth with the
real client but file sync is BLOCKED pending #5. The README's ownCloud
section already frames real-client compatibility as unverified - that was
accurate; #5 is the concrete gap.
