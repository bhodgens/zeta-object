// Command zmetad-fixture builds a canned zmetad SQLite export database
// (SCHEMA.md layout v5) for the e2e suite: e2e hosts have neither ZFS
// nor a zmetad daemon, so case 18-zmetad-events.sh primes a fixture DB
// with this builder and points zmetad_db_path at it before server start.
//
// Usage: zmetad-fixture <db-path> <mountpoint>
//
// <mountpoint> is the symlink-resolved bucket directory the datasets
// table maps onto the canned dataset "testpool/e2e" (the provider
// resolves bucket paths through that table only).
//
// Canned set (traceable, not invented):
//
//   - events rows 1-9 are the ROW-FORM of the recorded testdata fixture
//     internal/metadata/testdata/zfs-events-nested.txt exactly as the
//     drift-guard converter (fixtureRows, zmetad_paths_test.go) shapes
//     it: full_path = the resolved paths the legacy pipeline derives,
//     captured_at NULL, timestamp 0 (the recorded fixture carries no
//     time field), sizes on the TRUNCATE row only.
//   - event row 10: synthetic SETATTR with a real captured_at (the
//     wall-clock timestamp path on the wire).
//   - event row 11: synthetic nested CREATE with NULL full_path and
//     parent 999 (ABSENT from objmap) - the PARTIAL/conservative-match
//     case - and NULL captured_at (legacy style; zero-time on the wire).
//   - gaps: one row lost=7 (known loss) and one row lost=-1 (ring swap)
//     -> GapStats{KnownLost:7, Regressions:0, RingSwaps:1}; wire
//     recordsLost=7, ringSwaps=1, IsLossy=true (Contract 5).
//   - datasets: ("testpool/e2e", <mountpoint>).
//   - sync_state: one row with ring_guid (the dataset counts as polled).
//   - meta: db_schema_version=5, events_schema_version=2.
//   - objmap: the fixture's object ids; 999 deliberately absent.
//
// The DDL below is copied column-for-column from the pinned v5 schema in
// internal/metadata/zmetad_db_test.go (which is pinned against upstream
// contrib/zmetad/zmetad_db.c schema_sql).
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // pure-Go sqlite driver; no cgo (repo rule)
)

// dataset is the canned dataset name every table row keys on.
const dataset = "testpool/e2e"

// capturedAt is a fixed unix-seconds wall clock for the synthetic
// captured_at row (2025-10-01T00:00:00Z) - deterministic e2e output.
const capturedAt = int64(1759276800)

// zmetadV5DDL is the exact SCHEMA.md v5 layout, copied from
// internal/metadata/zmetad_db_test.go (pinned against upstream).
const zmetadV5DDL = `
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    dataset TEXT NOT NULL,
    txg INTEGER NOT NULL,
    timestamp INTEGER NOT NULL,
    captured_at INTEGER,
    object_id INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    path TEXT,
    old_path TEXT,
    uid INTEGER,
    gid INTEGER,
    mode INTEGER,
    size INTEGER,
    io_offset INTEGER,
    io_bytes INTEGER,
    parent INTEGER,
    old_parent INTEGER,
    target TEXT,
    old_size INTEGER,
    attrs INTEGER,
    full_path TEXT,
    old_full_path TEXT,
    UNIQUE(dataset, txg, object_id, event_type, timestamp)
);
CREATE INDEX IF NOT EXISTS idx_events_dataset_time ON events(dataset, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_object ON events(dataset, object_id);
CREATE INDEX IF NOT EXISTS idx_events_path ON events(dataset, path);
CREATE INDEX IF NOT EXISTS idx_events_full_path ON events(dataset, full_path);
CREATE TABLE IF NOT EXISTS objmap (
    dataset TEXT NOT NULL,
    object_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    parent INTEGER,
    PRIMARY KEY (dataset, object_id)
);
CREATE TABLE IF NOT EXISTS sync_state (
    dataset TEXT PRIMARY KEY,
    last_offset INTEGER NOT NULL,
    last_sync INTEGER NOT NULL,
    ring_guid INTEGER
);
CREATE TABLE IF NOT EXISTS datasets (
    dataset TEXT PRIMARY KEY,
    mountpoint TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS gaps (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    dataset TEXT NOT NULL,
    detected INTEGER NOT NULL,
    from_offset INTEGER,
    to_offset INTEGER,
    lost INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

// eventCols is the insert column list (mode is never written upstream -
// always NULL - and stays out of the canned rows too).
const eventCols = `(dataset, txg, timestamp, captured_at, object_id, event_type,
	path, old_path, uid, gid, size, io_offset, io_bytes,
	parent, old_parent, target, old_size, attrs, full_path, old_full_path)`

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <db-path> <mountpoint>\n", os.Args[0])
		os.Exit(2)
	}
	if err := build(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "zmetad-fixture: %v\n", err)
		os.Exit(1)
	}
}

// build creates the v5 fixture database at dbPath with the canned set
// documented in the package header. dbPath is removed first so repeated
// runs are idempotent.
func build(dbPath, mountpoint string) error {
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) { // #nosec G703 // dbPath is the operator-supplied argv output path of a test-fixture builder; it deletes exactly that file
		return fmt.Errorf("remove stale db: %w", err)
	}
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(zmetadV5DDL); err != nil {
		return fmt.Errorf("create v5 schema: %w", err)
	}
	if err := insertMeta(conn); err != nil {
		return err
	}
	if err := insertEvents(conn); err != nil {
		return err
	}
	return insertState(conn, mountpoint)
}

func insertMeta(conn *sql.DB) error {
	for _, kv := range [][2]string{
		{"db_schema_version", "5"},
		{"events_schema_version", "2"},
	} {
		if _, err := conn.Exec("INSERT INTO meta (key, value) VALUES (?, ?)", kv[0], kv[1]); err != nil {
			return fmt.Errorf("insert meta %s: %w", kv[0], err)
		}
	}
	return nil
}

func insertEvents(conn *sql.DB) error {
	// Rows 1-9: the row-form of testdata/zfs-events-nested.txt via the
	// drift-guard converter (fixtureRows): captured_at NULL, timestamp 0,
	// sizes on TRUNCATE only, full_path = the legacy-resolved paths.
	// Row 10: synthetic SETATTR with captured_at (wall-clock wire path).
	// Row 11: synthetic nested CREATE, NULL full_path (PARTIAL, ancestor
	// 999 absent from objmap - the conservative-match case), NULL
	// captured_at (legacy style).
	rows := []struct {
		txg, objectID               int64
		op, path, oldPath, fullPath any
		oldFullPath, target         any
		size, oldSize               any
		parent, oldParent           any
		ts                          int64
		cap                         any
	}{
		{txg: 31998, objectID: 50, op: "CREATE", path: "top.txt", fullPath: "top.txt"},
		{txg: 32000, objectID: 129, op: "CREATE", path: "edge", fullPath: "edge"},
		{txg: 32001, objectID: 130, op: "CREATE", path: "inner", fullPath: "edge/inner"},
		{txg: 32019, objectID: 257, op: "CREATE", path: "deep.txt", fullPath: "edge/inner/deep.txt"},
		{txg: 32020, objectID: 258, op: "CREATE", path: "shallow.txt", fullPath: "edge/inner/shallow.txt"},
		{txg: 32022, objectID: 257, op: "TRUNCATE", path: "deep.txt", fullPath: "edge/inner/deep.txt",
			size: int64(4096), oldSize: int64(1024)},
		{txg: 32030, objectID: 257, op: "RENAME", path: "renamed.txt", oldPath: "deep.txt",
			fullPath: "edge/inner/renamed.txt", oldFullPath: "edge/inner/deep.txt"},
		{txg: 32040, objectID: 131, op: "LINK", path: "hard.txt", fullPath: "edge/inner/hard.txt"},
		{txg: 32050, objectID: 258, op: "REMOVE", path: "shallow.txt", fullPath: "edge/inner/shallow.txt"},
		{txg: 32055, objectID: 257, op: "SETATTR", path: "renamed.txt", fullPath: "edge/inner/renamed.txt",
			cap: capturedAt, size: int64(2048)},
		{txg: 32060, objectID: 900, op: "CREATE", path: "orphan.txt", parent: int64(999)},
	}
	stmt := "INSERT INTO events " + eventCols + ` VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, NULL, NULL, ?, ?, ?, ?, NULL, ?, ?)`
	for _, r := range rows {
		args := []any{dataset, r.txg, r.ts, r.cap, r.objectID, r.op,
			r.path, r.oldPath, r.size, r.parent, r.oldParent, r.target,
			r.oldSize, r.fullPath, r.oldFullPath}
		if _, err := conn.Exec(stmt, args...); err != nil {
			return fmt.Errorf("insert event txg=%d %v: %w", r.txg, r.op, err)
		}
	}
	return nil
}

// insertState writes the datasets / sync_state / gaps / objmap rows.
func insertState(conn *sql.DB, mountpoint string) error {
	if _, err := conn.Exec("INSERT INTO datasets (dataset, mountpoint) VALUES (?, ?)",
		dataset, mountpoint); err != nil {
		return fmt.Errorf("insert datasets: %w", err)
	}
	if _, err := conn.Exec("INSERT INTO sync_state (dataset, last_offset, last_sync, ring_guid) VALUES (?, ?, ?, ?)",
		dataset, int64(1000), capturedAt, int64(987654321)); err != nil {
		return fmt.Errorf("insert sync_state: %w", err)
	}
	// Known loss (7 records) and one ring swap (-1 sentinel). NEVER
	// folded: GapStats must report KnownLost=7, Regressions=0,
	// RingSwaps=1 (SCHEMA.md section 4, Contract 5).
	for _, g := range []struct {
		detected int64
		from, to any
		lost     int64
	}{
		{detected: capturedAt - 800, from: int64(100), to: int64(107), lost: 7},
		{detected: capturedAt - 300, lost: -1},
	} {
		if _, err := conn.Exec("INSERT INTO gaps (dataset, detected, from_offset, to_offset, lost) VALUES (?, ?, ?, ?, ?)",
			dataset, g.detected, g.from, g.to, g.lost); err != nil {
			return fmt.Errorf("insert gap lost=%d: %w", g.lost, err)
		}
	}
	// objmap: the fixture's object ids (999 - the orphan row's parent -
	// is deliberately ABSENT: the conservative-match case).
	for _, m := range []struct {
		objectID int64
		name     string
		parent   any
	}{
		{50, "top.txt", nil},
		{129, "edge", nil},
		{130, "inner", int64(129)},
		{257, "renamed.txt", int64(130)},
		{258, "shallow.txt", int64(130)},
		{131, "hard.txt", int64(130)},
	} {
		if _, err := conn.Exec("INSERT INTO objmap (dataset, object_id, name, parent) VALUES (?, ?, ?, ?)",
			dataset, m.objectID, m.name, m.parent); err != nil {
			return fmt.Errorf("insert objmap %d: %w", m.objectID, err)
		}
	}
	return nil
}
