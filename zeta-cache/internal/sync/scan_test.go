package sync

// scan_test.go - collection-token skip + forced-full-scan fallback, the
// "on any doubt" degrade, and FullRescan bypassing the skip. Reuses the
// memfs stub's dir-<hex> tokens (same derivation as the gateway's
// colltoken.go).

import (
	"strings"
	"testing"
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
