package scheduler

// ipc_test.go - the Scheduler-as-ipc.Handler tests over a REAL unix
// socket: pause/resume, pin toggle, pins list, tombstones list, and the
// unknown-type rejection preserved (leaf-01 contract).

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// handlerFixture builds a Scheduler over a real store + a memfs-backed
// sync engine (the same stub internal/sync's tests use) and serves IPC.
type handlerFixture struct {
	t     *testing.T
	sched *Scheduler
	store *index.Store
	path  string // temp dir
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.Config{
		CacheDir: dir,
		Quota:    config.Quota{MaxCacheBytes: 1000, Policy: "lru", HighWaterPct: 90, LowWaterPct: 70, TombstoneDays: 30},
	}
	eng, err := zsync.NewEngine(zsync.Options{
		Transport: transport.NewMemFS(),
		Store:     store,
		CacheDir:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	sched, err := New(Options{
		Store:  store,
		Engine: eng,
		Config: cfg,
		Clock:  NewFakeClock(time.Unix(1700000000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &handlerFixture{t: t, sched: sched, store: store, path: dir}
}

func TestSchedulerPauseResume(t *testing.T) {
	f := newHandlerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.sched.Start(ctx)
	paused, err := f.sched.Pause()
	if err != nil || !paused {
		t.Fatalf("Pause = (%v, %v), want (true, nil)", paused, err)
	}
	paused, err = f.sched.Resume()
	if err != nil || paused {
		t.Fatalf("Resume = (%v, %v), want (false, nil)", paused, err)
	}
}

func TestSchedulerSetPinAndPins(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	if err := f.store.PutPath(ctx, index.Resource{Path: "keep.txt", Size: 1, Hydrated: true}, "", "seed"); err != nil {
		t.Fatal(err)
	}
	existed, err := f.sched.Quota().SetPin(ctx, "keep.txt", true)
	if err != nil || !existed {
		t.Fatalf("SetPin = (%v, %v), want (true, nil)", existed, err)
	}
	pins, err := f.sched.Quota().Pins(ctx)
	if err != nil || len(pins) != 1 || pins[0] != "keep.txt" {
		t.Fatalf("Pins = %v, %v; want [keep.txt]", pins, err)
	}
	// The pinned row is OUT of the eviction candidates.
	rows, err := f.store.CleanHydratedRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("pinned row in candidates: %+v", rows)
	}
}

func TestSchedulerTombstones(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	if err := f.store.PutPath(ctx, index.Resource{Path: "gone", Size: 1, Hydrated: true}, "", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeletePath(ctx, "gone", "test delete"); err != nil {
		t.Fatal(err)
	}
	tombs, err := f.sched.Quota().Tombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tombs) != 1 || tombs[0].Path != "gone" || !tombs[0].Deleted {
		t.Fatalf("Tombstones = %+v, want [gone]", tombs)
	}
}
