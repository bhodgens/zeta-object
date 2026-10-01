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

FOUND + FILED AS ISSUE #5 — **RESOLVED (2026-09-30)**:
- F-oc-2: the client's file-discovery requires oc: namespace properties
  (oc:fileid, oc:permissions, oc:size) that we did not emit; sync aborted
  with "The server file discovery reply is missing data."

  **Fix pin:** every PROPFIND 207 reply (allprop and named-prop, discovery +
  children, trailing-slash AND slash-less forms) now declares
  `xmlns:oc="http://owncloud.org/ns"` on the multistatus root and carries:
  - `oc:fileid` on every resource — DERIVED at request time (FNV-1a hash of
    bucket + "\x00" + key, masked to uint63). Same bucket+key ⇒ same id
    across requests and restarts; NEVER persisted (no sidecar, no database —
    project charter). Collisions are accepted; the id is a discovery hint.
  - `oc:permissions` — SIMPLIFIED grammar (the server's grant model has only
    read/write, so the richer real-grammar letters are not representable):
    | grant | file | collection |
    |---|---|---|
    | readwrite | `RW` | `RDNVCK` |
    | readonly | `R` | `RG` |
    (R = read; W = write file; N = mkdir/move/rename dir; D = delete file;
    C = create file in dir; K = delete dir; G = read versions — G appears
    only in the readonly-collection string to keep the four pinned values
    minimal and stable, matching the documented subset.)
  - `oc:size` on collections — aggregate contentLength of every key under
    the collection prefix, computed via a fully-paginated List at PROPFIND
    time (derived, never cached). Files report size via getcontentlength
    and carry NO oc:size.

CONCLUSION: the ownCloud frontend passes OCS negotiation and auth with the
real client; the last discovery blocker (#5, oc: properties) is RESOLVED
above and file sync proceeds to wire-level verification (e2e case 25 pins
the oc: properties). The README's ownCloud
section already frames real-client compatibility as unverified - that was
accurate; #5 is the concrete gap.


---

## Live acceptance re-run (2026-10-01, zfs-meta host, zeta-server @ 685b9ee + oc: props)

Setup: owncloud frontend single-bucket (bucket: zval), S3 frontend :9707, owncloud on :9603.

PASSED (wire-level, curl through the owncloud listener):
- PUT /remote.php/webdav/<key> -> 201 (auto-creates prefixes)
- GET byte-exact round-trip
- DELETE -> 204, subsequent GET -> 404
- PROPFIND collections (trailing slash and slash-less, F-oc-1 fix)
- oc:fileid / oc:permissions / oc:size present on allprop and named-prop
- status.php (any depth) answers the classic JSON - the 6.x client's first probe

REAL CLIENT (owncloudcmd):
- 2.5.4 (docker owncloud/client): negotiation OK, discovery LsColJob fails
  "Unknown error 207" parsing our prefixed d:/oc: 207 form (2.5.4-era csync
  parser limitation under investigation; the oc: props themselves are
  namespace-correct).
- 6.0.3 (official AppImage): status.php probe accepted; discovery OK; sync
  run blocked by an owncloudcmd 6.x CLI regression (owncloudcmd was removed
  in 7.x and the 6.x binary hangs in offscreen mode under docker).

REMAINING WORK (tracked): validate the 207 against a reference server 207
byte-for-byte (property set + structure) to close the 2.5.4 parse gap; the
charter's ZFS-validation rule applies to any follow-up.
