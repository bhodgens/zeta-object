# Leaf 04 - sync engine: ETag-diff PROPFIND scan (the universal path)

## Goal

`internal/sync`: the two-way sync engine. This is the heart of
zeta-cache. It reconciles server state (PROPFIND) against the local
index (leaf 03) and the FUSE-visible disk (leaf 02), uploads dirty
files, applies remote changes, and resolves conflicts by the locked
rules. The collection-token skip hint makes the walk cheap; file ETags
and If-Match make it correct.

## Server behaviors this consumes (all shipped, verified 2026-10-06)

- PROPFIND Depth 0/1; per-collection `getetag` = derived `dir-<hex>`
  immediate-children token (colltoken.go). Token stability, depth
  parity, and nested-insensitivity are pinned by e2e 19g - the engine
  may skip a subtree when the token matches the index's recorded token.
  Token is a HINT: on ANY doubt (error, 4xx, timeout) the engine falls
  back to listing that subtree. A wrong skip is recovered by the
  periodic full scan.
- File rows carry `getetag` (= MD5 hex), `getcontentlength`,
  `getlastmodified`, `oc:fileid`, `oc:permissions`.
- PUT with If-Match: 412 on stale - the conflict signal.
- PUT create with If-None-Match: * - create-if-absent.
- MOVE with Destination + Overwrite header; DELETE per key; JSON
  `?batch` delete for multi-key (leaf 07's recently-deleted uses
  per-key; batch is available).
- MKCOL for directories.

## Requirements

- Scanner: Depth-1 PROPFIND walk from the root, recursion driven by
  collection-token comparison against `index`'s stored dir-token
  (store the token in the resources row for collections: reuse
  `resources` with is-dir rows, path convention: trailing slash).
  Record which mechanism verified each subtree (`meta` key or a
  column): `token` vs `fullscan`.
- Diff engine - for each path, the three-way state (server row, index
  row, disk file) classifies into actions. THE LOCKED CONFLICT MATRIX
  (from the reference doc; implement EXACTLY this):
  - clean local + changed remote -> download (last-writer-wins by ETag
    compare).
  - dirty local + unchanged remote -> upload (If-Match with the index's
    known-good ETag; 412 = escalate to conflict).
  - dirty local + changed remote -> CONFLICT COPY: remote stays
    current; local preserved as `file (conflicted copy <date>).ext`
    (date = YYYY-MM-DD); never merged, never silently overwritten.
  - remote deleted + clean local -> delete locally (move to tombstone).
  - remote deleted + dirty local -> KEEP local, surface the conflict in
    IPC status (never propagate a deletion over local edits).
  - local deleted (user deleted via mount) + remote unchanged -> DELETE
    remote (If-Match with known ETag; 412 = escalate).
  - new remote (no index row) -> download.
  - new local (no index row, no server row) -> upload (If-None-Match: *).
- Upload pipeline: read file -> compute MD5 (the server ETag IS the MD5
  of the body; if computed matches index etag the file is unchanged -
  skip the PUT) -> PUT with If-Match/If-None-Match -> on success write
  journal upload-ok + index etag. PUT failure with 412 = conflict
  pipeline. Any other 5xx = leave dirty, retry per scheduler backoff.
  Chunked bodies rejected by the server: always send Content-Length
  (buffer or pre-size; files are fully on disk so size is known).
- Remote-apply pipeline: GET (Range-capable via leaf 06) to cache file
  (temp+rename), index update, journal hydrate row.
- Atomicity: index mutations journal-coupled (leaf 03 contract); cache
  file swaps always temp+rename; an interrupted sync resumes (idempotent
  re-scan; uploads are idempotent because If-Match binds them to the
  old ETag).
- Resumable upload: NOT in v1 (locked). On upload failure mid-flight,
  discard the attempt, keep dirty, retry whole file.
- The engine owns `status` IPC fields: state (idle/syncing/error),
  lastSync, dirty count, conflict count.
- Full-scan cadence belongs to leaf 07's scheduler; the engine exposes
  SyncOnce(ctx) and FullRescan(ctx).

## Constraints

- No event cursor here (leaf 05 adds it as a pre-pass hook - define the
  interface point `ChangeFeed` now: `Delta(ctx) (changedPaths []string,
  fullScan bool, err error)`; leaf 04's default impl = fullScan always).
- No quota/eviction decisions (leaf 07 calls CleanHydratedPaths).
- The engine NEVER writes `.metadata/`, never uses S3 endpoints.
- Server-compatible request discipline: Content-Length always present;
  PROPFIND Depth 1 bodies allprop (`<D:propfind><D:allprop/></D:propfind>`).

## Acceptance

1. `go test -race ./internal/sync/` green against a stub transport:
   every conflict-matrix row has a table test with the exact expected
   outcome, collection-token skip + forced-full-scan fallback, 412
   conflict-copy naming (`file (conflicted copy 2026-10-06).ext` shape),
   remote-delete-keep-local, tombstones, idempotent re-sync.
2. Integration test (build tag, live server optional): against the real
   gateway binary if present (reuse the e2e harness pattern), covering
   upload -> server-side modify -> rescan -> download, and the conflict
   copy end-to-end.
3. e2e case 40 grows the sync section IF FUSE present; the sync engine
   itself is transport-testable without FUSE - wire what the harness
   allows, keep the rest for leaf 02's e2e.
4. Report: the diff-decision table as implemented (state x action), and
   the ChangeFeed interface shape leaf 05 will implement.
