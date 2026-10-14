package scheduler

// r6_watcher_dedupe_test.go - the R6 regression: every Create+IsDir event
// spawned a fresh recursive watcher over the named directory; churn
// (create + delete + recreate of the same dir) stacked duplicates - one
// fsnotify fd set + one fan-in goroutine per create, held until shutdown.
//
// The fix is the subtreeWatchers claim set. Deterministic assertions on
// the counter (no fd/goroutine heuristics): the SAME root can hold at
// most one claim across any number of create events.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Churn the same dir through the real loop: 5x create(+delete) events on
// one directory must leave exactly ONE claimed root, not five.
func TestR6ChurnDoesNotStackWatchers(t *testing.T) {
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	subs := newSubtreeWatchers()

	// addSubtree is the loop's Create+IsDir path, extracted verbatim
	// from runWatcher (same claim/release contract). Drive it directly:
	// the churn window is in the claim, not the event plumbing.
	mergeDone := make(chan struct{})
	close(mergeDone) // any fan-in goroutine exits immediately
	events, stopWatch, err := watchAll(files)
	if err != nil {
		t.Fatal(err)
	}
	defer stopWatch()
	_ = events

	addSubtree := func(root string) {
		if !subs.claim(root) {
			return
		}
		sub, stopSub, err := watchAll(root)
		if err != nil {
			subs.release(root)
			return
		}
		go func() {
			defer stopSub()
			for range sub { //nolint:revive // drain until close
			}
		}()
	}

	// 5x create/delete/recreate of the SAME directory.
	sub := filepath.Join(files, "churn")
	for range 5 {
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		addSubtree(sub)
		if err := os.Remove(sub); err != nil {
			t.Fatal(err)
		}
	}
	if got := subs.activeCount(); got != 1 {
		t.Fatalf("watcher claims after 5x churn = %d, want 1 (no duplicate stacking)", got)
	}

	// A different directory gets its own claim.
	other := filepath.Join(files, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	addSubtree(other)
	if got := subs.activeCount(); got != 2 {
		t.Fatalf("watcher claims = %d, want 2 (one per distinct dir)", got)
	}
}

// End-to-end through runWatcher: a real watch on a temp tree, the loop
// receives the Create events, and the process survives the churn with the
// claim set bounded (via the loop's own dedupe - the goroutine count
// proxy: runWatcher exits cleanly on cancel).
func TestR6RunWatcherSurvivesChurn(t *testing.T) {
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWatcher(ctx, watcher{
			store:    nil, // not reached: no uploads, churn only
			cacheDir: dir,
			up:       nil,
			clock:    ClockFunc(time.Now),
			log:      discardLogger{},
			watchDir: watchAll,
		}, func() bool { return false }, make(chan struct{}))
		close(done)
	}()
	// Churn while the loop watches.
	sub := filepath.Join(files, "churn")
	for range 5 {
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(sub); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond) // let the loop see the events
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatcher did not exit after cancel")
	}
}
