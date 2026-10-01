package metadata

// Read-only accessor for the zmetad SQLite export database (layout
// version 5 per contrib/zmetad/SCHEMA.md in zfs-metadata). This is the
// only file in the tree that touches SQL or the sqlite driver; callers
// consume EventRow, GapStats, and the accessor methods. There is
// deliberately NO purge method: purge is `zmetad --purge` (the only
// mechanism that clears both the DB rows and the kernel ring).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go sqlite driver; no cgo, static binary preserved
)

// zmetadMaxDBSchemaVersion is the maximum db_schema_version this
// consumer understands (SCHEMA.md section 1 refuse-newer rule). The
// read path also requires exactly this version: the v5 full_path
// column is the read-path contract.
const zmetadMaxDBSchemaVersion = 5

// zmetadEventsSchemaVersion is the required events wire schema version
// (SCHEMA.md section 1).
const zmetadEventsSchemaVersion = "2"

// ZmetadDBMissingError means the database file does not exist. Callers
// treat every open failure as "provider unavailable", never a hard error.
type ZmetadDBMissingError struct{ Path string }

func (e *ZmetadDBMissingError) Error() string {
	return "metadata: zmetad database not found at " + e.Path
}

// ZmetadDBVersionError means the database's stored schema version is not
// acceptable: either newer than this consumer knows (Newer true -
// refuse-newer rule, never open with guessed semantics) or older than the
// required v5 layout (Newer false - zmetad migrates the database in place
// on upgrade, so the hint is to upgrade zmetad).
type ZmetadDBVersionError struct {
	Path    string
	Version string // stored value, "" when absent
	Newer   bool
	Which   string // "db_schema_version" (default) or "events_schema_version"
}

func (e *ZmetadDBVersionError) Error() string {
	which := e.Which
	if which == "" {
		which = "db_schema_version"
	}
	if which == "events_schema_version" {
		return fmt.Sprintf("metadata: zmetad database %s has events_schema_version %s; version %s required",
			e.Path, e.Version, zmetadEventsSchemaVersion)
	}
	if e.Version == "" {
		return fmt.Sprintf("metadata: zmetad database %s has no db_schema_version key (pre-v1 layout); version %d required - upgrade zmetad, which migrates the database in place",
			e.Path, zmetadMaxDBSchemaVersion)
	}
	if e.Newer {
		return fmt.Sprintf("metadata: zmetad database %s has db_schema_version %s, newer than maximum known version %d; refusing to open with guessed semantics",
			e.Path, e.Version, zmetadMaxDBSchemaVersion)
	}
	return fmt.Sprintf("metadata: zmetad database %s has db_schema_version %s; version %d required - upgrade zmetad, which migrates the database in place",
		e.Path, e.Version, zmetadMaxDBSchemaVersion)
}

// DatasetNotTrackedError means no "/"-rooted mountpoint in the datasets
// table is the path or an ancestor of it. This is an availability status
// the caller turns into a ProbeResult Reason, not a failure.
type DatasetNotTrackedError struct{ Path string }

func (e *DatasetNotTrackedError) Error() string {
	return "metadata: no zmetad-tracked dataset mountpoint contains path " + e.Path
}

// EventRow mirrors one zmetad `events` table row (SCHEMA.md section 2.1,
// layout version 5). Pointer fields distinguish "absent" from "present
// and zero" - the DB leaves absent fields NULL and consumers MUST NOT
// fabricate values. Mode is never written upstream (always NULL) and is
// intentionally omitted.
type EventRow struct {
	ID          int64
	Dataset     string
	Txg         uint64
	Timestamp   uint64 // kernel hrtime ns, MONOTONIC - not wall clock
	CapturedAt  *int64 // unix seconds at ingest; NULL in pre-v4 rows
	ObjectID    uint64
	Op          string // schema enum NAME: CREATE, REMOVE, RENAME, LINK, SYMLINK, TRUNCATE, SETATTR, WRITE, READ (or UNKNOWN)
	Path        *string
	OldPath     *string // RENAME only
	FullPath    *string // RESOLVED dataset-relative path, authoritative as of this row's event (v5); NULL = PARTIAL
	OldFullPath *string // RENAME: resolved path of old_path at insert time (v5)
	UID, GID    *uint64
	Size        *int64 // new size (TRUNCATE/SETATTR) or IO total
	IoOffset    *uint64
	IoBytes     *uint64
	Parent      *uint64 // parent dir object id (ground truth; not needed for reads on v5)
	OldParent   *uint64
	Target      *string // SYMLINK only
	OldSize     *int64
	Attrs       *uint64
}

// GapStats reports the dataset's loss classes per SCHEMA.md section 4.
// The classes are never folded: swaps carry the -1 sentinel and must
// never enter a record count.
type GapStats struct {
	KnownLost   uint64 // SUM(lost) over rows with lost > 0
	Regressions uint64 // COUNT(lost = 0): watermark regressions, count unknown
	RingSwaps   uint64 // COUNT(lost = -1): kernel-log identity swaps
}

// ZmetadDB is a read-only handle on the zmetad database. Safe for
// concurrent use (database/sql pool semantics).
type ZmetadDB struct {
	conn *sql.DB
	path string
}

// OpenZmetadDB opens the zmetad database READ-ONLY (modernc.org/sqlite,
// mode=ro, busy_timeout 2s; WAL lets readers run against the live
// daemon). Requires meta.db_schema_version == 5: a NEWER value is
// *ZmetadDBVersionError{Newer:true}; 1-4 is Newer:false with an upgrade
// hint in the message (zmetad migrates in place); a missing key is
// treated as pre-v1. A missing file is *ZmetadDBMissingError. Callers
// treat every open failure as "provider unavailable", never a hard
// error. Also verifies meta.events_schema_version == 2 (refuse condition
// per SCHEMA.md).
func OpenZmetadDB(ctx context.Context, path string) (*ZmetadDB, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ZmetadDBMissingError{Path: path}
		}
		return nil, fmt.Errorf("metadata: stat zmetad database %s: %w", path, err)
	}
	// Read-only DSN: never create, never write - including no pragma
	// writes (journal_mode stays whatever the daemon set; WAL readers
	// are safe against the live daemon).
	dsn := "file:" + path + "?mode=ro&busy_timeout=2000"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("metadata: open zmetad database %s: %w", path, err)
	}
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("metadata: open zmetad database %s: %w", path, err)
	}
	db := &ZmetadDB{conn: conn, path: path}
	if err := db.checkVersion(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return db, nil
}

// checkVersion applies the dual version gate (SCHEMA.md section 1).
func (db *ZmetadDB) checkVersion(ctx context.Context) error {
	dbVersion, err := db.metaValue(ctx, "db_schema_version")
	if err != nil {
		return err
	}
	if dbVersion == "" {
		// Missing key: treat as pre-v1, refuse with an upgrade hint.
		return &ZmetadDBVersionError{Path: db.path, Version: "", Newer: false, Which: "db_schema_version"}
	}
	n, parseErr := strconv.Atoi(dbVersion)
	if parseErr != nil {
		return &ZmetadDBVersionError{Path: db.path, Version: dbVersion, Newer: false, Which: "db_schema_version"}
	}
	if n > zmetadMaxDBSchemaVersion {
		return &ZmetadDBVersionError{Path: db.path, Version: dbVersion, Newer: true, Which: "db_schema_version"}
	}
	if n < zmetadMaxDBSchemaVersion {
		return &ZmetadDBVersionError{Path: db.path, Version: dbVersion, Newer: false, Which: "db_schema_version"}
	}
	eventsVersion, err := db.metaValue(ctx, "events_schema_version")
	if err != nil {
		return err
	}
	if eventsVersion != "" && eventsVersion != zmetadEventsSchemaVersion {
		return &ZmetadDBVersionError{Path: db.path, Version: eventsVersion, Newer: false, Which: "events_schema_version"}
	}
	return nil
}

// metaValue returns the stored value of a meta key, or "" when absent.
func (db *ZmetadDB) metaValue(ctx context.Context, key string) (string, error) {
	var value string
	err := db.conn.QueryRowContext(ctx,
		"SELECT value FROM meta WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("metadata: read meta key %s from %s: %w", key, db.path, err)
	}
	return value, nil
}

// Events returns the dataset's rows ordered (txg ASC, id ASC) - the
// SCHEMA.md section 7 tie-break. max <= 0 means no LIMIT. An unknown
// dataset yields an empty slice and a nil error. NULL columns decode as
// nil pointers; no value is ever fabricated.
func (db *ZmetadDB) Events(dataset string, max int) ([]EventRow, error) {
	query := `SELECT id, dataset, txg, timestamp, captured_at, object_id, event_type,
			path, old_path, full_path, old_full_path, uid, gid, size, io_offset, io_bytes,
			parent, old_parent, target, old_size, attrs
		FROM events WHERE dataset = ? ORDER BY txg ASC, id ASC`
	args := []any{dataset}
	if max > 0 {
		query += " LIMIT ?"
		args = append(args, max)
	}
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("metadata: query events for dataset %s: %w", dataset, err)
	}
	defer rows.Close()

	events := []EventRow{}
	for rows.Next() {
		var (
			row                                           EventRow
			txg, timestamp, objectID                      sql.NullInt64
			capturedAt, uid, gid, size, ioOffset, ioBytes sql.NullInt64
			parent, oldParent, oldSize, attrs             sql.NullInt64
			path, oldPath, fullPath, oldFullPath, target  sql.NullString
		)
		if err := rows.Scan(&row.ID, &row.Dataset, &txg, &timestamp, &capturedAt,
			&objectID, &row.Op, &path, &oldPath, &fullPath, &oldFullPath,
			&uid, &gid, &size, &ioOffset, &ioBytes, &parent, &oldParent,
			&target, &oldSize, &attrs); err != nil {
			return nil, fmt.Errorf("metadata: scan event row for dataset %s: %w", dataset, err)
		}
		row.Txg = toUint64(txg.Int64)
		row.Timestamp = toUint64(timestamp.Int64)
		row.ObjectID = toUint64(objectID.Int64)
		row.CapturedAt = nullInt64Ptr(capturedAt)
		row.Path = nullStringPtr(path)
		row.OldPath = nullStringPtr(oldPath)
		row.FullPath = nullStringPtr(fullPath)
		row.OldFullPath = nullStringPtr(oldFullPath)
		row.UID = nullUint64Ptr(uid)
		row.GID = nullUint64Ptr(gid)
		row.Size = nullInt64Ptr(size)
		row.IoOffset = nullUint64Ptr(ioOffset)
		row.IoBytes = nullUint64Ptr(ioBytes)
		row.Parent = nullUint64Ptr(parent)
		row.OldParent = nullUint64Ptr(oldParent)
		row.Target = nullStringPtr(target)
		row.OldSize = nullInt64Ptr(oldSize)
		row.Attrs = nullUint64Ptr(attrs)
		events = append(events, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metadata: iterate event rows for dataset %s: %w", dataset, err)
	}
	return events, nil
}

// GapStats returns the dataset's loss classes per SCHEMA.md section 4.
// Never folded: KnownLost sums rows with lost > 0 only - the -1
// ring-swap sentinel and the 0 regression sentinel NEVER enter it. A
// dataset with no gap rows yields all zeros and a nil error.
func (db *ZmetadDB) GapStats(dataset string) (GapStats, error) {
	var stats GapStats
	var knownLost, regressions, ringSwaps sql.NullInt64
	err := db.conn.QueryRow(`SELECT
			SUM(CASE WHEN lost > 0 THEN lost ELSE 0 END),
			SUM(CASE WHEN lost = 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN lost = -1 THEN 1 ELSE 0 END)
		FROM gaps WHERE dataset = ?`, dataset).
		Scan(&knownLost, &regressions, &ringSwaps)
	if err != nil {
		return stats, fmt.Errorf("metadata: query gap stats for dataset %s: %w", dataset, err)
	}
	stats.KnownLost = toUint64(knownLost.Int64)
	stats.Regressions = toUint64(regressions.Int64)
	stats.RingSwaps = toUint64(ringSwaps.Int64)
	return stats, nil
}

// ResolveDatasetByPath maps a symlink-resolved bucket path to its
// dataset via the `datasets` table ONLY (SCHEMA.md section 5): longest
// "/"-rooted mountpoint that is the path or an ancestor of it.
// Non-absolute mountpoints (verbatim "none"/"legacy") never match. No
// match: (*DatasetNotTrackedError), nil - an availability status, not a
// failure.
func (db *ZmetadDB) ResolveDatasetByPath(path string) (string, error) {
	rows, err := db.conn.Query("SELECT mountpoint, dataset FROM datasets")
	if err != nil {
		return "", fmt.Errorf("metadata: query datasets table: %w", err)
	}
	defer rows.Close()

	best := ""
	bestDataset := ""
	for rows.Next() {
		var mountpoint, dataset string
		if err := rows.Scan(&mountpoint, &dataset); err != nil {
			return "", fmt.Errorf("metadata: scan datasets row: %w", err)
		}
		if !strings.HasPrefix(mountpoint, "/") {
			continue // verbatim "none"/"legacy" entries never match
		}
		if !mountpointContains(mountpoint, path) {
			continue // separator guard: /testpool never claims /testpoolx
		}
		if len(mountpoint) > len(best) {
			best = mountpoint
			bestDataset = dataset
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("metadata: iterate datasets rows: %w", err)
	}
	if best == "" {
		return "", &DatasetNotTrackedError{Path: path}
	}
	return bestDataset, nil
}

// HasDataset reports whether sync_state has a row for the dataset.
func (db *ZmetadDB) HasDataset(dataset string) (bool, error) {
	var one int
	err := db.conn.QueryRow("SELECT 1 FROM sync_state WHERE dataset = ?", dataset).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metadata: query sync_state for dataset %s: %w", dataset, err)
	}
	return true, nil
}

// Close releases the read-only database handle.
func (db *ZmetadDB) Close() error {
	if err := db.conn.Close(); err != nil {
		return fmt.Errorf("metadata: close zmetad database %s: %w", db.path, err)
	}
	return nil
}

// toUint64 converts a non-negative SQLite INTEGER to uint64. Schema
// columns reaching here (txg, timestamp, object_id, uid, gid, io_*,
// parent, attrs, gap sums/counts) are never negative; a corrupt
// negative value clamps to 0 rather than wrapping.
func toUint64(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func nullUint64Ptr(n sql.NullInt64) *uint64 {
	if !n.Valid {
		return nil
	}
	v := toUint64(n.Int64)
	return &v
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// mountpointContains reports whether the bucket path is the mountpoint or
// lies beneath it. Containment is filepath-compatible: mountpoint == path,
// or strings.HasPrefix(path, mountpoint+"/") - the separator guard stops
// /mnt/tank from claiming /mnt/tankdata. Moved here from zfs_cmd.go with
// the CLI transport deletion (zmetad-provider-2026-09 leaf 04): the
// datasets-table longest-prefix resolution in ResolveDatasetByPath is its
// only production consumer.
func mountpointContains(mountpoint, path string) bool {
	return mountpoint == path || strings.HasPrefix(path, mountpoint+"/")
}
