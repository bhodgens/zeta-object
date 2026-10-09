# Leaf 01 - daemon FileProvider surface

## Goal

Give the zeta-cache daemon the IPC surface + on-disk contract a FileProvider
extension needs, so leaf 02's Swift sources are a pure adapter.

## Requirements

1. New IPC request types (additive; v1 envelope unchanged):
   - `enumerate` -> data: array of entries for a path (dir key with
     trailing slash): {name, key, isDir, size, mtime, materialized
     (local cache file exists), dirty}.
   - `download` {path} -> triggers hydration if not materialized
     (engine's download path); returns {materialized:true} when the
     local cache file is complete (poll-until-ready semantics
     documented: the request blocks until hydrated or ctx timeout).
   - `dehydrate` {path} -> evict ONE file: clean-only (refuses dirty,
     pinned, tombstoned - same refusals as leaf 07's `evict`, but
     keyed, not global).
   - `item` {path} -> single-entry metadata (the Lookup analog:
     exists, size, mtime, etag, materialized, dirty).
2. Local cache path exposure: the extension reads data bytes directly
   from cacheDir/files/<key> (same user). Document the rule: data via
   the FILE, metadata via IPC. `item`/`enumerate` responses must carry
   the cache-relative path so the extension can open it.
3. LaunchAgent artifact: gui/launchagents/com.zeta-object.zeta-cache.plist
   (documented, not installed) running the daemon at login with the
   config path argument.
4. README: new IPC table rows + the FileProvider integration section
   pointing at leaf 02.
5. e2e case 40 extension: assert enumerate/item/download/dehydrate round
   trips against the running daemon (independent of FUSE - these are
   IPC-only asserts, so they run on CI too).

## Constraints

- Frozen: existing IPC shapes (status/pause/resume/pin/pins/tombstones/
  conflicts.*/deleted.*/evict), internal/fusefs, internal/transport.
- New types go through the additive Handler seam (leaf 07 pattern).
- Do not edit internal/sync (the download path already exists via the
  engine - call it).
- No cgo.

## Acceptance

1. go test -race -count=1 ./... green with new IPC tests (every new
   type, error shapes, concurrent clients).
2. Case 40's new asserts green on CI (IPC-only, no FUSE needed).
3. Report: the full IPC table v2 (all types), the file-path contract,
   deviations.
