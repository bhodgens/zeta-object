package fusefs

import (
	"log"
	"time"
)

// timeAt is a tiny deadline abstraction so the bounded-flush logic is
// testable without sleeping: tests pass an already-expired deadline.
type timeAt struct{ at time.Time }

// deadlineAfter builds a deadline duration d in the future.
func deadlineAfter(d time.Duration) timeAt {
	return timeAt{at: time.Now().Add(d)}
}

// expired builds an already-reached deadline (tests + "no budget").
func expired() timeAt { return timeAt{at: time.Now().Add(-time.Second)} }

// reached reports whether the deadline has passed (flush budget spent).
func (t timeAt) reached() bool { return !time.Now().Before(t.at) }

// discardLog is the no-op logger used when callers pass nil.
func discardLog() *log.Logger { return log.New(discardWriter{}, "", 0) }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
