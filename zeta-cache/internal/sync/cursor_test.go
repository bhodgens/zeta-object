package sync

// cursor_test.go — leaf 05's cursor ChangeFeed tests (stub events
// endpoint per the leaf contract): delta feeding with exact cursor
// advancement, redelivery idempotence, loss-counter invalidation ->
// full scan, 503 -> permanent scan-only, cursor persistence across
// "restart" (feed reconstruction over the same store).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// stubEvents is the scriptable ?events endpoint.
type stubEvents struct {
	pages   []transport.EventHistory // served in sequence (last repeats)
	navail  int                      // calls answered 503 before pages
	failErr error                    // hard error injected once
	calls   []int64                  // sinceID argument per call
}

func (s *stubEvents) Events(_ context.Context, sinceID int64, maxEvents int) (*transport.EventHistory, error) {
	s.calls = append(s.calls, sinceID)
	if s.failErr != nil {
		err := s.failErr
		s.failErr = nil
		return nil, err
	}
	if s.navail > 0 {
		s.navail--
		return nil, transport.EventsNotAvailableError{}
	}
	if len(s.pages) == 0 {
		return &transport.EventHistory{Dataset: "stub"}, nil
	}
	page := s.pages[0]
	if len(s.pages) > 1 {
		s.pages = s.pages[1:]
	}
	return &page, nil
}

func newTestCursorFeed(t *testing.T, src *stubEvents) (*CursorFeed, *index.Store) {
	t.Helper()
	st, err := index.Open(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatalf("index open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f, err := NewCursorFeed(src, st, "bkt", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewCursorFeed: %v", err)
	}
	return f, st
}

func mustMeta(t *testing.T, st *index.Store, key string) string {
	t.Helper()
	v, err := st.MetaGet(context.Background(), key)
	if err != nil {
		t.Fatalf("meta %s: %v", key, err)
	}
	return v
}

// TestCursorFeedFirstContactEstablishesCursor: the first delta skips the
// historical stream (the startup reconcile already walked the tree) and
// plants the cursor at the newest id.
func TestCursorFeedFirstContactEstablishesCursor(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{
		Dataset: "tank", RecordsLost: 1, RingSwaps: 0,
		Events: []transport.Event{
			{ID: 1, Op: "create", Key: "a.txt"},
			{ID: 2, Op: "create", Key: "b.txt"},
		},
	}}}
	f, st := newTestCursorFeed(t, src)
	ctx := context.Background()

	paths, full, err := f.Delta(ctx)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("first contact = (%v, %v), want full scan with no paths", paths, full)
	}
	if got := mustMeta(t, st, cursorMetaKey("bkt")); got != "2" {
		t.Fatalf("cursor meta = %q, want 2 (newest id)", got)
	}
	if got := mustMeta(t, st, lossMetaKey("bkt")); got != "1:0" {
		t.Fatalf("loss meta = %q, want 1:0", got)
	}

	// Next delta resumes strictly after 2: the source must have been
	// called with since-id=2 and the new event fed as a path.
	src.pages = []transport.EventHistory{{
		Events: []transport.Event{{ID: 3, Op: "create", Key: "c.txt"}},
	}}
	paths, full, err = f.Delta(ctx)
	if err != nil {
		t.Fatalf("Delta 2: %v", err)
	}
	if full {
		t.Fatalf("second delta reported fullScan, want the cursor path")
	}
	if len(paths) != 1 || paths[0] != "c.txt" {
		t.Fatalf("paths = %v, want [c.txt]", paths)
	}
	if len(src.calls) != 2 || src.calls[1] != 2 {
		t.Fatalf("since-id calls = %v, want second call resuming at 2", src.calls)
	}
	if got := mustMeta(t, st, cursorMetaKey("bkt")); got != "3" {
		t.Fatalf("cursor meta = %q, want 3", got)
	}
}

// TestCursorFeedRenameYieldsBothPaths pins the rename rule (old AND new).
func TestCursorFeedRenameYieldsBothPaths(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{
		Events: []transport.Event{
			{ID: 1, Op: "create", Key: "a"},
		},
	}}}
	f, _ := newTestCursorFeed(t, src)
	ctx := context.Background()
	if _, _, err := f.Delta(ctx); err != nil { // establish cursor at 1
		t.Fatalf("establish: %v", err)
	}

	src.pages = []transport.EventHistory{{
		Events: []transport.Event{
			{ID: 2, Op: "rename", Key: "new.txt", OldKey: "old.txt"},
			{ID: 3, Op: "setattr", Key: "ignored.txt"}, // enrichment-only: no path
			{ID: 4, Op: "remove", Key: "gone.txt"},
		},
	}}
	paths, full, err := f.Delta(ctx)
	if err != nil || full {
		t.Fatalf("Delta = (%v, %v, %v); want paths, no full scan", paths, full, err)
	}
	want := map[string]bool{"new.txt": true, "old.txt": true, "gone.txt": true}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want exactly %v", paths, want)
	}
	for _, p := range paths {
		if !want[p] {
			t.Fatalf("unexpected path %q in %v", p, paths)
		}
	}
}

// TestCursorFeedRedeliveryIdempotent: a repeated event maps to the same
// path again (the diff engine no-ops on unchanged ETags) and the cursor
// never moves backwards.
func TestCursorFeedRedeliveryIdempotent(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{
		Events: []transport.Event{{ID: 1, Op: "create", Key: "a"}},
	}}}
	f, st := newTestCursorFeed(t, src)
	ctx := context.Background()
	if _, _, err := f.Delta(ctx); err != nil {
		t.Fatalf("establish: %v", err)
	}

	// Same event redelivered (server didn't advance; client re-asks at 1).
	page := transport.EventHistory{Events: []transport.Event{
		{ID: 2, Op: "truncate", Key: "a"},
	}}
	src.pages = []transport.EventHistory{page, page} // same page twice
	for i := range 2 {
		paths, full, err := f.Delta(ctx)
		if err != nil || full {
			t.Fatalf("delta %d = (%v, %v, %v)", i, paths, full, err)
		}
		if len(paths) != 1 || paths[0] != "a" {
			t.Fatalf("delta %d paths = %v, want [a] (redelivery harmless)", i, paths)
		}
	}
	if got := mustMeta(t, st, cursorMetaKey("bkt")); got != "2" {
		t.Fatalf("cursor = %q, want 2", got)
	}
}

// TestCursorFeedLossInvalidatesCursor: an advancing recordsLost counter
// invalidates the cursor -> full rescan, cursor deleted, baseline saved.
func TestCursorFeedLossInvalidatesCursor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lost   uint64
		swaps  uint64
		broken string
	}{
		{"recordsLost advanced", 6, 1, "recordsLost"},
		{"ringSwaps advanced", 5, 4, "ringSwaps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &stubEvents{pages: []transport.EventHistory{{
				RecordsLost: 5, RingSwaps: 1,
				Events: []transport.Event{{ID: 9, Op: "create", Key: "a"}},
			}}}
			f, st := newTestCursorFeed(t, src)
			ctx := context.Background()
			if _, _, err := f.Delta(ctx); err != nil {
				t.Fatalf("establish: %v", err)
			}
			if mustMeta(t, st, cursorMetaKey("bkt")) != "9" {
				t.Fatal("cursor not established")
			}

			src.pages = []transport.EventHistory{{
				RecordsLost: tc.lost, RingSwaps: tc.swaps,
				Events: []transport.Event{{ID: 10, Op: "create", Key: "new.txt"}},
			}}
			paths, full, err := f.Delta(ctx)
			if err != nil {
				t.Fatalf("Delta: %v", err)
			}
			if !full || paths != nil {
				t.Fatalf("%s: delta = (%v, %v), want authoritative full scan", tc.broken, paths, full)
			}
			// The cursor was deleted; the loss baseline advanced.
			if _, err := st.MetaGet(ctx, cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
				t.Fatalf("cursor meta after invalidation = %v, want deleted", err)
			}
			if got := mustMeta(t, st, lossMetaKey("bkt")); got != fmt.Sprintf("%d:%d", tc.lost, tc.swaps) {
				t.Fatalf("loss meta = %q, want the advanced values", got)
			}
			// The NEXT delta re-establishes (the rescan just completed is
			// the world's truth; its paths are not re-fed).
			src.pages = []transport.EventHistory{{
				RecordsLost: tc.lost, RingSwaps: tc.swaps,
				Events: []transport.Event{{ID: 10, Op: "create", Key: "new.txt"}},
			}}
			paths, full, err = f.Delta(ctx)
			if err != nil {
				t.Fatalf("re-establish Delta: %v", err)
			}
			if !full || paths != nil {
				t.Fatalf("re-establish = (%v, %v), want skip-and-replant", paths, full)
			}
			if mustMeta(t, st, cursorMetaKey("bkt")) != "10" {
				t.Fatal("cursor not re-established at the newest id")
			}
		})
	}
}

// TestCursorFeed503PermanentScanOnly: the contracted 503 answers a full
// scan every time (the permanent non-ZFS behavior) and persists nothing.
func TestCursorFeed503PermanentScanOnly(t *testing.T) {
	src := &stubEvents{navail: 3}
	f, st := newTestCursorFeed(t, src)
	ctx := context.Background()
	for i := range 3 {
		paths, full, err := f.Delta(ctx)
		if err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
		if !full || paths != nil {
			t.Fatalf("delta %d = (%v, %v), want scan-only", i, paths, full)
		}
	}
	if len(src.calls) != 3 {
		t.Fatalf("probe called %d times, want every delta (re-probe, never assume)", len(src.calls))
	}
	if _, err := st.MetaGet(ctx, cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
		t.Fatal("503 must never plant a cursor")
	}
	if !f.ProbeFresh(ctx) {
		t.Fatal("negative probe should be cached fresh after a 503")
	}
}

// TestCursorFeedHardErrorDegradesToScan: a transport failure is not a
// 503 — the feed degrades to the scan and surfaces NO error (the engine
// treats fullScan as authoritative).
func TestCursorFeedHardErrorDegradesToScan(t *testing.T) {
	src := &stubEvents{failErr: errors.New("connection reset")}
	f, _ := newTestCursorFeed(t, src)
	paths, full, err := f.Delta(context.Background())
	if err != nil {
		t.Fatalf("Delta must degrade, not error: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("hard error = (%v, %v), want full scan", paths, full)
	}
}

// TestCursorFeedCursorPersistsAcrossRestart: state lives in the store,
// not the feed struct — a reconstructed feed resumes at the stored id.
func TestCursorFeedCursorPersistsAcrossRestart(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{
		RecordsLost: 2, RingSwaps: 0,
		Events: []transport.Event{{ID: 7, Op: "create", Key: "a"}},
	}}}
	f, st := newTestCursorFeed(t, src)
	ctx := context.Background()
	if _, _, err := f.Delta(ctx); err != nil {
		t.Fatalf("establish: %v", err)
	}

	// "Restart": a fresh feed over the SAME store.
	f2, err := NewCursorFeed(src, st, "bkt", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	src.pages = []transport.EventHistory{{
		RecordsLost: 2, RingSwaps: 0,
		Events: []transport.Event{{ID: 8, Op: "remove", Key: "b"}},
	}}
	paths, full, err := f2.Delta(ctx)
	if err != nil || full {
		t.Fatalf("post-restart delta = (%v, %v, %v)", paths, full, err)
	}
	if len(paths) != 1 || paths[0] != "b" {
		t.Fatalf("post-restart paths = %v, want [b] (no replay of id<=7)", paths)
	}
	if len(src.calls) < 2 || src.calls[len(src.calls)-1] != 7 {
		t.Fatalf("restarted feed resumed at %v, want since-id=7", src.calls)
	}
}

// TestCursorFeedUnCursoredPageFallsBackToScan: events without ids can
// never be resumed from — permanent scan-only, no cursor planted.
func TestCursorFeedUnCursoredPageFallsBackToScan(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{
		Events: []transport.Event{{ID: 0, Op: "create", Key: "a"}},
	}}}
	f, st := newTestCursorFeed(t, src)
	paths, full, err := f.Delta(context.Background())
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("un-cursored page = (%v, %v), want full scan", paths, full)
	}
	if _, err := st.MetaGet(context.Background(), cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
		t.Fatal("un-cursored page must never plant a cursor")
	}
}

// TestCursorFeedEngineIntegration: the cursor feed plugs into the leaf-04
// engine — a delta pass over the named paths only.
func TestCursorFeedEngineIntegration(t *testing.T) {
	h := newHarness(t) // engine harness: memfs transport + store
	src := &stubEvents{pages: []transport.EventHistory{{
		Events: []transport.Event{{ID: 1, Op: "create", Key: "seed"}},
	}}}
	cf, err := NewCursorFeed(src, h.store, "bkt", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewCursorFeed: %v", err)
	}
	h.eng.feed = cf
	if err := h.eng.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce (establish): %v", err)
	}

	// Server gains a file (PutRaw is the harness's server-side writer);
	// the cursor names exactly that path.
	h.fs.PutRaw("changed.txt", "server body")
	src.pages = []transport.EventHistory{{
		Events: []transport.Event{{ID: 2, Op: "create", Key: "changed.txt"}},
	}}
	if err := h.eng.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce (delta): %v", err)
	}
	rep := h.eng.LastReport()
	if rep.Scanned != 1 {
		t.Fatalf("delta pass scanned %d paths, want exactly the cursor's 1 (report %+v)", rep.Scanned, rep)
	}
	if mustMeta(t, h.store, cursorMetaKey("bkt")) != "2" {
		t.Fatal("cursor did not advance after the engine pass")
	}
}

// TestEventsToPathsUnit pins the op mapping table directly.
func TestEventsToPathsUnit(t *testing.T) {
	paths := eventsToPaths([]transport.Event{
		{ID: 1, Op: "create", Key: "a"},
		{ID: 2, Op: "write", Key: "a"},
		{ID: 3, Op: "truncate", Key: "b"},
		{ID: 4, Op: "rename", Key: "d-new", OldKey: "d-old"},
		{ID: 5, Op: "link", Key: "hard"},
		{ID: 6, Op: "symlink", Key: "sym"},
		{ID: 7, Op: "setattr", Key: "meta-only"},
		{ID: 8, Op: "create", Key: "dir/"},
		{ID: 9, Op: "remove", Key: ""},
	})
	got := map[string]bool{}
	for _, p := range paths {
		if got[p] {
			t.Fatalf("duplicate path %q in %v", p, paths)
		}
		got[p] = true
	}
	for _, want := range []string{"a", "b", "d-new", "d-old"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, paths)
		}
	}
	for _, banned := range []string{"hard", "sym", "meta-only", "dir/", ""} {
		if got[banned] {
			t.Fatalf("banned path %q surfaced in %v", banned, paths)
		}
	}
}

// TestCursorMetaKeyNamesPins the meta-key layout (leaf-05-owned keys;
// the sync_adapter and other leaves must never collide).
func TestCursorMetaKeyNames(t *testing.T) {
	if cursorMetaKey("photos") != "cursor:photos" {
		t.Fatal("cursor key layout changed")
	}
	if lossMetaKey("photos") != "cursor-loss:photos" {
		t.Fatal("loss key layout changed")
	}
	if probeMetaKey("photos") != "cursor-probe:photos" {
		t.Fatal("probe key layout changed")
	}
}
