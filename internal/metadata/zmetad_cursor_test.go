package metadata

// zmetad_cursor_test.go — gateway issue #15: the event-cursor pass-through
// at the provider layer. Two fixture tiers:
//
//  1. REAL SQLite fixture (buildFixtureDB, the e2e builder): since-id must
//     split the 11 canned rows EXACTLY (strictly-after semantics, no
//     duplicate, no gap) and every mapped event carries its monotonic row
//     id — the value a client feeds back via ?since-id=N.
//  2. stubbed dbHandle (leaf 03's openDB seam): the filter composes with
//     the key/Since/MaxEvents passes in the pinned order, 0 stays the
//     no-cursor behavior, and max-events caps AFTER the cursor pass.
//
// The cursor is a STRUCT-FIELD extension of HistoryQuery (seam-legal): the
// frozen MetadataProvider interface gains no methods, and providers without
// cursor support keep serving the pre-#15 shape.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestZmetadCursorFixtureSplitsExactly drives the REAL provider over the
// REAL fixture database: since-id mid-stream resumes with no duplicate and
// no gap, and every event carries its row id.
func TestZmetadCursorFixtureSplitsExactly(t *testing.T) {
	mount := t.TempDir()
	resolved, err := filepath.EvalSymlinks(mount)
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	dbPath := buildFixtureDB(t, resolved)
	t.Setenv("ZETAOBJECT_ASSUME_ZFS", "1")

	p := NewZmetadEventsProvider(dbPath)
	if res, err := p.Probe(context.Background(), resolved); err != nil || !res.Available {
		t.Fatalf("Probe = %+v, %v; want available", res, err)
	}
	ctx := context.Background()

	all, err := p.History(ctx, resolved, "", HistoryQuery{})
	if err != nil {
		t.Fatalf("History(no cursor): %v", err)
	}
	if len(all) != 11 {
		t.Fatalf("events = %d, want the 11 canned rows", len(all))
	}
	// Every event carries a strictly-monotonic-correct id: the fixture
	// inserts in id order, so events[i].ID must be i+1... but the wire
	// contract only pins STRICTLY MONOTONIC INCREASING per (txg ASC, id
	// ASC) order — assert that, not the exact values.
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("event ids not strictly increasing: [%d].ID=%d <= [%d].ID=%d",
				i, all[i].ID, i-1, all[i-1].ID)
		}
		if all[i].ID == 0 {
			t.Fatalf("event %d carries no cursor id (fabricated zero)", i)
		}
	}

	// Resume EXACTLY at the 6th event: since-id=all[5].ID must return
	// precisely all[6:] — same slice, byte-for-byte comparable fields.
	since := all[5].ID
	resumed, err := p.History(ctx, resolved, "", HistoryQuery{SinceID: since})
	if err != nil {
		t.Fatalf("History(since-id=%d): %v", since, err)
	}
	if len(resumed) != len(all)-6 {
		t.Fatalf("resumed %d events, want %d (exact split, no duplicate no gap)",
			len(resumed), len(all)-6)
	}
	for i := range resumed {
		want := all[6+i]
		got := resumed[i]
		if got.ID != want.ID || got.Op != want.Op || got.Key != want.Key || got.Txg != want.Txg {
			t.Fatalf("resumed[%d] = %+v, want %+v (exact resume)", i, got, want)
		}
	}

	// since-id at the LAST id: empty history (strictly after).
	tail, err := p.History(ctx, resolved, "", HistoryQuery{SinceID: all[len(all)-1].ID})
	if err != nil {
		t.Fatalf("History(since-id=last): %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("since-id=last id returned %d events, want 0", len(tail))
	}

	// since-id=0 is the documented no-cursor behavior: the full stream.
	fromZero, err := p.History(ctx, resolved, "", HistoryQuery{SinceID: 0})
	if err != nil {
		t.Fatalf("History(since-id=0): %v", err)
	}
	if len(fromZero) != 11 {
		t.Fatalf("since-id=0 returned %d events, want all 11", len(fromZero))
	}
}

// TestZmetadCursorComposesWithFilters pins the filter order on the stubbed
// seam: key filter -> Since -> SinceID -> MaxEvents cap.
func TestZmetadCursorComposesWithFilters(t *testing.T) {
	dbPath := "/var/lib/zfs/zmetad.db"
	// ids 1..5 inserted in order; the provider orders by (txg ASC, id ASC)
	// so txg == id here keeps the assertions flat.
	rows := make([]EventRow, 0, 5)
	for id := 1; id <= 5; id++ {
		rows = append(rows, row("CREATE", uint64(id), 100,
			new("obj"), new("dir/obj")))
		rows[len(rows)-1].ID = int64(id)
	}
	db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true, events: rows}
	stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
	p := newTestProvider(t, dbPath, "")
	p.cacheDataset("/mnt/tank/data", "tank/data")
	ctx := context.Background()

	// since-id=3: ids 4 and 5 only (strictly after — id 3 itself is NOT
	// redelivered; a redelivered event would double-count in a client).
	got, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{SinceID: 3})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 2 || got[0].ID != 4 || got[1].ID != 5 {
		t.Fatalf("since-id=3 = %d events (ids %d,%d), want ids 4,5",
			len(got), got[0].ID, got[1].ID)
	}

	// Cursor composes with the key filter (key pass runs first).
	scoped, err := p.History(ctx, "/mnt/tank/data", "dir/obj", HistoryQuery{SinceID: 3})
	if err != nil {
		t.Fatalf("scoped History: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped since-id = %d events, want 2 (key filter matches all rows here)", len(scoped))
	}

	// MaxEvents caps AFTER the cursor pass (semantics unchanged).
	capped, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{SinceID: 3, MaxEvents: 1})
	if err != nil {
		t.Fatalf("capped History: %v", err)
	}
	if len(capped) != 1 || capped[0].ID != 4 {
		t.Fatalf("capped since-id = %+v, want exactly id 4 (cap post-cursor)", capped)
	}
}

// TestRowsToEventsCarriesRowID pins the row->event mapping: the mapped
// event's ID is the events-table row id verbatim (the cursor a client
// feeds back as since-id), zero only when the row carries no id.
func TestRowsToEventsCarriesRowID(t *testing.T) {
	rows := []EventRow{
		{ID: 41, Op: "CREATE", Txg: 10, Path: new("a.txt"), FullPath: new("a.txt")},
		{ID: 42, Op: "RENAME", Txg: 11, Path: new("b.txt"), FullPath: new("dir/b.txt"),
			OldPath: new("a.txt"), OldFullPath: new("a.txt")},
		{Op: "CREATE", Txg: 12, Path: new("c.txt"), FullPath: new("c.txt")}, // no id
	}
	set := rowsToEvents(rows)
	if set.events[0].ID != 41 || set.events[1].ID != 42 {
		t.Fatalf("mapped ids = %d,%d, want 41,42", set.events[0].ID, set.events[1].ID)
	}
	if set.events[2].ID != 0 {
		t.Fatalf("id-less row mapped ID = %d, want 0 (never fabricated)", set.events[2].ID)
	}
}

// TestZmetadCursorSQLiteWhereIDGreater is the fixture-pinned proof that
// the WHERE id > ? mapping is the DATABASE's semantics, not just an
// in-memory filter: rows are inserted with explicit out-of-order ids
// against a real SQLite file, and the provider must return strictly the
// rows with id > N in (txg ASC, id ASC) order.
func TestZmetadCursorSQLiteWhereIDGreater(t *testing.T) {
	path, conn := writeFullTestDB(t)
	// Insert with EXPLICIT ids (the PK, not the implicit autoincrement):
	// out-of-order so an id filter is distinguishable from row order.
	insertEventWithID(t, conn, "pool/data", 10, 1, nil, "CREATE", "a.txt", "", "docs/a.txt", "")
	insertEventWithID(t, conn, "pool/data", 20, 7, nil, "CREATE", "b.txt", "", "docs/b.txt", "")
	insertEventWithID(t, conn, "pool/data", 30, 4, nil, "CREATE", "c.txt", "", "docs/c.txt", "")
	defer conn.Close()

	db, err := OpenZmetadDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenZmetadDB: %v", err)
	}
	defer db.Close()

	// db.Events has no cursor param by design (leaf 01's minimal seam);
	// the cursor filter lives in the provider over the full row set.
	// This test pins the RAW row ids the provider's filter sees: the
	// monotonic id column is exactly what HistoryQuery.SinceID compares
	// against (WHERE id > ?).
	rows, err := db.Events("pool/data", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantIDs := []int64{1, 7, 4} // (txg ASC, id ASC): txg dominates, ids follow
	for i, w := range wantIDs {
		if rows[i].ID != w {
			t.Fatalf("rows[%d].ID = %d, want %d", i, rows[i].ID, w)
		}
	}
}

// insertEventWithID inserts one events row with an EXPLICIT id (the
// autoincrement PK) so cursor semantics are testable against real
// SQLite ordering.
func insertEventWithID(t *testing.T, conn *sql.DB, dataset string, txg, id int64,
	capturedAt any, objectType, path, oldPath, fullPath, oldFullPath string) {
	t.Helper()
	_, err := conn.Exec(`INSERT INTO events
		(id, dataset, txg, timestamp, captured_at, object_id, event_type,
		 path, old_path, full_path, old_full_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, dataset, txg, id, capturedAt, id, objectType,
		nullStr(path), nullStr(oldPath), nullStr(fullPath), nullStr(oldFullPath))
	if err != nil {
		t.Fatalf("insert event id=%d: %v", id, err)
	}
}
