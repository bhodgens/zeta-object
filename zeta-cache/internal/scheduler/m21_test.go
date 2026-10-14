package scheduler

// m21_test.go - regression test for the watcher drain guard (M21): the
// old code `if w.drain(ctx, pending) && len(pending) > 0 { continue }`
// was dead (drain empties pending on success) and inverted: a cancel
// mid-drain (drain returns false) fell through to armDebounce(), the
// opposite of the comment. After the fix a cancelled drain must NOT
// re-arm anything: the loop goes back to select, sees ctx.Done, and
// exits - exactly ONE upload attempt total, no re-drain.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

func TestWatcherCancelMidDrainDoesNotRearm(t *testing.T) {
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	// Two dirty rows: the drain picks up the first and BLOCKS in the
	// uploader; the second must never be attempted after cancel.
	for _, p := range []string{"a.txt", "b.txt"} {
		if err := store.PutPath(ctx, index.Resource{Path: p, Size: 1, Hydrated: true, Dirty: true}, "", "seed"); err != nil {
			t.Fatal(err)
		}
	}
	clock := NewFakeClock(time.Unix(0, 0))
	up := &recordingUploader{block: make(chan struct{})}
	wctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWatcher(wctx, watcher{
			store:    store,
			cacheDir: dir,
			up:       up,
			clock:    clock,
			log:      discardLogger{},
			watchDir: watchAll,
		}, func() bool { return false }, make(chan struct{}))
		close(done)
	}()

	// Let the watcher arm its poll timer, then fire it: the poll
	// refresh queues both dirty rows and starts the drain, which
	// blocks in the uploader on the FIRST key.
	deadline := time.Now().Add(2 * time.Second)
	for {
		clock.mu.Lock()
		n := len(clock.timers)
		clock.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never armed its timers")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(pollFallback)

	// Wait until the drain is parked in the blocking uploader.
	deadline = time.Now().Add(2 * time.Second)
	for {
		if n := len(up.uploads()); n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain never started (no upload attempt)")
		}
		time.Sleep(time.Millisecond)
	}

	// Cancel MID-drain, then release the blocked upload. The in-flight
	// attempt finishes; drain sees ctx.Err() and returns false; the
	// fixed guard continues WITHOUT arming the debounce and the loop
	// exits via ctx.Done. (The old guard fell through to armDebounce.)
	cancel()
	close(up.block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not exit within 2s of cancel")
	}
	// No re-arm / re-drain: exactly the one in-flight attempt, never a
	// second (the blocked upload itself returns nil here - a re-drain
	// would attempt b.txt or re-attempt a.txt).
	got := up.uploads()
	if len(got) != 1 {
		t.Fatalf("uploads after cancel mid-drain = %v, want exactly 1 (re-arm fired: M21)", got)
	}
}
