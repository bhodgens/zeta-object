package scheduler

// syncloop.go - the periodic sync loop (leaf 07): SyncOnce on connect,
// then every ~15min with exponential backoff + jitter on failures (cap
// 1h); a nightly FullRescan bypasses the collection-token skip. Failures
// log and surface in the engine's status.state=error (the engine owns
// that state; the loop only triggers passes).

import (
	"context"
	"math/rand/v2"
	"time"
)

// runSyncLoop drives the sync cadence until ctx is cancelled. paused is
// polled between passes; a pause holds the NEXT pass, and the running
// pass's ctx is the loop ctx (the engine aborts at path boundaries).
func runSyncLoop(
	ctx context.Context,
	run SyncRunner,
	clock Clock,
	paused func() bool,
	pauseCh <-chan struct{},
	onState func(err error),
) {
	failures := 0
	nextIn := time.Duration(0) // connect: sync immediately
	nightly := clock.NewTimer(nightlyPeriod)
	rnd := rand.Float64
	if f, ok := clock.(interface{ Rand() func() float64 }); ok {
		if r := f.Rand(); r != nil {
			rnd = r
		}
	}

	for {
		if err := sleepOrCtx(ctx, clock, nextIn); err != nil {
			return
		}
		// Hold while paused (wake on pause-state change to re-check).
		for paused() {
			select {
			case <-ctx.Done():
				return
			case <-pauseCh:
			case <-nightly:
				nightly = clock.NewTimer(nightlyPeriod)
			}
		}

		// A nightly tick that arrived during the last sleep/pass is
		// drained FIRST (buffered channel): the pass becomes a full
		// rescan instead of a plain sync.
		var err error
		select {
		case <-nightly:
			nightly = clock.NewTimer(nightlyPeriod)
			// Nightly full rescan: bypass the token skip (the
			// leaf-04 engine re-verifies every subtree).
			err = run.FullRescan(ctx)
		default:
			err = run.SyncOnce(ctx)
		}
		if onState != nil {
			onState(err)
		}
		if err != nil {
			failures++
			// Backoff base 30s, x2 per consecutive failure, +[0,25%)
			// jitter, capped at 1h. Deterministic-cap: jitter applies
			// only below the cap (see backoff()).
			nextIn = backoff(backoffBase, backoffCap, failures, jitterFrac, rnd)
			continue
		}
		failures = 0
		nextIn = syncInterval
	}
}
