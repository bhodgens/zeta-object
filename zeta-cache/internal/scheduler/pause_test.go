package scheduler

// pause_test.go - regression tests for the pause-broadcast seam (H4) and
// the watcher's poll fallback (M20).
//
// H4 regression shape: with the old close-and-replace wake channel, the
// first pause flip CLOSED the channel every loop held, so each select
// took the always-ready wake case and the loop only reached its timers
// by random tie-break: after Pause()+Resume() the sync loop stopped
// firing on its interval. Here the pass counts must be exact (no
// busy-spin) and the post-resume interval pass must fire.
//
// M20 regression shape: with the old select, the poll timer was only in
// the case set when fsnotify was unavailable; with a healthy watcher the
// poll tick never fired, so a dirty row with no covering event was never
// retried. It must upload on the first poll tick.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// newTestScheduler builds a Scheduler over a real store + memfs-backed
// sync engine (no network) with the FakeClock wired through the
// scheduler, so cadence is timer-driven. The sync pass is observed via
// engine.LastReport() (Start runs the scheduler's OWN engine - the
// standalone runner seam is not consulted).
func newTestScheduler(t *testing.T) (*Scheduler, *zsync.Engine, *FakeClock) {
	t.Helper()
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.Config{
		CacheDir: dir,
		Quota:    config.Quota{MaxCacheBytes: 1000, Policy: "lru", HighWaterPct: 90, LowWaterPct: 70, TombstoneDays: 30},
	}
	clock := NewFakeClock(time.Unix(0, 0))
	eng, err := zsync.NewEngine(zsync.Options{
		Transport: transport.NewMemFS(),
		Store:     store,
		CacheDir:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Store:    store,
		Engine:   eng,
		Config:   cfg,
		Uploader: UploadFunc(func(context.Context, string) error { return nil }),
		Clock:    clock,
		Logger:   discardLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, eng, clock
}

// seedDirtyLocal writes files/<key> with a dirty index row (a FUSE
// write the server has not seen): a sync pass uploads it, bumping
// Report.Uploads.
func seedDirtyLocal(t *testing.T, s *Scheduler, key string) {
	t.Helper()
	fdir := filepath.Join(s.cfg.CacheDir, "files")
	if err := os.MkdirAll(fdir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(fdir, filepath.FromSlash(key))
	if err := os.WriteFile(dst, []byte("body of "+key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutPath(context.Background(), index.Resource{
		Path: key, Size: 1, Hydrated: true, Dirty: true,
	}, "", "test seed dirty"); err != nil {
		t.Fatal(err)
	}
}

// waitReport polls (bounded real time) until engine.Uploads >= want.
func waitReport(t *testing.T, eng *zsync.Engine, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := eng.LastReport().Uploads; got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: report uploads = %d, want >= %d", what, eng.LastReport().Uploads, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSchedulerPauseResumeIntegration is the H4 regression: Pause() then
// Resume() must leave every loop strictly timer-driven (exact pass
// counts, no busy-spin) and shutdown < 2s.
func TestSchedulerPauseResumeIntegration(t *testing.T) {
	s, eng, clock := newTestScheduler(t)
	seedDirtyLocal(t, s, "a.txt")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pause BEFORE Start: the connect pass must be HELD (nothing fires
	// while paused, whatever the timers do - the exact post-resume count
	// below is the busy-spin probe).
	if _, err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	if got := eng.LastReport(); got.Uploads != 0 || got.Scanned != 0 {
		t.Fatalf("passes while paused: report=%+v, want zero (pause hold)", got)
	}

	// Resume: the wake token must release the held connect pass
	// immediately (the loop is parked in the paused-hold select).
	if _, err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	waitReport(t, eng, 1, "resume-released connect pass")
	time.Sleep(20 * time.Millisecond)
	if r := rowClean(t, s, "a.txt"); !r {
		t.Fatal("a.txt still dirty after the released connect pass")
	}

	// Strictly timer-driven after the flip: a sub-tick advance must not
	// fire another pass (busy-spin guard; with the H4 bug the loop burned
	// CPU on an always-ready wake case).
	clock.Advance(100 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if got := eng.LastReport().Scanned; got != 1 {
		t.Fatalf("scanned after 100ms = %d, want 1 (busy-spin)", got)
	}

	// The interval timer still works after the flip: seed dirty, advance
	// one interval, the pass runs and cleans the row.
	seedDirtyLocal(t, s, "b.txt")
	clock.Advance(syncInterval)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !rowClean(t, s, "b.txt") && time.Now().After(deadline) {
			t.Fatal("b.txt never uploaded by the post-resume interval pass (H4: timers starved)")
		}
		if rowClean(t, s, "b.txt") {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Shutdown < 2s: cancel must quiesce the loops quickly.
	start := time.Now()
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = s.Pause() // must stay safe after cancel (no panic/hang)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("post-cancel Pause hung")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown bookkeeping took %s, want < 2s", d)
	}
}

// rowClean reports whether the index row for key exists and is clean.
func rowClean(t *testing.T, s *Scheduler, key string) bool {
	t.Helper()
	r, err := s.store.Get(context.Background(), key)
	if err != nil {
		return false
	}
	return !r.Dirty
}

// TestWatcherPollFiresWithHealthyWatcher is the M20 regression: with a
// healthy fsnotify watcher the poll tick must still fire and refresh the
// dirty set (before the fix the poll timer was selected only when
// events==nil, so a dirty row with no covering event was never retried).
func TestWatcherPollFiresWithHealthyWatcher(t *testing.T) {
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.PutPath(ctx, index.Resource{Path: "seeded.txt", Size: 1, Hydrated: true, Dirty: true}, "", "seed"); err != nil {
		t.Fatal(err)
	}
	clock := NewFakeClock(time.Unix(0, 0))
	up := &recordingUploader{}
	go runWatcher(ctx, watcher{
		store:    store,
		cacheDir: dir,
		up:       up,
		clock:    clock,
		log:      discardLogger{},
		watchDir: watchAll, // REAL fsnotify: healthy watcher
	}, func() bool { return false }, make(chan struct{}))

	// Let the watcher goroutine register its timers, then advance.
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
	// No event will arrive for seeded.txt; the debounce must NOT
	// upload it (it was never queued by an event).
	clock.Advance(debounceDelay + debounceDelay)
	if got := up.uploads(); len(got) != 0 {
		t.Fatalf("debounce uploaded without events: %v", got)
	}

	// The poll tick must fire WITH the healthy watcher and upload it.
	clock.Advance(pollFallback)
	pollDeadline := time.Now().Add(2 * time.Second)
	for {
		if got := up.uploads(); len(got) > 0 {
			if got[0] != "seeded.txt" {
				t.Fatalf("uploads = %v, want [seeded.txt]", got)
			}
			return // PASS
		}
		if time.Now().After(pollDeadline) {
			t.Fatal("poll tick never uploaded the dirty row with a healthy watcher (M20)")
		}
		time.Sleep(time.Millisecond)
	}
}
