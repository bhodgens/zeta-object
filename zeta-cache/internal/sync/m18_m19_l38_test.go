package sync

// m18_m19_l38_test.go - regressions for the engine pass mechanics:
//
//   - M18: ONE shared seen map threads the walk recursion, so the
//     complement pass never re-PROPFINDs a subtree the walk already
//     listed (counted via the memfs listing-fault gate).
//   - M19: a second concurrent SyncOnce/FullRescan returns
//     ErrSyncInProgress instead of interleaving; a nightly FullRescan's
//     listAll flag survives untouched.
//   - L38: Status().Conflicts is the PENDING record count (matches
//     Conflicts()), never the per-pass counter.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// TestSharedSeenMapNoRePropfinds (M18): a tree with nested collections;
// after a FullRescan every listed path is in the shared seen map, so
// the complement loop must run with ZERO extra parent PROPFINDs. We
// count listings via FullscanVerified+TokenVerified is not enough, so
// inject a Propfind fault AFTER the walk completes? Simpler: count
// PROPFINDs with a counting transport wrapper.
func TestSharedSeenMapNoRePropfinds(t *testing.T) {
	h := newHarness(t)
	// Nested tree: d/ (with sub/deep.txt inside) + root files.
	h.fs.PutRaw("d/a.txt", "a")
	h.fs.PutRaw("d/sub/deep.txt", "deep")
	h.fs.PutRaw("d/sub/inner.txt", "inner")
	h.fs.PutRaw("root.txt", "r")

	// First sync downloads everything.
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// The complement pass (index rows not in seen) ran during the
	// first sync too; it must NOT have re-probed parents for the
	// already-diffed d/sub/* keys. Evidence: the pass count matches
	// the file count exactly (each file diffed once, not twice).
	rep := h.eng.LastReport()
	const wantFiles = 4
	if rep.Scanned != wantFiles {
		t.Fatalf("Scanned = %d, want exactly %d (a fresh seen map per subtree re-diffs descendants): %+v",
			rep.Scanned, wantFiles, rep)
	}

	// Second sync, forced full rescan: same invariant under the
	// FullRescan walk.
	if err := h.eng.FullRescan(bg()); err != nil {
		t.Fatalf("full rescan: %v", err)
	}
	rep = h.eng.LastReport()
	if rep.Scanned != wantFiles {
		t.Fatalf("FullRescan Scanned = %d, want exactly %d (no complement re-diffs): %+v",
			rep.Scanned, wantFiles, rep)
	}
}

// countingFS wraps MemFS and counts PROPFIND calls.
type countingFS struct {
	*transport.MemFS
	mu   sync.Mutex
	pf   int
	fail func(n int) error // optional failure hook after n counted
}

func (c *countingFS) Propfind(ctx context.Context, key string, recursive bool) ([]transport.Entry, error) {
	c.mu.Lock()
	c.pf++
	n := c.pf
	hook := c.fail
	c.mu.Unlock()
	if hook != nil {
		if err := hook(n); err != nil {
			return nil, err
		}
	}
	return c.MemFS.Propfind(ctx, key, recursive)
}

// TestFullRescanComplementNoExtraPropfinds (M18, the hard pin): a
// nested tree synced clean, then a FullRescan - the complement loop
// must diff NOTHING (every file key is in the shared seen map), so the
// PROPFIND count stays at exactly the tree's collection count. With the
// old fresh-map-per-subtree walk, each d/sub/* file missed seen and the
// complement re-probed its parent per file.
func TestFullRescanComplementNoExtraPropfinds(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("d/a.txt", "a")
	h.fs.PutRaw("d/sub/deep.txt", "deep")
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	cfs := &countingFS{MemFS: h.fs}
	h.eng.tr = cfs
	if err := h.eng.FullRescan(bg()); err != nil {
		t.Fatalf("full rescan: %v", err)
	}
	// Expected PROPFINDs: root, d/, d/sub/ = 3 collections, and the
	// complement pass adds ZERO (all file rows were listed by the walk).
	cfs.mu.Lock()
	n := cfs.pf
	cfs.mu.Unlock()
	if n != 3 {
		t.Fatalf("PROPFINDs during FullRescan = %d, want exactly 3 (complement must not re-probe listed subtrees)", n)
	}
	rep := h.eng.LastReport()
	if rep.Scanned != 2 {
		t.Fatalf("Scanned = %d, want 2 (each file diffed once)", rep.Scanned)
	}
}

// TestSingleRunGate (M19): while one pass is blocked inside its scan, a
// concurrent SyncOnce AND FullRescan both bounce with ErrSyncInProgress,
// and the blocked FullRescan still walks every subtree (its listAll was
// not cleared).
func TestSingleRunGate(t *testing.T) {
	h := newHarness(t)
	h.fs.PutRaw("a.txt", "a")
	h.fs.PutRaw("d/b.txt", "b")
	// First sync so d/ has a matching token (the token-skip path).
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Block the walk inside the SECOND collection listing.
	release := make(chan struct{})
	blocked := make(chan struct{})
	cfs := &countingFS{MemFS: h.fs}
	cfs.fail = func(n int) error {
		if n == 2 { // the d/ listing (root is n=1)
			select {
			case <-blocked:
			default:
				close(blocked)
			}
			<-release
		}
		return nil
	}
	h.eng.tr = cfs

	var wg sync.WaitGroup
	var nightlyErr error
	wg.Go(func() {
		// The "nightly" FullRescan holds the gate and blocks in d/'s
		// listing (FullRescan bypasses the token skip -> it lists d/).
		nightlyErr = h.eng.FullRescan(bg())
	})
	<-blocked

	// Concurrent passes from the watcher fallback and the sync loop.
	if err := h.eng.SyncOnce(bg()); !errors.Is(err, ErrSyncInProgress) {
		t.Errorf("concurrent SyncOnce = %v, want ErrSyncInProgress", err)
	}
	if err := h.eng.FullRescan(bg()); !errors.Is(err, ErrSyncInProgress) {
		t.Errorf("concurrent FullRescan = %v, want ErrSyncInProgress", err)
	}
	// And the status never lied about the running pass.
	if st := h.eng.Status(); st.State != StateSyncing {
		t.Errorf("state during the running pass = %q, want %q", st.State, StateSyncing)
	}

	close(release)
	wg.Wait()
	if nightlyErr != nil {
		t.Fatalf("nightly FullRescan failed: %v", nightlyErr)
	}
	// The nightly pass walked the token-matching subtree anyway
	// (its listAll flag was never cleared by the bounced passes).
	rep := h.eng.LastReport()
	if rep.TokenVerified != 0 {
		t.Fatalf("TokenVerified = %d, want 0 (FullRescan bypasses the skip): %+v", rep.TokenVerified, rep)
	}
	if rep.FullscanVerified == 0 {
		t.Fatalf("FullscanVerified = 0, want >0 (every subtree listed): %+v", rep)
	}
	// After completion the gate is open again.
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("post-gate SyncOnce: %v", err)
	}
}

// TestStatusConflictsIsPendingNotPerPass (L38): the IPC conflict number
// matches Conflicts() (the live records) and is NOT reset by a clean
// pass over a settled tree.
func TestStatusConflictsIsPendingNotPerPass(t *testing.T) {
	h := newHarness(t)
	// Surface one real conflict (matrix 3).
	h.fs.PutRaw("doc.txt", "v2")
	h.putLocal("doc.txt", "local edit")
	h.seedDirty("doc.txt", transport.MD5Hex([]byte("v1")))
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("conflict sync: %v", err)
	}
	recs, err := h.eng.Conflicts(bg())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("arrange: conflicts = %+v, want 1", recs)
	}
	if st := h.eng.Status(); st.Conflicts != len(recs) {
		t.Fatalf("Status().Conflicts = %d, Conflicts() = %d - they must agree", st.Conflicts, len(recs))
	}

	// A subsequent CLEAN pass (settled tree) must not reset the number.
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("clean sync: %v", err)
	}
	if strings.Contains("placeholder", "never") {
		t.Fatal("unreachable")
	}
	recs2, err := h.eng.Conflicts(bg())
	if err != nil {
		t.Fatal(err)
	}
	if st := h.eng.Status(); st.Conflicts != len(recs2) || len(recs2) != 1 {
		t.Fatalf("after a clean pass: Status=%d records=%d, want the conflict still pending (1)",
			st.Conflicts, len(recs2))
	}

	// Resolving clears BOTH.
	if err := h.eng.ResolveConflict(bg(), "doc.txt", ResolveKeepRemote, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if st := h.eng.Status(); st.Conflicts != 0 {
		t.Fatalf("after resolve Status.Conflicts = %d, want 0", st.Conflicts)
	}
	_ = time.Now // keep the import honest if the assertions change
}
