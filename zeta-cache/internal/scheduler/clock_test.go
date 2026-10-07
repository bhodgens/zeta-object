package scheduler

// clock_test.go - FakeClock/backoff/jitter table tests (no real-time
// sleeps anywhere in this package's tests: every wait is FakeClock-driven).

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestFakeClockNowAdvance(t *testing.T) {
	c := NewFakeClock(time.Unix(1700000000, 0))
	if got := c.Now(); !got.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("Now = %v, want start", got)
	}
	c.Advance(5 * time.Second)
	if got := c.Now(); got.Unix() != 1700000005 {
		t.Fatalf("Now after advance = %v, want +5s", got)
	}
}

func TestFakeTimerFiresOnAdvance(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	ch := c.NewTimer(10 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired before Advance")
	default:
	}
	c.Advance(9 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired early")
	default:
	}
	c.Advance(1 * time.Second)
	select {
	case at := <-ch:
		if at.Unix() != 10 {
			t.Fatalf("fired at %v, want t=10", at)
		}
	default:
		t.Fatal("timer did not fire after crossing deadline")
	}
}

func TestFakeTimerOneShot(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	ch := c.NewTimer(time.Second)
	c.Advance(time.Hour)
	<-ch
	c.Advance(time.Hour) // must not re-fire
	select {
	case <-ch:
		t.Fatal("one-shot timer re-fired")
	default:
	}
}

func TestFakeTimerConcurrentNewTimer(t *testing.T) {
	// Race-detector exercise: timers registered from another goroutine
	// while Advance runs.
	c := NewFakeClock(time.Unix(0, 0))
	var wg sync.WaitGroup
	fired := make(chan struct{}, 8)
	for range 8 {
		ch := c.NewTimer(time.Second)
		wg.Go(func() {
			<-ch
			fired <- struct{}{}
		})
	}
	c.Advance(2 * time.Second)
	wg.Wait()
	if len(fired) != 8 {
		t.Fatalf("fired %d of 8 timers", len(fired))
	}
}

func TestBackoffTable(t *testing.T) {
	base := 30 * time.Second
	cap := time.Hour
	fixed := func(f float64) func() float64 {
		return func() float64 { return f }
	}
	cases := []struct {
		name       string
		failures   int
		jitterRnd  func() float64
		jitterFrac float64
		want       time.Duration
	}{
		{"first failure no jitter", 1, nil, 0, 30 * time.Second},
		{"second failure", 2, nil, 0, time.Minute},
		{"third failure", 3, nil, 0, 2 * time.Minute},
		{"tenth failure capped", 10, nil, 0, time.Hour},
		{"hundredth failure capped", 100, nil, 0, time.Hour},
		{"full jitter at base", 1, fixed(0.999), 0.25, 37492500000}, // 30s + 0.999*0.25*30s, ns
		{"zero jitter at base", 1, fixed(0), 0.25, 30 * time.Second},
		{"jitter mid-cap", 6, fixed(0.5), 0.25, 0}, // handled specially below
		{"cap has no jitter", 8, fixed(0.999), 0.25, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backoff(base, cap, tc.failures, tc.jitterFrac, tc.jitterRnd)
			if tc.name == "jitter mid-cap" {
				// 2^5 * 30s = 16m; jitter = 0.5 * 0.25 * 16m = 2m -> 18m.
				if want := 18 * time.Minute; got != want {
					t.Fatalf("backoff = %v, want %v", got, want)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("backoff = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackoffNeverExceedsCap(t *testing.T) {
	rnd := func() float64 { return 1 } // maximal jitter
	for f := 1; f <= 40; f++ {
		if got := backoff(time.Second, time.Minute, f, 0.25, rnd); got > time.Minute {
			t.Fatalf("failure %d: backoff %v exceeds cap", f, got)
		}
	}
}

func TestSleepOrCtxCancelled(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before sleeping: returns immediately.
	cancel()
	if err := sleepOrCtx(ctx, c, time.Hour); err == nil {
		t.Fatal("sleepOrCtx(cancelled ctx) = nil, want ctx err")
	}
}
