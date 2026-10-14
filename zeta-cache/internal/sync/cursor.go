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
//
// Durability follows the at-least-once protocol (leaf-05 spec 36-38):
// Delta NEVER persists the cursor - it maps the page, keeps the newest
// id + loss counters pending in memory, and returns the paths. The
// ENGINE calls Ack AFTER the diff-apply pass succeeded; only then does
// the feed durably plant/advance the stored cursor (and the loss
// baseline). A crash between Delta and Ack leaves the stored cursor
// behind, so the next Delta re-fetches from the old id and the page is
// REDIRECTED (correct - the diff no-ops on unchanged ETags). A crash
// after Ack never redelivers - also correct. At-most-once (advancing
// inside Delta) would lose events and is forbidden.
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

	// pending is the page Delta mapped and the engine has NOT yet
	// acked: the newest id + loss counters to commit on Ack, and
	// plantCursor for the first-contact case.
	pendingID    int64
	pendingLoss  [2]uint64
	pendValid    bool
	plantCursor  bool // first contact: Ack plants the cursor at pendingID
	skipProbedAt time.Time
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
//
// Delta NEVER advances the stored cursor (at-least-once, see the type
// doc): the page's newest id + loss counters stay pending until the
// engine calls Ack after its diff-apply commits.
func (f *CursorFeed) Delta(ctx context.Context) ([]string, bool, error) {
	f.pendValid = false
	f.plantCursor = false
	if err := f.loadState(ctx); err != nil {
		f.log.Printf("sync: cursor state load failed (%v); full scan", err)
		return nil, true, nil
	}

	// L37: a still-fresh negative probe (plain bucket) is TRUSTED -
	// skip the fetch and answer the honest full scan directly. The
	// stamp only expires with the wall clock, so no fetch is needed
	// to know the answer will be 503 again.
	if f.ProbeFresh(ctx) {
		f.log.Printf("sync: negative events probe fresh (re-probe in %s); full scan without fetch",
			time.Duration(probeInterval-(time.Duration(f.now().Unix()-f.probeStampedAt(ctx))*time.Second)).Round(time.Second))
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
	// is invalid, full rescan. The baseline update is pending like any
	// other commit: it lands on Ack, so a crash before the rescan
	// finishes redelivers (the counters only ever move forward).
	if f.haveLoss && (hist.RecordsLost > f.lastLoss[0] || hist.RingSwaps > f.lastLoss[1]) {
		f.log.Printf("sync: event loss advanced (recordsLost %d>%d, ringSwaps %d>%d); cursor invalidated, full rescan",
			f.lastLoss[0], hist.RecordsLost, f.lastLoss[1], hist.RingSwaps)
		f.pendValid = true
		f.plantCursor = false
		f.pendingID = 0
		f.pendingLoss = [2]uint64{hist.RecordsLost, hist.RingSwaps}
		f.dropCursorMemory()
		f.clearCursorStorage(ctx)
		return nil, true, nil
	}

	// First contact (no stored cursor): do NOT plant anything here -
	// Ack of the initial full scan plants the cursor at the newest id.
	// The index was just reconciled at daemon start (leaf 03), so
	// everything on disk is already reflected; feeding the full event
	// stream would re-diff the world for nothing. A page that carries
	// no ids (an un-cursored provider) plants nothing: scan-only stays
	// permanent.
	if f.lastCursor == 0 {
		if n := len(hist.Events); n > 0 && hist.Events[n-1].ID > 0 {
			f.pendValid = true
			f.plantCursor = true
			f.pendingID = hist.Events[n-1].ID
			f.pendingLoss = [2]uint64{hist.RecordsLost, hist.RingSwaps}
		}
		return nil, true, nil
	}

	// Un-cursored page (a provider that carries no ids): cannot resume
	// safely — permanent scan-only.
	if len(hist.Events) > 0 && hist.Events[len(hist.Events)-1].ID == 0 {
		return nil, true, nil
	}

	paths := eventsToPaths(hist.Events)
	// Pending, NOT durable: a crash before the engine's Ack redelivers
	// this page (the diff no-ops on unchanged ETags - harmless).
	f.pendValid = true
	f.plantCursor = false
	newest := f.lastCursor
	if n := len(hist.Events); n > 0 && hist.Events[n-1].ID > newest {
		newest = hist.Events[n-1].ID
	}
	f.pendingID = newest
	f.pendingLoss = [2]uint64{hist.RecordsLost, hist.RingSwaps}
	return paths, false, nil
}

// Ack implements Acker: the engine calls it AFTER the pass's diff-apply
// committed, and only then does the feed durably advance the cursor
// (first contact plants it) and the loss baseline. Best-effort: a save
// failure just means the next Delta redelivers (at-least-once).
func (f *CursorFeed) Ack(ctx context.Context) {
	if !f.pendValid {
		return
	}
	if f.plantCursor && f.pendingID > 0 {
		f.log.Printf("sync: cursor established at id %d (acked after the initial full scan)", f.pendingID)
	} else if f.pendingID > f.lastCursor {
		f.lastCursor = f.pendingID
	}
	if err := f.store.MetaSet(ctx, cursorMetaKey(f.bucket), strconv.FormatInt(f.pendingID, 10)); err != nil {
		f.log.Printf("sync: cursor ack save failed (%v); next delta redelivers (at-least-once)", err)
		return
	}
	f.lastCursor = f.pendingID
	if err := f.store.MetaSet(ctx, lossMetaKey(f.bucket),
		fmt.Sprintf("%d:%d", f.pendingLoss[0], f.pendingLoss[1])); err != nil {
		f.log.Printf("sync: cursor loss baseline save failed: %v", err)
	}
	f.lastLoss = f.pendingLoss
	f.haveLoss = true
	f.pendValid = false
	f.plantCursor = false
}

// dropCursorMemory resets the in-memory cursor after a loss invalidation
// (the next delta is a fresh first contact; Ack re-plants).
func (f *CursorFeed) dropCursorMemory() {
	f.lastCursor = 0
}

// clearCursorStorage deletes the stored cursor (loss invalidation).
func (f *CursorFeed) clearCursorStorage(ctx context.Context) {
	if err := f.store.MetaDelete(ctx, cursorMetaKey(f.bucket)); err != nil {
		f.log.Printf("sync: cursor invalidation failed: %v", err)
	}
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
	return nil
}

// Ack (above) is the ONLY writer of the cursor and loss baseline after
// this point: Delta keeps the page pending so a crash between Delta and
// Ack redelivers it (at-least-once).

// probeStampedAt returns the unix second the negative probe was stamped
// at (0 = no stamp). L37: the write in stampProbe has a reader again.
func (f *CursorFeed) probeStampedAt(ctx context.Context) int64 {
	v, err := f.store.MetaGet(ctx, probeMetaKey(f.bucket))
	if err != nil {
		return 0
	}
	stamped, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return stamped
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
