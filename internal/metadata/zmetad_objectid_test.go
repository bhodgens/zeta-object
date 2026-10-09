package metadata

// zmetad_objectid_test.go — ResolveObjectID tests (zfs-metadata#13
// consumer leaf). The DB is a hand-built fixture (the writeTestDB helper
// shape): the newest-row-naming-the-path rule, the old_full_path rename
// arm, and the ObjectIDNotFoundError on an unnamed path.

import (
	"errors"
	"testing"
)

func TestResolveObjectID(t *testing.T) {
	path, conn := writeFullTestDB(t)
	defer conn.Close()

	// Two generations of "doc.txt": created as object 100, then a RENAME
	// "draft.txt" -> "doc.txt" on object 100 (newest row), plus an
	// unrelated row. The newest row naming doc.txt carries object 100.
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, dataset, txg, timestamp, object_id, event_type, path, full_path)
		  VALUES (1, 'testpool/e2e', 10, 1000, 200, 'CREATE', 'other.txt', 'other.txt')`, nil},
		{`INSERT INTO events (id, dataset, txg, timestamp, object_id, event_type, path, old_path, full_path, old_full_path)
		  VALUES (2, 'testpool/e2e', 11, 1001, 100, 'CREATE', 'draft.txt', NULL, 'draft.txt', NULL)`, nil},
		{`INSERT INTO events (id, dataset, txg, timestamp, object_id, event_type, path, old_path, full_path, old_full_path)
		  VALUES (3, 'testpool/e2e', 12, 1002, 100, 'RENAME', 'doc.txt', 'draft.txt', 'doc.txt', 'draft.txt')`, nil},
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	_ = path

	db, err := OpenZmetadDB(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Current name: resolved via the newest (rename) row -> object 100.
	got, err := db.ResolveObjectID("testpool/e2e", "doc.txt")
	if err != nil {
		t.Fatalf("ResolveObjectID(doc.txt): %v", err)
	}
	if got != 100 {
		t.Fatalf("doc.txt object id = %d, want 100", got)
	}

	// Old name: the rename row's old_full_path also resolves -> 100
	// (rename-stable identity).
	got, err = db.ResolveObjectID("testpool/e2e", "draft.txt")
	if err != nil {
		t.Fatalf("ResolveObjectID(draft.txt): %v", err)
	}
	if got != 100 {
		t.Fatalf("draft.txt object id = %d, want 100 (rename-stable)", got)
	}

	// Unrelated object.
	got, err = db.ResolveObjectID("testpool/e2e", "other.txt")
	if err != nil {
		t.Fatalf("ResolveObjectID(other.txt): %v", err)
	}
	if got != 200 {
		t.Fatalf("other.txt object id = %d, want 200", got)
	}

	// A path no row names: ObjectIDNotFoundError.
	_, err = db.ResolveObjectID("testpool/e2e", "ghost.txt")
	var notFound *ObjectIDNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("ghost.txt: expected ObjectIDNotFoundError, got %v", err)
	}

	// Degenerate inputs reject without a query.
	if _, err := db.ResolveObjectID("", "x"); !errors.As(err, &notFound) {
		t.Fatalf("empty dataset: expected ObjectIDNotFoundError, got %v", err)
	}
	if _, err := db.ResolveObjectID("testpool/e2e", ""); !errors.As(err, &notFound) {
		t.Fatalf("empty path: expected ObjectIDNotFoundError, got %v", err)
	}
}
