package sync

// cursor.go — leaf 05's event-cursor ChangeFeed (client-cache
// docs/plans/client-cache-2026-10/05-event-cursor.md), consuming the
// gateway's since-id pass-through (zeta-object#15). It replaces the
// always-fullScan default ONLY where the probe proves the bucket carries
// a zmetad events provider; scan-only stays the PERMANENT behavior for
// non-ZFS buckets (master decision 7: the cursor is an optimization,
// never a dependency — no zeta-cache feature may REQUIRE zfs-metadata).
//
// Contract implemented here (leaf 05):
//   - Capability discovery FIRST: every Delta probes via ?events. A 503
//     (transport.EventsNotAvailable) = plain bucket -> full scan, cursor
//     disabled. The probe result is cached with a re-probe interval
//     (server config can change between runs) — a negative answer is
//     retried after probeInterval, a positive one is trusted until a
//     probe fails.
//   - Cursor state: index meta key cursor:<bucket> = last-seen event id
//     (the wire `id` from #15 — a timestamp cursor is FORBIDDEN:
//     gethrtime resets on reboot, second-resolution captured_at loses
//     same-second events). Cursor keys are leaf-05-owned; the engine
//     never reads them.
//   - Delta: fetch ?events&since-id=N (bounded), map event keys ->
//     changed paths (create/rename/remove/truncate; rename yields BOTH
//     old and new path), fullScan=false. The stored cursor advances ONLY
//     after the page is fully mapped (at-least-once redelivery on crash
//     is correct; at-most-once is not).
//   - Loss honesty: envelope recordsLost/ringSwaps counters — either
//     advancing vs the last-seen values INVALIDATES the cursor: full
//     rescan, cursor reset to the newest id AFTER the rescan completes
//     (i.e. on the NEXT delta, since the engine owns the scan). The ring
//     is a bounded kernel buffer: offline-past-retention falls into the
//     same loss rule.
//   - Events are change DETECTION only: conflicts stay file-ETag based
//     (leaf 04 rules). Redelivery is harmless: the diff engine sees
//     unchanged ETags and no-ops.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// Cursor meta keys (leaf-05-owned, one per bucket; the daemon serves one
// bucket per mount, but the key carries the bucket name so a relocated
// cache dir cannot resurrect another bucket's cursor).
func cursorMetaKey(bucket string) string { return "cursor:" + bucket }

// lossMetaKey is the last-seen loss counters, "recordsLost:ringSwaps".
func lossMetaKey(bucket string) string { return "cursor-loss:" + bucket }

// probeMetaKey records when the cursor was last proven UNAVAILABLE (the
// negative probe cache; re-probed after probeInterval).
func probeMetaKey(bucket string) string { return "cursor-probe:" + bucket }

// probeInterval is how long a negative probe (plain bucket) is trusted.
// Server config can change (zmetad attached later), so "no" expires.
const probeInterval = 15 * time.Minute

// eventsPageLimit bounds one delta fetch (the server caps at 10000).
const eventsPageLimit = 10000

// EventsSource is the wire seam CursorFeed consumes: the transport
// client's Events method (real client) or a stub in tests.
type EventsSource interface {
	Events(ctx context.Context, sinceID int64, maxEvents int) (*transport.EventHistory, error)
}

// CursorFeed is the zmetad-backed ChangeFeed. Construct with NewCursorFeed.
type CursorFeed struct {
	src    EventsSource
	store  *index.Store
	bucket string
	now    func() time.Time
	log    *log.Logger

	// lastCursor/lastLoss cache the persisted state in memory between
	// Delta calls (the meta table stays authoritative).
	lastCursor int64
	lastLoss   [2]uint64
	haveLoss   bool
}

// NewCursorFeed validates options and returns a ready feed. The cursor
// state loads lazily per Delta (the store may not be open yet at
// construction).
func NewCursorFeed(src EventsSource, store *index.Store, bucket string, logger *log.Logger) (*CursorFeed, error) {
	if src == nil {
		return nil, fmt.Errorf("sync: CursorFeed requires an EventsSource")
	}
	if store == nil {
		return nil, fmt.Errorf("sync: CursorFeed requires the index Store")
	}
	if bucket == "" {
		return nil, fmt.Errorf("sync: CursorFeed requires the bucket name")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &CursorFeed{src: src, store: store, bucket: bucket, now: time.Now, log: logger}, nil
}

// Delta implements ChangeFeed. Per the seam contract: (paths, false, nil)
// when the cursor path is live, (nil, true, nil) whenever the full scan
// is the honest answer (no provider, loss counters advanced, un-cursored
// page). A nil error with fullScan=true is authoritative for the engine.
func (f *CursorFeed) Delta(ctx context.Context) ([]string, bool, error) {
	if err := f.loadState(ctx); err != nil {
		f.log.Printf("sync: cursor state load failed (%v); full scan", err)
		return nil, true, nil
	}

	hist, err := f.src.Events(ctx, f.lastCursor, eventsPageLimit)
	if err != nil {
		if errors.As(err, &transport.EventsNotAvailableError{}) {
			// Plain bucket: cursor disabled, PERMANENTLY scan-only for
			// this bucket until the probe goes positive again. Cache
			// the negative probe with a re-probe interval.
			f.stampProbe(ctx)
			return nil, true, nil
		}
		// Any other fetch failure: degrade to the scan (the engine
		// treats feed errors as "on any doubt" already, but returning
		// the explicit full scan keeps the contract local).
		f.log.Printf("sync: events fetch failed (%v); full scan", err)
		return nil, true, nil
	}

	// Loss honesty FIRST: the counters describe the WHOLE history, not
	// the page — compare against the last-seen values regardless of the
	// cursor. Either advancing means records were DROPPED upstream (the
	// ring is bounded; offline-past-retention shows up here): the cursor
	// is invalid, full rescan.
	if f.haveLoss && (hist.RecordsLost > f.lastLoss[0] || hist.RingSwaps > f.lastLoss[1]) {
		f.log.Printf("sync: event loss advanced (recordsLost %d>%d, ringSwaps %d>%d); cursor invalidated, full rescan",
			f.lastLoss[0], hist.RecordsLost, f.lastLoss[1], hist.RingSwaps)
		f.invalidate(ctx, hist)
		return nil, true, nil
	}

	// First contact (no stored cursor): establish the cursor at the
	// newest id WITHOUT feeding paths — the index was just reconciled at
	// daemon start (leaf 03), so everything on disk is already reflected;
	// feeding the full event stream would re-diff the world for nothing.
	// A page that carries no ids (an un-cursored provider) plants
	// nothing: scan-only stays permanent.
	if f.lastCursor == 0 {
		if n := len(hist.Events); n > 0 && hist.Events[n-1].ID > 0 {
			f.saveState(ctx, hist)
			f.log.Printf("sync: cursor established at id %d (%d historical events skipped)", hist.Events[n-1].ID, n)
		}
		return nil, true, nil
	}

	// Un-cursored page (a provider that carries no ids): cannot resume
	// safely — permanent scan-only.
	if len(hist.Events) > 0 && hist.Events[len(hist.Events)-1].ID == 0 {
		return nil, true, nil
	}

	paths := eventsToPaths(hist.Events)
	f.saveState(ctx, hist)
	return paths, false, nil
}

// eventsToPaths maps one events page onto changed FILE keys per the leaf-05
// contract: create/rename/remove/truncate surface; rename yields BOTH the
// old and the new path; enrichment-only ops (link/symlink/setattr) map to
// nothing (their ETags are the rename/truncate's business). Dir-shaped
// keys (trailing slash) are skipped — the engine diffs file keys only.
func eventsToPaths(events []transport.Event) []string {
	seen := map[string]bool{}
	var paths []string
	add := func(k string) {
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		paths = append(paths, k)
	}
	for _, e := range events {
		switch e.Op {
		case "create", "remove", "truncate", "write":
			add(e.Key)
		case "rename":
			add(e.OldKey) // both endpoints: a rename moves content
			add(e.Key)
		default:
			// link/symlink/setattr: no path to re-diff in v1.
		}
	}
	// Dir-shaped keys (trailing slash) are collection noise: the engine
	// diffs FILE keys only (its own dir rows carry the trailing slash
	// convention, but the feed's contract names file keys).
	out := paths[:0]
	for _, p := range paths {
		if p != "" && p[len(p)-1] != '/' {
			out = append(out, p)
		}
	}
	return out
}

// loadState reads cursor + loss + probe-cache from the meta table. A
// missing key is a fresh state (cursor 0, no loss baseline), not an error.
func (f *CursorFeed) loadState(ctx context.Context) error {
	f.lastCursor = 0
	f.haveLoss = false
	if v, err := f.store.MetaGet(ctx, cursorMetaKey(f.bucket)); err == nil {
		n, parseErr := strconv.ParseInt(v, 10, 64)
		if parseErr != nil {
			return fmt.Errorf("cursor meta %q: %w", v, parseErr)
		}
		f.lastCursor = n
	} else if !errors.Is(err, index.ErrNotFound) {
		return err
	}
	if v, err := f.store.MetaGet(ctx, lossMetaKey(f.bucket)); err == nil {
		var rl, rs uint64
		if _, err := fmt.Sscanf(v, "%d:%d", &rl, &rs); err == nil {
			f.lastLoss = [2]uint64{rl, rs}
			f.haveLoss = true
		}
	} else if !errors.Is(err, index.ErrNotFound) {
		return err
	}
	// Negative-probe cache: a recent 503 means the full scan below is
	// wasted anyway — but Delta must still return the honest fullScan,
	// so we do NOT short-circuit here; the cache only exists so future
	// work (skipping the probe inside its interval) can trust it.
	return nil
}

// saveState persists the newest id and the loss counters read off this
// page. Called only after the page's paths are fully mapped (the
// at-least-once rule: a crash before this point redelivers the page; a
// crash after it never loses one).
func (f *CursorFeed) saveState(ctx context.Context, hist *transport.EventHistory) {
	newest := f.lastCursor
	if n := len(hist.Events); n > 0 && hist.Events[n-1].ID > newest {
		newest = hist.Events[n-1].ID
	}
	if err := f.store.MetaSet(ctx, cursorMetaKey(f.bucket), strconv.FormatInt(newest, 10)); err != nil {
		f.log.Printf("sync: cursor save failed (%v); next delta redelivers (at-least-once)", err)
		return
	}
	f.lastCursor = newest
	if err := f.store.MetaSet(ctx, lossMetaKey(f.bucket),
		fmt.Sprintf("%d:%d", hist.RecordsLost, hist.RingSwaps)); err != nil {
		f.log.Printf("sync: cursor loss baseline save failed: %v", err)
	}
	f.lastLoss = [2]uint64{hist.RecordsLost, hist.RingSwaps}
	f.haveLoss = true
}

// invalidate drops the cursor after a loss advance: the rescan the engine
// now runs is authoritative; the NEXT delta re-establishes the cursor at
// the (already advanced) newest id without feeding its paths.
func (f *CursorFeed) invalidate(ctx context.Context, hist *transport.EventHistory) {
	if err := f.store.MetaDelete(ctx, cursorMetaKey(f.bucket)); err != nil {
		f.log.Printf("sync: cursor invalidation failed: %v", err)
	}
	f.lastCursor = 0
	// The loss baseline still updates: one invalidation per advance, not
	// one per delta until the end of time.
	if err := f.store.MetaSet(ctx, lossMetaKey(f.bucket),
		fmt.Sprintf("%d:%d", hist.RecordsLost, hist.RingSwaps)); err != nil {
		f.log.Printf("sync: cursor loss baseline save failed: %v", err)
	}
	f.lastLoss = [2]uint64{hist.RecordsLost, hist.RingSwaps}
}

// stampProbe records "probe negative at <now>" (the re-probe interval's
// anchor). Best-effort: losing it only costs one extra probe.
func (f *CursorFeed) stampProbe(ctx context.Context) {
	_ = f.store.MetaSet(ctx, probeMetaKey(f.bucket),
		strconv.FormatInt(f.now().Unix(), 10))
}

// ProbeFresh reports whether a negative probe is still inside its cache
// interval (exported for tests; future work may skip the fetch inside it).
func (f *CursorFeed) ProbeFresh(ctx context.Context) bool {
	v, err := f.store.MetaGet(ctx, probeMetaKey(f.bucket))
	if err != nil {
		return false
	}
	stamped, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false
	}
	return f.now().Unix()-stamped < int64(probeInterval/time.Second)
}

var _ ChangeFeed = (*CursorFeed)(nil)
