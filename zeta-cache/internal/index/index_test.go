package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// diskSet builds a diskCheck probe over an explicit existing-path set.
func diskSet(existing ...string) func(string) bool {
	set := map[string]bool{}
	for _, p := range existing {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := OpenWithDiskCheck(filepath.Join(t.TempDir(), "sub", "index.db"), diskSet())
	if err != nil {
		t.Fatalf("OpenWithDiskCheck = %v, want nil", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.write.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func journalCount(t *testing.T, s *Store, path string) int {
	t.Helper()
	rows, err := s.read.Query(`SELECT COUNT(*) FROM journal WHERE path = ?`, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	rows.Next()
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- open/migrate -------------------------------------------------------

func TestOpenMigratesToV1(t *testing.T) {
	s := open(t)
	if got := userVersion(t, s); got != schemaVersion {
		t.Fatalf("user_version = %d, want %d", got, schemaVersion)
	}
	var mode string
	if err := s.write.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	// Tables exist and the resources indexes are in place.
	for _, tbl := range []string{"resources", "journal", "meta"} {
		var name string
		err := s.write.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
}

func TestOpenUpgradesLegacyV0Placeholder(t *testing.T) {
	// A leaf-01 placeholder DB (user_version=0, no schema) must migrate.
	path := filepath.Join(t.TempDir(), "index.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenWithDiskCheck(path, diskSet())
	if err != nil {
		t.Fatalf("OpenWithDiskCheck(legacy) = %v, want nil", err)
	}
	defer s.Close()
	// Leaf 07 bumped the schema to v2 (additive eviction columns); the
	// legacy v0 placeholder migrates through v1 to the current version.
	if got := userVersion(t, s); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s := open(t)
	_ = s
	// Build a DB with a user_version NEWER than the package's current
	// schema directly (bumped as the package migrates forward).
	futureVersion := schemaVersion + 1
	future, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, futureVersion)); err != nil {
		t.Fatal(err)
	}
	future.Close()
	if _, err := OpenWithDiskCheck(path, diskSet()); err == nil {
		t.Fatal("OpenWithDiskCheck(newer user_version) = nil error, want refusal")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error %q does not mention 'newer'", err)
	}
}

// --- CRUD ---------------------------------------------------------------

func TestGetPutPathRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	in := Resource{Path: "docs/a.txt", ETag: "\"abc\"", Mtime: 1700000000, Size: 42,
		Hydrated: true, Dirty: false}
	if err := s.PutPath(ctx, in, OpHydrate, "test hydrate"); err != nil {
		t.Fatal(err)
	}
	out, err := s.Get(ctx, "docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip: got %+v want %+v", out, in)
	}
	// Upsert overwrites, journal row landed in the same tx.
	in2 := in
	in2.Dirty = true
	in2.Size = 43
	if err := s.PutPath(ctx, in2, OpLocalWrite, "edited"); err != nil {
		t.Fatal(err)
	}
	out, _ = s.Get(ctx, "docs/a.txt")
	if !out.Dirty || out.Size != 43 {
		t.Errorf("upsert lost: %+v", out)
	}
	if n := journalCount(t, s, "docs/a.txt"); n != 2 {
		t.Errorf("journal rows = %d, want 2 (one per PutPath)", n)
	}
}

func TestGetNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(nope) = %v, want ErrNotFound", err)
	}
}

func TestPutPathNoOpSkipsJournal(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.PutPath(ctx, Resource{Path: "x"}, "", "state-only"); err != nil {
		t.Fatal(err)
	}
	if n := journalCount(t, s, "x"); n != 0 {
		t.Errorf("journal rows = %d, want 0 for op-less PutPath", n)
	}
}

func TestPutPathRejectsUnknownJournalOp(t *testing.T) {
	s := open(t)
	err := s.PutPath(context.Background(), Resource{Path: "x"}, "mystery-op", "")
	if err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Errorf("PutPath(unknown op) = %v, want 'unknown op' error", err)
	}
	// The index mutation must have rolled back WITH the journal row.
	if _, err := s.Get(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("row survived failed tx: %v", err)
	}
}

func TestDeletePathTombstoneLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.PutPath(ctx, Resource{Path: "d/f", ETag: "\"e\"", Hydrated: true, Size: 5}, OpHydrate, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePath(ctx, "d/f", "rm"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, "d/f")
	if err != nil {
		t.Fatal(err)
	}
	// Tombstone: deleted=1, body state zeroed, etag kept for the grace table.
	if !r.Deleted || r.Hydrated || r.Dirty || r.ETag != "\"e\"" {
		t.Errorf("tombstone wrong: %+v", r)
	}
	// Delete of an unknown path creates the tombstone either way.
	if err := s.DeletePath(ctx, "d/ghost", ""); err != nil {
		t.Fatal(err)
	}
	if r, err = s.Get(ctx, "d/ghost"); err != nil || !r.Deleted {
		t.Errorf("DeletePath(ghost): (%+v, %v), want tombstone", r, err)
	}
	// Tombstones never show as dirty, never as eviction candidates.
	dirty, err := s.DirtyPaths(ctx)
	if err != nil || len(dirty) != 0 {
		t.Errorf("DirtyPaths after tombstones = %v, %v", dirty, err)
	}
	evict, err := s.CleanHydratedPaths(ctx, nil)
	if err != nil || len(evict) != 0 {
		t.Errorf("eviction candidates = %v, %v, want none (tombstones excluded)", evict, err)
	}
}

// --- dirty + eviction ---------------------------------------------------

func TestDirtyPaths(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, r := range []Resource{
		{Path: "b", Dirty: true},
		{Path: "a", Dirty: true},
		{Path: "c", Dirty: true, Deleted: true}, // tombstone: excluded
		{Path: "d"},
	} {
		if err := s.PutPath(ctx, r, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.DirtyPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("DirtyPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DirtyPaths = %v, want %v", got, want)
		}
	}
}

func TestCleanHydratedPathsEvictionQuery(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, r := range []Resource{
		{Path: "clean1", Hydrated: true},                      // candidate
		{Path: "clean2", Hydrated: true},                      // candidate, but pinned
		{Path: "dirty-hydrated", Hydrated: true, Dirty: true}, // dirty: stays local
		{Path: "dry-clean"},                                   // not hydrated
		{Path: "tomb", Hydrated: true, Deleted: true},         // tombstone
	} {
		if err := s.PutPath(ctx, r, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CleanHydratedPaths(ctx, map[string]bool{"clean2": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "clean1" {
		t.Errorf("eviction candidates = %v, want [clean1]", got)
	}
	// Same query, no pins: both candidates.
	got, err = s.CleanHydratedPaths(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("eviction candidates (no pins) = %v, want [clean1 clean2]", got)
	}
}

// --- journal ------------------------------------------------------------

func TestJournalAppendAndSince(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, op := range []string{OpLocalWrite, OpUploadOK, OpHydrate} {
		if err := s.JournalAppend(ctx, op, "j/p", ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.JournalSince(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("JournalSince(0) = %d rows, want 3", len(rows))
	}
	// Monotone ids, ops in order.
	ops := []string{rows[0].Op, rows[1].Op, rows[2].Op}
	want := []string{OpLocalWrite, OpUploadOK, OpHydrate}
	for i := range want {
		if ops[i] != want[i] || rows[i].Path != "j/p" || rows[i].TS == 0 {
			t.Errorf("row %d = %+v, want op=%s path=j/p ts!=0", i, rows[i], want[i])
		}
	}
	tail, err := s.JournalSince(ctx, rows[1].ID)
	if err != nil || len(tail) != 1 || tail[0].Op != OpHydrate {
		t.Errorf("JournalSince(%d) = %v, %v; want only the hydrate row", rows[1].ID, tail, err)
	}
	if err := s.JournalAppend(ctx, "bogus", "p", ""); err == nil {
		t.Error("JournalAppend(bogus) = nil error, want rejection")
	}
}

// --- reconcile rules table ----------------------------------------------

func TestReconcileRulesTable(t *testing.T) {
	ctx := context.Background()
	onDisk := diskSet("dirty-keep", "gone-hydrated-actually-there")

	t.Run("R1 dirty on disk no upload-ok keeps dirty", func(t *testing.T) {
		s, err := OpenWithDiskCheck(filepath.Join(t.TempDir(), "i.db"), onDisk)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.PutPath(ctx, Resource{Path: "dirty-keep", Dirty: true, Hydrated: true}, OpLocalWrite, ""); err != nil {
			t.Fatal(err)
		}
		rep, err := s.ReconcilePass(ctx, onDisk)
		if err != nil {
			t.Fatal(err)
		}
		if rep.KeptDirty != 1 {
			t.Errorf("R1: KeptDirty = %d, want 1; %+v", rep.KeptDirty, rep)
		}
		r, _ := s.Get(ctx, "dirty-keep")
		if !r.Dirty {
			t.Errorf("R1 demoted a dirty file: %+v", r)
		}
	})

	t.Run("R2 hydrated but missing file demotes", func(t *testing.T) {
		s, err := OpenWithDiskCheck(filepath.Join(t.TempDir(), "i.db"), onDisk)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.PutPath(ctx, Resource{Path: "evicted-but-index-says-here", Hydrated: true}, OpUploadOK, ""); err != nil {
			t.Fatal(err)
		}
		rep, err := s.ReconcilePass(ctx, onDisk)
		if err != nil {
			t.Fatal(err)
		}
		if rep.DemotedDry != 1 {
			t.Errorf("R2: DemotedDry = %d, want 1; %+v", rep.DemotedDry, rep)
		}
		r, _ := s.Get(ctx, "evicted-but-index-says-here")
		if r.Hydrated {
			t.Errorf("R2 did not demote: %+v", r)
		}
		// Recovery writes no journal rows.
		if n := journalCount(t, s, "evicted-but-index-says-here"); n != 1 {
			t.Errorf("reconcile wrote journal rows: %d, want 1 (the original upload-ok)", n)
		}
	})

	t.Run("R3 journal upload-ok with dirty index is report-only", func(t *testing.T) {
		s, err := OpenWithDiskCheck(filepath.Join(t.TempDir(), "i.db"), onDisk)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// Simulate the torn state: upload-ok journaled, index still dirty
		// (as if the clean transition never landed).
		if err := s.PutPath(ctx, Resource{Path: "stale-dirty", Dirty: true, Hydrated: true}, OpUploadOK, ""); err != nil {
			t.Fatal(err)
		}
		// Force index dirty after the journal said upload-ok.
		if err := s.PutPath(ctx, Resource{Path: "stale-dirty", Dirty: true, Hydrated: true}, "", "repair"); err != nil {
			t.Fatal(err)
		}
		rep, err := s.ReconcilePass(ctx, onDisk)
		if err != nil {
			t.Fatal(err)
		}
		// The journal NEVER overwrites index state: still dirty, counted
		// as unresolved for leaf 04's ETag re-probe.
		r, _ := s.Get(ctx, "stale-dirty")
		if !r.Dirty {
			t.Errorf("R3: journal overwrote index state: %+v", r)
		}
		if rep.Unresolved != 1 || rep.UploadOKs != 1 {
			t.Errorf("R3 report wrong: %+v", rep)
		}
	})

	t.Run("tombstones and unjournalled rows untouched", func(t *testing.T) {
		s, err := OpenWithDiskCheck(filepath.Join(t.TempDir(), "i.db"), diskSet())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.PutPath(ctx, Resource{Path: "tomb", Hydrated: true, Deleted: true}, OpDelete, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.PutPath(ctx, Resource{Path: "unjournalled", Hydrated: true}, "", ""); err != nil {
			t.Fatal(err)
		}
		rep, err := s.ReconcilePass(ctx, diskSet())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Examined != 0 {
			t.Errorf("Examined = %d, want 0 (no journal-attributable rows)", rep.Examined)
		}
		// The unjournalled hydrated row keeps its state: reconcile only
		// resolves paths with MY journal history.
		r, _ := s.Get(ctx, "unjournalled")
		if !r.Hydrated {
			t.Errorf("unjournalled row was demoted: %+v", r)
		}
	})

	t.Run("demotion corrects state when reopened", func(t *testing.T) {
		// The real crash story: write, kill the connection (simulate the
		// body vanishing from the cache dir), reopen, reconcile runs in
		// Open and demotes.
		path := filepath.Join(t.TempDir(), "i.db")
		s, err := OpenWithDiskCheck(path, diskSet("p"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutPath(ctx, Resource{Path: "p", Hydrated: true}, OpHydrate, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// Reopen with the file now gone from disk.
		s2, err := OpenWithDiskCheck(path, diskSet())
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		r, err := s2.Get(ctx, "p")
		if err != nil {
			t.Fatal(err)
		}
		if r.Hydrated {
			t.Errorf("reopen+reconcile did not demote missing file: %+v", r)
		}
	})
}

// --- crash simulation ---------------------------------------------------

func TestCrashSimulationTornTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	s, err := OpenWithDiskCheck(path, diskSet())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Kill the connection mid-transaction: index mutation + journal row
	// are uncommitted. Neither may survive.
	prev := CrashPoint
	CrashPoint = func(string) error { return errors.New("simulated crash: SIGKILL") }
	t.Cleanup(func() { CrashPoint = prev })

	err = s.TornPut(ctx, Resource{Path: "torn", Hydrated: true}, OpUploadOK, "")
	if err == nil || !strings.Contains(err.Error(), "crash") {
		t.Fatalf("TornPut = %v, want the simulated crash", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the torn run recovered via reconcile; no phantom row, no
	// phantom journal entry.
	s2, err := OpenWithDiskCheck(path, diskSet())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Get(ctx, "torn"); !errors.Is(err, ErrNotFound) {
		t.Errorf("torn index row survived: %v", err)
	}
	if rows, _ := s2.JournalSince(ctx, 0); len(rows) != 0 {
		t.Errorf("torn journal rows survived: %+v", rows)
	}
}

// --- WAL concurrency ----------------------------------------------------

func TestWALConcurrencyWriterAndReaders(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.PutPath(ctx, Resource{Path: "seed", Hydrated: true}, OpHydrate, ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	stop := make(chan struct{})

	// Single writer: continuous index mutations with journal rows.
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r := Resource{Path: "hot", ETag: "\"v\"", Size: int64(i), Hydrated: true}
			if err := s.PutPath(ctx, r, OpLocalWrite, "concurrency"); err != nil {
				errs <- err
				return
			}
		}
	})

	// WAL readers: eviction query + journal walk + dirty paths, racing the
	// writer. Must never block it out or error.
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.CleanHydratedPaths(ctx, nil); err != nil {
					errs <- err
					return
				}
				if _, err := s.JournalSince(ctx, 0); err != nil {
					errs <- err
					return
				}
				if _, err := s.DirtyPaths(ctx); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	time.Sleep(750 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent op failed: %v", err)
	}
}

// --- tombstone retention groundwork -------------------------------------

func TestExpiredTombstonesRetention(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-31 * 24 * time.Hour)
	rows := []Resource{
		{Path: "expired-old", Deleted: true, Mtime: old.Unix()},
		{Path: "fresh", Deleted: true, Mtime: now.Add(-time.Hour).Unix()},
		{Path: "alive-old", Mtime: old.Unix()}, // not a tombstone
	}
	for _, r := range rows {
		if err := s.PutPath(ctx, r, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ExpiredTombstones(ctx, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "expired-old" {
		t.Errorf("ExpiredTombstones = %v, want [expired-old]", got)
	}
}

// --- meta ---------------------------------------------------------------

func TestMetaSetGet(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.MetaGet(ctx, "last-full-scan"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MetaGet(missing) = %v, want ErrNotFound", err)
	}
	if err := s.MetaSet(ctx, "last-full-scan", "1700000000"); err != nil {
		t.Fatal(err)
	}
	if err := s.MetaSet(ctx, "last-full-scan", "1700000001"); err != nil {
		t.Fatal(err)
	}
	v, err := s.MetaGet(ctx, "last-full-scan")
	if err != nil || v != "1700000001" {
		t.Errorf("MetaGet = %q, %v; want 1700000001, nil", v, err)
	}
}

// --- persistence across reopen ------------------------------------------

func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	s, err := OpenWithDiskCheck(path, diskSet())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.PutPath(ctx, Resource{Path: "p", ETag: "\"e\"", Size: 1, Hydrated: true}, OpUploadOK, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenWithDiskCheck(path, diskSet("p"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	r, err := s2.Get(ctx, "p")
	if err != nil || !r.Hydrated || r.ETag != "\"e\"" {
		t.Errorf("state lost across reopen: %+v, %v", r, err)
	}
	if rows, _ := s2.JournalSince(ctx, 0); len(rows) != 1 {
		t.Errorf("journal lost across reopen: %d rows", len(rows))
	}
}
