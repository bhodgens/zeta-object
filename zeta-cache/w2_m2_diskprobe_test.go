package main

// w2_m2_diskprobe_test.go - the W2-M2 regression: main must wire a REAL
// disk probe (stat files/<path>) into index.OpenWithDiskCheck. With the
// nil probe, reconcile rule R2 was production-dead: a hydrated file
// deleted behind the daemon stayed hydrated=1 forever and quota summed
// phantom bytes. Pins the probe's semantics (present/absent, slash keys,
// subdirectory keys) and the full demotion through the real store.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// TestCacheDiskProbeSemantics - the probe reports exactly what os.Stat
// sees under files/, slash-keyed, for nested keys too.
func TestCacheDiskProbeSemantics(t *testing.T) {
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	if err := os.MkdirAll(filepath.Join(files, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "docs", "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := cacheDiskProbe(dir)
	if !probe("docs/a.txt") {
		t.Error("probe(docs/a.txt) = false, want true (file exists)")
	}
	if probe("docs/missing.txt") {
		t.Error("probe(docs/missing.txt) = true, want false")
	}
	if probe("docs") {
		t.Error("probe(docs) = true for a DIRECTORY; the probe must only see files")
	}
	if probe("never/existed") {
		t.Error("probe(never/existed) = true, want false")
	}
}

// TestW2M2RealProbeDemotesDeletedFile - the end-to-end R2 behavior main
// now gets: a journalled clean hydrated row whose disk file is gone is
// demoted at OpenWithDiskCheck (with the daemon's own probe), while the
// nil-probe sentinel keeps treating everything as present.
func TestW2M2RealProbeDemotesDeletedFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "i.db")

	// Seed a clean hydrated row + its disk file, then close (the
	// reconcile at next open must decide).
	store, err := index.OpenWithDiskCheck(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := "seed bytes"
	if err := store.PutPath(t.Context(), index.Resource{
		Path: "a.txt", ETag: transport.MD5Hex([]byte(body)), Mtime: 1728211200,
		Size: int64(len(body)), Hydrated: true,
	}, index.OpHydrate, "w2-m2 seed"); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(dir, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "a.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The file is deleted BEHIND the daemon (no journal entry).
	if err := os.Remove(filepath.Join(files, "a.txt")); err != nil {
		t.Fatal(err)
	}

	// Reopen with the REAL probe (what main.go wires): rule R2 must
	// demote the ghost row.
	realStore, err := index.OpenWithDiskCheck(dbPath, cacheDiskProbe(dir))
	if err != nil {
		t.Fatalf("reopen with real probe: %v", err)
	}
	defer realStore.Close()
	row, err := realStore.Get(t.Context(), "a.txt")
	if err != nil {
		t.Fatalf("get after real-probe reconcile: %v", err)
	}
	if row.Hydrated {
		t.Fatalf("row still hydrated=%v after the disk file was deleted behind the daemon (R2 dead again)", row.Hydrated)
	}

	// Control: a SECOND db where the file still exists stays hydrated.
	keepPath := filepath.Join(files, "b.txt")
	if err := os.WriteFile(keepPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store2, err := index.OpenWithDiskCheck(filepath.Join(dir, "i2.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.PutPath(t.Context(), index.Resource{
		Path: "b.txt", ETag: transport.MD5Hex([]byte(body)), Mtime: 1728211200,
		Size: int64(len(body)), Hydrated: true,
	}, index.OpHydrate, "w2-m2 control"); err != nil {
		t.Fatal(err)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	keepStore, err := index.OpenWithDiskCheck(filepath.Join(dir, "i2.db"), cacheDiskProbe(dir))
	if err != nil {
		t.Fatalf("reopen control: %v", err)
	}
	defer keepStore.Close()
	row2, err := keepStore.Get(t.Context(), "b.txt")
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if !row2.Hydrated {
		t.Error("control row demoted although its disk file exists - over-eager probe")
	}
}

// TestW2M2ProbeMatchesEngineCacheLayout - the probe must stat the same
// path the sync engine materializes into (<cacheDir>/files/<key>), or R2
// would demote rows the engine considers hydrated.
func TestW2M2ProbeMatchesEngineCacheLayout(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "staging"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(filepath.Join(dir, "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fs := transport.NewMemFS()
	eng, err := zsync.NewEngine(zsync.Options{
		Transport: fs, Store: store, CacheDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	fs.PutRaw("n/a.txt", "hydrated body")
	if err := eng.SyncOnce(t.Context()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	probe := cacheDiskProbe(dir)
	if !probe("n/a.txt") {
		t.Error("probe(n/a.txt) = false after the engine hydrated it - probe/layout mismatch")
	}
}
