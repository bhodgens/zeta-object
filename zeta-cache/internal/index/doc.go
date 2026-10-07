// Package index owns the client-side SQLite index DB (modernc.org/sqlite,
// no cgo): the resources index, the append-only sync journal, and the
// daemon-start reconcile pass. All state here lives on the CLIENT device
// only - nothing in this file is ever sent to the server beyond per-file
// protocol fields (etag/size/mtime), so it is charter-clean.
//
// # Schema v1 (PRAGMA user_version = 1)
//
//	CREATE TABLE resources (
//	  path     TEXT PRIMARY KEY,
//	  etag     TEXT NOT NULL DEFAULT '',
//	  mtime    INTEGER NOT NULL DEFAULT 0,  -- unix seconds, remote-or-local mtime
//	  size     INTEGER NOT NULL DEFAULT 0,
//	  hydrated INTEGER NOT NULL DEFAULT 0,  -- body present in the local cache dir
//	  dirty    INTEGER NOT NULL DEFAULT 0,  -- local writes not yet uploaded
//	  deleted  INTEGER NOT NULL DEFAULT 0   -- tombstone feeding the deletion-grace
//	                                      -- table (leaf 07 GUI restore)
//	);
//	CREATE INDEX idx_resources_evict ON resources(hydrated, dirty, deleted);
//	CREATE INDEX idx_resources_dirty ON resources(dirty) WHERE dirty = 1;
//
//	CREATE TABLE journal (
//	  id     INTEGER PRIMARY KEY AUTOINCREMENT,
//	  ts     INTEGER NOT NULL,              -- unix seconds
//	  op     TEXT NOT NULL,                 -- local-write | upload-ok |
//	                                        -- upload-conflict | delete | hydrate
//	  path   TEXT NOT NULL,
//	  detail TEXT NOT NULL DEFAULT ''
//	);
//	CREATE INDEX idx_journal_path_op ON journal(path, op);
//
//	CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
//
// The journal is append-only: it records MY operations so a reconnect can
// answer "I changed it" vs "the world changed it". Journal rows are NEVER
// deleted by eviction; compaction is out of v1 scope. Tombstone expiry
// pruning is leaf 07 (see ExpiredTombstones for the retention query
// groundwork).
//
// # Crash safety
//
// Every operation-driven state transition writes its journal row in the
// SAME transaction as the index mutation. A torn run (process death,
// killed connection) either lands both or neither; PRAGMA synchronous=FULL
// keeps the commit durable across power loss. Recovery from a torn run is
// the reconcile pass, not the journal alone.
//
// # Daemon-start reconcile rules (ReconcilePass)
//
// Run at daemon start before the sync loop (leaf 04 wires the call). Given
// the index row, the disk probe (file present in the local cache dir?),
// and the journal, exactly these rules apply - and nothing else:
//
//	| Rule | index state            | disk     | journal          | action                          |
//	|------|------------------------|----------|------------------|---------------------------------|
//	| R1   | dirty=1, deleted=0     | present  | no upload-ok     | still dirty: KEEP (no change)   |
//	| R2   | hydrated=1, dirty=0    | absent   | any              | demote: hydrated=0              |
//	| R3   | dirty=1                | any      | upload-ok exists | DEFERRED to leaf 04's ETag      |
//	|      |                        |          |                  | re-probe; journal alone NEVER   |
//	|      |                        |          |                  | overwrites index state (it is   |
//	|      |                        |          |                  | only counted in the report)     |
//
// Rows matching none of the rules are left untouched. Deleted=1 tombstones
// are never touched by reconcile. Reconcile corrections (R2) are recovery,
// not user operations: they write no journal rows.
//
// # Lock discipline
//
// One database file, two connection pools:
//
//   - write pool: MaxOpenConns(1), every transaction begins with
//     _txlock=immediate so the write lock is taken at BEGIN. This is the
//     SINGLE WRITER: all mutations (resources upserts/deletes, journal
//     appends, meta writes, reconcile demotions) go through it, serialized
//     by the pool itself - no ad-hoc mutex needed.
//   - read pool: MaxOpenConns(4), deferred transactions, never writes.
//
// WAL mode (persistent in the file) lets the read pool run concurrently
// with the writer without blocking it - the FUSE path (leaf 02) and the
// sync engine (leaf 04) share the Store without locking each other out.
// busy_timeout(5000) on every connection absorbs checkpoint contention.
// synchronous=FULL on every connection makes the journal's crash story
// real: a committed transaction is on disk.
package index
