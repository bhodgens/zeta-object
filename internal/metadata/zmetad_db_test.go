package metadata

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// zmetadV5DDL is the exact SCHEMA.md v5 layout (pinned against
// contrib/zmetad/zmetad_db.c schema_sql at 8a839cd2e): events with
// captured_at + full_path + old_full_path, sync_state with ring_guid,
// datasets, objmap, gaps, meta.
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

// writeTestDB builds a minimal SQLite file with the exact SCHEMA.md v5
// schema using the driver itself (rw open). Empty version strings omit
// the corresponding meta key.
func writeTestDB(t *testing.T, dbSchemaVersion string, eventsSchemaVersion string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zmetad.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(zmetadV5DDL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if dbSchemaVersion != "" {
		if _, err := conn.Exec("INSERT INTO meta (key, value) VALUES ('db_schema_version', ?)",
			dbSchemaVersion); err != nil {
			t.Fatalf("insert db_schema_version: %v", err)
		}
	}
	if eventsSchemaVersion != "" {
		if _, err := conn.Exec("INSERT INTO meta (key, value) VALUES ('events_schema_version', ?)",
			eventsSchemaVersion); err != nil {
			t.Fatalf("insert events_schema_version: %v", err)
		}
	}
	return path
}

// writeFullTestDB builds a v5 DB and returns an open rw handle for
// fixture inserts; the caller closes it.
func writeFullTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := writeTestDB(t, "5", "2")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen test db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return path, conn
}

func TestOpenZmetadDB_VersionGate(t *testing.T) {
	tests := []struct {
		name           string
		dbVersion      string
		eventsVersion  string
		wantOK         bool
		wantNewer      bool
		wantWhich      string // "" means don't check
		wantHintSubstr string
	}{
		{name: "v5 accepted", dbVersion: "5", eventsVersion: "2", wantOK: true},
		{name: "v5 without events key accepted", dbVersion: "5", eventsVersion: "", wantOK: true},
		{
			name: "v6 refused newer", dbVersion: "6", eventsVersion: "2",
			wantNewer: true, wantWhich: "db_schema_version",
		},
		{
			name: "v4 refused with upgrade hint", dbVersion: "4", eventsVersion: "2",
			wantWhich: "db_schema_version", wantHintSubstr: "upgrade zmetad",
		},
		{
			name: "missing db key refused as pre-v1", dbVersion: "", eventsVersion: "2",
			wantWhich: "db_schema_version", wantHintSubstr: "upgrade zmetad",
		},
		{
			name: "events_schema_version 3 refused", dbVersion: "5", eventsVersion: "3",
			wantWhich: "events_schema_version",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestDB(t, tc.dbVersion, tc.eventsVersion)
			db, err := OpenZmetadDB(context.Background(), path)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("OpenZmetadDB: unexpected error: %v", err)
				}
				if err := db.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				return
			}
			if err == nil {
				db.Close()
				t.Fatal("OpenZmetadDB: expected error, got nil")
			}
			var vErr *ZmetadDBVersionError
			if !errors.As(err, &vErr) {
				t.Fatalf("expected *ZmetadDBVersionError, got %T: %v", err, err)
			}
			if vErr.Newer != tc.wantNewer {
				t.Errorf("Newer = %v, want %v", vErr.Newer, tc.wantNewer)
			}
			if vErr.Which != tc.wantWhich {
				t.Errorf("Which = %q, want %q", vErr.Which, tc.wantWhich)
			}
			if tc.wantHintSubstr != "" && !strings.Contains(err.Error(), tc.wantHintSubstr) {
				t.Errorf("error %q missing hint %q", err.Error(), tc.wantHintSubstr)
			}
			if !strings.HasPrefix(err.Error(), "metadata: ") {
				t.Errorf("error %q does not start with 'metadata: '", err.Error())
			}
		})
	}
}

func TestOpenZmetadDB_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	_, err := OpenZmetadDB(context.Background(), path)
	var mErr *ZmetadDBMissingError
	if !errors.As(err, &mErr) {
		t.Fatalf("expected *ZmetadDBMissingError, got %T: %v", err, err)
	}
	if mErr.Path != path {
		t.Errorf("Path = %q, want %q", mErr.Path, path)
	}
	if !strings.HasPrefix(err.Error(), "metadata: ") {
		t.Errorf("error %q does not start with 'metadata: '", err.Error())
	}
}

// insertEvent is the fixture writer for events rows; nil arguments bind
// NULL, mirroring the daemon's own binds.
func insertEvent(t *testing.T, conn *sql.DB, dataset string, txg int64, id int64,
	capturedAt any, objectType, path, oldPath, fullPath, oldFullPath string) {
	t.Helper()
	_, err := conn.Exec(`INSERT INTO events
		(dataset, txg, timestamp, captured_at, object_id, event_type,
		 path, old_path, full_path, old_full_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dataset, txg, id, capturedAt, id, objectType,
		nullStr(path), nullStr(oldPath), nullStr(fullPath), nullStr(oldFullPath))
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestZmetadDB_Events(t *testing.T) {
	path, conn := writeFullTestDB(t)
	// Out-of-order inserts; ordering must come back (txg ASC, id ASC).
	insertEvent(t, conn, "pool/data", 20, 2, int64(1700000000), "WRITE", "b.txt", "", "docs/b.txt", "")
	insertEvent(t, conn, "pool/data", 10, 1, int64(1699999999), "CREATE", "a.txt", "", "docs/a.txt", "")
	insertEvent(t, conn, "pool/data", 10, 3, int64(1700000001), "REMOVE", "a.txt", "", "docs/a.txt", "")
	insertEvent(t, conn, "other/data", 5, 9, int64(1700000002), "CREATE", "x", "", "x", "")

	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	rows, err := db.Events("pool/data", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	// (txg ASC, id ASC): AUTOINCREMENT ids follow insertion order, so the
	// WRITE row (txg 20, inserted first) has id 1 and the two txg 10 rows
	// have ids 2 and 3. Correct order: [2 3 1] - txg dominates even
	// though the txg-20 row has the smallest id.
	if got := []int64{rows[0].ID, rows[1].ID, rows[2].ID}; got[0] != 2 || got[1] != 3 || got[2] != 1 {
		t.Errorf("order by id = %v, want [2 3 1]", got)
	}
	if rows[0].Txg != 10 || rows[1].Txg != 10 || rows[2].Txg != 20 {
		t.Errorf("txg order wrong: %d, %d, %d", rows[0].Txg, rows[1].Txg, rows[2].Txg)
	}
	if rows[0].Op != "CREATE" || rows[0].Dataset != "pool/data" {
		t.Errorf("row0 = %+v, want CREATE on pool/data", rows[0])
	}
	// Fully populated row decodes non-nil pointers.
	if rows[0].Path == nil || *rows[0].Path != "a.txt" {
		t.Errorf("Path = %v, want a.txt", rows[0].Path)
	}
	if rows[0].FullPath == nil || *rows[0].FullPath != "docs/a.txt" {
		t.Errorf("FullPath = %v, want docs/a.txt", rows[0].FullPath)
	}
	if rows[0].CapturedAt == nil || *rows[0].CapturedAt != 1699999999 {
		t.Errorf("CapturedAt = %v, want 1699999999", rows[0].CapturedAt)
	}
	// NULL optional columns decode as nil pointers, never fabricated.
	if rows[0].OldPath != nil || rows[0].OldFullPath != nil {
		t.Errorf("OldPath/OldFullPath should be nil for CREATE: %v %v", rows[0].OldPath, rows[0].OldFullPath)
	}
	if rows[0].UID != nil || rows[0].GID != nil || rows[0].Size != nil ||
		rows[0].IoOffset != nil || rows[0].IoBytes != nil || rows[0].Parent != nil ||
		rows[0].OldParent != nil || rows[0].Target != nil || rows[0].OldSize != nil ||
		rows[0].Attrs != nil {
		t.Errorf("unbound optional columns must be nil: %+v", rows[0])
	}

	// max=1 limits.
	limited, err := db.Events("pool/data", 1)
	if err != nil {
		t.Fatalf("Events(max=1): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != 2 {
		t.Errorf("Events(max=1) = %+v, want single row id 2 (first in txg,id order)", limited)
	}

	// Unknown dataset: empty slice, nil error.
	empty, err := db.Events("nope/none", 0)
	if err != nil {
		t.Fatalf("Events(unknown): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("Events(unknown) = %v, want empty non-nil slice", empty)
	}
}

func TestZmetadDB_EventsNullCapturedAndPaths(t *testing.T) {
	path, conn := writeFullTestDB(t)
	// A PARTIAL row: NULL captured_at, NULL full_path/old_full_path.
	insertEvent(t, conn, "pool/data", 1, 1, nil, "CREATE", "a.txt", "", "", "")

	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	rows, err := db.Events("pool/data", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].CapturedAt != nil {
		t.Errorf("CapturedAt = %v, want nil (NULL must not fabricate)", *rows[0].CapturedAt)
	}
	if rows[0].FullPath != nil {
		t.Errorf("FullPath = %v, want nil for PARTIAL row", *rows[0].FullPath)
	}
	if rows[0].OldFullPath != nil {
		t.Errorf("OldFullPath = %v, want nil", *rows[0].OldFullPath)
	}
	if rows[0].Path == nil || *rows[0].Path != "a.txt" {
		t.Errorf("Path = %v, want a.txt", rows[0].Path)
	}
}

func TestZmetadDB_GapStats(t *testing.T) {
	path, conn := writeFullTestDB(t)
	for _, lost := range []int64{5, 3, 0, -1} {
		if _, err := conn.Exec(
			"INSERT INTO gaps (dataset, detected, from_offset, to_offset, lost) VALUES (?, 1700000000, NULL, NULL, ?)",
			"pool/data", lost); err != nil {
			t.Fatalf("insert gap: %v", err)
		}
	}
	// Other dataset's rows must not leak in.
	if _, err := conn.Exec(
		"INSERT INTO gaps (dataset, detected, lost) VALUES ('other/data', 1700000000, 99)"); err != nil {
		t.Fatalf("insert other gap: %v", err)
	}

	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	stats, err := db.GapStats("pool/data")
	if err != nil {
		t.Fatalf("GapStats: %v", err)
	}
	// The -1 sentinel must NEVER enter KnownLost (issue #9 guard):
	// 5 + 3 = 8, not 7 and not 9.
	want := GapStats{KnownLost: 8, Regressions: 1, RingSwaps: 1}
	if stats != want {
		t.Errorf("GapStats = %+v, want %+v", stats, want)
	}

	// Dataset with no gap rows: all zeros, nil error.
	zero, err := db.GapStats("empty/ds")
	if err != nil {
		t.Fatalf("GapStats(empty): %v", err)
	}
	if zero != (GapStats{}) {
		t.Errorf("GapStats(empty) = %+v, want zero", zero)
	}
}

func TestZmetadDB_ResolveDatasetByPath(t *testing.T) {
	path, conn := writeFullTestDB(t)
	for ds, mp := range map[string]string{
		"testpool":         "/testpool",
		"testpool/scratch": "/testpool/scratch",
		"unmounted":        "none",
		"legacypool":       "legacy",
	} {
		if _, err := conn.Exec("INSERT INTO datasets (dataset, mountpoint) VALUES (?, ?)",
			ds, mp); err != nil {
			t.Fatalf("insert dataset: %v", err)
		}
	}

	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "exact mountpoint", path: "/testpool", want: "testpool"},
		{name: "shallow child falls to parent", path: "/testpool/a/b", want: "testpool"},
		{name: "longest matching ancestor wins", path: "/testpool/scratch/x", want: "testpool/scratch"},
		{name: "scratch exact", path: "/testpool/scratch", want: "testpool/scratch"},
		{name: "separator guard", path: "/testpoolx", wantErr: true},
		{name: "separator guard deep", path: "/testpoolx/y", wantErr: true},
		{name: "unrelated path", path: "/opt/other", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.ResolveDatasetByPath(tc.path)
			if tc.wantErr {
				if _, ok := errors.AsType[*DatasetNotTrackedError](err); !ok {
					t.Fatalf("expected *DatasetNotTrackedError, got %T: %v", err, err)
				}
				if !strings.HasPrefix(err.Error(), "metadata: ") {
					t.Errorf("error %q does not start with 'metadata: '", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveDatasetByPath(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("ResolveDatasetByPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestZmetadDB_ResolveDatasetByPath_NoRows(t *testing.T) {
	path, _ := writeFullTestDB(t)
	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	_, err = db.ResolveDatasetByPath("/anything")
	if _, ok := errors.AsType[*DatasetNotTrackedError](err); !ok {
		t.Fatalf("empty datasets table: expected *DatasetNotTrackedError, got %T: %v", err, err)
	}
}

func TestZmetadDB_ResolveDatasetByPath_OnlyNoneLegacy(t *testing.T) {
	path, conn := writeFullTestDB(t)
	for ds, mp := range map[string]string{"a": "none", "b": "legacy"} {
		if _, err := conn.Exec("INSERT INTO datasets (dataset, mountpoint) VALUES (?, ?)",
			ds, mp); err != nil {
			t.Fatalf("insert dataset: %v", err)
		}
	}

	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	_, err = db.ResolveDatasetByPath("/none")
	if _, ok := errors.AsType[*DatasetNotTrackedError](err); !ok {
		t.Fatalf("none/legacy only: expected *DatasetNotTrackedError, got %T: %v", err, err)
	}
}

func TestZmetadDB_HasDataset(t *testing.T) {
	path, conn := writeFullTestDB(t)
	if _, err := conn.Exec(
		"INSERT INTO sync_state (dataset, last_offset, last_sync, ring_guid) VALUES ('pool/data', 42, 1700000000, 7)"); err != nil {
		t.Fatalf("insert sync_state: %v", err)
	}

	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	has, err := db.HasDataset("pool/data")
	if err != nil {
		t.Fatalf("HasDataset(present): %v", err)
	}
	if !has {
		t.Error("HasDataset(pool/data) = false, want true")
	}

	has, err = db.HasDataset("pool/absent")
	if err != nil {
		t.Fatalf("HasDataset(absent): %v", err)
	}
	if has {
		t.Error("HasDataset(pool/absent) = true, want false")
	}
}

// TestMountpointContains pins the containment gate ResolveDatasetByPath
// relies on (moved from the deleted zfs_cmd_test.go with leaf 04).
func TestMountpointContains(t *testing.T) {
	tests := []struct {
		mountpoint, path string
		want             bool
	}{
		{"/mnt/tank/data", "/mnt/tank/data", true},
		{"/mnt/tank", "/mnt/tank/data", true},
		{"/mnt/tank", "/mnt/tank/data/deep/k", true},
		{"/mnt/tank", "/mnt/tankdata", false},  // prefix but not path-prefix
		{"/mnt/tank", "/mnt/tank/datax", true}, // datax is a genuine child
		{"/mnt/tank/data", "/mnt/tank", false},
		{"/mnt/tank", "/opt/other", false},
		{"", "", true}, // degenerate: equal empty strings
	}
	for _, tc := range tests {
		if got := mountpointContains(tc.mountpoint, tc.path); got != tc.want {
			t.Errorf("mountpointContains(%q, %q) = %v, want %v", tc.mountpoint, tc.path, got, tc.want)
		}
	}
}
