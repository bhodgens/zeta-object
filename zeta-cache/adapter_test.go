package main

// adapter_test.go - regression for the journal-op contract between the
// fusefs.Index adapter and index.Store: MarkDirty must PERSIST the dirty
// row (an unknown journal op rolls the whole PutPath transaction back,
// which silently lost every prompt-upload - found by the case-40 live
// mount round-trip on CI, 2026-10-07).

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

func newAdapterTestStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("index.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAdapterMarkDirtyPersistsRowAndJournal(t *testing.T) {
	store := newAdapterTestStore(t)
	a := newIndexAdapter(store).(indexAdapter) //nolint:forcetypeassert // test-only: concrete type carries the ctx/store fields

	if err := a.MarkDirty("fuse-roundtrip.txt", 11, 1700000000); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}

	// The row must EXIST and be dirty - the watcher's drain skips keys
	// whose row is missing or clean, so a lost row silently loses the
	// upload forever.
	r, err := a.store.Get(context.Background(), "fuse-roundtrip.txt")
	if err != nil {
		t.Fatalf("row missing after MarkDirty: %v", err)
	}
	if !r.Dirty {
		t.Fatalf("row dirty = false, want true: %+v", r)
	}

	// The journal must carry the local-write op (the crash-recovery
	// record; reconcile rule R1 keys off it).
	if err := a.MarkDirty("second.txt", 3, 1700000001); err != nil {
		t.Fatalf("MarkDirty second: %v", err)
	}
	// JournalSince from 0 must show both writes as local-write ops.
	entries, err := store.JournalSince(context.Background(), 0)
	if err != nil {
		t.Fatalf("JournalSince: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Op == index.OpLocalWrite {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("journal local-write entries = %d, want >= 2 (%+v)", n, entries)
	}
}

func TestAdapterMarkCleanClearsDirty(t *testing.T) {
	store := newAdapterTestStore(t)
	a := newIndexAdapter(store).(indexAdapter) //nolint:forcetypeassert // test-only: concrete type carries the ctx/store fields

	if err := a.MarkDirty("x.txt", 5, 1700000000); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := a.MarkClean("x.txt", "deadbeef", 5); err != nil {
		t.Fatalf("MarkClean: %v", err)
	}
	r, err := a.store.Get(context.Background(), "x.txt")
	if err != nil {
		t.Fatalf("row missing after MarkClean: %v", err)
	}
	if r.Dirty || r.ETag != "deadbeef" {
		t.Fatalf("row = %+v, want clean with etag deadbeef", r)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "x.txt")); !os.IsNotExist(err) {
		// sanity only: the adapter never writes cache files itself
		_ = err
	}
}
