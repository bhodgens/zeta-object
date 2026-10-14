package scheduler

// pause_h4_probe_test.go - the H4 contract pinned at the wake seam. The
// original H4 fix's own regression test (pause_test.go's integration
// test) passed 10/10 on 814d7c0^ because its assertions (pass counts via
// LastReport, real-time sleeps) are insensitive to the busy-spin: the
// paused hold loop still reached Resume by re-checking paused() on every
// spin iteration.
//
// The H4 defect's REAL signature is the spin itself: setPaused CLOSED
// the state channel and replaced it, every loop captured its channel
// once at Start, so after the first post-Start flip each loop held a
// CLOSED channel (permanently ready). While paused, the hold select
// takes the always-ready wake case on EVERY iteration - the loop burns
// a full core doing nothing until resume. The fix made the wake
// channels never-closed buffered-1 channels: a paused loop PARKS.
//
// Seam: a real Scheduler on a FakeClock (no timer advances during the
// probe) + process CPU time (syscall.Getrusage) around a 400ms paused
// window. Parked loops cost microseconds; a spinning hold loop burns
// the window wall-clock on its core. The bound (100ms CPU in a 400ms
// window) sits far from both: pre-fix measures ~600ms, post-fix <10ms.

import (
	"context"
	"os"
	"path"
	"syscall"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

func newProbeScheduler(t *testing.T) (*Scheduler, *zsync.Engine, *FakeClock) {
	t.Helper()
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(path.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.Config{
		CacheDir: dir,
		Quota:    config.Quota{MaxCacheBytes: 1000, Policy: "lru", HighWaterPct: 90, LowWaterPct: 70, TombstoneDays: 30},
	}
	clock := NewFakeClock(time.Unix(0, 0))
	eng, err := zsync.NewEngine(zsync.Options{
		Transport: transport.NewMemFS(),
		Store:     store,
		CacheDir:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Store:    store,
		Engine:   eng,
		Config:   cfg,
		Uploader: UploadFunc(func(context.Context, string) error { return nil }),
		Clock:    clock,
		Logger:   discardLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, eng, clock
}

func join(base, name string) string { return base + "/" + name }

func seedDirty(t *testing.T, s *Scheduler, key string) {
	t.Helper()
	fdir := join(s.cfg.CacheDir, "files")
	if err := os.MkdirAll(fdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(join(fdir, key), []byte("body of "+key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutPath(context.Background(), index.Resource{
		Path: key, Size: 1, Hydrated: true, Dirty: true,
	}, "", "h4 probe seed"); err != nil {
		t.Fatal(err)
	}
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano()+ru.Stime.Nano()) * time.Nanosecond
}

// TestPausedLoopsParkNotSpin: with the daemon paused and NO timer
// activity, the loops must PARK. On 814d7c0^ the loops hold closed wake
// channels after the first post-Start flip, so the paused hold select
// spins continuously - process CPU climbs with wall clock.
func TestPausedLoopsParkNotSpin(t *testing.T) {
	s, eng, _ := newProbeScheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seedDirty(t, s, "a.txt")

	// Flip 1 pre-Start (harmless), Start, then the FIRST POST-START
	// flip - the one that poisons the pre-fix loop channels.
	if _, err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	time.Sleep(100 * time.Millisecond) // loops start, hold the connect pass
	if _, err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for eng.LastReport().Scanned < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("connect pass never ran after resume: %+v", eng.LastReport())
		}
		time.Sleep(time.Millisecond)
	}

	// SECOND post-Start flip: Pause. Pre-fix this close lands on a
	// channel nobody holds - every loop keeps its closed channel and
	// the sync loop's paused hold spins. Post-fix every loop parks.
	if _, err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the flip land

	// 400ms paused window, FakeClock frozen (no timer fires; any CPU
	// burned is the spin).
	before := cpuTime()
	time.Sleep(400 * time.Millisecond)
	used := cpuTime() - before

	// A parked process uses microseconds; a spinning select loop burns
	// the window. Bound at 25% of the window.
	if used > 100*time.Millisecond {
		t.Fatalf("paused daemon burned %s of CPU in a 400ms window (busy-spin on a closed wake channel)", used)
	}
}
