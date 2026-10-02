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
  dotfile buckets AND as an opt-in on ZFS buckets; ZFS buckets default to
  FAUX VERSIONING derived from existing ZFS snapshots (listing + reads
  from the snapshot window - no per-upload snapshots, no zfs-metadata#15
  dependency for v1; that remains a future data-true upgrade). User
  decision 2026-10-02.

## Goal

Versioning-enabled buckets preserve old versions on overwrite; delete
markers hide without destroying; ?versionId reads any version. Mechanism
per bucket kind:

- **Non-ZFS buckets:** sidecar store - real per-write versioning.
- **ZFS buckets (default, "snapshots" mode):** faux versioning from
  existing ZFS snapshots. VersionId = snapshot name; ?versions lists
  snapshots (newest first) where the key exists; ?versionId reads
  `<mountpoint>/.zfs/snapshot/<snap>/<relpath>`. No delete markers (a
  deleted key has no snapshot presence going forward; old snapshots still
  hold the data). No new ZFS capability required - works against ANY
  snapshot policy the host already runs.
- **ZFS buckets, opt-in "sidecar" mode:** identical to the dotfile path -
  true per-write versions + delete markers.
- **ZFS "both" mode:** sidecar per-write versions MERGED with
  snapshot-derived entries in ?versions; ?versionId resolves sidecar ids
  first, then snapshot names.

Mechanism selection: server config key `zfs_versioning` - one of
`snapshots` (default) | `sidecar` | `both`; applies to ZFS-backed buckets
only (non-ZFS always sidecar). README documents per-bucket semantics:
snapshot mode = windowed history (whatever the host's snapshot policy
retains), sidecar mode = exact per-write but only for zeta-object writes.

## Architecture

- VersionId formats: sidecar `<unix-micros>-<8hex>`; snapshot mode the
  snapshot NAME (e.g. `auto-20261002-143000`) - the durable handle while
  the snapshot exists; expired-snapshot reads answer 404 honestly.
- Sidecar path: versioned data files under
  `<bucket>/.metadata/.versions/<key-sha>/<versionId>` (key-sha = sha256
  of the key); the `<key>.meta` sidecar gains `Versioning`, `Versions
  []{ID,IsLatest,IsDeleteMarker,Size,ETag,LastModified}` and
  `CurrentVersionID`. PUT creates a version when enabled; DELETE writes a
  delete marker (data untouched).
- ZFS snapshot path: enumerate the dataset's snapshots via
  `zfs list -H -t snapshot -o name,creation -d 1 <dataset>` (ONE exec,
  semaphore-bounded - the zfsExecSem discipline; acceptable for the
  versions read path; the zero-exec rule was pinned for the events hot
  path). Per-(snapshot, key) existence = stat under
  `<mountpoint>/.zfs/snapshot/<snap>/<relpath>` (snapdir hidden still
  allows direct access). List = newest-first existence walk; Open =
  direct file open. Mountpoint from zmetad's `datasets` table where
  available, else ResolveDataset-style fallback is NOT used (v1:
  snapshots mode requires zmetad tracking - same prerequisite as
  ?events).
- Charter check: `.metadata/.versions/` is object-derived state in the
  bucket's own metadata area; snapshots are ZFS state; enumeration is
  read-only exec. No server-owned identity state. OK.

## Architecture

- VersionId format: `<unix-micros>-<8hex>` (opaque, sortable, matches
  the derived-listing txg style for the ZFS path).
- Sidecar path: versioned data files under
  `<bucket>/.metadata/.versions/<key-sha>/<versionId>` (key-sha = sha256
  of the key, avoiding path-length/hostile-key issues); the `<key>.meta`
  sidecar gains `Versioning`, `Versions []{ID,IsLatest,IsDeleteMarker,
  Size,ETag,LastModified}` and `CurrentVersionID`. PUT with versioning
  enabled: write data to the versions dir, update sidecar. DELETE: append
  delete marker to sidecar (no data touched). GET: current or ?versionId
  from the versions dir.
- ZFS snapshot path (default for ZFS buckets): versionStore backed by the
  dataset's EXISTING snapshots - no new snapshots are taken, no
  zfs-metadata#15 dependency. VersionId = snapshot name. List(bucket,
  key): enumerate `zfs list -H -t snapshot -o name,creation -d 1 <ds>`
  (one exec, semaphore-bounded), stat
  `<mountpoint>/.zfs/snapshot/<snap>/<relpath>` per snapshot newest-first,
  emit entries for snapshots containing the key. Open(bucket, key, id):
  open the file under that snapshot; unknown/expired snapshot or missing
  key -> 404-class error, never current-data substitution. No delete
  markers in this mode. zfs-metadata#15 (snapshot-on-write) remains a
  future upgrade: denser versions, zero code change here.
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
    PutDeleteMarker(bucket, key string) (VersionEntry, error) // snapshot store: ErrDeleteMarkersUnsupported
    List(bucket, key string) ([]VersionEntry, error)          // newest first
    Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error)
}
// sidecarVersionStore (dotfile buckets + opt-in ZFS) and
// zfsSnapshotVersionStore (ZFS default). Constructor:
// versionStoreFor(bucketPath string, zdb *metadata.ZmetadDB, mode string) versionStore
// mode: "sidecar" | "snapshots" | "both" (both = mergeStore wrapper).
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

### Contract 3: config + snapshot enumeration (FROZEN)

```go
// config.json: "zfs_versioning": "snapshots" | "sidecar" | "both"
//   (default "snapshots"; applies to ZFS-backed buckets only).
// main.go config struct: ZfsVersioning string `json:"zfs_versioning"`,
// default + validation at config load.

// File: internal/frontend/s3/zfssnapshots.go
// ListSnapshots(ctx, dataset) ([]SnapInfo, error) - newest first.
//   ONE `zfs list -H -t snapshot -o name,creation -d 1 <dataset>` exec,
//   semaphore-bounded (reintroduce zfsExecSem-style channel), 5s timeout,
//   argv-only. SnapInfo{Name string /* full ds@snap */, Short string,
//   Creation time.Time}.
// SnapPath(mountpoint, snapShort, relPath string) string -
//   <mountpoint>/.zfs/snapshot/<snapShort>/<relPath> (pure helper).
```

## Child Document Index

| # | Document | Type | Dependencies | Est. Context |
|---|----------|------|-------------|-------------|
| 01 | 01-versionstore-sidecar.md | leaf | none | ~45K |
| 02 | 02-versionstore-snapshots.md | leaf | 01 (interface) | ~40K |
| 03 | 03-versioning-handlers.md | leaf | 01, 02 | ~50K |
| 04 | 04-versions-listing.md | leaf | 03 | ~35K |
| 05 | 05-versioning-e2e.md | leaf | 03, 04 | ~40K |

02 may start after 01's interface file exists; must land before 03.

## Brief contents per leaf (mandatory: leaf template shape)

- 01: sidecarVersionStore + tests: state round-trip, PutVersion writes
  `<sha>/<id>` data file + sidecar update (atomic via the existing
  writeFileAtomic helper), PutDeleteMarker appends, List newest-first,
  Open reads by id (current + old), delete-marker Open returns
  ErrIsDeleteMarker, never-versioned bucket = zero behavior change,
  -race. Files: internal/frontend/s3/versionstore.go (interface +
  sidecarVersionStore + versionStoreFor factory taking mode),
  versionstore_test.go. Interface file lands FIRST in task 1 so leaf 02
  can compile against it.
- 02: zfsSnapshotVersionStore + zfssnapshots.go per Contract 3
  (ListSnapshots exec + SnapPath helper + semaphore) + tests with a
  scripted fake zfs runner (like the old zfsRunner seam): List walks
  snapshots newest-first with stat checks against a temp-dir fake
  snapdir, Open reads the snapshot file, unknown snapshot -> 404-class,
  PutDeleteMarker -> ErrDeleteMarkersUnsupported, State/SetState via the
  same bucket-level sidecar marker as the sidecar store (bucket-level
  state is path-independent and mechanism-independent).
- 03: handlers per Contract 2 + versionStoreFor wiring into GET/PUT/
  DELETE object paths via the mode config (versioning-enabled buckets
  only - off buckets byte-identical, pinned by tests) + both-mode merge
  in ?versionId resolution (sidecar id first, then snapshot name) +
  README section replacing the "S3 versioning is not implemented"
  limitation (config key, per-mode semantics table: sidecar = exact
  per-write/delete markers, snapshots = windowed faux versioning no
  delete markers, both = merged; zfs-metadata#15 noted as the future
  densification).
- 04: ?versions listing rewrite per Contract 2 wire shape
  (Version/DeleteMarker XML entries, IsLatest, x-amz-delete-marker) +
  never-versioned fallback preserved + unit tests (both stores +
  merged).
- 05: e2e `<n+1>-versioning.sh` (sidecar/non-ZFS path): enable -> PUT v1
  -> PUT v2 -> GET current=v2 -> GET ?versionId=v1=v1-bytes -> DELETE
  (marker) -> GET 404 -> GET ?versionId=v2 still 200 -> ?versions shows
  2 versions + marker newest-first -> suspend semantics -> disable.
  ZFS snapshots-mode parity: add section 10 to
  scripts/zfs-validate/run-zfs-validation.sh checks.py - create
  snapshots manually (sudo zfs snapshot testpool/zval@s1, write, @s2),
  ?versions lists them, ?versionId=<snap> reads snapshot bytes.

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
