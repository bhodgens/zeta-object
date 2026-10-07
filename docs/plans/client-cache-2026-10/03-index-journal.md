# Leaf 03 - client index DB + sync journal

## Goal

`internal/index`: the client-owned SQLite state - the resources index
and the append-only sync journal. Both structures are charter-clean
(client device only). The purge/eviction pass and the reconnect diff
both depend on this being query-shaped, not crawl-shaped.

## Requirements

- modernc.org/sqlite (no cgo). DB at the configured indexDB path.
  WAL mode ON at open (FUSE path and sync engine must not block each
  other). `PRAGMA synchronous=FULL` for the journal's crash story.
- Schema v1 (embed a user_version):
  - `resources(path TEXT PRIMARY KEY, etag TEXT, mtime INTEGER,
    size INTEGER, hydrated INTEGER, dirty INTEGER, deleted INTEGER)` -
    one row per known remote-or-local path. `deleted=1` rows are
    tombstones feeding the deletion-grace table (leaf 07 GUI restore).
  - `journal(id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, op TEXT,
    path TEXT, detail TEXT)` - append-only record of MY operations
    (local-write, upload-ok, upload-conflict, delete, hydrate). Used to
    answer "I changed it" vs "the world changed it" on reconnect.
  - `meta(key TEXT PRIMARY KEY, value TEXT)` - schema version, last
    full-scan time, cursor state placeholder (leaf 05 owns its keys).
- APIs (interface + impl): Get/PutPath/DeletePath/DirtyPaths/
  CleanHydratedPaths (the eviction query: hydrated AND clean AND NOT
  pinned - pins column arrives in leaf 07, keep the query parameterized),
  JournalAppend, JournalSince(id), ReconcilePass (see below).
- Daemon-start reconcile: journal rows vs disk vs index disagreement
  resolution - a dirty file present on disk with no journal upload-ok =
  still dirty (keep); index says hydrated but file missing = demote to
  not-hydrated; journal says upload-ok but index dirty=1 = re-mark
  clean only if ETag re-probe (deferred to leaf 04's scan; journal alone
  never overwrites index state). Write the rules as a table in the
  package doc and implement exactly them.
- Crash safety: every state transition writes the journal row in the
  SAME transaction as the index mutation. A torn run recovers via the
  reconcile pass. Test it: kill the DB connection mid-sequence (simulated
  via a failing hook), reopen, run reconcile, assert the state table.
- Purge: `DELETE FROM resources WHERE hydrated=1 AND dirty=0 AND ...`
  in ONE query; journal rows are NEVER deleted by eviction (append-only
  until a compaction command - out of v1 scope).
- Concurrency: single writer connection + WAL readers; document the
  lock discipline; tests must run index ops concurrently with reads
  under -race.

## Constraints

- No transport, no FUSE, no sync policy - pure state layer.
- The index NEVER stores server-side state beyond per-file protocol
  fields (etag, size, mtime) - that is protocol data about files, not
  server-owned bookkeeping; charter-clean because it lives on the client.

## Acceptance

1. `go test -race ./internal/index/` green: CRUD, WAL concurrency,
   reconcile table cases, eviction query shape, tombstone lifecycle.
2. Crash-simulation test green (torn transaction -> reopen -> reconcile).
3. Report: final schema DDL, the reconcile rules table, and the eviction
   query text.
