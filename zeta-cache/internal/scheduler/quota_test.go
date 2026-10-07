package scheduler

// quota_test.go - the quota/eviction engine table tests: high/low-water
// transitions, clean-only eviction under EVERY policy (dirty survives),
// pin filter (SQL column), min-free hard stop, tombstone expiry, battery
// defer with an injected probe. All on the FakeClock.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

// qtest harness: a real index.Store (temp DB) + a cache dir with REAL
// files (eviction unlinks them) + engine with fake clock/probes.
type qtest struct {
	t       *testing.T
	store   *index.Store
	dir     string
	clock   *FakeClock
	eng     *QuotaEngine
	free    uint64
	battery bool
}

func newQTest(t *testing.T, mutate func(*config.Config)) *qtest {
	t.Helper()
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "index.db"), func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.Config{
		CacheDir: dir,
		Quota: config.Quota{
			MaxCacheBytes:   1000,
			MinFreeDevBytes: 0,
			Policy:          "lru",
			HighWaterPct:    90,
			LowWaterPct:     70,
			TombstoneDays:   30,
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	clock := NewFakeClock(time.Unix(1700000000, 0))
	q := &qtest{
		t:     t,
		store: store,
		dir:   dir,
		clock: clock,
	}
	q.eng = NewQuotaEngine(store, cfg, clock, log.New(os.Stderr, "quota-test: ", 0))
	q.eng.SetFreeProbe(func(string) (uint64, error) { return q.free, nil })
	q.eng.SetBatteryProbe(func() bool { return q.battery })
	return q
}

// addFile plants a cache file and its index row. dirty/pinned/deleted
// flags shape the eviction candidate set.
func (q *qtest) addFile(path string, size int64, ageDays int, dirty, pinned, deleted bool) {
	q.t.Helper()
	full := filepath.Join(q.dir, "files", filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		q.t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o600); err != nil {
		q.t.Fatal(err)
	}
	mtime := q.clock.Now().Add(-time.Duration(ageDays) * 24 * time.Hour).Unix()
	r := index.Resource{
		Path: path, ETag: fmt.Sprintf("\"%s\"", path), Mtime: mtime, Size: size,
		Hydrated: !deleted, Dirty: dirty, Deleted: deleted, Pinned: pinned,
		LastAccess: mtime,
	}
	if err := q.store.PutPath(context.Background(), r, "", "test seed"); err != nil {
		q.t.Fatal(err)
	}
}

func (q *qtest) existsOnDisk(path string) bool {
	_, err := os.Stat(filepath.Join(q.dir, "files", filepath.FromSlash(path)))
	return err == nil
}

func (q *qtest) usage() int64 {
	u, err := q.eng.Usage(context.Background())
	if err != nil {
		q.t.Fatal(err)
	}
	return u
}

// --- high/low-water transitions -----------------------------------------

func TestNoEvictionBelowHighWater(t *testing.T) {
	q := newQTest(t, nil) // cap 1000, high 900, low 700
	q.addFile("a", 500, 1, false, false, false)
	n, err := q.eng.RunPass(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("RunPass below high-water = (%d, %v), want (0, nil)", n, err)
	}
	if !q.existsOnDisk("a") {
		t.Fatal("file evicted below high-water")
	}
}

func TestEvictionUntilLowWater(t *testing.T) {
	q := newQTest(t, nil)
	q.addFile("a", 400, 3, false, false, false)
	q.addFile("b", 400, 2, false, false, false)
	q.addFile("c", 200, 1, false, false, false)
	// usage 1000 > high 900: evict LRU first (a, then b) until <= 700.
	n, err := q.eng.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("evicted %d, want 1 (a only: 1000-400=600 <= 700)", n)
	}
	if q.existsOnDisk("a") {
		t.Error("a still on disk after eviction")
	}
	if !q.existsOnDisk("b") || !q.existsOnDisk("c") {
		t.Error("b/c evicted though low-water was reached")
	}
	r, err := q.store.Get(context.Background(), "a")
	if err != nil || r.Hydrated {
		t.Errorf("index row after eviction: (%+v, %v), want hydrated=0", r, err)
	}
	// Journal row landed.
	rows, err := q.store.JournalSince(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range rows {
		if j.Op == index.OpEvict && j.Path == "a" {
			found = true
		}
	}
	if !found {
		t.Error("no evict journal row for a")
	}
}

func TestHighWaterCrossedTriggersOnlyPass(t *testing.T) {
	// Repeated passes below high-water are no-ops (the loop calls
	// RunPass at its cadence; the engine decides).
	q := newQTest(t, nil)
	q.addFile("a", 500, 1, false, false, false)
	for range 3 {
		if n, err := q.eng.RunPass(context.Background()); n != 0 || err != nil {
			t.Fatalf("repeated pass = (%d, %v), want (0, nil)", n, err)
		}
	}
}

// --- clean-only eviction under every policy (dirty survives) ------------

func TestDirtySurvivesUnderEveryPolicy(t *testing.T) {
	for _, policy := range []string{"size", "size+age", "lru"} {
		t.Run(policy, func(t *testing.T) {
			q := newQTest(t, func(c *config.Config) { c.Quota.Policy = policy })
			// Dirty file is the BIGGEST and OLDEST: every ordering puts
			// it first if the filter leaks; it must survive.
			q.addFile("dirty-big", 900, 30, true, false, false)
			q.addFile("clean-small", 500, 1, false, false, false)
			n, err := q.eng.RunPass(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("%s: evicted %d, want 1", policy, n)
			}
			if !q.existsOnDisk("dirty-big") {
				t.Fatalf("%s: DIRTY file was evicted", policy)
			}
			if q.existsOnDisk("clean-small") {
				t.Fatalf("%s: clean candidate not evicted", policy)
			}
			if u := q.usage(); u != 900 {
				t.Errorf("%s: usage after pass = %d, want 900 (dirty held)", policy, u)
			}
		})
	}
}

func TestPolicyOrdering(t *testing.T) {
	t.Run("size biggest first", func(t *testing.T) {
		q := newQTest(t, func(c *config.Config) { c.Quota.Policy = "size" })
		q.addFile("small", 100, 30, false, false, false)
		q.addFile("big", 400, 1, false, false, false)
		// usage 500; force a crossing with a consistent tighter pair
		// (high 40% = 400, low 30% = 300).
		q.eng.cfg.Quota.HighWaterPct = 40
		q.eng.cfg.Quota.LowWaterPct = 30
		n, err := q.eng.RunPass(context.Background())
		if err != nil || n != 1 {
			t.Fatalf("= (%d, %v), want (1, nil)", n, err)
		}
		if q.existsOnDisk("big") {
			t.Error("size policy did not evict the biggest first")
		}
	})
	t.Run("lru least-recently-accessed first", func(t *testing.T) {
		q := newQTest(t, func(c *config.Config) { c.Quota.Policy = "lru" })
		q.addFile("recent", 400, 0, false, false, false) // lastAccess now
		q.addFile("old", 400, 10, false, false, false)   // lastAccess 10d ago
		// usage 800; high 60% = 480, low 50% = 400: evicting the LRU
		// file (old) lands exactly on low-water -> exactly one.
		q.eng.cfg.Quota.HighWaterPct = 60
		q.eng.cfg.Quota.LowWaterPct = 50
		n, err := q.eng.RunPass(context.Background())
		if err != nil || n != 1 {
			t.Fatalf("= (%d, %v), want (1, nil)", n, err)
		}
		if q.existsOnDisk("old") {
			t.Error("lru policy did not evict the least-recently-accessed")
		}
	})
	t.Run("size+age combined score", func(t *testing.T) {
		q := newQTest(t, func(c *config.Config) { c.Quota.Policy = "size+age" })
		// a: big+recent (4MiB-equivalent, age 0); b: small+ancient.
		// Score a = 400/(1MiB) + 64*0 ~= 0.0004; b = 100/(1MiB)+64*1 = 64.1
		q.addFile("big-recent", 400, 0, false, false, false)
		q.addFile("small-ancient", 100, 40, false, false, false)
		// usage 500; high 45% = 450, low 40% = 400: evicting the
		// top-scored candidate (small-ancient: score 64.1 vs ~0)
		// lands exactly on low-water -> exactly one eviction.
		q.eng.cfg.Quota.HighWaterPct = 45
		q.eng.cfg.Quota.LowWaterPct = 40
		n, err := q.eng.RunPass(context.Background())
		if err != nil || n != 1 {
			t.Fatalf("= (%d, %v), want (1, nil)", n, err)
		}
		if q.existsOnDisk("small-ancient") {
			t.Error("size+age did not score age heavily enough to evict the ancient file first")
		}
	})
}

// --- pin filter ----------------------------------------------------------

func TestPinnedFilesNeverEvicted(t *testing.T) {
	q := newQTest(t, func(c *config.Config) { c.Quota.Policy = "size" })
	// usage 950 > high-water 900; the ONLY qualifying candidate is
	// "clean" (pinned-big is filtered in SQL).
	q.addFile("pinned-big", 500, 1, false, true, false)
	q.addFile("clean", 450, 2, false, false, false)
	n, err := q.eng.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("evicted %d, want 1 (only the unpinned candidate)", n)
	}
	if !q.existsOnDisk("pinned-big") {
		t.Fatal("PINNED file was evicted")
	}
	// And the candidate query never lists the pinned row.
	rows, err := q.store.CleanHydratedRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Pinned {
			t.Errorf("pinned row %s in candidate set", r.Path)
		}
	}
}

func TestSetPinFiltersAndUnpinsRestores(t *testing.T) {
	q := newQTest(t, nil)
	q.addFile("a", 500, 1, false, false, false)
	existed, err := q.eng.SetPin(context.Background(), "a", true)
	if err != nil || !existed {
		t.Fatalf("SetPin = (%v, %v), want (true, nil)", existed, err)
	}
	pins, err := q.eng.Pins(context.Background())
	if err != nil || len(pins) != 1 || pins[0] != "a" {
		t.Fatalf("Pins = %v, %v; want [a]", pins, err)
	}
	if _, err := q.eng.SetPin(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	pins, _ = q.eng.Pins(context.Background())
	if len(pins) != 0 {
		t.Fatalf("Pins after unpin = %v, want empty", pins)
	}
}

func TestSyncConfigPinsNewlyPinned(t *testing.T) {
	q := newQTest(t, nil)
	q.eng.cfg.Pins = []string{"proj/", "a"}
	q.addFile("a", 100, 1, false, false, false)
	newly, err := q.eng.SyncConfigPins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(newly) != 1 || newly[0] != "a" {
		t.Fatalf("newly pinned = %v, want [a]", newly)
	}
	// Second run: nothing new.
	newly, err = q.eng.SyncConfigPins(context.Background())
	if err != nil || len(newly) != 0 {
		t.Fatalf("second SyncConfigPins = %v, %v; want empty", newly, err)
	}
}

// --- min-free device hard stop -------------------------------------------

func TestMinFreeHardStopBlocksWritesNotEviction(t *testing.T) {
	q := newQTest(t, func(c *config.Config) {
		c.Quota.MinFreeDevBytes = 500
	})
	q.free = 100 // below min-free
	if q.eng.WriteAllowed(context.Background()) {
		t.Fatal("WriteAllowed = true below min-free, want false (hard stop)")
	}
	// Eviction STILL runs under the hard stop (it frees device space):
	// usage 950 > high-water 900; the LRU victim is b (lastAccess 2d
	// older). Evicting b (450) lands at 500 <= low 700: one eviction.
	q.addFile("a", 500, 1, false, false, false)
	q.addFile("b", 450, 2, false, false, false)
	n, err := q.eng.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("eviction did not run despite high usage under min-free stop")
	}
	if q.existsOnDisk("b") {
		t.Fatal("eviction pass did not free files under min-free stop")
	}
	if !q.existsOnDisk("a") {
		t.Fatal("more files evicted than the low-water target needed")
	}
}

func TestMinFreeProbeFailureFailsOpen(t *testing.T) {
	q := newQTest(t, func(c *config.Config) { c.Quota.MinFreeDevBytes = 10 })
	q.eng.SetFreeProbe(func(string) (uint64, error) { return 0, errors.New("boom") })
	if !q.eng.WriteAllowed(context.Background()) {
		t.Fatal("WriteAllowed with failed probe = false, want fail-open true")
	}
}

func TestNoCapNoEviction(t *testing.T) {
	q := newQTest(t, func(c *config.Config) { c.Quota.MaxCacheBytes = 0 })
	q.addFile("a", 999999, 1, false, false, false)
	if n, err := q.eng.RunPass(context.Background()); n != 0 || err != nil {
		t.Fatalf("RunPass with no cap = (%d, %v), want (0, nil)", n, err)
	}
}

// --- working-set overflow reporting ---------------------------------------

func TestOverflowReportedWhenNothingQualifies(t *testing.T) {
	q := newQTest(t, nil)
	// Only dirty files: usage > high-water but nothing can evict.
	q.addFile("d1", 600, 1, true, false, false)
	q.addFile("d2", 400, 2, true, false, false)
	n, err := q.eng.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("evicted %d dirty files", n)
	}
	if _, _, overflow, _, _ := q.eng.Stats(context.Background()); overflow == 0 {
		t.Fatal("overflow = 0, want reported > 0 (working set over cap)")
	}
}

// --- tombstone expiry ------------------------------------------------------

func TestTombstoneExpiryPrune(t *testing.T) {
	q := newQTest(t, nil)
	// Tombstone aged 40 days (retention 30): pruned. Aged 10: kept.
	q.addFile("gone-old", 0, 40, false, false, true)
	q.addFile("gone-new", 0, 10, false, false, true)
	pruned, err := q.eng.PruneTombstones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != "gone-old" {
		t.Fatalf("pruned = %v, want [gone-old]", pruned)
	}
	if _, err := q.store.Get(context.Background(), "gone-new"); err != nil {
		t.Fatalf("fresh tombstone pruned: %v", err)
	}
	if _, err := q.store.Get(context.Background(), "gone-old"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("expired tombstone survived: %v", err)
	}
}

func TestTombstonesListForRestoreView(t *testing.T) {
	q := newQTest(t, nil)
	q.addFile("deleted-file", 100, 1, false, false, true)
	tombs, err := q.eng.Tombstones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tombs) != 1 || tombs[0].Path != "deleted-file" || !tombs[0].Deleted {
		t.Fatalf("Tombstones = %+v, %v", tombs, err)
	}
}

// --- battery deferral -------------------------------------------------------

func TestBatteryDeferBlocksPass(t *testing.T) {
	q := newQTest(t, nil)
	q.battery = true // discharging
	q.addFile("a", 950, 1, false, false, false)
	n, err := q.eng.RunPass(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("RunPass on battery = (%d, %v), want (0, nil)", n, err)
	}
	if !q.existsOnDisk("a") {
		t.Fatal("evicted while on battery (deferral broken)")
	}
	// Plugged in: the pass runs.
	q.battery = false
	if n, err := q.eng.RunPass(context.Background()); n != 1 || err != nil {
		t.Fatalf("RunPass on AC = (%d, %v), want (1, nil)", n, err)
	}
}

func TestBatteryProbeNotSetOnLinux(t *testing.T) {
	q := newQTest(t, nil)
	// The constructor wires the battery probe on darwin only; simulate
	// the Linux wiring explicitly.
	q.eng.SetBatteryProbe(nil)
	q.addFile("a", 950, 1, false, false, false)
	if n, err := q.eng.RunPass(context.Background()); n != 1 || err != nil {
		t.Fatalf("RunPass without battery probe = (%d, %v), want (1, nil)", n, err)
	}
}

// --- TouchAccessed feeds the LRU policy ------------------------------------

func TestTouchAccessedChangesLRUOrder(t *testing.T) {
	q := newQTest(t, nil)
	q.addFile("a", 650, 10, false, false, false) // oldest
	q.addFile("b", 300, 1, false, false, false)
	// Touch a to NOW: b becomes the LRU victim.
	if err := q.store.TouchAccessed(context.Background(), "a", q.clock.Now()); err != nil {
		t.Fatal(err)
	}
	// usage 950 > high 90% (900); low 70% (700): one eviction of b
	// (300) lands at 650 <= 700.
	if n, err := q.eng.RunPass(context.Background()); n != 1 || err != nil {
		t.Fatalf("= (%d, %v), want (1, nil)", n, err)
	}
	if q.existsOnDisk("b") {
		t.Error("lru evicted the recently-touched file")
	}
}
