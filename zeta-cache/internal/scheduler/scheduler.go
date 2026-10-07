package scheduler

// scheduler.go - the assembly: three loops + the quota engine behind ONE
// self-contained Start(ctx), so main.go's wiring is a single line (the
// parent adds it at merge; leaf 06 owns main.go in the meantime).
//
// Pause/resume: Pause/Resume flip one atomic flag shared by all loops.
// Boundaries (documented):
//   - prompt-upload: the in-flight single-file upload FINISHES (a per-file
//     PUT is atomic - the file either stays dirty or the index is clean);
//     further uploads hold and coalesce until Resume.
//   - sync: the running SyncOnce is allowed to finish at the engine's
//     whole-path boundaries (each diffAndApply is atomic); the next pass
//     holds. We do NOT abort mid-pass: the engine's report/idempotency
//     make a finished pass strictly better than a torn one.
//   - eviction: passes hold entirely (no pass is ever mid-flight for
//     long; quota pressure waits).
// A paused daemon still serves FUSE reads from cache (the loops are the
// only thing paused; nothing here touches the mount).

import (
	"context"
	"fmt"
	stdlog "log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
)

// Options configures the Scheduler. Store and Engine are required; the
// upload entry is optional (falls back to SyncOnce - see below).
type Options struct {
	Store  *index.Store
	Engine *zsync.Engine
	// Config drives quota/pins/policy.
	Config *config.Config
	// Uploader is the per-file upload entry (fusefs.FS implements it).
	// nil = prompt-upload falls back to engine.SyncOnce for the dirty
	// set (tradeoff: full-scan PUTs instead of one PUT per dirty file;
	// correct but heavier - the parent's merge wiring should pass the
	// mounted fusefs.FS here).
	Uploader Uploader
	// Clock defaults to real time; tests inject a FakeClock.
	Clock Clock
	// Logger defaults to the standard logger.
	Logger Logger
}

// Scheduler owns the three daemon loops. Construct with New, start with
// Start (once), pause/resume with Pause/Resume, stop by cancelling ctx.
type Scheduler struct {
	store  *index.Store
	engine *zsync.Engine
	cfg    *config.Config
	quota  *QuotaEngine
	up     Uploader
	clock  Clock
	log    Logger

	paused  atomic.Bool
	stateCh chan struct{} // closed-and-replaced on every pause-state flip
	pauseMu sync.Mutex

	started atomic.Bool

	mu          sync.Mutex
	lastSync    time.Time
	lastSyncErr string
}

// New validates options and returns a ready Scheduler.
func New(o Options) (*Scheduler, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("scheduler: Store is required")
	}
	if o.Engine == nil {
		return nil, fmt.Errorf("scheduler: Engine is required")
	}
	if o.Config == nil {
		return nil, fmt.Errorf("scheduler: Config is required")
	}
	clock := o.Clock
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	logger := o.Logger
	if logger == nil {
		logger = stdlog.Default()
	}
	s := &Scheduler{
		store:   o.Store,
		engine:  o.Engine,
		cfg:     o.Config,
		up:      o.Uploader,
		clock:   clock,
		log:     logger,
		stateCh: make(chan struct{}),
	}
	s.quota = NewQuotaEngine(o.Store, o.Config, clock, toStdLogger(logger))
	return s, nil
}

// toStdLogger adapts the minimal Logger to *log.Logger where the quota
// engine wants one (its Printf-only use makes this lossless).
func toStdLogger(l Logger) *stdlog.Logger {
	if std, ok := l.(*stdlog.Logger); ok {
		return std
	}
	return stdlog.New(writerFunc(l.Printf), "", 0)
}

type writerFunc func(format string, args ...any)

func (f writerFunc) Write(p []byte) (int, error) {
	f("%s", p)
	return len(p), nil
}

// Start launches the three loops in goroutines. It is self-contained:
// the ONLY thing main.go needs is
//
//	sched, _ := scheduler.New(...); sched.Start(ctx)
//
// i.e. one line: `go sched.Run(ctx)` semantics with panic safety.
// Start returns immediately; cancel ctx to stop (shutdown < 2s asserted
// in tests: every loop selects on ctx.Done and every wait goes through
// sleepOrCtx/FakeClock).
func (s *Scheduler) Start(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return // already started
	}
	// Config pins: apply once at start; newly-pinned paths hydrate via
	// the first sync (the engine's matrix-1 download covers a not-yet-
	// hydrated row; a full GET here would duplicate its logic).
	if newly, err := s.quota.SyncConfigPins(ctx); err != nil {
		s.log.Printf("scheduler: config pins: %v", err)
	} else if len(newly) > 0 {
		s.log.Printf("scheduler: %d config pin(s) applied: %v", len(newly), newly)
	}

	go s.runPromptUpload(ctx)
	go s.runSync(ctx)
	go s.runEvictionLoop(ctx)
}

// runPromptUpload wraps runWatcher with this scheduler's state.
func (s *Scheduler) runPromptUpload(ctx context.Context) {
	w := watcher{
		store:    s.store,
		cacheDir: s.cfg.CacheDir,
		up:       s.up,
		clock:    s.clock,
		log:      s.log,
		watchDir: watchAll,
	}
	if s.up == nil {
		// Documented fallback: without the per-file uploader the loop
		// still coalesces events and NUDGES the sync loop via a short
		// next-tick; the dirty set is uploaded by the engine.
		w.up = UploadFunc(func(ctx context.Context, key string) error {
			return s.engine.SyncOnce(ctx)
		})
	}
	runWatcher(ctx, w, s.paused.Load, s.wakeCh())
}

// runSync wraps runSyncLoop with status callbacks.
func (s *Scheduler) runSync(ctx context.Context) {
	runSyncLoop(ctx, s.engine, s.clock, s.paused.Load, s.wakeCh(), func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.lastSyncErr = err.Error()
			return
		}
		s.lastSync = s.clock.Now()
		s.lastSyncErr = ""
	})
}

// evictionLoopState bundles the eviction loop's inputs (mirrors the
// other loops' parameter shape; the Scheduler wires its own fields in).
type evictionLoopState struct {
	clock  Clock
	quota  *QuotaEngine
	log    Logger
	paused func() bool
	wake   <-chan struct{}
}

// runEvictionLoop drives nightly + high-water eviction passes and the
// nightly tombstone prune until ctx is cancelled. The high-water check
// polls at quotaCheckInterval (cheap: one index sum); the leaf's
// "immediately when the high-water mark is crossed" is bounded by this
// cadence.
const quotaCheckInterval = time.Minute

func runEvictionLoop(ctx context.Context, st evictionLoopState) {
	nightly := st.clock.NewTimer(nightlyPeriod)
	check := st.clock.NewTimer(quotaCheckInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.wake:
			continue // pause-state flip: re-evaluate
		case <-nightly:
			nightly = st.clock.NewTimer(nightlyPeriod)
			if st.paused() {
				continue
			}
			if n, err := st.quota.RunPass(ctx); err != nil && ctx.Err() == nil {
				st.log.Printf("scheduler: nightly eviction: %v", err)
			} else if n > 0 {
				st.log.Printf("scheduler: evicted %d file(s) (nightly)", n)
			}
			if _, err := st.quota.PruneTombstones(ctx); err != nil {
				st.log.Printf("scheduler: tombstone prune: %v", err)
			}
		case <-check:
			check = st.clock.NewTimer(quotaCheckInterval)
			if st.paused() {
				continue
			}
			usage, err := st.quota.Usage(ctx)
			if err != nil {
				st.log.Printf("scheduler: quota usage: %v", err)
				continue
			}
			if st.quota.highWater() > 0 && usage > st.quota.highWater() {
				if n, err := st.quota.RunPass(ctx); err != nil && ctx.Err() == nil {
					st.log.Printf("scheduler: eviction pass: %v", err)
				} else if n > 0 {
					st.log.Printf("scheduler: evicted %d file(s) (high-water)", n)
				}
			}
		}
	}
}

// runEviction wraps runEvictionLoop with this scheduler's state.
func (s *Scheduler) runEvictionLoop(ctx context.Context) {
	runEvictionLoop(ctx, evictionLoopState{
		clock:  s.clock,
		quota:  s.quota,
		log:    s.log,
		paused: s.paused.Load,
		wake:   s.wakeCh(),
	})
}

// Pause suspends all loops (boundaries documented on Scheduler).
func (s *Scheduler) Pause() (bool, error) {
	return s.setPaused(true), nil
}

// Resume restarts the loops.
func (s *Scheduler) Resume() (bool, error) {
	return s.setPaused(false), nil
}

func (s *Scheduler) setPaused(v bool) bool {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	was := s.paused.Swap(v)
	if was != v {
		close(s.stateCh)
		s.stateCh = make(chan struct{})
	}
	return v
}

// stateCh returns the CURRENT state-change channel (callers re-fetch it
// after every wake; each channel fires exactly once per flip).
func (s *Scheduler) wakeCh() <-chan struct{} {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	return s.stateCh
}

// Quota exposes the quota engine (IPC status wiring + pins).
func (s *Scheduler) Quota() *QuotaEngine { return s.quota }

// IPCStatus builds the scheduler's slice of the IPC status payload.
func (s *Scheduler) IPCStatus(base func() time.Time) (lastSync any, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSync.IsZero() {
		return nil, s.lastSyncErr
	}
	return s.lastSync.Unix(), s.lastSyncErr
}
