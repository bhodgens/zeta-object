package scheduler

// quota.go - the quota/eviction policy engine (leaf 07). No loop or
// transport dependencies: this file is the internal package the master
// plan's open question says COULD be shared with a future FileProvider
// extension, so it stays free of fsnotify/FUSE coupling.
//
// Locked rules (leaf text):
//   - candidates = hydrated AND clean AND NOT pinned, ONE SQL query
//     (index.EvictionPredicate, enforced in SQL via the pinned column)
//   - order by policy: size (biggest first), size+age (combined score),
//     lru (least-recently-accessed first)
//   - evict = delete the cache FILE first, then index.Evict (hydrated=0
//     + journal row in one tx). Never evict dirty; never evict pinned.
//   - high-water (default 90% of cap) starts eviction; eviction targets
//     low-water (default 70%)
//   - working set > cap: evict what qualifies, keep the rest, report the
//     overflow count in status (eviction never fails a hydration)
//   - minFreeDeviceBytes is a HARD STOP ON WRITES (eviction still runs)

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

// QuotaEngine runs eviction passes against the index + cache dir.
type QuotaEngine struct {
	store *index.Store
	cfg   *config.Config
	clock Clock
	log   *log.Logger

	// freeBytes probes free device space on the cache dir's device.
	// Injectable for tests; defaults to statfsDeviceFree.
	freeBytes func(dir string) (uint64, error)
	// deferEviction is the battery probe (macOS). Injectable; nil means
	// never defer (Linux).
	deferEviction func() bool

	mu           sync.Mutex
	evictedTotal int64
	lastEvictAt  time.Time
	overflow     int // candidates needed but unpinnable/unqualifying
	writeBlocked bool // min-free hard stop state (surfaced via status)
}

// NewQuotaEngine builds the engine. cfg must be non-nil (the quota block
// drives everything).
func NewQuotaEngine(store *index.Store, cfg *config.Config, clock Clock, logger *log.Logger) *QuotaEngine {
	if logger == nil {
		logger = discardLog()
	}
	q := &QuotaEngine{
		store:    store,
		cfg:      cfg,
		clock:    clock,
		log:      logger,
		freeBytes: statfsDeviceFree,
	}
	if runtime.GOOS == "darwin" {
		q.deferEviction = BatteryDischarging
	}
	return q
}

// SetFreeProbe overrides the device-free probe (tests).
func (q *QuotaEngine) SetFreeProbe(f func(string) (uint64, error)) { q.freeBytes = f }

// SetBatteryProbe overrides the battery deferral probe (tests). Pass nil
// to disable deferral.
func (q *QuotaEngine) SetBatteryProbe(f func() bool) { q.deferEviction = f }

// Water marks in bytes (0 cap = unlimited: eviction only runs when a cap
// is configured).
func (q *QuotaEngine) highWater() int64 {
	return q.cfg.Quota.MaxCacheBytes * int64(q.cfg.Quota.HighWaterPct) / 100
}

func (q *QuotaEngine) lowWater() int64 {
	return q.cfg.Quota.MaxCacheBytes * int64(q.cfg.Quota.LowWaterPct) / 100
}

// Usage sums the sizes of all hydrated, non-tombstoned rows (the cache
// working set: clean AND dirty AND pinned - pinned rows are part of the
// set; they just never qualify for eviction).
func (q *QuotaEngine) Usage(ctx context.Context) (int64, error) {
	rows, err := q.store.HydratedRows(ctx)
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, r := range rows {
		sum += r.Size
	}
	return sum, nil
}

// WriteAllowed reports whether the min-free hard stop currently blocks
// WRITES (hydration/staging). Eviction is never blocked by it.
func (q *QuotaEngine) WriteAllowed(ctx context.Context) bool {
	if q.cfg.Quota.MinFreeDevBytes <= 0 {
		return true
	}
	free, err := q.freeBytes(q.cfg.CacheDir)
	if err != nil {
		// Probe failure must not wedge the daemon either way: allow
		// writes (the FUSE path reports EIO on real ENOSPC anyway).
		q.log.Printf("quota: device free probe failed: %v (writes allowed)", err)
		return true
	}
	allowed := int64(free) >= q.cfg.Quota.MinFreeDevBytes
	q.mu.Lock()
	q.writeBlocked = !allowed
	q.mu.Unlock()
	return allowed
}

// evictable orders the eviction candidates by policy. Returns the ordered
// candidate rows (clean, hydrated, not pinned, not tombstoned - the SQL
// already enforced that; this is pure ordering).
func (q *QuotaEngine) evictable(ctx context.Context) ([]index.Resource, error) {
	rows, err := q.store.CleanHydratedRows(ctx)
	if err != nil {
		return nil, err
	}
	switch q.cfg.Quota.Policy {
	case "size":
		// Biggest first; tie-break oldest-mtime then path for determinism.
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Size != rows[j].Size {
				return rows[i].Size > rows[j].Size
			}
			if rows[i].Mtime != rows[j].Mtime {
				return rows[i].Mtime < rows[j].Mtime
			}
			return rows[i].Path < rows[j].Path
		})
	case "size+age":
		// Combined score: bigger AND older evict first. Score is
		// size (bytes) weighted with age fraction of the retention
		// window so neither term dominates; deterministic tie-break
		// on path.
		now := q.clock.Now().Unix()
		const ageWindow = int64(30 * 24 * 3600) // 30s-day normalization window
		type scored struct {
			row   index.Resource
			score float64
		}
		scoredRows := make([]scored, len(rows))
		for i, r := range rows {
			age := min(max(now-r.Mtime, 0), ageWindow)
			ageFrac := float64(age) / float64(ageWindow)
			// Size in MiB + age fraction*64: a 1 MiB file counts as
			// much as the full age window.
			sizeMiB := float64(r.Size) / (1024 * 1024)
			scoredRows[i] = scored{row: r, score: sizeMiB + 64*ageFrac}
		}
		sort.Slice(scoredRows, func(i, j int) bool {
			if scoredRows[i].score != scoredRows[j].score {
				return scoredRows[i].score > scoredRows[j].score
			}
			return scoredRows[i].row.Path < scoredRows[j].row.Path
		})
		for i := range scoredRows {
			rows[i] = scoredRows[i].row
		}
	default: // "lru" (the config default): least-recently-accessed first;
		// never-accessed (lastAccess 0) first among those, oldest mtime
		// as the proxy.
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].LastAccess != rows[j].LastAccess {
				return rows[i].LastAccess < rows[j].LastAccess
			}
			if rows[i].Mtime != rows[j].Mtime {
				return rows[i].Mtime < rows[j].Mtime
			}
			return rows[i].Path < rows[j].Path
		})
	}
	return rows, nil
}

// cachePath is the local cache file for key (the leaf-02 layout:
// <cacheDir>/files/<key>).
func (q *QuotaEngine) cachePath(key string) string {
	return filepath.Join(q.cfg.CacheDir, "files", filepath.FromSlash(key))
}

// RunPass executes ONE eviction pass: while usage > low-water, evict the
// next candidate; stop when at/below low-water, candidates exhausted
// (overflow is recorded), or ctx cancelled. Returns the number evicted.
func (q *QuotaEngine) RunPass(ctx context.Context) (int, error) {
	if q.cfg.Quota.MaxCacheBytes <= 0 {
		return 0, nil // no cap configured: nothing to enforce
	}
	// Battery deferral (macOS): on battery AND discharging, wait.
	if q.deferEviction != nil && q.deferEviction() {
		q.log.Printf("quota: eviction deferred (battery discharging)")
		return 0, nil
	}
	usage, err := q.Usage(ctx)
	if err != nil {
		return 0, fmt.Errorf("quota: usage: %w", err)
	}
	if usage <= q.highWater() {
		return 0, nil // below high-water: nothing to do
	}
	candidates, err := q.evictable(ctx)
	if err != nil {
		return 0, fmt.Errorf("quota: candidates: %w", err)
	}
	evicted := 0
	for _, r := range candidates {
		if ctx.Err() != nil {
			return evicted, ctx.Err()
		}
		if usage <= q.lowWater() {
			break // reached the eviction target
		}
		if err := q.evictOne(ctx, r); err != nil {
			// One failed eviction (unlink EPERM, index busy) must not
			// abort the pass; the next candidate is tried.
			q.log.Printf("quota: evict %s: %v (skipped)", r.Path, err)
			continue
		}
		usage -= r.Size
		evicted++
	}
	q.mu.Lock()
	if usage > q.lowWater() {
		// Working set exceeds what eviction can free (dirty/pinned
		// hold the rest): keep the rest, report the overflow.
		q.overflow = int((usage - q.lowWater()) / max64(q.cfg.Quota.MaxCacheBytes/100, 1))
		if q.overflow == 0 {
			q.overflow = 1
		}
	} else {
		q.overflow = 0
	}
	q.evictedTotal += int64(evicted)
	q.lastEvictAt = q.clock.Now()
	q.mu.Unlock()
	if usage > q.lowWater() {
		q.log.Printf("quota: overflow after pass: ~%d files above low-water (dirty/pinned held)", q.overflowSnapshot())
	}
	return evicted, nil
}

func (q *QuotaEngine) overflowSnapshot() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.overflow
}

// evictOne removes one clean file: cache file first (the remote copy is
// authoritative), then the index demotion + journal row in one tx. If the
// process dies between the two, the daemon-start reconcile R2 rule
// repairs the row (hydrated=1 + disk absent -> demote).
func (q *QuotaEngine) evictOne(ctx context.Context, r index.Resource) error {
	if err := os.Remove(q.cachePath(r.Path)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("quota: unlink %s: %w", r.Path, err)
	}
	return q.store.Evict(ctx, r.Path, "eviction: "+q.cfg.Quota.Policy)
}

// PruneTombstones runs the deletion-grace expiry pass and returns the
// pruned paths.
func (q *QuotaEngine) PruneTombstones(ctx context.Context) ([]string, error) {
	retention := time.Duration(q.cfg.Quota.TombstoneDays) * 24 * time.Hour
	pruned, err := q.store.PruneTombstones(ctx, retention, q.clock.Now())
	if err != nil {
		return nil, err
	}
	if len(pruned) > 0 {
		q.log.Printf("quota: pruned %d expired tombstones", len(pruned))
	}
	return pruned, nil
}

// Tombstones lists the deletion-grace rows for the IPC restore view
// (leaf 08 consumes; restore itself is leaf 08).
func (q *QuotaEngine) Tombstones(ctx context.Context) ([]index.Resource, error) {
	paths, err := q.store.TombstonedPaths(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]index.Resource, 0, len(paths))
	for _, p := range paths {
		r, err := q.store.Get(ctx, p)
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Stats returns the quota slice of the IPC status.
func (q *QuotaEngine) Stats(ctx context.Context) (usage, cap int64, overflow int, evicted int64, lastEvict time.Time) {
	usage, _ = q.Usage(ctx)
	cap = q.cfg.Quota.MaxCacheBytes
	q.mu.Lock()
	defer q.mu.Unlock()
	return usage, cap, q.overflow, q.evictedTotal, q.lastEvictAt
}

// Pins implements the pin half of the quota engine: SetPin toggles the
// column (hydration on pin-add is the Scheduler's job - it has the
// transport), Pins lists, and IsPinned is the pin-add detection used by
// the daemon wiring.
func (q *QuotaEngine) SetPin(ctx context.Context, path string, pinned bool) (bool, error) {
	existed, err := q.store.SetPinned(ctx, path, pinned)
	if err != nil {
		return false, err
	}
	_ = q.store.JournalAppend(ctx, index.OpPin, path, fmt.Sprintf("pinned=%v (ipc)", pinned))
	return existed, nil
}

// Pins lists the pinned paths.
func (q *QuotaEngine) Pins(ctx context.Context) ([]string, error) {
	return q.store.PinnedPaths(ctx)
}

// SyncConfigPins applies the config's static pins[] to the index at
// daemon start (config pins always win) and returns paths that became
// pinned NOW (the caller hydrates them). Pins for paths with no index
// row yet are recorded on the row when the row appears (the upsert's
// pin-preserving conflict clause); only rows that EXIST now are reported.
func (q *QuotaEngine) SyncConfigPins(ctx context.Context) ([]string, error) {
	var newly []string
	for _, p := range q.cfg.Pins {
		p = strings.TrimSuffix(p, "/")
		r, err := q.store.Get(ctx, p)
		if errors.Is(err, index.ErrNotFound) {
			continue // not in the index yet; nothing to pin/hydrate now
		} else if err != nil {
			return newly, fmt.Errorf("quota: config pin %s: %w", p, err)
		}
		if r.Pinned {
			continue // already pinned: not newly
		}
		if _, err := q.store.SetPinned(ctx, p, true); err != nil {
			return newly, fmt.Errorf("quota: config pin %s: %w", p, err)
		}
		_ = q.store.JournalAppend(ctx, index.OpPin, p, "pinned=true (config)")
		newly = append(newly, p)
	}
	return newly, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func discardLog() *log.Logger { return log.New(discardWriter{}, "", 0) }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
