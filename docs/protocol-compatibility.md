# Protocol Compatibility Matrix

This is the honest capability matrix for every client protocol zeta-object
speaks: what is implemented, what degrades (and how the degradation is
expressed on the wire), and what is absent. Nothing here is emulated
silently - every gap is a documented status code or a refused method.

Each row's proof is linked: an e2e case under `scripts/e2e/cases/`, the
ceph/s3-tests conformance ratchet (`make conformance`), or the ZFS
validation harness (`scripts/zfs-validate/run-zfs-validation.sh`, run
against real OpenZFS on the zfs-meta host - latest:
[docs/validation-zmetad-2026-10-01.md](validation-zmetad-2026-10-01.md)).

Reading the columns:

- **Implemented** - full wire behavior, covered by tests.
- **Degrades** - the request is answered, but with the documented
  narrower semantics. The response says so (an error code, a header, or
  a capabilities document) - never a fake success.
- **Absent** - the method or subresource answers a clean error
  (`501 NotImplemented`, `405`, `404`, or the protocol's equivalent).
  A capability that the backing filesystem cannot provide is simply
  absent, never faked.

## Legend

- e2e case numbers refer to `scripts/e2e/cases/<n>-<name>.sh`.
- "conformance" = covered by the ceph/s3-tests ratchet subset.
- "zfs-validate" = proven on a real OpenZFS host by the validation
  harness.
- Upstream links refer to [bhodgens/zfs-metadata](https://github.com/bhodgens/zfs-metadata)
  issues (the ZFS events/snapshots stack this server reads).

## S3 API

### Objects and buckets

| Operation | Status | Notes / proof |
|---|---|---|
| PUT / GET / HEAD / DELETE object | Implemented | HEAD carries full metadata; DELETE idempotent 204. e2e 02/03, conformance |
| PUT ?uploads / parts / complete (multipart) | Implemented | parts 1-10000, expiry sweeper, `ETag` assembly. e2e 05, conformance |
| ListObjectsV2 (+V1 shape) | Implemented | prefix/delimiter/continuation-token/`encoding-type=url`. e2e 04, conformance |
| CopyObject | Implemented | COPY/REPLACE directives for metadata AND tags. e2e 08, conformance |
| Batch DeleteObjects | Implemented | per-key result entries. e2e 08 |
| Range requests - single span | Implemented | 206/416, suffix and open-ended forms, clamping. e2e 06, conformance |
| Range requests - multi-span | Implemented | RFC 9110 `multipart/byteranges`, span coalescing, 100-part cap (over cap = 200 full body), all-unsatisfiable = 416. e2e 29 |
| Conditional GET | Implemented | If-Match / If-None-Match / If-(Un)Modified-Since, evaluated before Range. e2e 06, conformance |
| Presigned URLs | Implemented | query-form SigV4, expiry honored, region-aware. e2e 07, 30 |
| SigV4 (header + `aws-chunked` streaming) | Implemented | trailers skipped (not checksum-verified - see Absent). e2e 09 |
| SigV4 region | Configurable | `region` config key, default `us-east-1`; explicit = strict compare (mismatch fails `SignatureDoesNotMatch` naming the expected region); default = permissive-unset is a test-only mode, a launched server is always strict. GetBucketLocation reports it. e2e 30 |
| Object tagging | Implemented | `x-amz-tagging` on PUT/COPY (COPY/REPLACE directives), `?tagging` GET/PUT/DELETE, `TagCount` on GET/HEAD; S3 limits enforced (10 tags, 128B keys, 256B values, reserved `aws:` -> `InvalidTag`). e2e 30, boto3 interop e2e 12 |
| Versioning | Implemented, per mechanism | see the versioning table below. e2e 32, zfs-validate section 10 |
| `?versions` listing | Implemented | versioned buckets: per-key versions + delete markers, newest-first, `IsLatest`, mid-key `version-id-marker` resume. Never-versioned buckets keep the legacy listing. e2e 32 |
| GetBucketLocation | Implemented | reports the configured region. e2e 30 |
| Batch delete of 0 keys | Degrades | accepted, empty result |

### S3 - absent (clean errors)

| Operation | Response |
|---|---|
| ACLs (`PutBucketAcl`/`GetBucketAcl`, `x-amz-acl`) | `501 NotImplemented` - authorization is per-identity grants |
| Bucket policies | absent - same grant model |
| Object-level ACLs | absent |
| S3 lifecycle configuration | absent (Event Actions cover some workflows differently) |
| Checksum verification (`x-amz-checksum-*`, CRC32C/SHA trailers) | trailers are parsed and skipped, never verified |
| `?versionId` on COPY source | `501 NotImplemented` |
| MFA delete | absent |
| Other-region SigV4a | absent (SigV4 only) |

### S3 versioning - mechanisms per bucket kind (config `zfs_versioning`)

| Mode | Bucket kind | Mechanism | Delete markers | `?versionId` reads | History fidelity |
|------|-------------|-----------|----------------|--------------------|------------------|
| `snapshots` (default) | ZFS-backed | faux versioning from the dataset's EXISTING snapshots; VersionId = snapshot name; no snapshots are taken by zeta-object | no | `<mountpoint>/.zfs/snapshot/<snap>/<key>`, honest 404 when the snapshot or key is gone | whatever the host's snapshot policy retains |
| `sidecar` | any (always for non-ZFS buckets) | true per-write versioning: copies under `.metadata/.versions/<key-sha>/` + bookkeeping in the object's sidecar | yes | exact bytes of every recorded write | exact per-write, zeta-object writes only |
| `both` | ZFS-backed | sidecar per-write versions MERGED with snapshot entries | yes (sidecar-authoritative) | sidecar IDs first, then snapshot names | merged |

Versioning state lives in the bucket (`.metadata/.versioning` marker);
versioned copies live in the bucket's `.metadata/.versions/`. A deleted
key under `snapshots` mode has no marker - old snapshots still hold the
bytes. e2e 32 (sidecar), zfs-validate section 10 (snapshots, real ZFS).

## S3 extensions (ZFS-backed buckets)

| Extension | Status | Proof |
|---|---|---|
| `GET ?events` - per-key / bucket event history | Implemented | served from zmetad's SQLite export (DB layout 5-6); insert-time-resolved `full_path` makes nested keys exact. e2e 18-zmetad, zfs-validate |
| `GET ?events&versions` - derived versions listing | Implemented | separate extension, NOT S3 versioning; carries `IsLossy`/`RecordsLost`/`RingSwaps`. e2e 17 |
| Loss accounting | Implemented | `recordsLost` = lifetime known-lost (survives retention); `ringSwaps` = kernel-log identity swaps, never folded into record counts. zfs-validate |
| Freshness | Degrades | events appear within one zmetad poll (default 30s); `SIGUSR1` forces an out-of-band collect |
| events=off dataset | Degrades | zmetad prunes untracked datasets -> clean `503` (same wire behavior as the capability being absent). zfs-validate section 5 |
| Purge | Operator-only | `zmetad --purge <dataset>` on the host; deliberately NOT an HTTP endpoint (audit-flavored data; write credentials must not grant erasure) |

## WebDAV (RFC 4918 class 1 subset)

Implemented methods: `OPTIONS`, `PROPFIND` (Depth 0/1), `GET`, `HEAD`,
`PUT`, `DELETE`, `MKCOL`, `COPY`, `MOVE`, `LOCK`, `UNLOCK`.

| Capability | Status | Notes / proof |
|---|---|---|
| Basic authentication | Implemented | one identity registry shared with S3/SFTP/FTP. e2e 19 |
| PROPFIND Depth 0/1 | Implemented | `Depth: infinity` -> `403` with `<propfind-finite-depth/>`. e2e 19 |
| LOCK / UNLOCK | Implemented | exclusive write locks on FILES, Depth 0, `Timeout: Second-N` capped 3600s, `opaquelocktoken:` UUIDs, refresh via empty-body + `If`, expiry leases, 423 enforcement on PUT/DELETE/MOVE (dest and MOVE source), read methods exempt. Locks are server-side advisory state under `<bucket>/.metadata/.locks/` - a kernel-enforced fence stays an open question (zfs-metadata#14). e2e 31 |
| Collection locks | Absent | files only; collections/root answer `405` + `Allow`. davfs2 builds that hard-require directory locks: `use_locks 0` (documented) |
| Shared locks, Depth-infinity locks | Absent | `400` |
| Conditional requests (If-Match etc.) | Implemented | e2e 06 |
| Chunked PUT | Implemented | real clients send it. e2e case for issue #6 |
| PROPPATCH, dead properties | Absent | `405` + `Allow` |
| Collection COPY/MOVE | Absent | `403` (a recursive fake copy risks partial state) |
| Quotas, versioning | Absent | not exposed |

Mount notes: davfs2 works with its DEFAULT config (`use_locks 1`); macOS
Finder and Windows mount with Basic auth. e2e 19 (59 asserts), e2e 31
(47 asserts); mount-level checks are manual by design.

## ownCloud (OCS over the WebDAV data plane)

| Capability | Status | Notes / proof |
|---|---|---|
| OCS v1+v2 `config`, `cloud/capabilities`, `cloud/user` | Implemented | XML envelopes; the capabilities document advertises exactly what is served. e2e 23 |
| `/status.php` capability ping | Implemented | unauthenticated by design, any path depth. e2e 23 + unit |
| `/remote.php/webdav/**` data plane | Implemented | full WebDAV subset above, with prefix stripping for classic and oCIS client shapes. e2e 24 |
| `?tagging`/`LOCK` through the owncloud port | Implemented | same data plane. e2e 30/31 |
| Shares, provisioning, app passwords, oCIS endpoints | Absent | documented `404` OCS envelope, never a silent success |
| Real desktop-client sync | Manual gate | connect->sync->delete pass documented in `docs/plans/owncloud-2026-09/decision.md` §6-7 - NOT yet run |

## FTP / FTPS

| Capability | Status | Notes |
|---|---|---|
| AUTH TLS (explicit FTPS) | Implemented | e2e 20 |
| LIST/NLST/MLSD, STOR/RETR/DELE, MKD/RMD, SIZE/MDTM/STAT | Implemented | buckets as top-level directories; `dir/` zero-byte markers |
| RNFR/RNTO rename | Degrades | Get+Put+Delete; no atomicity |
| REST resume, CHMOD-style SITE commands | Absent | rejected `502`-class |
| Multipart, conditional reads | Absent | the capability set cannot express them |

## SFTP

| Capability | Status | Notes |
|---|---|---|
| Password + public-key auth | Implemented | `sshPublicKeys` per identity; one key = one identity across the registry. e2e 21 |
| open/read/write/close, readdir, mkdir/rmdir, stat | Implemented | `dir/` marker convention |
| setstat/fsetstat (chmod/chown/utimes) | Degrades | `SSH_FX_PERMISSION_DENIED` |
| symlink/readlink | Degrades | `SSH_FX_OP_UNSUPPORTED` |
| rename | Degrades | Get+Put+Delete; no atomicity |
| Multipart, conditional reads, versioning | Absent | no SFTP expression |

## Cross-cutting guarantees (every protocol)

| Guarantee | Mechanism | Proof |
|---|---|---|
| Parity: an attached metadata provider never changes core responses | parity gate (`make parity-test`) | parity_test.go |
| Atomic writes: a crash never truncates an object | temp + fsync + rename | unit tests |
| Per-key write serialization | `lockObject` seam | unit + `-race` |
| One identity registry across all frontends | shared registry | e2e 18/26/27 |
| Path-traversal rejection (`..`, `.metadata` segments) | key validation | e2e 03/19 |
| Principal breadcrumbs (`user.zeta.*` xattrs) + optional append-only audit log | xattr stamping at the write path | e2e 28 |
| Per-package coverage floors + conformance ratchet | CI + `make conformance` | `.github/workflows/check.yml` |

Behavior contract: the authoritative per-operation S3 behavior reference
(request/response shapes, error codes, divergences from AWS) is
[docs/s3-behavior.md](s3-behavior.md) - maintained under the same
documentation contract as zmetad's `SCHEMA.md`: a behavior change
without a doc change is a bug.
