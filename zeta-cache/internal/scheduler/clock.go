package scheduler

import (
	"context"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// Clock is the injectable clock. Production uses ClockFunc(time.Now);
// tests use a manual fake so no test sleeps in real time.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) <-chan time.Time
}

// ClockFunc adapts time.Now; timers are real-time (production only).
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time { return f() }

// NewTimer implements Clock with the real timer.
func (f ClockFunc) NewTimer(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	time.AfterFunc(d, func() { ch <- time.Now() })
	return ch
}

// FakeClock is the deterministic test clock: Now advances only via
// Advance, and NewTimer fires from Advance (no real time involved). Safe
// for concurrent use.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]struct{}
}

// NewFakeClock starts a fake clock at t.
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t, timers: map[*fakeTimer]struct{}{}}
}

// Now implements Clock.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d, firing every timer whose deadline
// has arrived. The fires are ATOMIC with respect to consumers: all values
// are delivered while the clock lock is held, in DESCENDING deadline order
// (the later-due timer fires first). A consumer parked on the earliest-due
// timer therefore only wakes once every later-due value is already buffered
// in its own channel, so its drain select sees all of them in one pass.
//
// Why descending: firing earliest-first left a window - the consumer of
// the earliest timer could wake and run its drain select BEFORE a later
// timer fired, dropping that value to the next pass. TestSyncLoopNightlyFullRescan
// hit it under -race (the 15m sleep beat the 24h nightly tick, so the
// nightly pass degraded to a plain sync). Timer channels are cap-1 one-
// shots, so the sends never block and are safe to do under the lock; a
// concurrent NewTimer simply waits for the lock and registers a FUTURE
// deadline, unaffected. c.now jumps straight to the target (no per-fire
// back-and-forth), so a racing Now()/NewTimer never observes a
// mid-advance clock.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	var due []*fakeTimer
	for ft := range c.timers {
		// ft.deadline is set once at NewTimer (under this same lock)
		// and never mutated, so it is safe to read here.
		if !ft.deadline.After(target) {
			due = append(due, ft)
		}
	}
	// Later-due fires first (ties are interchangeable: cap-1 one-shot
	// channels, each timer fires exactly once).
	sort.Slice(due, func(i, j int) bool { return due[i].deadline.After(due[j].deadline) })
	c.now = target
	for _, ft := range due {
		delete(c.timers, ft)
		ft.mu.Lock()
		if ft.fired {
			ft.mu.Unlock()
			continue
		}
		ft.fired = true
		ft.mu.Unlock()
		// Fire value is the timer's own deadline (the old code sent
		// c.Now() at fire time, which equalled the deadline because it
		// set c.now first - same value, minus the lock re-acquisition).
		ft.ch <- ft.deadline
	}
	c.mu.Unlock()
}

// Rand gives runSyncLoop a deterministic jitter draw (always 0: base
// backoff delays exactly, so table tests can Advance fixed durations).
// Production ClockFunc does not implement this and uses math/rand/v2.
func (c *FakeClock) Rand() func() float64 { return func() float64 { return 0 } }

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
	fired    bool
	mu       sync.Mutex
}

// NewTimer implements Clock: the returned channel fires when Advance
// crosses the deadline.
func (c *FakeClock) NewTimer(d time.Duration) <-chan time.Time {
	ft := &fakeTimer{ch: make(chan time.Time, 1)}
	c.mu.Lock()
	ft.deadline = c.now.Add(d)
	c.timers[ft] = struct{}{}
	c.mu.Unlock()
	return ft.ch
}

// backoff computes the next retry delay: base * 2^(failures-1), capped at
// max, with deterministic-able jitter in [0, jitterFrac) of the delay.
// failures = number of consecutive failures so far (>= 1). jitter may be
// nil in tests (then rand is used); pass a fixed func for tables.
func backoff(base, max time.Duration, failures int, jitterFrac float64, rnd func() float64) time.Duration {
	if base <= 0 {
		base = 30 * time.Second
	}
	if max <= 0 {
		max = time.Hour
	}
	if failures < 1 {
		failures = 1
	}
	d := base
	for range failures - 1 {
		d *= 2
		if d >= max {
			return max // cap: no jitter past the cap keeps the bound hard
		}
	}
	if d > max {
		d = max
	}
	if jitterFrac <= 0 {
		return d
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	// The jitter must never push the delay PAST the cap: clamp the
	// jittered result, then truncate to whole nanoseconds deterministically
	// from the random draw (0.999*0.25*30s = 7.4925s truncates to 7.4925s
	// within float64 precision - tables use exact ns expectations).
	j := time.Duration(float64(d) * jitterFrac * rnd())
	if d+j > max {
		return max
	}
	return d + j
}

// sleepOrCtx waits for d on clock, returning early on ctx cancel. It is
// the ONLY way the loops wait: tests inject a FakeClock and drive time
// themselves; cancellation is always honored.
func sleepOrCtx(ctx context.Context, clock Clock, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-clock.NewTimer(d):
		return nil
	}
}
