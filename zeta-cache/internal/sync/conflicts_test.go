package sync

// conflicts_test.go - leaf-08 acceptance: persistent conflict records
// (matrix 3 + matrix 5), resolve in both modes, and restore hit /
// honest-miss. The harness comes from helpers_test.go; the memfs
// listing ETag is the RAW body MD5 (colltoken.go's fileETagLocked), so
// seeds use transport.MD5Hex directly.
//
// Resolve tests call the engine pipelines directly (download/upload/
// deleteLocal) to ARRANGE a live conflict record deterministically; the
// scan side is covered by TestConflictRecorded* (which assert the
// matrix wiring through a real SyncOnce).

import (
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// arrangeConflictCopy drives the matrix-3 conflict for real (SyncOnce)
// and returns the recorded copy key.
func arrangeConflictCopy(t *testing.T, h *harness) string {
	t.Helper()
	h.fs.PutRaw("doc.txt", "v2") // remote changed
	h.putLocal("doc.txt", "local edit")
	h.seedDirty("doc.txt", transport.MD5Hex([]byte("v1")))
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return "doc (conflicted copy 2026-10-06).txt"
}

func TestConflictRecordedOnConflictCopy(t *testing.T) {
	h := newHarness(t)
	copyKey := arrangeConflictCopy(t, h)
	recs, err := h.eng.Conflicts(bg())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("conflicts = %d (%+v), want 1", len(recs), recs)
	}
	rec := recs[0]
	if rec.Path != "doc.txt" || rec.Kind != "conflict-copy" {
		t.Errorf("record = %+v, want doc.txt/conflict-copy", rec)
	}
	if rec.CopyPath != copyKey {
		t.Errorf("copyPath = %q, want %q", rec.CopyPath, copyKey)
	}
	if rec.DetectedAt != fixedNow.Unix() {
		t.Errorf("detectedAt = %d, want %d", rec.DetectedAt, fixedNow.Unix())
	}
}

func TestConflictRecordedOnKeptLocal(t *testing.T) {
	h := newHarness(t)
	// Organic matrix 5: two passes - (1) upload the local edit, (2)
	// remote deleted under the now-dirty... no: matrix 5 needs the row
	// DIRTY when the remote disappears. Dirty it again after pass 1.
	h.fs.PutRaw("doc.txt", "v1")
	h.putLocal("doc.txt", "v1")
	h.seedDirty("doc.txt", transport.MD5Hex([]byte("v1")))
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatal(err)
	}
	// Row is clean at etag(v1). Re-dirty it with a local edit; the
	// remote then disappears -> matrix 5 (dirty + remote deleted).
	h.putLocal("doc.txt", "local edit")
	h.seedDirty("doc.txt", transport.MD5Hex([]byte("v1")))
	_ = h.fs.Delete(bg(), "doc.txt", "")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatal(err)
	}
	recs, err := h.eng.Conflicts(bg())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Kind != "kept-local" || recs[0].CopyPath != "" {
		t.Fatalf("conflicts = %+v, want one kept-local without copy", recs)
	}
}

func TestConflictRecordSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	arrangeConflictCopy(t, h)

	// A NEW engine over the SAME store sees the record (meta rows
	// persist; this is the daemon-restart case).
	eng2, err := NewEngine(Options{
		Transport: h.fs, Store: h.store, CacheDir: h.dir,
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := eng2.Conflicts(bg())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("after restart conflicts = %d, want 1 (records must persist)", len(recs))
	}
}

func TestResolveKeepLocalUploadsLocalCopy(t *testing.T) {
	h := newHarness(t)
	copyKey := arrangeConflictCopy(t, h)

	if err := h.eng.ResolveConflict(bg(), "doc.txt", ResolveKeepLocal, false); err != nil {
		t.Fatalf("resolve keep-local: %v", err)
	}
	if got := h.fs.ServerBodyOrEmpty("doc.txt"); got != "local edit" {
		t.Errorf("server body = %q, want the local edit", got)
	}
	if got := h.localBody("doc.txt"); got != "local edit" {
		t.Errorf("local body = %q, want untouched local edit", got)
	}
	if row := h.row("doc.txt"); row.Dirty {
		t.Errorf("row still dirty after keep-local: %+v", row)
	}
	recs, _ := h.eng.Conflicts(bg())
	if len(recs) != 0 {
		t.Errorf("conflicts after resolve = %+v, want empty", recs)
	}
	// Without confirm the preserved copy survives untouched.
	if got := h.localBody(copyKey); got != "local edit" {
		t.Errorf("copy body = %q, want preserved", got)
	}
}

func TestResolveKeepRemoteDownloadsRemoteOverLocal(t *testing.T) {
	h := newHarness(t)
	copyKey := arrangeConflictCopy(t, h)

	// Resolve keep-remote WITHOUT confirm: the remote applies locally
	// but the preserved copy file MUST survive (explicit-flag rule).
	if err := h.eng.ResolveConflict(bg(), "doc.txt", ResolveKeepRemote, false); err != nil {
		t.Fatalf("resolve keep-remote: %v", err)
	}
	if got := h.localBody("doc.txt"); got != "v2" {
		t.Errorf("local body = %q, want v2", got)
	}
	if got := h.localBody(copyKey); got != "local edit" {
		t.Errorf("copy body = %q, want preserved local edit (no confirm)", got)
	}
	// The arrange's SyncOnce already pushed the copy as a new file
	// (matrix 3), so the copy row is CLEAN here; the point of this
	// assertion is that it SURVIVED (not tombstoned by the resolve).
	if row := h.row(copyKey); row.Deleted {
		t.Errorf("copy row after no-confirm resolve = %+v, want live (survives without confirm)", row)
	}
	if row := h.row("doc.txt"); row.Dirty || row.Deleted {
		t.Errorf("live row after keep-remote = %+v, want clean", row)
	}
	recs, _ := h.eng.Conflicts(bg())
	if len(recs) != 0 {
		t.Errorf("conflicts after resolve = %+v, want empty", recs)
	}
}

func TestResolveConfirmRemovesCopyBothModes(t *testing.T) {
	h := newHarness(t)
	copyKey := arrangeConflictCopy(t, h)
	if err := h.eng.ResolveConflict(bg(), "doc.txt", ResolveKeepRemote, true); err != nil {
		t.Fatal(err)
	}
	if got := h.localBody(copyKey); got != "" {
		t.Errorf("copy survived confirm=true: %q", got)
	}
	if row := h.row(copyKey); !row.Deleted {
		t.Errorf("copy row = %+v, want tombstoned", row)
	}

	// Same for keep-local + confirm (second conflict on another file).
	h.fs.PutRaw("b.txt", "rv2")
	h.putLocal("b.txt", "lv")
	h.seedDirty("b.txt", transport.MD5Hex([]byte("rv")))
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ResolveConflict(bg(), "b.txt", ResolveKeepLocal, true); err != nil {
		t.Fatal(err)
	}
	if got := h.fs.ServerBodyOrEmpty("b.txt"); got != "lv" {
		t.Errorf("b server = %q, want lv", got)
	}
	if body := h.localBody("b (conflicted copy 2026-10-06).txt"); body != "" {
		t.Errorf("b copy survived confirm=true: %q", body)
	}
}

func TestResolveUnknownPathAndMode(t *testing.T) {
	h := newHarness(t)
	if err := h.eng.ResolveConflict(bg(), "nope.txt", ResolveKeepLocal, false); err == nil {
		t.Error("resolve unknown path = nil, want error")
	}
	if err := h.eng.ResolveConflict(bg(), "nope.txt", "delete-both", false); err == nil {
		t.Error("resolve bad mode = nil, want error")
	}
}

func TestResolveKeepLocalOnKeptLocalConflict(t *testing.T) {
	h := newHarness(t)
	// Arrange a kept-local conflict organically, then resolve
	// keep-local: the local bytes re-upload (recreating the file the
	// remote deleted - the user's edit wins).
	h.fs.PutRaw("doc.txt", "v1")
	h.putLocal("doc.txt", "v1")
	h.seedDirty("doc.txt", transport.MD5Hex([]byte("v1")))
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatal(err)
	}
	h.putLocal("doc.txt", "local edit")
	h.seedDirty("doc.txt", transport.MD5Hex([]byte("v1")))
	_ = h.fs.Delete(bg(), "doc.txt", "")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ResolveConflict(bg(), "doc.txt", ResolveKeepLocal, false); err != nil {
		t.Fatalf("resolve keep-local on kept-local: %v", err)
	}
	if got := h.fs.ServerBodyOrEmpty("doc.txt"); got != "local edit" {
		t.Errorf("server = %q, want the local edit re-uploaded", got)
	}
}

func TestRestoreDeletedHit(t *testing.T) {
	h := newHarness(t)
	// A file deleted LOCALLY only: the tombstone is fresh and the
	// server still lists the path - restore must re-download.
	h.fs.PutRaw("gone.txt", "server copy")
	h.putLocal("gone.txt", "x")
	h.seedClean("gone.txt", transport.MD5Hex([]byte("x")))
	if err := h.store.DeletePath(bg(), "gone.txt", "test delete"); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RestoreDeleted(bg(), "gone.txt"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := h.localBody("gone.txt"); got != "server copy" {
		t.Errorf("restored body = %q, want server copy", got)
	}
	if row := h.row("gone.txt"); row.Deleted || row.Dirty {
		t.Errorf("restored row = %+v, want clean live row", row)
	}
}

func TestRestoreDeletedHonestMiss(t *testing.T) {
	h := newHarness(t)
	// Organic matrix 4: the remote delete lands through a real scan
	// (clean local + remote gone -> tombstone), leaving NOTHING to
	// restore - the error must say so, never ok.
	h.fs.PutRaw("gone.txt", "v")
	h.putLocal("gone.txt", "v")
	h.seedClean("gone.txt", transport.MD5Hex([]byte("v")))
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatal(err)
	}
	_ = h.fs.Delete(bg(), "gone.txt", "")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatal(err)
	}
	if row := h.row("gone.txt"); !row.Deleted {
		t.Fatalf("arrange: gone.txt row = %+v, want tombstone", row)
	}
	err := h.eng.RestoreDeleted(bg(), "gone.txt")
	if err == nil {
		t.Fatal("restore of a server-gone path = nil, want honest error")
	}
	if !strings.Contains(err.Error(), "no longer serves") {
		t.Errorf("error %q, want the honest not-served message", err)
	}
}

func TestRestoreNotDeletedRefused(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("live.txt", "v")
	h.putLocal("live.txt", "v")
	h.seedClean("live.txt", transport.MD5Hex([]byte("v")))
	if err := h.eng.RestoreDeleted(bg(), "live.txt"); err == nil {
		t.Error("restore of a live path = nil, want error")
	}
	if err := h.eng.RestoreDeleted(bg(), "never-existed.txt"); err == nil {
		t.Error("restore of an unknown path = nil, want error")
	}
}
