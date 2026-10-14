package sync

// cursor_test.go — leaf 05's cursor ChangeFeed tests (stub events
// endpoint per the leaf contract): delta feeding with exact cursor
// advancement, redelivery idempotence, loss-counter invalidation ->
// full scan, 503 -> scan-only with a cached negative probe, cursor
// persistence across "restart" (feed reconstruction over the same
// store), and the AT-LEAST-ONCE ack protocol (H11): a page durably
// advances the cursor ONLY on Ack, so a crash between Delta and Ack
// redelivers.
//
// The old at-most-once contract (cursor advanced inside Delta) is gone
// per docs/plans/client-cache-2026-10/05-event-cursor.md 36-38.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

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
// the ACK of that initial full scan plants the cursor at the newest id -
// NOT Delta itself (at-least-once: a crash before the scan commits must
// not strand a cursor over un-applied history).
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
	// H11: Delta must NOT have planted anything yet.
	if _, err := st.MetaGet(ctx, cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("cursor meta after Delta = %v, want absent until Ack", err)
	}
	// The engine's scan succeeded -> Ack plants the cursor.
	f.Ack(ctx)
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
	paths, full, err = deltaThenAck(ctx, f)
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
	if _, _, err := deltaThenAck(ctx, f); err != nil { // establish cursor at 1
		t.Fatalf("establish: %v", err)
	}

	src.pages = []transport.EventHistory{{
		Events: []transport.Event{
			{ID: 2, Op: "rename", Key: "new.txt", OldKey: "old.txt"},
			{ID: 3, Op: "setattr", Key: "ignored.txt"}, // enrichment-only: no path
			{ID: 4, Op: "remove", Key: "gone.txt"},
		},
	}}
	paths, full, err := deltaThenAck(ctx, f)
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
	if _, _, err := deltaThenAck(ctx, f); err != nil {
		t.Fatalf("establish: %v", err)
	}

	// Same event redelivered (server didn't advance; client re-asks at 1).
	page := transport.EventHistory{Events: []transport.Event{
		{ID: 2, Op: "truncate", Key: "a"},
	}}
	src.pages = []transport.EventHistory{page, page} // same page twice
	for i := range 2 {
		paths, full, err := deltaThenAck(ctx, f)
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

// TestCursorFeedCrashBetweenDeltaAndAckRedelivers THE at-least-once pin
// (H11): Delta returns paths but does NOT persist; a "crash" (no Ack,
// feed reconstructed over the same store) replays the page because the
// stored cursor never moved.
func TestCursorFeedCrashBetweenDeltaAndAckRedelivers(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{
		Events: []transport.Event{{ID: 1, Op: "create", Key: "a"}},
	}}}
	st := func() *index.Store {
		s, err := index.Open(t.TempDir() + "/index.db")
		if err != nil {
			t.Fatalf("index open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}()
	ctx := context.Background()

	// Pass 1: establish + ack (cursor=1).
	f1, err := NewCursorFeed(src, st, "bkt", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := deltaThenAck(ctx, f1); err != nil {
		t.Fatalf("establish: %v", err)
	}
	if mustMeta(t, st, cursorMetaKey("bkt")) != "1" {
		t.Fatal("arrange: cursor not at 1")
	}

	// Pass 2: the page arrives, paths are returned - then the process
	// "crashes": NO Ack. The stored cursor must still be 1.
	src.pages = []transport.EventHistory{{
		Events: []transport.Event{
			{ID: 2, Op: "write", Key: "a"},
			{ID: 3, Op: "remove", Key: "b"},
		},
	}}
	f2, err := NewCursorFeed(src, st, "bkt", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	paths, full, err := f2.Delta(ctx)
	if err != nil || full {
		t.Fatalf("pre-crash delta = (%v, %v, %v); want paths", paths, full, err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want 2 mapped keys", paths)
	}
	if got, err := st.MetaGet(ctx, cursorMetaKey("bkt")); err != nil || got != "1" {
		t.Fatalf("cursor after unacked Delta = (%q, %v), want 1 (nothing durable before Ack)", got, err)
	}

	// "Reboot": a fresh feed over the same store MUST redeliver the
	// page (since-id still 1), and only its Ack advances to 3.
	src.pages = []transport.EventHistory{{
		Events: []transport.Event{
			{ID: 2, Op: "write", Key: "a"},
			{ID: 3, Op: "remove", Key: "b"},
		},
	}}
	f3, err := NewCursorFeed(src, st, "bkt", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(src.calls) > 0 && src.calls[len(src.calls)-1] != 1 {
		t.Fatalf("reboot resumed at %v, want since-id=1 (redelivery)", src.calls)
	}
	paths, full, err = deltaThenAck(ctx, f3)
	if err != nil || full {
		t.Fatalf("redelivered delta = (%v, %v, %v)", paths, full, err)
	}
	if len(paths) != 2 {
		t.Fatalf("redelivered paths = %v, want the SAME page again", paths)
	}
	if got := mustMeta(t, st, cursorMetaKey("bkt")); got != "3" {
		t.Fatalf("cursor after Ack = %q, want 3", got)
	}
}

// TestCursorFeedEngineErrorDoesNotAdvanceCursor: when the ENGINE's pass
// fails (or the ctx cancels) it never Acks - the next Delta refetches
// the same page. Driven through a real engine whose scan errors.
func TestCursorFeedEngineErrorDoesNotAdvanceCursor(t *testing.T) {
	h := newHarness(t)
	src := &stubEvents{pages: []transport.EventHistory{{
		Events: []transport.Event{{ID: 1, Op: "create", Key: "seed"}},
	}}}
	cf, err := NewCursorFeed(src, h.store, "bkt", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	h.eng.feed = cf
	if err := h.eng.SyncOnce(bg()); err != nil { // establishes + acks at 1
		t.Fatalf("SyncOnce (establish): %v", err)
	}

	// The pass itself fails (Propfind fault) AFTER delta returned paths.
	h.fs.PutRaw("x.txt", "v")
	src.pages = []transport.EventHistory{{
		Events: []transport.Event{{ID: 2, Op: "create", Key: "x.txt"}},
	}}
	h.fs.FailPropfinds()
	if err := h.eng.SyncOnce(bg()); err == nil {
		t.Fatal("arrange: sync should fail")
	}
	h.fs.ClearFaults()
	if got, merr := h.store.MetaGet(bg(), cursorMetaKey("bkt")); merr != nil || got != "1" {
		t.Fatalf("cursor after failed pass = (%q, %v), want 1 (no Ack on error)", got, merr)
	}
	// The page redelivers on the next (healthy) pass and lands.
	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("recovery sync: %v", err)
	}
	if mustMeta(t, h.store, cursorMetaKey("bkt")) != "2" {
		t.Fatal("redelivered page not committed after recovery")
	}
	if got := h.localBody("x.txt"); got != "v" {
		t.Fatalf("x.txt = %q, want downloaded v", got)
	}
}

// TestCursorFeedLossInvalidatesCursor: an advancing recordsLost counter
// invalidates the cursor -> full rescan, cursor deleted, baseline saved
// on the rescan's Ack.
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
			if _, _, err := deltaThenAck(ctx, f); err != nil {
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
			// The cursor was deleted immediately (the rescan must
			// refetch from scratch if we crash); the baseline is
			// pending until the rescan's Ack.
			if _, err := st.MetaGet(ctx, cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
				t.Fatalf("cursor meta after invalidation = %v, want deleted", err)
			}
			f.Ack(ctx) // the rescan completed
			if got := mustMeta(t, st, lossMetaKey("bkt")); got != fmt.Sprintf("%d:%d", tc.lost, tc.swaps) {
				t.Fatalf("loss meta = %q, want the advanced values", got)
			}
			// The NEXT delta re-establishes (the rescan just completed is
			// the world's truth; its paths are not re-fed).
			src.pages = []transport.EventHistory{{
				RecordsLost: tc.lost, RingSwaps: tc.swaps,
				Events: []transport.Event{{ID: 10, Op: "create", Key: "new.txt"}},
			}}
			paths, full, err = deltaThenAck(ctx, f)
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

// TestCursorFeed503ScanOnlyWithCachedProbe: the contracted 503 answers
// a full scan, persists no cursor, and stamps the negative-probe cache;
// deltas INSIDE the probe interval (L37) skip the fetch entirely.
func TestCursorFeed503ScanOnlyWithCachedProbe(t *testing.T) {
	src := &stubEvents{navail: 1}
	f, st := newTestCursorFeed(t, src)
	ctx := context.Background()

	// First delta: the 503 probe answers, the stamp is written.
	paths, full, err := f.Delta(ctx)
	if err != nil {
		t.Fatalf("delta 0: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("delta 0 = (%v, %v), want scan-only", paths, full)
	}
	if _, err := st.MetaGet(ctx, cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
		t.Fatal("503 must never plant a cursor")
	}
	if !f.ProbeFresh(ctx) {
		t.Fatal("negative probe should be cached fresh after a 503")
	}

	// Second delta inside the interval: NO fetch (the stamp is trusted).
	n := len(src.calls)
	paths, full, err = f.Delta(ctx)
	if err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("delta 1 = (%v, %v), want the honest full scan", paths, full)
	}
	if len(src.calls) != n {
		t.Fatalf("probe fetched again (%d calls), want the cached stamp to skip the fetch", len(src.calls))
	}

	// After the interval expires the probe re-fires (config can change).
	probedAt := f.probeStampedAt(ctx)
	clock := time.Unix(probedAt+int64(probeInterval/time.Second)+1, 0)
	f.now = func() time.Time { return clock }
	paths, full, err = f.Delta(ctx)
	if err != nil {
		t.Fatalf("delta 2: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("delta 2 = (%v, %v), want scan-only after re-probe", paths, full)
	}
	if len(src.calls) != n+1 {
		t.Fatalf("expired stamp must re-probe: calls = %v", src.calls)
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
	if _, _, err := deltaThenAck(ctx, f); err != nil {
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
	paths, full, err := deltaThenAck(ctx, f2)
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
// never be resumed from — permanent scan-only, no cursor planted. The
// guard under test (cursor.go's "un-cursored page" branch) only runs
// AFTER a cursor exists - on a fresh store the first-contact branch
// returns earlier, so this test must establish a cursor first.
func TestCursorFeedUnCursoredPageFallsBackToScan(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{ //nolint:dupl // mirror of the first-contact test, different branch
		Dataset: "tank",
		Events: []transport.Event{
			{ID: 1, Op: "create", Key: "seed.txt"},
		},
	}}}
	f, st := newTestCursorFeed(t, src)
	ctx := context.Background()

	// Establish a cursor (first contact + Ack plants it at id 1).
	if paths, full, err := deltaThenAck(ctx, f); err != nil || !full {
		t.Fatalf("establish = (%v, %v, %v), want full scan", paths, full, err)
	}
	if got := mustMeta(t, st, cursorMetaKey("bkt")); got != "1" {
		t.Fatalf("cursor = %q, want 1 before the un-cursored page", got)
	}

	// NOW serve an un-cursored page: with a live cursor, the
	// un-cursored-page guard must answer the full scan and plant
	// NOTHING (the pending id is 0, so Ack must not advance anything).
	src.pages = []transport.EventHistory{{
		Events: []transport.Event{{ID: 0, Op: "create", Key: "a"}},
	}}
	paths, full, err := deltaThenAck(ctx, f)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("un-cursored page = (%v, %v), want full scan", paths, full)
	}
	if got := mustMeta(t, st, cursorMetaKey("bkt")); got != "1" {
		t.Fatalf("cursor meta = %q after an un-cursored page, want still 1 (no cursor advanced/planted)", got)
	}
}

// TestCursorFeedEngineIntegration: the cursor feed plugs into the leaf-04
// engine — a delta pass over the named paths only, with the ack landing
// after the pass commits.
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
