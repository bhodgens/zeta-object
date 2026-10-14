package index

// r5_tombstone_restamp_test.go - the R5 regression: tombstones written by
// the PRE-upgrade DeletePath carry mtime=0 (no restamp convention yet).
// The first PruneTombstones after the upgrade pruned them immediately
// (0 < any cutoff), wiping the deletion-grace window before the user
// ever saw it. The fix: Open restamps deleted=1 AND mtime=0 rows with
// the open time, granting a full retention window from the upgrade
// moment. One-time by construction (restamped rows have mtime > 0).

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestR5PreUpgradeTombstoneRestampedOnOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/index.db"

	// Seed a v0-style DB: current schema, but the tombstone carries
	// mtime=0 like the old DeletePath wrote.
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.Exec(`INSERT INTO resources (path, etag, mtime, size, hydrated, dirty, deleted)
	                            VALUES ('old.txt', 'deadbeef', 0, 0, 0, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	// A post-upgrade tombstone (real mtime) must NOT be touched.
	fresh := time.Now().Unix()
	if _, err := st.write.Exec(`INSERT INTO resources (path, etag, mtime, size, hydrated, dirty, deleted)
	                            VALUES ('new.txt', '', ?, 0, 0, 0, 1)`, fresh); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open: the restamp runs inside Open.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ctx := context.Background()

	var oldMtime int64
	if err := st2.read.QueryRow(`SELECT mtime FROM resources WHERE path = 'old.txt'`).Scan(&oldMtime); err != nil {
		t.Fatal(err)
	}
	if oldMtime <= 0 {
		t.Fatalf("pre-upgrade tombstone still has mtime %d after open, want > 0", oldMtime)
	}
	// The fresh tombstone keeps its timestamp.
	var newMtime int64
	if err := st2.read.QueryRow(`SELECT mtime FROM resources WHERE path = 'new.txt'`).Scan(&newMtime); err != nil {
		t.Fatal(err)
	}
	if newMtime != fresh {
		t.Fatalf("fresh tombstone mtime moved: %d, want %d", newMtime, fresh)
	}

	// A prune at a NORMAL retention (e.g. 30 days) must keep the
	// restamped tombstone - it now has a full grace window.
	pruned, err := st2.PruneTombstones(ctx, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("pruned = %v at normal retention, want none (restamped window full)", pruned)
	}
	var n int
	if err := st2.read.QueryRow(`SELECT COUNT(*) FROM resources WHERE deleted = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("tombstone count = %d, want 2 (both survive normal retention)", n)
	}
}

// Idempotence: a second open must be a no-op (restamped rows no longer
// match mtime=0) - and a tombstone pruned at an expired cutoff still
// prunes normally.
func TestR5RestampIdempotentAndPruneStillWorks(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/index.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.Exec(`INSERT INTO resources (path, etag, mtime, size, hydrated, dirty, deleted)
	                            VALUES ('t0.txt', '', 0, 0, 0, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var m1 int64
	if err := st2.read.QueryRow(`SELECT mtime FROM resources WHERE path = 't0.txt'`).Scan(&m1); err != nil {
		if err == sql.ErrNoRows {
			t.Fatal("tombstone vanished on first open")
		}
		t.Fatal(err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
	st3, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	var m2 int64
	if err := st3.read.QueryRow(`SELECT mtime FROM resources WHERE path = 't0.txt'`).Scan(&m2); err != nil {
		t.Fatal(err)
	}
	if m1 != m2 {
		t.Fatalf("second open moved the restamped mtime: %d -> %d", m1, m2)
	}
	// An EXPIRED tombstone still prunes (the fix must not immortalize).
	gone, err := st3.PruneTombstones(context.Background(), time.Second, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0] != "t0.txt" {
		t.Fatalf("expired prune = %v, want [t0.txt]", gone)
	}
}
