package sync

// scan_test.go - collection-token skip + forced-full-scan fallback, the
// "on any doubt" degrade, and FullRescan bypassing the skip. Reuses the
// memfs stub's dir-<hex> tokens (same derivation as the gateway's
// colltoken.go).

import (
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

func TestCollectionTokenSkip(t *testing.T) {
	h := newHarness(t)
	// Subtree: d/a.txt on the server and in the index, clean.
	h.fs.PutRaw("d/a.txt", "one")
	h.putLocal("d/a.txt", "one")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	rep := h.eng.LastReport()
	if rep.FullscanVerified == 0 {
		t.Fatalf("first sync should have walked the subtree: %+v", rep)
	}
	// The dir row with the token was recorded.
	tok, err := h.store.Get(bg(), "d/")
	if err != nil {
		t.Fatalf("dir row missing after first sync: %v", err)
	}
	if !strings.HasPrefix(tok.ETag, "dir-") {
		t.Fatalf("dir token = %q, want dir-<hex>", tok.ETag)
	}

	// Second sync: nothing changed anywhere -> the subtree is SKIPPED
	// (token matches), and no actions run.
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	rep = h.eng.LastReport()
	if rep.TokenVerified == 0 {
		t.Fatalf("second sync should skip via token: %+v", rep)
	}
	if rep.Downloads+rep.Uploads+rep.ConflictCopies != 0 {
		t.Fatalf("second sync ran actions: %+v", rep)
	}

	// Third sync with a FORCED full rescan: the skip is bypassed even
	// though the token still matches.
	if err := h.eng.FullRescan(bg()); err != nil {
		t.Fatalf("full rescan: %v", err)
	}
	rep = h.eng.LastReport()
	if rep.FullscanVerified == 0 || rep.TokenVerified != 0 {
		t.Fatalf("FullRescan must list every subtree: %+v", rep)
	}
}

func TestTokenMismatchWalksSubtree(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("d/a.txt", "one")
	h.putLocal("d/a.txt", "one")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Server-side change under d/: the token MUST move (immediate
	// children tuple changes), so the next scan walks the subtree and
	// downloads the new generation.
	h.fs.PutRaw("d/a.txt", "two")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	rep := h.eng.LastReport()
	if rep.FullscanVerified == 0 {
		t.Fatalf("changed subtree must be walked, not skipped: %+v", rep)
	}
	if got := h.localBody("d/a.txt"); got != "two" {
		t.Fatalf("local after change = %q, want %q", got, "two")
	}
}

func TestPropfindFaultFallsBackToErrorNotSilentSkip(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("d/a.txt", "one")
	h.putLocal("d/a.txt", "one")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Inject a Propfind-only failure: on ANY doubt the engine must NOT
	// claim a clean sync - it surfaces the error (state=error) and the
	// next successful pass recovers. It must never silently skip the
	// whole tree as if nothing changed.
	h.fs.FailPropfinds()
	defer h.fs.ClearFaults()
	err := h.eng.SyncOnce(bg())
	if err == nil {
		t.Fatalf("SyncOnce with failing Propfind must error, got nil")
	}
	if st := h.eng.Status(); st.State != StateError {
		t.Fatalf("status.state = %q, want %q", st.State, StateError)
	}
	// Recovery: clear the fault, sync again, all green.
	h.fs.ClearFaults()
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("recovery sync: %v", err)
	}
	if st := h.eng.Status(); st.State != StateIdle {
		t.Fatalf("status.state after recovery = %q, want %q", st.State, StateIdle)
	}
}

// TestEmptyBucketRoot404IsEmptySync pins the gateway interaction the
// live mount round-trip found (2026-10-07): an EMPTY bucket 404s on the
// root PROPFIND (the webdav view resolves a collection only when it
// holds content). The engine must treat root+ErrNotExist as an empty
// tree - the sync succeeds with no actions and its own later PUTs
// create the collection - while a 404 below the root stays an error.
func TestEmptyBucketRoot404IsEmptySync(t *testing.T) {
	h := newHarness(t)
	// The local side has a dirty file (the "first client" write); the
	// server answers 404 for the root listing (bucket exists, empty).
	h.putLocal("a.txt", "first client write")
	h.fs.FailPropfindsWith(transport.ErrNotExist)

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync on empty bucket (root 404): %v", err)
	}
	rep := h.eng.LastReport()
	if rep.Uploads != 1 {
		t.Fatalf("uploads = %d, want 1 (the local file must upload): %+v", rep.Uploads, rep)
	}
	h.fs.ClearFaults()

	// The server now holds the file; a second sync is a no-op.
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	rep = h.eng.LastReport()
	if rep.Uploads+rep.Downloads+rep.ConflictCopies != 0 {
		t.Fatalf("second sync ran actions on a settled tree: %+v", rep)
	}
}

// TestNonRoot404StaysError pins the complement: a 404 below the root
// (the parent listing just named that child) is a genuine error.
func TestNonRoot404StaysError(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("d/a.txt", "x")
	h.putLocal("d/a.txt", "x")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	h.fs.FailPropfindsWith(transport.ErrNotExist)
	if err := h.eng.SyncOnce(bg()); err == nil {
		t.Fatal("subtree 404 must stay an error, not an empty tree")
	}
}

// TestSubdirSelfRowSkippedWithTrailingSlash pins the live-found descent
// bug (2026-10-09): the inner walk's self row carries the trailing
// slash ('sub/') while dirKey is bare ('sub') - the failed equality
// made the engine descend into 'sub//' and the nested file was never
// downloaded (scanned=2 downloads=1 on the live gateway repro).
func TestSubdirSelfRowSkippedWithTrailingSlash(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("sub/deep.txt", "sub content")
	h.fs.PutRaw("root.txt", "root content")

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	rep := h.eng.LastReport()
	if rep.Downloads != 2 {
		t.Fatalf("downloads = %d, want 2 (nested file must download): %+v", rep.Downloads, rep)
	}
	if _, err := h.store.Get(bg(), "sub/deep.txt"); err != nil {
		t.Fatalf("sub/deep.txt row missing: %v", err)
	}
}
