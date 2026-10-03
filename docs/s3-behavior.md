# zeta-object S3 Behavior Reference

Status: CONTRACT - version 1.0, 2026-10-02.

This document is the stable behavior contract between zeta-object's S3
frontend and S3 client authors. It is written so a client developer can
implement against zeta-object without reading its source, and it answers
one question per operation: **what does zeta-object do here, and where
does it differ from AWS S3?**

Relationship to other documents:

- [docs/protocol-compatibility.md](protocol-compatibility.md) - the
  one-glance Implemented/Degrades/Absent matrix for ALL protocols
  (S3, WebDAV, ownCloud, FTP, SFTP). This document is the S3 deep dive.
- `docs/conformance/2026-09-28-matrix.md` - the ceph/s3-tests run
  evidence: which upstream tests pass, and the triage classes for those
  that do not. This document states the behavior; that document proves
  it against the industry suite.
- README.md - operator-facing setup. Behavior questions come here.

## Maintenance rule (the upstream-zfs documentation contract)

Following the zfs project's documentation practice (and zmetad's
`SCHEMA.md`, this repo's model for consumer contracts):

1. **A behavior change without a doc change is a bug.** Every PR that
   changes what a response looks like updates this document in the same
   commit.
2. **Every statement is pinned to source.** Sections cite the file that
   implements the behavior. When this document and the code disagree,
   the code wins and the doc is stale - file it.
3. **Additive evolution.** New operations and headers are added; existing
   documented behavior is never silently reinterpreted. A breaking
   divergence change is called out in the commit message and the
   Changes section at the bottom.
4. **Divergences from AWS are labeled [DIVERGENCE]** inline and are
   also listed in the summary section. A divergence is a *deliberate*
   difference - it is recorded, never "fixed" to match AWS.

## Scope

Covers the S3 frontend only (`internal/frontend/s3/`). The other
protocols (WebDAV, ownCloud, FTP, SFTP) are covered by
[docs/protocol-compatibility.md](protocol-compatibility.md).

Proof links: e2e cases under `scripts/e2e/cases/` (wire-level, SigV4),
the conformance ratchet (`make conformance`, ceph/s3-tests subset), and
the ZFS validation harness (`scripts/zfs-validate/run-zfs-validation.sh`,
real OpenZFS). Latest live run:
[docs/validation-zmetad-2026-10-01.md](validation-zmetad-2026-10-01.md)
(70/70 on real ZFS).

---

## 1. Conventions

### 1.1 Error envelope

Every error is an XML document, `Content-Type: application/xml`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>NoSuchKey</Code>
  <Message>The specified key does not exist.</Message>
</Error>
```

[DIVERGENCE] AWS errors carry `RequestId`, `HostId`, and `Resource`
elements; zeta-object omits all three. Clients that parse `Code` and
`Message` are unaffected; clients that log `RequestId` will not find one.
(errors_to_s3.go, xml.go:75)

### 1.2 Transport

- HTTPS only, TLS 1.2 minimum. No plain HTTP listener (a conformance
  fallback env var exists for CI; production is HTTPS).
- The bundled self-signed certificate covers `localhost` /
  `127.0.0.1`; clients use `--no-verify-ssl`-equivalent settings or
  install the cert.

### 1.3 Authentication

SigV4 only (header form and presigned query form). No SigV2, no
SigV4a, no anonymous access (every request must authenticate; there is
no public-bucket concept - grants are per identity).

- Clock skew: the `x-amz-date` must be within **15 minutes** of server
  time; outside that, `RequestTimeTooSkewed` / 403.
- **Region:** the scope region in the credential must match the
  server's configured `region` (default `us-east-1`). Mismatch fails
  `SignatureDoesNotMatch` / 403 with a message naming the expected
  region. (The permissive default mode that accepts any well-formed
  region is reachable only in tests; a launched server is always
  strict.) [DIVERGENCE] AWS per-bucket regions; zeta-object has one
  server-wide region. (region.go, auth_adapter.go)
- Unknown access key: `InvalidAccessKeyId` / 403.
- Wrong signature: `SignatureDoesNotMatch` / 403.
- Malformed Authorization header: `AuthorizationHeaderMalformed` / 400.

(auth_adapter.go)

### 1.4 Identity and authorization

One identity registry (config `identities` + the env pair) shared by
all protocols. Grants are bucket-level or prefix/op/time-scoped rich
expressions. Denied operations answer `AccessDenied` / 403 - identical
to AWS's denial shape. No ACLs, no bucket policies, no IAM users.

### 1.5 Key and bucket naming

**Keys** (`validateObjectKey`, object_handlers.go:1373):

- 1..1024 UTF-8 characters (AWS: same length bound).
- No null bytes.
- **[DIVERGENCE]** No path segment may be exactly `..` or `.metadata`
  (AWS permits `..` inside keys; here safety wins - a key is a
  filesystem path). `a..b` and `..hidden` are fine; only whole
  segments are rejected.
- **[DIVERGENCE]** Keys must be in canonical form: `a//b` is rejected
  where AWS would treat it as distinct from `a/b`. No key may end in
  `/` (AWS permits a literal `asdf/` key; the directory-mapped layout
  cannot represent it - conformance matrix "trailing-slash key"
  divergence).
- Keys that would collide with metadata (a `.metadata` first segment)
  are percent-encoded into the `!data/` shadow directory instead of
  rejected.

**Buckets** (`validateBucketName`, bucket_handlers.go:370): AWS rules -
3..63 chars, lowercase letters/digits/hyphens/periods, must start and
end with a letter or digit, no consecutive periods. Dots in general are
accepted (bucket-as-directory does not use virtual-host styling).

### 1.6 Limits

| Limit | Value | Where |
|---|---|---|
| Max object size (single PUT, non-multipart) | 5 GiB default, `max_put_bytes` backend option | fsbackend.go:108, object.go:94 |
| Multipart part number | 1..10000 | multipart_handlers.go:177 |
| Multipart minimum non-final part | 5 MiB at complete (`EntityTooSmall`) | multipart_handlers.go:33 |
| In-progress multipart expiry | 7 days (sweeper) | multipart_handlers.go:26 |
| ListObjects `max-keys` cap | 1000 (over-cap silently clamped, AWS behavior) | object_handlers.go:1005 |
| Object tagging | 10 tags/object, key <= 128 B, value <= 256 B, `aws:` prefix reserved (`InvalidTag`) | objectmodel/tags.go |
| Presigned URL `X-Amz-Expires` | 1..604800 seconds (AWS bound) | auth_adapter.go:370 |

### 1.7 Metadata conventions

- `Content-Type` defaults to `binary/octet-stream` when the PUT carried
  none.
- `x-amz-meta-*` headers are stored verbatim in the sidecar
  (`customMetadata`, original casing preserved) and echoed on GET/HEAD.
  Multi-valued headers are joined with `, `.
  [DIVERGENCE] AWS lowercases `x-amz-meta-*` key names; zeta-object
  preserves the client's casing on the wire.
- `ETag` is the MD5 digest, quoted on the wire. Multipart ETags are the
  assembled-object MD5, NOT the AWS `"<md5>-<N>"` suffix form.
  [DIVERGENCE] Clients that parse the `-N` suffix to detect multipart
  uploads will not find it.
- `Last-Modified` is HTTP date format (RFC 7231 IMF-fixdate), server
  clock, UTC.
- No `x-amz-request-id` / `x-amz-id-2` headers on any response.
  [DIVERGENCE]

---

## 2. Bucket operations

### 2.1 CreateBucket (PUT /)

- 200 on success (empty `Location` body is not emitted;
  [DIVERGENCE] AWS returns `200` with a `Location` header).
- Invalid name: `InvalidBucketName` / 400.
- Bucket already exists: `BucketAlreadyOwnedByYou` / 409.
  [DIVERGENCE] us-east-1 AWS returns 200 idempotently; zeta-object
  always 409s (matches other-region AWS).
- Custom-configured buckets (from the `buckets` config map) cannot be
  created via the API: idempotent 200 if the path exists, otherwise
  `BucketAlreadyExists` / 409.

### 2.2 DeleteBucket (DELETE /)

- 204 on success; `BucketNotEmpty` / 409 if any object or in-progress
  multipart upload remains (in-progress uploads count - the 68-error
  cascade in the conformance matrix documents this invariant).
- Custom-configured buckets: `AccessDenied` / 403, never deletable via
  API.

### 2.3 HeadBucket / GetBucketLocation

- HeadBucket: 200/404, no body.
- GetBucketLocation: `LocationConstraint` = the configured region
  (empty for us-east-1, matching AWS's US Standard convention).

### 2.4 ListObjectsV2 (GET /?list-type=2)

Implemented: `prefix`, `delimiter`, `continuation-token`,
`start-after`, `encoding-type=url`, `max-keys` (cap 1000, over-cap
silently clamped). Common-prefix rollup matches AWS. Truncation sets
`IsTruncated` + `NextContinuationToken`.

[DIVERGENCE] **ListObjectsV1 is not implemented**: a V1-style request
(`?marker=` without `list-type=2`) is served by the V2 handler; the
`marker` parameter is honored as `continuation-token`, but the V1
response shape (`Marker`, `NextMarker`) is only emitted for the legacy
?versions-less listing path of never-versioned buckets. Clients should
use V2. (dispatch.go:347)

Absent: `fetch-owner` is ignored (Owner elements are never emitted).

### 2.5 DeleteObjects (POST /?delete)

Batch delete, per-key result entries, up to 1000 keys per request
(request body XML). Quiet mode (`?quiet`) not implemented.

---

## 3. Object operations

### 3.1 PutObject

- Writes are atomic (temp + fsync + rename): a crash never truncates.
- Over 5 GiB (or the backend's `max_put_bytes`): `InvalidArgument` /
  400 naming the limit. [DIVERGENCE] AWS answers `EntityTooLarge` /
  400.
- `x-amz-tagging` header: parsed and validated BEFORE the write;
  invalid tags (`InvalidTag` / 400) leave no object behind.
- `x-amz-meta-*` preserved verbatim (see 1.7).
- Content-MD5: validated when provided (invalid digest ->
  `InvalidDigest` / 400); not required.
- `aws-chunked` streaming (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`,
  `STREAMING-UNSIGNED-PAYLOAD-TRAILER`): chunk signatures verified per
  chunk. [DIVERGENCE] trailing checksum headers (`x-amz-checksum-*`)
  are parsed and skipped, never verified.

### 3.2 GetObject / HeadObject

Response headers: `Content-Type` (default `binary/octet-stream`),
`ETag` (quoted MD5), `Last-Modified`, `Accept-Ranges: bytes`,
`x-amz-tagging-count` (only when the object is tagged),
`x-amz-delete-marker: true` (only when a marker hides the key).

Order of evaluation (each short-circuits):

1. Bucket exists -> 404 `NoSuchBucket`.
2. Key valid -> 400 `InvalidArgument`.
3. Delete-marker check (versioning-enabled buckets): hidden key ->
   404 `NotFound` + `x-amz-delete-marker: true`.
4. Object exists -> 404 `NoSuchKey`.
5. Conditional headers (see 3.3).
6. Range (see 3.4).

### 3.3 Conditional GET (If-* headers)

AWS precedence, fully implemented (object_handlers.go:363):
If-Match -> If-Unmodified-Since (only when If-Match absent) ->
If-None-Match -> If-Modified-Since (only when If-None-Match absent).
Failures: 412 `PreconditionFailed` / 304 (304 carries ETag +
Last-Modified, no Content-Length). `If-Match: *` matches any existing
object; list-of-ETags supported; weak validators (`W/`) tolerated.

### 3.4 Range requests

- Single span: 206 + `Content-Range: bytes s-e/size`; unsatisfiable ->
  416 `InvalidRange` + `Content-Range: bytes */size`. Suffix (`-N`)
  and open-ended (`N-`) forms; end clamps to size.
- **Multi-span (RFC 9110 `multipart/byteranges`)**: 206 with a random
  hex boundary, per-part `Content-Range` (+ object `Content-Type`),
  streamed in ascending offset order - never whole-object buffered.
  Spans are coalesced when overlapping/adjacent; over 100 parts the
  response is 200 full body; **specs present but none satisfiable ->
  416** (RFC 9110 14.2); syntactically malformed headers -> 200 full
  body (RFC 9110 14.1.1 ignore rule). e2e 29.
- HEAD suppresses bodies everywhere.

### 3.5 CopyObject (PUT /dest with x-amz-copy-source)

- Server-side copy through the neutral model (bytes never pass through
  the client). Source form `bucket/key` (URL-encoded); a
  `?versionId=` source answers `501 NotImplemented`.
  [DIVERGENCE] AWS copies from that version.
- `x-amz-metadata-directive`: COPY (default, keep source metadata) |
  REPLACE (take the request's Content-Type + x-amz-meta-*). REPLACE on
  an identical source+dest key is allowed. [DIVERGENCE] AWS requires
  REPLACE for same-key copies.
- `x-amz-tagging-directive`: COPY (default) | REPLACE, resolved via
  `objectmodel.ResolveCopyTags`. Tags on the destination are exact per
  the directive.
- The destination ETag is the new object's MD5.

### 3.6 DeleteObject

- Idempotent 204 whether or not the key exists (AWS behavior).
- Versioning-enabled buckets: see section 5.

### 3.7 Multipart upload

Full lifecycle: initiate (`?uploads`), upload part (`partNumber` 1..10000),
list parts, list uploads, complete, abort. At complete, non-final parts
under 5 MiB -> `EntityTooSmall`; part numbers out of order or unknown ->
`InvalidPart` / `InvalidPartOrder`. Sessions expire after 7 days
(sweeper). An in-progress upload blocks bucket deletion
(`BucketNotEmpty`).

[DIVERGENCE] Completed multipart objects surface in the event history
(`?events`) as a single RENAME event (tmp assembly file -> final name)
- the documented F-live-4 semantics; AWS has no equivalent surface.

### 3.8 Batch DeleteObjects (POST /?delete)

Up to 1000 keys, per-key `<DeleteResult>` entries. Absent: quiet mode.

---

## 4. Object tagging

- PUT with `x-amz-tagging` header: URL-encoded `key=value&...`; invalid
  -> `InvalidTag` / 400 BEFORE any write (rejected requests create no
  object). 0-byte objects can be tagged.
- `GET /key?tagging`: `<Tagging><TagSet><Tag><Key/><Value/></Tag>...`;
  untagged -> 404 `NoSuchTagSet`; missing key -> 404 `NoSuchKey`.
- `PUT /key?tagging`: full replace (S3 semantics), same validation.
- `DELETE /key?tagging`: 204; subsequent GET -> 404 `NoSuchTagSet`;
  `TagCount` header disappears.
- COPY: `x-amz-tagging-directive` COPY (default, tags travel) |
  REPLACE (tags from the copy request's `x-amz-tagging` header).
- Limits (section 1.6) enforced at BOTH entry points (header and XML
  body).
- Storage: the per-object `.metadata/` sidecar, both bucket kinds
  (ZFS-native tag storage is upstream zfs-metadata#13; the `tagStore`
  seam is the swap point).

---

## 5. Versioning

Bucket state machine: Off (default) / Enabled / Suspended. Read and
write via `?versioning` (XML `<VersioningConfiguration><Status>`).
Unknown status values -> `MalformedXML` / 400. State lives in the
bucket (`.metadata/.versioning` marker) - it is per-bucket, not
per-mechanism.

### 5.1 Mechanisms (config `zfs_versioning`, ZFS buckets only)

| Mode | Mechanism | Delete markers | Fidelity |
|---|---|---|---|
| `snapshots` (default) | VersionId = the name of an EXISTING host snapshot; reads under `.zfs/snapshot/<snap>/<key>` | no | windowed: whatever the host's snapshot policy retains |
| `sidecar` | per-write copies under `.metadata/.versions/<sha256(key)>/<id>` + sidecar index | yes | exact per-write, zeta-object writes only |
| `both` | merged: sidecar IDs resolve first, then snapshot names | yes | merged |

Non-ZFS buckets always use `sidecar`. Mechanism selection is server
config, not per-bucket API.

### 5.2 Wire semantics (all modes)

- PUT overwrite on an Enabled bucket: the OLD bytes are recorded and
  remain readable; `x-amz-version-id` is NOT returned on plain PUTs.
  [DIVERGENCE] AWS returns it on every versioned PUT.
- DELETE on an Enabled bucket: 204, no data destroyed. Sidecar mode
  records a delete marker (plain GET -> 404 +
  `x-amz-delete-marker: true`; `GET ?versionId=<marker-id>` -> 405
  Method Not Allowed with the same header). Snapshots mode records no
  marker - DELETE answers 409 `Conflict` ("delete markers are not
  supported by this bucket's versioning mechanism"). [DIVERGENCE]
- `GET ?versionId=<id>`: 200 + `x-amz-version-id` echoing the id.
  Unknown/unknown-shaped sidecar id -> 400 `InvalidArgument`;
  expired/absent snapshot id -> 404 `NoSuchKey` (honest: the version
  window is host policy). A never-satisfied marker id -> 405.
- Suspended: writes and deletes are plain; previously recorded versions
  stay readable. [DIVERGENCE] AWS S3 retains prior versions under
  Suspended; zeta-object's plain overwrite resets the key's sidecar, so
  pre-suspend version ids on that key can become unknown (400). Tested
  and documented in e2e 32.
- Off: byte-identical to pre-versioning behavior; no versioning state
  is written.
- `?versions` listing (versioned buckets): per-key `<Version>` and
  `<DeleteMarker>` entries, newest-first, `IsLatest` per key,
  `version-id-marker` resume is real mid-key S3 semantics.
  Never-versioned buckets keep the legacy listing byte-identical.
  [DIVERGENCE] snapshot-mode entries carry no `ETag` and date from
  snapshot creation; a key's current bytes appear only after its first
  recorded overwrite/delete (a create-only key lists no entry).

### 5.3 Snapshots mode prerequisites and honesty rules

- The dataset must be tracked by zmetad (same prerequisite as `?events`);
  enumeration is ONE semaphore-bounded `zfs list -H -t snapshot` exec.
- An expired snapshot, an untracked dataset, or a missing key answers
  404 `NoSuchKey` - current-data substitution NEVER happens.
- zeta-object never creates, deletes, or rolls back snapshots. Host
  snapshot policy is the retention story. Upstream zfs-metadata#15
  (snapshot-on-write) is the future densification; when it lands, no
  zeta-object code changes.

---

## 6. ZFS metadata extensions

- `GET /<bucket>/<key>?events` and `GET /<bucket>?events`: event
  history from zmetad's export (DB layout 5..8 accepted; additive
  evolution, refuse-newer). Envelope: `dataset`, `recordsLost`,
  `ringSwaps`, `events[]` (`op`, `key`, `oldKey`, `txg`, `timestamp`,
  `sizeOld`, `sizeNew`). Nested keys are exact via insert-time
  `full_path`. uid/gid are never exposed. e2e 18-zmetad, zfs-validate.
- `GET /<bucket>?events&versions`: derived listing extension
  (`IsLossy`/`RecordsLost`/`RingSwaps`), distinct from S3 versioning.
- `recordsLost` is a lifetime figure (survives retention);
  `ringSwaps` counts kernel-log identity swaps; the two are never
  folded.
- Freshness: within one zmetad poll (default 30 s); `SIGUSR1` forces
  an out-of-band collect. An `events=off` dataset is pruned from
  tracking -> `?events` answers 503 (documented; verified live).
- Purge: operator-only (`zmetad --purge <dataset>` on the host), not
  an HTTP endpoint.

---

## 7. Differences from AWS S3 - the short list

Client authors: if your code does only these things, it is compatible.

1. **No `RequestId`/`HostId` in error XML.**
2. **Region is one server-wide value** (config `region`, default
   us-east-1); strict scope compare when set.
3. **No ACLs / bucket policies / public buckets** - identity grants
   only; every request authenticates.
4. **Key canonicalization is stricter**: no `..`/`.metadata` segments,
   no `a//b` aliasing, no keys ending in `/`.
5. **ETag is always plain MD5** - no multipart `-N` suffix form.
6. **Multipart complete ETag** matches AWS only in being an MD5 (see 5).
7. **`x-amz-meta-*` casing is preserved**, not lowercased.
8. **No `x-amz-request-id`/`x-amz-id-2` headers.**
9. **Versioning mechanisms differ per bucket kind** (section 5):
   snapshots mode is windowed faux versioning with no delete markers;
   sidecar mode has no `x-amz-version-id` on plain PUTs and no
   suspended-mode version retention.
10. **Snapshots-mode DELETE** answers `409 Conflict` (not the AWS
    marker flow).
11. **ListObjectsV1 is not implemented** (V2 handler serves it; use V2).
12. **CopyObject from a `?versionId=` source** answers `501`.
13. **Same-key COPY** does not require REPLACE.
14. **Checksum trailers are parsed, not verified.**
15. **CreateBucket 200 has no `Location` header**; duplicate create is
    always `BucketAlreadyOwnedByYou`/409.

## 8. Error code index

Codes the S3 frontend can return, with the emitting area:

| Code | HTTP | Emitted by |
|---|---|---|
| AccessDenied | 403 | grants, custom-bucket delete, anonymous |
| AuthorizationHeaderMalformed | 400 | SigV4 header shape, bad region token |
| BucketAlreadyExists | 409 | CreateBucket (custom bucket, path missing) |
| BucketAlreadyOwnedByYou | 409 | CreateBucket (duplicate) |
| BucketNotEmpty | 409 | DeleteBucket (objects or in-progress uploads) |
| Conflict | 409 | versioning mechanism conflicts (snapshots mode) |
| EntityTooSmall | 400 | multipart complete (non-final part < 5 MiB) |
| InternalError | 500 | catch-all; message is generic, details are logged |
| InvalidArgument | 400 | key/bucket validation, unknown sidecar versionId, oversize PUT |
| InvalidBucketName | 400 | CreateBucket name rules |
| InvalidPart | 400 | multipart complete (unknown part/ETag) |
| InvalidPartOrder | 400 | multipart complete (parts out of order) |
| InvalidRange | 416 | single-span and all-unsatisfiable multi-span |
| InvalidTag | 400 | tag limits / `aws:` prefix / bad encoding |
| InvalidAccessKeyId | 403 | unknown access key |
| MalformedXML | 400 | bad XML bodies (?versioning status, ?delete) |
| MethodNotAllowed | 405 | delete-marker versioned GET; wrong method |
| NoSuchBucket | 404 | all bucket-scoped operations |
| NoSuchKey | 404 | missing object, missing version |
| NoSuchTagSet | 404 | ?tagging on untagged object |
| NoSuchUpload | 404 | multipart ops on unknown/aborted upload |
| NotImplemented | 501 | ?versionId COPY source, absent capabilities |
| NotFound | 404 | delete-marker-hidden plain GET |
| PreconditionFailed | 412 | If-Match / If-Unmodified-Since failures |
| RequestTimeTooSkewed | 403 | > 15 min clock skew |
| SignatureDoesNotMatch | 403 | signature or region mismatch |

Sources: `errors_to_s3.go` (mapping table), `objectmodel/errors.go`
(codes), handler sites (grep `writeS3Error`).

## 9. Proof surface

| Surface | Command / path |
|---|---|
| Unit tests | `make test` (1100+ test functions) |
| Wire e2e | `make e2e` - 703 asserts, 34 cases (29 multirange, 30 region, 30 tagging, 31 webdav-lock, 32 versioning among the new) |
| Interop | e2e 12 (boto3 incl. tagging), 13 (mc), 22 (rclone) |
| Conformance | `make conformance` - ceph/s3-tests subset vs ratchet |
| Parity | `make parity-test` - a metadata provider never changes core responses |
| Real ZFS | `scripts/zfs-validate/run-zfs-validation.sh` - 70/70 on zfs-meta (2026-10-02), incl. versioning section 10 |
| Race | `make test-race` |
| Vulnerability | `make vuln` (govulncheck) |
| Secrets | `make secrets` (gitleaks) |

## Changes

- **1.0 (2026-10-02):** initial contract. Covers SigV4 + region
  configuration, key/bucket naming, tagging, multi-span Range,
  versioning (sidecar + snapshots + both), ZFS metadata extensions,
  error index, differences-from-AWS list.
