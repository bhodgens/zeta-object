# S3 Versioning - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 5 leaf documents
- **Scope:** S3 versioning (zeta-object#12): bucket versioning state
  (PUT/GET ?versioning), versioned PUT/DELETE with delete markers,
  ?versionId reads, ?versions listing. Dual: sidecar+versioned-files for
  dotfile buckets; zmetad-history-backed for ZFS buckets (metadata-true,
  data-current within the snapshot window) pending upstream
  zfs-metadata#15 - the ZFS data-read story lands as a follow-up when
  upstream answers; v1 ZFS path serves metadata versioning + current-data
  reads, documented per-bucket.

## Goal

Versioning-enabled buckets preserve old versions on overwrite; delete
markers hide without destroying; ?versionId reads any version (sidecar
path: all versions; ZFS path: current data, metadata history); ?versions
lists newest-first. Parity suite across both paths for the shared
subset.

## Architecture

- VersionId format: `<unix-micros>-<8hex>` (opaque, sortable, matches
  the derived-listing txg style for the ZFS path).
- Sidecar path: versioned data files under
  `<bucket>/.metadata/.versions/<key-sha>/<versionId>` (key-sha = sha256
  of the key, avoiding path-length/hostile-key issues); the `<key>.meta`
  sidecar gains `Versioning`, `Versions []{ID,IsLatest,IsDeleteMarker,Txg
  (zfs),Size,ETag,LastModified}` and `CurrentVersionID`. PUT with
  versioning enabled: write data to the versions dir, update sidecar.
  DELETE: append delete marker to sidecar (no data touched). GET: current
  or ?versionId from the versions dir.
- ZFS path: bucket versioning state identical (sidecar marker - state is
  bucket-level, path-independent); PUT/DELETE recorded by zmetad
  automatically; ?versions derived from zmetad rows (full_path) instead
  of sidecar Versions; ?versionId reads serve CURRENT data with an
  x-amz-version-id header of the requested id (documented degradation
  until zfs-metadata#15 lands: data-true versioning needs snapshots).
  The provider seam (versionStore interface) makes the ZFS
  snapshot-backed reader a drop-in later.
- Charter check: `.metadata/.versions/` is object-derived state in the
  bucket's own metadata area (like uploads staging), not server-owned
  identity state. OK.

## Interface Contracts

### Contract 1: versionStore (FROZEN)

```go
// File: internal/frontend/s3/versionstore.go
package s3

type VersionEntry struct {
    ID             string
    IsLatest       bool
    IsDeleteMarker bool
    Size           int64
    ETag           string
    LastModified   time.Time
}

type versionStore interface {
    State(bucket string) (string, error) // "Off"|"Enabled"|"Suspended"
    SetState(bucket, state string) error
    PutVersion(bucket, key string, r io.Reader, size int64, etag string) (VersionEntry, error)
    PutDeleteMarker(bucket, key string) (VersionEntry, error)
    List(bucket, key string) ([]VersionEntry, error) // newest first
    Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error)
}
// sidecarVersionStore (v1) + zfsVersionStore (metadata via zmetad DB;
// Open serves current bytes, documented). Constructor:
// versionStoreFor(bucketPath string, zdb *metadata.ZmetadDB) versionStore
```

### Contract 2: wire surface (FROZEN)

- `PUT/GET /<bucket>?versioning` (SubResource "versioning"): body
  `<VersioningConfiguration><Status>Enabled|Suspended|Off`...; response
  GET echoes state (omitting the element = Off).
- `GET /<bucket>/<key>?versionId=<id>`: 200 + x-amz-version-id;
  unknown id -> 400 InvalidArgument; delete-marker id -> 405 Method
 NotAllowed with x-amz-delete-marker: true header.
- Plain GET on delete-marked key -> 404 NotFound (x-amz-delete-marker:
  true).
- `GET /<bucket>?versions` (already routed): when versioning was ever
  enabled, render from versionStore (Version+DeleteMarker entries,
  newest-first, IsLatest); the old current-only listing stays for
  never-versioned buckets.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context |
|---|----------|------|-------------|-------------|
| 01 | 01-versionstore-sidecar.md | leaf | none | ~45K |
| 02 | 02-versionstore-zfs.md | leaf | 01 (interface) | ~40K |
| 03 | 03-versioning-handlers.md | leaf | 01, 02 | ~50K |
| 04 | 04-versions-listing.md | leaf | 03 | ~35K |
| 05 | 05-versioning-e2e.md | leaf | 03, 04 | ~40K |

02 may start after 01's contract is pinned (interface-only dependency)
but must land before 03.

## Brief contents per leaf (mandatory: leaf template shape)

- 01: sidecarVersionStore + tests: state round-trip, PutVersion writes
  `<sha>/<id>` data file + sidecar update (atomic via the existing
  writeFileAtomic helper), PutDeleteMarker appends, List newest-first,
  Open reads by id (current + old), delete-marker Open returns
  ErrIsDeleteMarker, never-versioned bucket = zero behavior change,
  -race. Key file: internal/frontend/s3/, storage.go for atomic-write
  helper, object_handlers.go for sidecar read/write sites.
- 02: zfsVersionStore: State/SetState via the same bucket-level sidecar
  marker (bucket-level state is path-independent); List from
  metadata.ZmetadDB Events (dataset via ResolveDatasetByPath) mapped to
  VersionEntry (create/rename = versions; remove = delete markers;
  txg-anchored ids `txg-<n>`); Open: current data + requested id
  echoed; docs comment citing zfs-metadata#15 + the documented
  degradation. Fixture DB from scripts/e2e/fixtures/zmetad-fixture for
  tests.
- 03: handlers per Contract 2 + wiring versionStoreFor into the GET/PUT/
  DELETE object paths (versioning-enabled buckets only - off buckets
  byte-identical behavior, pinned by tests) + README section replacing
  the "S3 versioning is not implemented" limitation (per-bucket note for
  the ZFS metadata-vs-data distinction + zfs-metadata#15 link).
- 04: ?versions listing rewrite per Contract 2 wire shape
  (Version/DeleteMarker XML entries, IsLatest, x-amz-delete-marker) +
  never-versioned fallback preserved + unit tests.
- 05: e2e `<n+1>-versioning.sh`: enable -> PUT v1 -> PUT v2 -> GET
  current=v2 -> GET ?versionId=v1=v1-bytes -> DELETE (marker) -> GET 404
  -> GET ?versionId=v2 still 200 -> ?versions shows 2 versions + marker
  newest-first -> suspend -> PUT overwrites in place (suspended
  semantics: new version replaces current, no new version retained per
  S3) -> disable. Plus parity note: run the same asserts against a ZFS
  bucket when the zfs-validate harness next runs (add a case file for
  it in scripts/zfs-validate/checks-section, appended to the harness's
  checks.py as section 10).

## Dispatch Protocol

Standard. 01 and 02 in parallel AFTER 01's contract file exists (pin the
interface file in leaf 01 task 1 so 02 can compile against it); 03 after
both; 04 after 03; 05 last.

## Coding Conventions

Stdlib-first; errors `s3: `; hyphens in prose; sidecar JSON additive
(backward-compat HARD requirement - old sidecars decode fine); frozen
interfaces untouched; charter note above restated in 01's brief.

## Completion Tracking Table

| Leaf | State | Review | Commit | Notes |
|------|-------|--------|--------|-------|
| 01-versionstore-sidecar.md | pending | - | - | |
| 02-versionstore-zfs.md | pending | - | - | |
| 03-versioning-handlers.md | pending | - | - | |
| 04-versions-listing.md | pending | - | - | |
| 05-versioning-e2e.md | pending | - | - | |

## Integration Test Plan

make test / lint NEW_FROM_REV / e2e green; parity: sidecar vs ZFS paths
share the acceptance suite (05's case runs against both via config;
zfs-validate harness gets section 10).

## Review Checklist

- [ ] Contracts verbatim; sidecar byte-compat + charter note present.
- [ ] Off-bucket behavior byte-identical (tests prove).
- [ ] ZFS data-read degradation documented per-bucket + upstream link.
- [ ] Delete-marker semantics correct (404 vs 405 paths).

## Open Questions

- zfs-metadata#15 answer determines when Open() becomes data-true for
  ZFS buckets (follow-up leaf; out of scope here).
