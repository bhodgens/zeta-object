package sync

// pipeline_test.go - the upload/remote-apply pipelines against the memfs
// fault injection: 412 conflict-copy naming, MD5-skip, 5xx leaves dirty,
// idempotent re-sync, ChangeFeed seam, and the engine-owned status.

import (
	"context"
	"errors"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// Test412OnUploadFaultInjection drives the upload path into the 412:
// the memfs answers ErrConflict when If-Match misses, and the engine
// must land the conflict copy with the dated name.
func Test412OnUploadFaultInjection(t *testing.T) {
	h := newHarness(t)
	key := "notes.txt"
	etagKnown := transport.MD5Hex([]byte("known"))
	// Server row exists with a DIFFERENT generation than the index's
	// known-good etag, but list the index etag so matrix 2 (upload)
	// fires; the stub's Put then 412s because the stored body's MD5
	// differs from our If-Match.
	h.fs.PutRaw(key, "server body")
	h.putLocal(key, "local body")
	h.seedDirty(key, etagKnown)
	// Fail only THIS key's Put with 412 (If-Match stale): the conflict
	// copy's own create-upload must succeed in the same pass.
	h.fs.FailPutsWith(&transport.GateError{Gate: "put", Cause: transport.ErrConflict, OnlyKey: &key})

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := h.localBody("notes (conflicted copy 2026-10-06).txt")
	if got != "local body" {
		t.Fatalf("conflict copy body = %q, want local body preserved", got)
	}
	if body := h.localBody(key); body == "local body" {
		t.Fatalf("remote should have been applied over the local path")
	}
	rep := h.eng.LastReport()
	if rep.ConflictCopies != 1 {
		t.Fatalf("ConflictCopies = %d, want 1: %+v", rep.ConflictCopies, rep)
	}
	if st := h.eng.Status(); st.Conflicts != 1 {
		t.Fatalf("status.Conflicts = %d, want 1", st.Conflicts)
	}
}

// TestMD5SkipNoPUT: dirty content whose bytes already match the known
// etag skips the PUT entirely (the server ETag IS the MD5).
func TestMD5SkipNoPUT(t *testing.T) {
	h := newHarness(t)
	key := "same.txt"
	etag := transport.MD5Hex([]byte("identical"))
	h.fs.PutRaw(key, "identical")
	h.putLocal(key, "identical")
	h.seedDirty(key, etag)

	// Inject a Put failure: if the engine tried to PUT, the sync errors.
	h.fs.FailPutsWith(&transport.GateError{Gate: "put", Cause: errors.New("must not PUT")})
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync should have skipped the PUT: %v", err)
	}
	rep := h.eng.LastReport()
	if rep.SkippedUploads != 1 || rep.Uploads != 0 {
		t.Fatalf("report = %+v, want SkippedUploads=1 Uploads=0", rep)
	}
	if h.row(key).Dirty {
		t.Fatalf("row should be clean after the skip")
	}
}

// TestOtherErrorLeavesDirty: a non-412 Put failure leaves the row dirty
// for the scheduler's retry (and SyncOnce reports it).
func TestOtherErrorLeavesDirty(t *testing.T) {
	h := newHarness(t)
	key := "big.bin"
	h.putLocal(key, "local")
	h.seedDirty(key, transport.MD5Hex([]byte("old")))
	h.fs.PutRaw(key, "old")
	h.fs.FailPutsWith(&transport.GateError{Gate: "put", Cause: errors.New("503 backend")})

	err := h.eng.SyncOnce(bg())
	if err == nil {
		t.Fatalf("sync should surface the failed upload")
	}
	if !h.row(key).Dirty {
		t.Fatalf("row must stay dirty after a non-412 failure")
	}
	// State flips to error (run() surfaces the failure); the dirty row
	// survives for the scheduler's retry either way.
	if st := h.eng.Status(); st.State != StateError {
		t.Fatalf("state = %q, want %q", st.State, StateError)
	}
}

// TestIdempotentResync: a second SyncOnce immediately after the first
// performs ZERO actions (the "atomicity: interrupted sync resumes"
// requirement - uploads are idempotent because If-Match binds them).
func TestIdempotentResync(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("a.txt", "remote a")
	h.fs.PutRaw("d/b.txt", "remote b")
	h.putLocal("c.txt", "local c")
	h.putLocal("d/b.txt", "remote b")

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := h.eng.LastReport()

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	second := h.eng.LastReport()

	if second.Downloads+second.Uploads+second.ConflictCopies+second.RemoteDeletes+second.LocalDeletes+second.RemoteDeleteKept != 0 {
		t.Fatalf("second sync ran actions: %+v (first: %+v)", second, first)
	}
	if second.SkippedUploads != 0 {
		t.Fatalf("second sync should have had nothing to upload at all: %+v", second)
	}
}

// TestChangeFeedDeltaPath: a feed that names one changed path syncs just
// that path (fullScan=false), and a failing feed degrades to full scan.
func TestChangeFeedDeltaPath(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("x/changed.txt", "new")
	h.putLocal("x/changed.txt", "old")
	h.seedClean("x/changed.txt", transport.MD5Hex([]byte("old")))
	h.fs.PutRaw("untouched.txt", "stable")
	h.putLocal("untouched.txt", "stable")
	h.seedClean("untouched.txt", transport.MD5Hex([]byte("stable")))

	// Named-path delta: only the changed file is diffed -> download.
	h.eng.feed = deltaFeed{paths: []string{"x/changed.txt"}}
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("delta sync: %v", err)
	}
	rep := h.eng.LastReport()
	if rep.Scanned != 1 || rep.Downloads != 1 {
		t.Fatalf("delta report = %+v, want Scanned=1 Downloads=1", rep)
	}
	if got := h.localBody("x/changed.txt"); got != "new" {
		t.Fatalf("delta download body = %q", got)
	}

	// Failing feed: degrade to the full scan, still correct.
	h.eng.feed = failingFeed{}
	h.fs.PutRaw("untouched.txt", "stable v2")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("degraded sync: %v", err)
	}
	if got := h.localBody("untouched.txt"); got != "stable v2" {
		t.Fatalf("degraded sync did not pick up the change: %q", got)
	}
}

// deltaFeed is a leaf-05-shaped feed stub returning a fixed path list.
type deltaFeed struct{ paths []string }

func (f deltaFeed) Delta(context.Context) ([]string, bool, error) {
	return f.paths, false, nil
}

// failingFeed always errors (the engine must degrade to full scan).
type failingFeed struct{}

func (failingFeed) Delta(context.Context) ([]string, bool, error) {
	return nil, false, errors.New("cursor 503")
}

// TestStatusFields pins the engine-owned IPC values across a lifecycle.
func TestStatusFields(t *testing.T) {
	h := newHarness(t)
	st := h.eng.Status()
	if st.State != StateIdle || st.LastSync != 0 || st.Dirty != 0 || st.Conflicts != 0 {
		t.Fatalf("initial status = %+v", st)
	}

	h.fs.PutRaw("s.txt", "v1")
	h.putLocal("s.txt", "v1")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	st = h.eng.Status()
	if st.State != StateIdle {
		t.Fatalf("state after sync = %q", st.State)
	}
	if st.LastSync != fixedNow.Unix() {
		t.Fatalf("lastSync = %d, want %d", st.LastSync, fixedNow.Unix())
	}

	// A conflict bumps the count and it persists (meta table).
	h.fs.PutRaw("s.txt", "v2")
	h.putLocal("s.txt", "mine")
	h.seedDirty("s.txt", transport.MD5Hex([]byte("v1")))
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("conflict sync: %v", err)
	}
	st = h.eng.Status()
	if st.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1: %+v", st.Conflicts, st)
	}
	if st.Dirty != 0 {
		t.Fatalf("dirty = %d, want 0 (the conflict copy uploads within the same pass)", st.Dirty)
	}
}
