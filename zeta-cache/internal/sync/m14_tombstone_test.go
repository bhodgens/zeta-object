package sync

// m14_tombstone_test.go - M14 regression: a tombstone may only delete
// the remote when the server still holds the EXACT generation the
// tombstone recorded (Delete carries no wire precondition, so the
// listing-vs-tombstone ETag comparison IS the guard). On a changed
// generation the tombstone is kept and a kept-local conflict is
// surfaced - another writer's data is never deleted.

import (
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// TestTombstoneKeptOnRemoteGenerationChange: the user deletes a synced
// file (tombstone at etag(v1)), but another client RE-CREATED the key
// with new content before our pass. The delete must NOT fire; the
// tombstone stands and the conflict is surfaced.
func TestTombstoneKeptOnRemoteGenerationChange(t *testing.T) {
	h := newHarness(t)
	key := "doc.txt"
	etagV1 := transport.MD5Hex([]byte("v1"))
	// Synced at v1, then deleted locally (tombstone keeps the etag).
	h.fs.PutRaw(key, "v1")
	h.putLocal(key, "v1")
	h.seedClean(key, etagV1)
	if err := h.store.DeletePath(bg(), key, "user delete via mount"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	// Another client re-created the key with DIFFERENT content.
	h.fs.PutRaw(key, "someone else's new content")

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, present := h.fs.ServerBody(key); !present {
		t.Fatal("the remote file was DELETED despite carrying a foreign generation - M14 regression")
	}
	if row := h.row(key); !row.Deleted {
		t.Fatalf("tombstone row = %+v, want kept (deleted=1)", h.row(key))
	}
	recs, err := h.eng.Conflicts(bg())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Path != key || recs[0].Kind != "kept-local" {
		t.Fatalf("conflicts = %+v, want one kept-local for %s", recs, key)
	}
	if st := h.eng.Status(); st.Conflicts != 1 {
		t.Fatalf("status.Conflicts = %d, want 1 (the kept record)", st.Conflicts)
	}
}

// TestTombstoneDeletesMatchingGeneration: the control - when the remote
// still lists the recorded generation, the DELETE fires and the
// tombstone is confirmed (the matrix-6 happy path through the guard).
func TestTombstoneDeletesMatchingGeneration(t *testing.T) {
	h := newHarness(t)
	key := "gone.txt"
	etagV1 := transport.MD5Hex([]byte("v1"))
	h.fs.PutRaw(key, "v1")
	h.putLocal(key, "v1")
	h.seedClean(key, etagV1)
	if err := h.store.DeletePath(bg(), key, "user delete via mount"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, present := h.fs.ServerBody(key); present {
		t.Fatal("matching generation must still be deleted (matrix 6)")
	}
	rep := h.eng.LastReport()
	if rep.LocalDeletes != 1 {
		t.Fatalf("LocalDeletes = %d, want 1: %+v", rep.LocalDeletes, rep)
	}
}
