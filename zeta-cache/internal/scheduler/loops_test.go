package scheduler

// loops_test.go - the loop-level tests: sync-loop backoff cadence on the
// FakeClock, pause/resume, and the <2s shutdown-latency assertion (leaf
// constraint). Watcher debounce/coalesce live in watcher_test.go.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

// fakeRunner records SyncOnce/FullRescan calls and fails on demand.
type fakeRunner struct {
	mu       sync.Mutex
	syncs    int
	rescans  int
	failNext int // fail this many upcoming passes
	err      error
}

func (f *fakeRunner) SyncOnce(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
	if f.failNext > 0 {
		f.failNext--
		return f.err
	}
	return nil
}

func (f *fakeRunner) FullRescan(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescans++
	if f.failNext > 0 {
		f.failNext--
		return f.err
	}
	return nil
}

func (f *fakeRunner) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs, f.rescans
}

// waitSyncs polls until syncs >= want (the loop goroutine needs a
// scheduling beat to record a call; nothing here sleeps on the FAKE
// clock, only the poll loop on the real one, bounded).
func waitSyncs(t *testing.T, run *fakeRunner, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, _ := run.counts()
		if s >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: syncs = %d, want >= %d", what, s, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSyncLoopImmediateThenInterval(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	run := &fakeRunner{}
	never := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, run, clock, func() bool { return false }, never, nil)
		close(done)
	}()
	waitSyncs(t, run, 1, "connect")
	clock.Advance(syncInterval)
	waitSyncs(t, run, 2, "after one interval")
	if _, r := run.counts(); r != 0 {
		t.Fatalf("rescans = %d, want 0", r)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sync loop did not stop within 2s of cancel")
	}
}

func TestSyncLoopBackoffOnFailures(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	run := &fakeRunner{failNext: 2, err: errors.New("server down")}
	never := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, run, clock, func() bool { return false }, never, nil)
		close(done)
	}()
	waitSyncs(t, run, 1, "connect") // fail 1 -> backoff 30s
	clock.Advance(30 * time.Second)
	waitSyncs(t, run, 2, "after first backoff") // fail 2 -> backoff 60s
	clock.Advance(45 * time.Second)
	if s, _ := run.counts(); s != 2 {
		t.Fatalf("sync fired early at 45s of a 60s backoff: %d", s)
	}
	clock.Advance(15 * time.Second)
	waitSyncs(t, run, 3, "after second backoff") // success -> reset to 15m
	clock.Advance(syncInterval)
	waitSyncs(t, run, 4, "after success interval")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("no stop within 2s")
	}
}

func TestSyncLoopNightlyFullRescan(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	run := &fakeRunner{}
	never := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, run, clock, func() bool { return false }, never, nil)
		close(done)
	}()
	waitSyncs(t, run, 1, "connect")
	clock.Advance(nightlyPeriod) // the nightly tick beats the 15m sleep
	// wait for the loop goroutine to drain the pass (real-time poll,
	// bounded; the clock itself is fake).
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, r := run.counts()
		if r >= 1 {
			if s != 1 {
				t.Fatalf("nightly pass was a plain sync (syncs=%d rescans=%d), want 0/1", s, r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nightly rescan did not run: syncs=%d rescans=%d", s, r)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("no stop within 2s")
	}
}

func TestSyncLoopPausedHolds(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	run := &fakeRunner{}
	var mu sync.Mutex
	paused := false
	pauseCh := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, run, clock, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return paused
		}, pauseCh, nil)
		close(done)
	}()
	waitSyncs(t, run, 1, "connect")
	mu.Lock()
	paused = true
	mu.Unlock()
	close(pauseCh) // state-change signal
	clock.Advance(10 * syncInterval)
	if s, _ := run.counts(); s != 1 {
		t.Fatalf("syncs while paused = %d, want 1 (held)", s)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("paused loop did not stop within 2s")
	}
}

// --- shutdown latency --------------------------------------------------------

// loopBundle runs the three REAL loop functions against a real store at
// production cadence, for the shutdown-latency assertion.
func TestShutdownLatencyUnder2s(t *testing.T) {
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.Config{
		CacheDir: dir,
		Quota: config.Quota{MaxCacheBytes: 1000, Policy: "lru", HighWaterPct: 90, LowWaterPct: 70, TombstoneDays: 30},
	}
	// REAL clock: the assertion is wall-clock shutdown latency.
	clock := ClockFunc(time.Now)
	eng := NewQuotaEngine(store, cfg, clock, nil)
	eng.SetFreeProbe(func(string) (uint64, error) { return 1 << 40, nil })
	eng.SetBatteryProbe(nil)
	run := &fakeRunner{}

	ctx, cancel := context.WithCancel(context.Background())
	neverPaused := func() bool { return false }
	never := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); runSyncLoop(ctx, run, clock, neverPaused, never, nil) }()
	go func() { defer wg.Done(); runWatcher(ctx, watcher{
		store:    store,
		cacheDir: dir,
		up:       UploadFunc(func(context.Context, string) error { return nil }),
		clock:    clock,
		log:      discardLogger{},
		watchDir: watchAll,
	}, neverPaused, never) }()
	go func() { defer wg.Done(); runEvictionLoop(ctx, evictionLoopState{clock: clock, quota: eng, log: discardLogger{}, paused: neverPaused, wake: never}) }()

	// Give the loops a beat to reach their waits, then cancel and time.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("shutdown took %s, want < 2s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loops did not stop within 2s of cancel")
	}
}
