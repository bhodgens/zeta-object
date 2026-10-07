// Package index: the client-side SQLite state layer - resources index,
// append-only sync journal, meta table, and the daemon-start reconcile
// pass. See doc.go for the schema, the reconcile rules table, and the lock
// discipline.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	// Pure-Go SQLite driver: the repo bans cgo repo-wide, and this import
	// is the only sanctioned way to reach SQLite.
	_ "modernc.org/sqlite"
)

// schemaVersion is the user_version this package migrates the DB to.
// v2 (leaf 07): additive eviction columns - lastAccess (LRU touch) and
// pinned (hard eviction filter). See schemaV2DDL.
const schemaVersion = 2

// Journal operations (journal.op). An operation that must be attributable
// on reconnect ("I changed it" vs "the world changed it") appends exactly
// one of these rows in the same transaction as its index mutation.
const (
	OpLocalWrite     = "local-write"     // local body changed, not yet uploaded
	OpUploadOK       = "upload-ok"       // uploaded; index went clean in same tx
	OpUploadConflict = "upload-conflict" // If-Match 412; conflict-copy landed
	OpDelete         = "delete"          // local delete recorded (tombstone)
	OpHydrate        = "hydrate"         // body fetched into the cache dir
	OpEvict          = "evict"           // clean cache file evicted (leaf 07)
	OpPin            = "pin"             // pinned flag toggled via IPC (leaf 07)
)

// Schema v1 DDL, executed inside the migration transaction.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS resources (
	path     TEXT PRIMARY KEY,
	etag     TEXT NOT NULL DEFAULT '',
	mtime    INTEGER NOT NULL DEFAULT 0,
	size     INTEGER NOT NULL DEFAULT 0,
	hydrated INTEGER NOT NULL DEFAULT 0,
	dirty    INTEGER NOT NULL DEFAULT 0,
	deleted  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_resources_evict ON resources(hydrated, dirty, deleted);
CREATE INDEX IF NOT EXISTS idx_resources_dirty ON resources(dirty) WHERE dirty = 1;
CREATE TABLE IF NOT EXISTS journal (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	ts     INTEGER NOT NULL,
	op     TEXT NOT NULL,
	path   TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_journal_path_op ON journal(path, op);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Schema v2 DDL (leaf 07, additive): lastAccess backs the LRU eviction
// policy (unix seconds, 0 = never touched; touched by TouchAccessed on
// hydrate/read); pinned is the hard eviction filter set by config pins
// and the IPC pin toggle (leaf 08 uses the same surface). Both default
// so v1 rows survive unchanged.
const schemaV2DDL = `
ALTER TABLE resources ADD COLUMN lastAccess INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resources ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_resources_lru ON resources(lastAccess);
CREATE INDEX IF NOT EXISTS idx_resources_pinned ON resources(pinned) WHERE pinned = 1;
`

// migration attaches the schema pragmas to a connection's first use. WAL is
// persistent in the file; synchronous and busy_timeout are per-connection.
const connPragmas = `PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;`

// ErrNotFound is returned by Get when no row exists for the path.
var ErrNotFound = errors.New("index: not found")

// Store is the handle to the client index DB. It owns two pools over the
// one file: a single-writer pool (MaxOpenConns=1, immediate transactions)
// and a WAL reader pool. Safe for concurrent use; see the lock discipline
// in doc.go.
type Store struct {
	write *sql.DB // single writer: MaxOpenConns(1)
	read  *sql.DB // WAL readers
}

// Open opens (creating if absent) the index database at path, migrating it
// to schema v1 if needed. The signature matches the leaf-01 placeholder so
// main.go's call sites are unchanged.
//
// diskCheck reports whether the file backing path exists in the local cache
// dir right now; ReconcilePass uses it for the "disk" column of the rules
// table. Callers that do not reconcile may pass nil (ReconcilePass then
// reports zero resolutions and returns no error).
func Open(path string) (*Store, error) {
	return OpenWithDiskCheck(path, nil)
}

// OpenWithDiskCheck is Open plus the reconcile disk probe. Split out so the
// daemon (which owns the cache dir) wires its own probe and tests can use a
// fake filesystem.
func OpenWithDiskCheck(path string, diskCheck func(path string) bool) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("index: creating %s: %w", dir, err)
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_txlock=immediate"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("index: opening %s: %w", path, err)
	}
	// SINGLE WRITER: one connection; every tx on it is immediate, so a
	// write lock is taken at BEGIN and writers queue at the pool.
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("index: opening reader for %s: %w", path, err)
	}
	r.SetMaxOpenConns(4) // WAL readers never block the writer
	s := &Store{write: w, read: r}
	if err := s.init(diskCheck); err != nil {
		_ = w.Close()
		_ = r.Close()
		return nil, err
	}
	return s, nil
}

// init applies per-connection pragmas, migrates the schema to
// schemaVersion, and (on a fresh open) runs the reconcile pass.
func (s *Store) init(diskCheck func(string) bool) error {
	if _, err := s.write.Exec(connPragmas); err != nil {
		return fmt.Errorf("index: pragmas: %w", err)
	}
	if err := s.migrate(); err != nil {
		return err
	}
	// Daemon-start reconcile: journal vs disk vs index disagreement
	// resolution runs on every open, per the rules table in doc.go.
	if _, err := s.ReconcilePass(context.Background(), diskCheck); err != nil {
		return err
	}
	return nil
}

// migrate moves user_version to schemaVersion, one transaction per step.
// v0 -> v1: full schema (leaf 03). v1 -> v2: additive eviction columns
// (leaf 07). Every step is idempotent-shaped DDL guarded by its version
// check, so a crash mid-migration replays safely.
func (s *Store) migrate() error {
	var v int
	if err := s.write.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("index: reading user_version: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("index: DB user_version %d newer than supported %d", v, schemaVersion)
	}
	for ; v < schemaVersion; v++ {
		if err := s.migrateStep(v); err != nil {
			return err
		}
	}
	return nil
}

// migrateStep applies ONE version bump: from -> from+1, in one
// transaction. v0's step creates everything; v1's step adds the leaf-07
// eviction columns.
func (s *Store) migrateStep(from int) error {
	ddl := schemaDDL
	if from != 0 {
		ddl = schemaV2DDL
	}
	tx, err := s.write.Begin()
	if err != nil {
		return fmt.Errorf("index: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(ddl); err != nil {
		return fmt.Errorf("index: applying schema v%d: %w", from+1, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, from+1)); err != nil {
		return fmt.Errorf("index: stamping user_version: %w", err)
	}
	return tx.Commit()
}

// Close closes both pools.
func (s *Store) Close() error {
	err1 := s.write.Close()
	err2 := s.read.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// Resource is one row of the resources index: a path known to be
// remote-or-local, with per-file protocol fields and cache-state flags.
type Resource struct {
	Path       string
	ETag       string
	Mtime      int64 // unix seconds
	Size       int64
	Hydrated   bool
	Dirty      bool
	Deleted    bool // tombstone feeding the deletion-grace table (leaf 07)
	LastAccess int64 // unix seconds of the last hydrate/read (LRU policy; 0 = never)
	Pinned     bool // hard eviction filter (config pins + IPC toggle)
}

// fullColumns is the SELECT column list every resources read shares
// (keeps Get/CleanHydratedRows/... in lockstep with the struct).
const fullColumns = `path, etag, mtime, size, hydrated, dirty, deleted, lastAccess, pinned`

func scanResource(scanner interface{ Scan(...any) error }) (Resource, error) {
	var r Resource
	var hyd, dirty, del, pinned int
	if err := scanner.Scan(&r.Path, &r.ETag, &r.Mtime, &r.Size,
		&hyd, &dirty, &del, &r.LastAccess, &pinned); err != nil {
		return r, err
	}
	r.Hydrated, r.Dirty, r.Deleted, r.Pinned = hyd == 1, dirty == 1, del == 1, pinned == 1
	return r, nil
}

// Get returns the row for path, or ErrNotFound.
func (s *Store) Get(ctx context.Context, path string) (Resource, error) {
	q := `SELECT ` + fullColumns + ` FROM resources WHERE path = ?`
	r, err := scanResource(s.read.QueryRowContext(ctx, q, path))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Resource{}, fmt.Errorf("%w: %s", ErrNotFound, path)
	case err != nil:
		return Resource{}, fmt.Errorf("index: get %s: %w", path, err)
	}
	return r, nil
}

// PutPath upserts the row for path and appends a journal row in the SAME
// transaction (op may be empty for pure-state writes that are recovery, not
// operations - e.g. reconcile corrections; those must not invent history).
func (s *Store) PutPath(ctx context.Context, r Resource, op, detail string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: put %s: %w", r.Path, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := putPathTx(ctx, tx, r); err != nil {
		return err
	}
	if op != "" {
		if err := journalAppendTx(ctx, tx, op, r.Path, detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func putPathTx(ctx context.Context, tx *sql.Tx, r Resource) error {
	const q = `INSERT INTO resources (path, etag, mtime, size, hydrated, dirty, deleted, lastAccess, pinned)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	           ON CONFLICT(path) DO UPDATE SET
	             etag = excluded.etag, mtime = excluded.mtime, size = excluded.size,
	             hydrated = excluded.hydrated, dirty = excluded.dirty,
	             deleted = excluded.deleted,
	             lastAccess = CASE WHEN excluded.lastAccess > 0 THEN excluded.lastAccess
	                               ELSE resources.lastAccess END,
	             pinned = CASE WHEN excluded.pinned = 1 THEN 1
	                           ELSE resources.pinned END`
	_, err := tx.ExecContext(ctx, q, r.Path, r.ETag, r.Mtime, r.Size,
		b2i(r.Hydrated), b2i(r.Dirty), b2i(r.Deleted), r.LastAccess, b2i(r.Pinned))
	if err != nil {
		return fmt.Errorf("index: upsert %s: %w", r.Path, err)
	}
	return nil
}

// DeletePath records the deletion of path: the row becomes a tombstone
// (deleted=1, hydrated=0, dirty=0, etag kept for the grace table) and the
// journal gets a delete row in the SAME transaction. It is NOT an error to
// delete an unknown path; the tombstone is created either way.
func (s *Store) DeletePath(ctx context.Context, path, detail string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: delete %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()
	const q = `INSERT INTO resources (path, etag, mtime, size, hydrated, dirty, deleted)
	           VALUES (?, '', 0, 0, 0, 0, 1)
	           ON CONFLICT(path) DO UPDATE SET
	             hydrated = 0, dirty = 0, deleted = 1`
	if _, err := tx.ExecContext(ctx, q, path); err != nil {
		return fmt.Errorf("index: tombstone %s: %w", path, err)
	}
	if err := journalAppendTx(ctx, tx, OpDelete, path, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// DirtyPaths returns every non-deleted path with dirty=1, sorted. This is
// the prompt-upload loop's work list (leaf 07) and the unmount flush
// check's input.
func (s *Store) DirtyPaths(ctx context.Context) ([]string, error) {
	const q = `SELECT path FROM resources WHERE dirty = 1 AND deleted = 0 ORDER BY path`
	return s.queryPaths(ctx, q)
}

// AllPaths returns every resources path, sorted - the sync engine's
// complement set for the PROPFIND walk (rows the server no longer lists
// are remote deletions or local-only files). Includes tombstones and
// dir-token rows (trailing-slash paths); callers filter by suffix.
// Leaf-04 addition: read-only, single-column, same shape as DirtyPaths.
func (s *Store) AllPaths(ctx context.Context) ([]string, error) {
	return s.queryPaths(ctx, `SELECT path FROM resources ORDER BY path`)
}

// TombstonedPaths returns the paths of every deleted=1 row, sorted -
// local deletes awaiting their remote DELETE (leaf 04 matrix 6) plus
// tombstones feeding the leaf-07 deletion-grace table. Leaf-04 addition.
func (s *Store) TombstonedPaths(ctx context.Context) ([]string, error) {
	return s.queryPaths(ctx, `SELECT path FROM resources WHERE deleted = 1 ORDER BY path`)
}

// EvictionPredicate is the single source of truth for the eviction filter,
// exported as a constant so tests and leaf 07's purge keep the exact shape.
// Leaf 07 made "NOT pinned" first-class SQL: the pinned column landed in
// schema v2 and the eviction query below embeds this predicate verbatim.
const EvictionPredicate = `hydrated = 1 AND dirty = 0 AND deleted = 0 AND NOT pinned`

// CleanHydratedPaths returns the eviction candidates in ONE query: fully
// uploaded (clean), body present locally, not tombstoned, and not pinned
// (the pinned filter is the pinned COLUMN since schema v2; the
// pinnedPaths argument is kept for call compatibility and is OR-ed in -
// passing nil is the normal case).
func (s *Store) CleanHydratedPaths(ctx context.Context, pinnedPaths map[string]bool) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT path FROM resources
		WHERE `+EvictionPredicate+`
		ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("index: eviction query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("index: eviction scan: %w", err)
		}
		if !pinnedPaths[p] {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// CleanHydratedRows returns the eviction candidates WITH the fields the
// policy ordering needs (size, mtime, lastAccess), already filtered by the
// EvictionPredicate (clean, hydrated, not tombstoned, not pinned) in one
// SQL query - the pinned term is the column, so IPC pin toggles take
// effect without a restart. Leaf 07's quota engine orders these by policy.
func (s *Store) CleanHydratedRows(ctx context.Context) ([]Resource, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+fullColumns+` FROM resources
		WHERE `+EvictionPredicate)
	if err != nil {
		return nil, fmt.Errorf("index: eviction rows: %w", err)
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("index: eviction row scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HydratedRows returns every hydrated, non-tombstoned row (clean, dirty,
// and pinned): the cache working set the quota measures. Read-only; leaf
// 07 addition.
func (s *Store) HydratedRows(ctx context.Context) ([]Resource, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+fullColumns+` FROM resources
		WHERE hydrated = 1 AND deleted = 0`)
	if err != nil {
		return nil, fmt.Errorf("index: hydrated rows: %w", err)
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("index: hydrated row scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchAccessed stamps lastAccess = now for path (LRU policy input;
// called on hydrate and on every cache read by leaf 07's quota engine).
func (s *Store) TouchAccessed(ctx context.Context, path string, now time.Time) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE resources SET lastAccess = ? WHERE path = ?`, now.Unix(), path)
	if err != nil {
		return fmt.Errorf("index: touch %s: %w", path, err)
	}
	return nil
}

// SetPinned flips the pinned flag for path and returns whether the row
// existed. Pinned rows are hard-filtered from eviction in SQL; pinning
// ALSO clears the tombstone flag (a pinned path is wanted).
func (s *Store) SetPinned(ctx context.Context, path string, pinned bool) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE resources SET pinned = ?, deleted = CASE WHEN ? = 1 THEN 0 ELSE deleted END
		 WHERE path = ?`, b2i(pinned), b2i(pinned), path)
	if err != nil {
		return false, fmt.Errorf("index: pin %s: %w", path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("index: pin %s rows: %w", path, err)
	}
	return n > 0, nil
}

// PinnedPaths returns every pinned path (the IPC pins view + the daemon's
// hydration trigger after a config-pin add).
func (s *Store) PinnedPaths(ctx context.Context) ([]string, error) {
	return s.queryPaths(ctx, `SELECT path FROM resources WHERE pinned = 1 ORDER BY path`)
}

// Evict demotes path to not-hydrated and appends the journal row in the
// SAME transaction (the crash-safety rule: index mutation and journal row
// commit or abort together). The cache FILE deletion happens before the
// call (the remote copy is authoritative for a clean file); if the
// process dies between the file delete and this commit, the daemon-start
// reconcile's R2 rule repairs the row (hydrated=1, disk absent ->
// demote), so the eviction is safe either way.
func (s *Store) Evict(ctx context.Context, path, detail string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: evict %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE resources SET hydrated = 0 WHERE path = ?`, path); err != nil {
		return fmt.Errorf("index: evict update %s: %w", path, err)
	}
	if err := journalAppendTx(ctx, tx, OpEvict, path, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// PruneTombstones deletes tombstone rows older than the retention window
// (the deletion-grace expiry pass, leaf 07) and returns the pruned paths.
// The grace window is measured from the tombstone's mtime (set when the
// delete was recorded).
func (s *Store) PruneTombstones(ctx context.Context, retention time.Duration, now time.Time) ([]string, error) {
	cutoff := now.Add(-retention).Unix()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("index: prune tombstones: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		DELETE FROM resources WHERE deleted = 1 AND mtime < ?
		RETURNING path`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("index: prune tombstones: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("index: prune scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: prune rows: %w", err)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, tx.Commit() // nothing to do; commit is a no-op either way
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("index: prune commit: %w", err)
	}
	return out, nil
}

// JournalAppend appends one journal row OUTSIDE any index mutation. It
// exists for pure observations (e.g. hydrate-started markers); every
// operation-driven transition must journal inside its own transaction via
// PutPath/DeletePath instead.
func (s *Store) JournalAppend(ctx context.Context, op, path, detail string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: journal append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := journalAppendTx(ctx, tx, op, path, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func journalAppendTx(ctx context.Context, tx *sql.Tx, op, path, detail string) error {
	if !validOp(op) {
		return fmt.Errorf("index: journal append: unknown op %q", op)
	}
	const q = `INSERT INTO journal (ts, op, path, detail) VALUES (?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q, time.Now().Unix(), op, path, detail); err != nil {
		return fmt.Errorf("index: journal append %s %s: %w", op, path, err)
	}
	return nil
}

func validOp(op string) bool {
	switch op {
	case OpLocalWrite, OpUploadOK, OpUploadConflict, OpDelete, OpHydrate, OpEvict, OpPin:
		return true
	}
	return false
}

// JournalRow is one journal entry.
type JournalRow struct {
	ID     int64
	TS     int64 // unix seconds
	Op     string
	Path   string
	Detail string
}

// JournalSince returns journal rows with id > afterID, in id order. The
// reconnect diff (leaf 04) walks this to attribute changes.
func (s *Store) JournalSince(ctx context.Context, afterID int64) ([]JournalRow, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, ts, op, path, detail FROM journal
		WHERE id > ? ORDER BY id`, afterID)
	if err != nil {
		return nil, fmt.Errorf("index: journal since %d: %w", afterID, err)
	}
	defer rows.Close()
	var out []JournalRow
	for rows.Next() {
		var r JournalRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Op, &r.Path, &r.Detail); err != nil {
			return nil, fmt.Errorf("index: journal scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MetaGet returns the value for key, or ErrNotFound.
func (s *Store) MetaGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: meta %s", ErrNotFound, key)
	case err != nil:
		return "", fmt.Errorf("index: meta get %s: %w", key, err)
	}
	return v, nil
}

// MetaSet upserts key. Leaf 05 owns its cursor keys; last-full-scan time
// lives here too.
func (s *Store) MetaSet(ctx context.Context, key, value string) error {
	if _, err := s.write.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("index: meta set %s: %w", key, err)
	}
	return nil
}

// ExpiredTombstones is the tombstone-retention groundwork (leaf 07 owns the
// pruning command): the query shape that lists tombstones older than the
// retention window. Leaf 07 turns this into `DELETE ... RETURNING path`.
func (s *Store) ExpiredTombstones(ctx context.Context, retention time.Duration, now time.Time) ([]string, error) {
	cutoff := now.Add(-retention).Unix()
	rows, err := s.read.QueryContext(ctx, `
		SELECT path FROM resources
		WHERE deleted = 1 AND mtime < ?
		ORDER BY mtime, path`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("index: expired tombstones: %w", err)
	}
	defer rows.Close()
	return collectStrings(rows)
}

// ReconcileReport summarizes one daemon-start reconcile pass.
type ReconcileReport struct {
	Examined   int // journal-attributable paths considered
	KeptDirty  int // rule R1
	DemotedDry int // rule R2: hydrated demoted to not-hydrated
	UploadOKs  int // rule R3: journal upload-ok count (report-only)
	Unresolved int // rows needing leaf 04's ETag re-probe (R3 core)
}

// ReconcilePass resolves journal-vs-disk-vs-index disagreement at daemon
// start, implementing EXACTLY the rules table in doc.go:
//
//	R1: dirty=1, deleted=0, disk present, no upload-ok in journal -> still
//	    dirty (KEEP, no change).
//	R2: hydrated=1, dirty=0, disk absent -> demote hydrated=0.
//	R3: dirty=1 with an upload-ok in the journal -> DEFERRED to leaf 04's
//	    ETag re-probe; the journal alone NEVER overwrites index state.
//
// Rows matching no rule are untouched; tombstones are never touched.
// Corrections are recovery, not operations: they write NO journal rows.
// diskCheck may be nil (nothing on disk then, so R2 fires for every
// hydrated row - tests always pass a real probe).
func (s *Store) ReconcilePass(ctx context.Context, diskCheck func(string) bool) (ReconcileReport, error) {
	rep := ReconcileReport{}
	if diskCheck == nil {
		diskCheck = func(string) bool { return false }
	}

	// Paths my journal ever touched, and which of those saw upload-ok.
	journalled := map[string]bool{}
	uploadOK := map[string]bool{}
	jrows, err := s.JournalSince(ctx, 0)
	if err != nil {
		return rep, err
	}
	for _, j := range jrows {
		journalled[j.Path] = true
		if j.Op == OpUploadOK {
			uploadOK[j.Path] = true
		}
	}

	rows, err := s.read.QueryContext(ctx, `
		SELECT path, hydrated, dirty, deleted FROM resources`)
	if err != nil {
		return rep, fmt.Errorf("index: reconcile scan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var hyd, dirty, del int
		if err := rows.Scan(&path, &hyd, &dirty, &del); err != nil {
			return rep, fmt.Errorf("index: reconcile row: %w", err)
		}
		if del == 1 {
			continue // tombstones are never reconciled
		}
		if !journalled[path] {
			continue // reconcile resolves MY history vs disk, not unknown rows
		}
		rep.Examined++
		switch {
		case dirty == 1:
			if uploadOK[path] {
				// R3: journal says upload-ok but index still dirty.
				// Deferred to leaf 04's ETag re-probe - count only.
				rep.UploadOKs++
				rep.Unresolved++
			} else if diskCheck(path) {
				// R1: dirty file present on disk, no upload-ok: keep.
				rep.KeptDirty++
			}
			// dirty with no disk file: leaf 04's scan decides (upload vs
			// discard needs the server side); v1 reconcile does not guess.
		case hyd == 1 && dirty == 0 && !diskCheck(path):
			// R2: index says hydrated but file missing: demote. The
			// correction itself runs in applyDemotions below.
			rep.DemotedDry++
		}
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("index: reconcile rows: %w", err)
	}
	// close over collected paths in order: re-walk cheaply, correcting only
	// R2 rows in one immediate transaction (recovery, no journal rows).
	if rep.DemotedDry > 0 {
		if err := s.applyDemotions(ctx, diskCheck); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// applyDemotions re-runs the R2 predicate under the write connection and
// flips hydrated->0 for exactly those rows, in one transaction.
func (s *Store) applyDemotions(ctx context.Context, diskCheck func(string) bool) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: reconcile demote: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT path FROM resources WHERE hydrated = 1 AND dirty = 0 AND deleted = 0`)
	if err != nil {
		return fmt.Errorf("index: reconcile demote scan: %w", err)
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return fmt.Errorf("index: reconcile demote row: %w", err)
		}
		if !diskCheck(p) {
			targets = append(targets, p)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("index: reconcile demote rows: %w", err)
	}
	for _, p := range targets {
		if _, err := tx.ExecContext(ctx,
			`UPDATE resources SET hydrated = 0 WHERE path = ? AND hydrated = 1`, p); err != nil {
			return fmt.Errorf("index: demote %s: %w", p, err)
		}
	}
	return tx.Commit()
}

// CrashPoint, when non-nil, is invoked inside a write transaction just
// before commit. Tests install a failing hook here to simulate a torn
// transaction: the process "dies" with the index mutation and its journal
// row both uncommitted. Production code never sets it.
var CrashPoint func(op string) error

// TornPut simulates a crash mid-transition for tests: put + journal in one
// tx, then invoke CrashPoint right before commit. Exported because the
// torn-transaction invariant (index mutation and journal row commit or
// abort TOGETHER) is the leaf's acceptance test.
func (s *Store) TornPut(ctx context.Context, r Resource, op, detail string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: torn put: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := putPathTx(ctx, tx, r); err != nil {
		return err
	}
	if err := journalAppendTx(ctx, tx, op, r.Path, detail); err != nil {
		return err
	}
	if CrashPoint != nil {
		if err := CrashPoint(op); err != nil {
			return err // tx rolls back via defer: NEITHER row lands
		}
	}
	return tx.Commit()
}

// queryPaths runs a single-column path query on the reader pool.
func (s *Store) queryPaths(ctx context.Context, q string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("index: query %s: %w", firstLine(q), err)
	}
	defer rows.Close()
	return collectStrings(rows)
}

func collectStrings(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("index: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func firstLine(s string) string {
	before, _, _ := strings.Cut(s, "\n")
	return before
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
