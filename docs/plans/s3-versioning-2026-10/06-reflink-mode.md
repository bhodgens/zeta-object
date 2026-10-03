# Reflink (Block-Clone) Versioning Mode - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below. Concern-split commits with explicit paths;
> gates before each code commit. Check `git log origin/main..main` and
> `git log --oneline -5` before EVERY commit (sibling sessions commit
> concurrently); never stage sibling files.

## Meta

- **Parent:** ../master.md
- **Scope:** third `zfs_versioning` mode "reflink" (per-write versions as
  FICLONE block clones), the retention config key, capture-path
  integration, e2e case 33, zfs-validate section 11, docs.
- **Dependencies:** leaves 01-05 landed (sidecar store, snapshots store,
  handlers, ?versions listing, e2e/zfs-validate coverage).
- **User decision:** approved design 2026-10-02 — reflink becomes the
  DEFAULT for ZFS buckets.

## Goal

On a versioning-ENABLED ZFS bucket in reflink mode, BEFORE the plain
overwrite of the current object, the server reflink-copies (FICLONE,
`unix.IoctlFileClone`) the CURRENT object file into
`<bucket>/.metadata/.versions-r/<key-sha>/<versionId>` — a block clone,
O(1) in bytes instead of a full rewritten copy. Bookkeeping is the SAME
versioned-sidecar shape the sidecar mode uses (`Versioning` / `Versions`
array / `CurrentVersionID` in the key's `.meta`), so `?versions`,
`?versionId`, and delete markers work identically. On FICLONE failure
(cross-dataset, old ZFS, non-ZFS dev FS) the capture FAILS SOFT: one WARN
log, the version is skipped, the PUT never breaks.

## Contracts

### Contract R1: config surface (owner: config.go; consumer: s3_wiring.go, README)

```go
// zfs_versioning: "snapshots" | "sidecar" | "reflink" | "both"
//   default "reflink" (defaultZfsVersioning); unknown value aborts startup.
// zfs_versioning_reflink_retention: int, count of version DATA files
//   retained PER KEY (newest N kept); 0/unset = unlimited; negative
//   value aborts startup. Server-wide v1 — per-bucket override is a
//   future key (README documents this).
```

The s3 package receives retention through the seam
`InstallZfsVersioningReflinkRetention(int)` (same pattern as
`InstallZfsVersioningMode`).

### Contract R2: on-disk layout (owner: reflinkversions.go; consumer: none outside the s3 package)

- Version data: `<bucket>/.metadata/.versions-r/<key-sha>/<versionId>`
  — key-sha discipline identical to sidecar mode (sha256 hex of the
  key), DISJOINT directory name `.versions-r` so the two mechanisms'
  on-disk state never mixes.
- VersionId: the sidecar format `<unix-micros>-<8hex>` (shared
  `newStateVersionID`), so `isSidecarShapedVersionID` wire rules apply
  unchanged.
- Bookkeeping: the key's existing `.meta` sidecar
  (`Versioning`/`Versions`/`CurrentVersionID`) — no new sidecar fields.
- Clone op: open src O_RDONLY; create dst
  O_WRONLY|O_CREATE|O_EXCL 0644; `unix.IoctlFileClone(dstFd, srcFd)`;
  GOOS-split files (linux = the ioctl; darwin/other = typed
  `errReflinkUnsupported`). On ioctl failure the dst file is removed.

### Contract R3: capture/record split (owner: versioning_handlers.go)

The backend still rewrites the sidecar wholesale on Put, so the
capture/record split is KEPT: the reflink DATA copy happens at capture
time (pre-overwrite); the record step after the successful Put writes
ONLY the sidecar bookkeeping entry (pre-minted id, ETag = the sidecar's
stored ETag of the old object, Size = stat of the old object). The
captured history rides the capture exactly as in sidecar mode.

### Contract R4: both mode (owner: versionstore.go factory)

"both" = sidecar + reflink MERGED, snapshots stay OUT (per-write
histories from both mechanisms are the same timeline; merging snapshot
entries in would double-render). Writes go through the reflink layout;
Open resolves `.versions-r` data first and falls back to the plain
`.versions` layout for history written while the mode was "sidecar"
(operator switched modes mid-history). The zero-value seam state ("")
still maps to the sidecar store — only the CONFIG default changes.

### Contract R5: retention (owner: reflinkversions.go prune)

After each successful capture+record with retention N > 0: delete the
OLDEST version data files beyond N per key (VERSIONS counted, delete
markers never pruned and never counted) and then rewrite the sidecar
without the pruned entries. Crash-safe order: data files FIRST, sidecar
SECOND — a crash between leaves orphaned bytes, never a lying sidecar
(same invariant as recordCapturedObjectVersion). Prune errors are
logged, never fail the PUT (housekeeping, not the primary operation).

### Contract R6: delete semantics

Versioning Enabled + reflink mode = delete marker ONLY (plain delete
suppressed; data + versions untouched) — identical to sidecar Enabled
semantics; `deleteObjectVersionedMarker` is REUSED unchanged (the mode
dispatch already flows through `versionStoreForBucket`).

## Tasks

1. Config surface: `defaultZfsVersioning` -> "reflink", vocabulary
   validation, `zfs_versioning_reflink_retention` key + non-negative
   validation, config tests, config.json.example, pin
   zfs_versioning=snapshots explicitly in the zfs-validate harness
   config (it previously relied on the default).
2. `reflinkversions.go` (reflinkVersionStore embedding
   sidecarVersionStore with `versionsDirName=".versions-r"`, factory
   cases, GOOS-split clone files, capture integration in
   versioning_handlers.go, seam retention install). Unit tests: factory
   dispatch, fail-soft capture, round-trip via the clone seam fake
   (GOOS fallback path), linux real-ioctl probe where the FS supports
   it (skips honestly otherwise — the ioctl itself is proven live by
   zfs-validate section 11).
3. Retention prune + tests (order, crash-safety invariant: every
   remaining sidecar entry has a data file, pruned ids are gone).
4. e2e case 33 (fail-soft-honest: PUTs always 200; version asserts
   probe-based — bytes of any listed version round-trip, count bounded
   by writes and by retention), zfs-validate section 11 (server restart
   with reflink config; on-disk .versions-r existence, clone evidence,
   ?versionId old bytes, retention prune, delete marker), README +
   case-32 comment refresh.

## Self-Verification Checklist

- [ ] Gates per commit: go build, go vet, gofmt (own files),
      golangci-lint 0 issues, go test ./...; make e2e before the final
      commit.
- [ ] zfs-validate ALL green incl. new section 11 checks.
- [ ] Charter: .versions-r is object-derived state in the bucket's own
      metadata area; no server-owned identity state.
- [ ] Frozen Contract 1 interface untouched.

## Review Checklist (for review agent)

- [ ] Fail-soft never breaks a PUT (WARN once, no version record).
- [ ] Prune order: data files before sidecar rewrite.
- [ ] Retention counts versions, not markers.
- [ ] both-mode Open fallback never substitutes current data.
