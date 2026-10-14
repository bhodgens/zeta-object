package scheduler

// w2_m4_syncinprogress_test.go - the W2-M4 regression: ErrSyncInProgress
// is BENIGN in the sync loop. The live pass covers the same index, so a
// coalesced skip must NOT increment the failure count or grow the backoff
// toward the 1h cap - the loop retries after one short interval instead.
// A real failure immediately after must still back off from the base.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
)

// benignRunner returns a FIXED error from every pass (unlike fakeRunner,
// whose err only fires while failNext > 0 - a coalesced-skip scenario is
// one where the gate bounces EVERY attempt) and counts its calls.
type benignRunner struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (b *benignRunner) SyncOnce(context.Context) error   { return b.hit() }
func (b *benignRunner) FullRescan(context.Context) error { return b.hit() }

func (b *benignRunner) hit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.err
}

func (b *benignRunner) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *benignRunner) set(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// waitCalls polls until the runner has been called want times (the loop
// goroutine needs a scheduling beat to record a call).
func waitCalls(t *testing.T, run *benignRunner, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := run.count(); n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: calls = %d, want >= %d", what, run.count(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSyncLoopErrSyncInProgressIsBenign: a runner that bounces with
// ErrSyncInProgress never backs off - consecutive skips retry at
// syncInProgressRetry, and the failure count stays at zero (the next
// REAL failure backs off from the 30s base, not 30s<<n).
func TestSyncLoopErrSyncInProgressIsBenign(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	run := &benignRunner{err: zsync.ErrSyncInProgress}
	never := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, run, clock, func() bool { return false }, never, nil)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("no stop within 2s")
		}
	}()

	waitCalls(t, run, 1, "connect")
	waitTimerSettled(t, clock, "post-skip timer")

	// THE REGRESSION: each skip's retry timer is the SHORT benign
	// interval, not the failure backoff (which would be 30s, then 60s,
	// ... climbing toward the 1h cap). Five consecutive skips all
	// retry after one minute.
	for i := 2; i <= 6; i++ {
		clock.Advance(syncInProgressRetry)
		waitCalls(t, run, i, "benign skip retried")
		waitTimerSettled(t, clock, "post-skip timer")
	}

	// Now a REAL failure: the backoff starts from the BASE (the
	// benign skips incremented no failures), not from 30s<<5. Advance
	// one skip interval so the pending pass returns the real error;
	// the loop then registers a 30s backoff timer (base, failures=1 -
	// had the benign skips been counted, it would be 30s<<6 = 32min
	// and the next pass could not fire within the advance below).
	run.set(errors.New("server down"))
	clock.Advance(syncInProgressRetry)
	waitCalls(t, run, 7, "real failure after benign skips")
	deadline := time.Now().Add(2 * time.Second)
	for {
		clock.mu.Lock()
		short := false
		for ft := range clock.timers {
			if d := ft.deadline.Sub(clock.now); d <= backoffBase+time.Second {
				short = true
			}
		}
		clock.mu.Unlock()
		if short {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("base backoff timer not registered within 2s of the real failure")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(backoffBase + time.Second) // base + jitter headroom
	waitCalls(t, run, 8, "backoff from base, not 30s<<6")
}

// TestSyncLoopRealFailureStillBacksOff: the benign path must not have
// broken the failure backoff - consecutive real failures still double
// (fakeRunner with failNext drives the original cadence; a condensed
// version of TestSyncLoopBackoffOnFailures as the companion pin).
func TestSyncLoopRealFailureStillBacksOff(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	run := &fakeRunner{failNext: 2, err: errors.New("server down")}
	never := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, run, clock, func() bool { return false }, never, nil)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("no stop within 2s")
		}
	}()

	waitSyncs(t, run, 1, "connect") // fail 1 -> 30s
	waitTimerSettled(t, clock, "backoff-30s timer")
	clock.Advance(backoffBase)
	waitSyncs(t, run, 2, "after first backoff") // fail 2 -> 60s
	waitTimerSettled(t, clock, "backoff-60s timer")
	clock.Advance(45 * time.Second)
	if s, _ := run.counts(); s != 2 {
		t.Fatalf("sync fired early at 45s of a 60s backoff: %d", s)
	}
	clock.Advance(15 * time.Second)
	waitSyncs(t, run, 3, "after second backoff")
}
