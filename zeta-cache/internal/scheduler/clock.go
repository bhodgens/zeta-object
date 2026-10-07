package scheduler

import (
	"context"
	"math/rand/v2"
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

// Advance moves the clock forward, firing every timer whose deadline has
// arrived (in deadline order), before returning.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var (
			best          *fakeTimer
			deadline      time.Time
			foundDeadline bool
		)
		for ft := range c.timers {
			ft.mu.Lock()
			dl := ft.deadline
			ft.mu.Unlock()
			if !dl.After(target) && (!foundDeadline || dl.Before(deadline)) {
				best, deadline, foundDeadline = ft, dl, true
			}
		}
		if best == nil {
			break
		}
		c.now = deadline
		delete(c.timers, best) // one-shot
		c.mu.Unlock()
		best.fire()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

// Rand gives runSyncLoop a deterministic jitter draw (always 0: base
// backoff delays exactly, so table tests can Advance fixed durations).
// Production ClockFunc does not implement this and uses math/rand/v2.
func (c *FakeClock) Rand() func() float64 { return func() float64 { return 0 } }

type fakeTimer struct {
	c        *FakeClock
	deadline time.Time
	ch       chan time.Time
	fired    bool
	mu       sync.Mutex
}

func (t *fakeTimer) fire() {
	t.mu.Lock()
	if t.fired {
		t.mu.Unlock()
		return
	}
	t.fired = true
	t.mu.Unlock()
	t.ch <- t.c.Now()
}

// NewTimer implements Clock: the returned channel fires when Advance
// crosses the deadline.
func (c *FakeClock) NewTimer(d time.Duration) <-chan time.Time {
	ft := &fakeTimer{c: c, ch: make(chan time.Time, 1)}
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
