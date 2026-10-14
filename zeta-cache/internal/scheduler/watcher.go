package scheduler

// watcher.go - the prompt-upload loop (leaf 07): fsnotify (pure Go;
// inotify on Linux, kqueue/FSEvents-backed on macOS) on the cache dir,
// ~2s debounce, uploads for paths the index marks dirty.
//
// Watch scope: <cacheDir>/files (recursive) - hydrated bodies and the
// dirty generation both live there (leaf-02 layout). staging/ is NOT
// watched: staging files are mid-flight by definition; their rename INTO
// files/ is the event that matters.
//
// Coalescing rule (leaf text): events arriving while an upload drain is
// running fold into one pending set; the drain runs synchronously in the
// loop goroutine, so events during it simply queue in the channel buffer
// and the next debounce pass drains them together. No queue explosion:
// the pending set is a MAP (one entry per path).
//
// Pause behavior: while paused, events keep coalescing into pending and
// the debounce timer does NOT arm; on resume the pending set drains. An
// in-flight single-file upload always finishes (safe boundary: per-file
// PUT is atomic - either the file stays dirty or the index is clean).

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Uploader is the per-file upload entry (leaf 04/02's engine exposes it
// via fusefs.FS.UploadFile; until that wiring exists the Scheduler falls
// back to SyncOnce - see scheduler.go).
type Uploader interface {
	UploadFile(ctx context.Context, key string) error
}

// UploadFunc adapts a function to Uploader.
type UploadFunc func(ctx context.Context, key string) error

// UploadFile implements Uploader.
func (f UploadFunc) UploadFile(ctx context.Context, key string) error { return f(ctx, key) }

// Logger is the minimal logging seam (satisfied by *log.Logger).
type Logger interface {
	Printf(format string, args ...any)
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// Loop constants (leaf: ~2s debounce).
const (
	debounceDelay = 2 * time.Second
	// pollFallback re-scans the dirty set at this cadence even without
	// events, catching writes the watcher missed (new subtrees created
	// after the recursive watch was built) and retrying failures.
	pollFallback = 30 * time.Second
)

// keyOf maps a cache-dir path to its bucket key ("" when outside files/).
func keyOf(cacheDir, path string) string {
	root := filepath.Join(cacheDir, "files")
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// watchAll recursively adds every subtree of root and returns a merged
// event channel plus a stop func. A missing root is created (a fresh
// cache dir has no files/ until the first hydration).
func watchAll(root string) (<-chan fsnotify.Event, func(), error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, nil, err
		}
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	events := make(chan fsnotify.Event, 256)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Add(path)
		}
		return nil
	})
	if err != nil {
		_ = w.Close()
		return nil, nil, err
	}
	go func() {
		defer close(events)
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				events <- ev
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				_ = err // dropped: the poll fallback re-scans dirty state
			}
		}
	}()
	return events, func() { _ = w.Close() }, nil
}

// subtreeWatchers is the R6 dedupe set: one claim per directory root, so
// a create/delete/recreate cycle of the same dir cannot stack duplicate
// recursive watchers (each claim would otherwise hold fsnotify fds and a
// fan-in goroutine until shutdown). claim atomically takes the root
// (false = already watched); release returns it after a failed watchAll
// AND when the directory is deleted (fsnotify Remove / Rename-away): a
// recreated dir must be re-watchable, and its watcher is dead anyway
// (the watched inode is gone). activeCount/rootsOf are test seams.
type subtreeWatchers struct {
	mu    sync.Mutex
	roots map[string]bool
}

func newSubtreeWatchers() *subtreeWatchers {
	return &subtreeWatchers{roots: map[string]bool{}}
}

func (s *subtreeWatchers) claim(root string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roots[root] {
		return false
	}
	s.roots[root] = true
	return true
}

func (s *subtreeWatchers) release(root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roots, root)
}

// activeCount is the number of currently-claimed roots (test seam; the
// deterministic assertion that churn keeps the watcher count bounded).
func (s *subtreeWatchers) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.roots)
}

// watcher carries the loop state.
type watcher struct {
	store    StoreView
	cacheDir string
	up       Uploader // nil -> SyncOnce fallback
	clock    Clock
	log      Logger
	watchDir func(root string) (<-chan fsnotify.Event, func(), error)
}

// drain uploads every pending path that is STILL dirty (the index is the
// truth: sync may have cleaned it, or the flush may never have happened).
// Empties pending. Returns false when ctx was cancelled mid-drain.
func (w *watcher) drain(ctx context.Context, pending map[string]bool) bool {
	for key := range pending {
		delete(pending, key)
		if ctx.Err() != nil {
			return false
		}
		r, err := w.store.Get(ctx, key)
		if err != nil || !r.Dirty {
			continue // cleaned by sync, deleted, or never tracked dirty
		}
		if w.up != nil {
			if err := w.up.UploadFile(ctx, key); err != nil {
				if errors.Is(err, context.Canceled) {
					return false
				}
				w.log.Printf("watch: upload %s: %v (stays dirty; retry on next event/poll/sync)", key, err)
			}
		}
		// up == nil: handled by the scheduler's sync-loop fallback (the
		// dirty set is uploaded by SyncOnce's matrix-2/8 path).
	}
	return true
}

// runWatcher drives the prompt-upload loop until ctx is cancelled. The
// paused channel carries the current paused state (true = paused); the
// loops share one atomic.Bool via this channel so pause takes effect at
// file boundaries.
func runWatcher(
	ctx context.Context,
	w watcher,
	paused func() bool,
	pauseCh <-chan struct{}, // receives a token on every pause-state CHANGE
) {
	events, stopWatch, err := w.watchDir(filepath.Join(w.cacheDir, "files"))
	if err != nil {
		w.log.Printf("watch: fsnotify unavailable (%v); dirty polling every %s only", err, pollFallback)
	} else {
		defer stopWatch()
	}
	// mergeCh is the loop's single event source: the root watch's events
	// (nil when fsnotify is unavailable) plus any subtrees added at
	// runtime, funneled through one fan-in goroutine. The loop always
	// selects on it; the poll timer below is the no-watcher heartbeat.
	// LOW (undrained mergeCh): when the loop exits (ctx done) nobody
	// drains mergeCh, so every fan-in goroutine blocks forever on its
	// forward send. mergeDone is closed on BOTH paths out (loop return
	// and root-watch close), and the forwarders select on it - they
	// drop the event once the loop is gone.
	mergeCh := make(chan fsnotify.Event, 256)
	mergeDone := make(chan struct{})
	done := func() {
		select {
		case <-mergeDone:
		default:
			close(mergeDone)
		}
	}
	defer done()
	go func() {
		defer done()
		if events == nil {
			return
		}
		for ev := range events {
			select {
			case mergeCh <- ev:
			case <-mergeDone:
				return
			}
		}
	}()
	pending := map[string]bool{}
	var debounce <-chan time.Time
	poll := w.clock.NewTimer(pollFallback)

	armDebounce := func() {
		if debounce == nil && !paused() {
			debounce = w.clock.NewTimer(debounceDelay)
		}
	}

	// addSubtree watches a newly-created directory (the recursive watch
	// from watchAll only covers subtrees that existed at build time) and
	// folds its events into the same merged stream.
	// R6: the subtreeWatchers set guards it - directory churn (create +
	// delete + recreate of the same dir) used to stack one recursive
	// watcher PER create (fds + fan-in goroutines grow without bound
	// until shutdown). The root watch itself still covers the whole
	// tree, so skipping an already-covered root loses no events.
	subs := newSubtreeWatchers()
	// watchSub is the seam for subtree adds (the same injection point as
	// the root watch: production passes watchAll for both; tests count
	// registrations). Guarded against recursion - the seam must not hand
	// back the root feed channel.
	watchSub := w.watchDir
	addSubtree := func(root string) {
		if !subs.claim(root) {
			return
		}
		sub, stopSub, err := watchSub(root)
		if err != nil {
			subs.release(root)
			return
		}
		go func() {
			defer stopSub()
			for ev := range sub {
				select {
				case mergeCh <- ev:
				case <-mergeDone:
					return
				}
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			// Safe boundary: drains are synchronous in this goroutine,
			// so an in-flight single-file upload has finished.
			return

		case <-pauseCh:
			if paused() {
				debounce = nil // hold; events keep coalescing
			} else {
				armDebounce() // resume: drain what accumulated
			}

		case ev, ok := <-mergeCh:
			if !ok {
				mergeCh = nil // root watch closed; the poll timer carries on
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					// A directory created after watchAll's initial
					// walk: fold its subtree into the watch (poll
					// alone would miss writes under it for up to a
					// full poll cycle - the watch keeps it tight).
					// R6 residual: addSubtree re-claims the root -
					// if the dir was deleted and recreated, the
					// delete released the claim, so the recreated
					// dir gets its (fresh) watcher instead of being
					// skipped for the life of the mount.
					addSubtree(ev.Name)
				}
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				if _, err := os.Stat(ev.Name); err != nil && os.IsNotExist(err) {
					// Directory deletion (Remove, or the rename-away
					// half of a move): the subtree watcher over that
					// inode is dead - its fan-in goroutine sees the
					// watch close. Release the claim so a recreated
					// directory is watched again (a stale claim here
					// is exactly the gap the 30s poll was papering
					// over). Rename-away only when the name is truly
					// gone: a rename WITHIN the tree fires Rename on
					// the old name but the dir still exists.
					subs.release(ev.Name)
				}
			}
			key := keyOf(w.cacheDir, ev.Name)
			if key == "" {
				continue
			}
			pending[key] = true
			armDebounce()

		case <-poll:
			// Poll tick: ALWAYS armed (watcher healthy or not). It
			// refreshes the whole dirty set - the retry path for
			// failed uploads and the safety net for missed events
			// and subtrees the watch never saw.
			poll = w.clock.NewTimer(pollFallback)
			if keys, err := w.store.DirtyPaths(ctx); err == nil {
				for _, k := range keys {
					pending[k] = true
				}
			}
			if paused() || len(pending) == 0 {
				continue
			}
			if !w.drain(ctx, pending) {
				// Cancelled mid-drain: leave re-arming to shutdown.
				continue
			}
			// Events that arrived during the drain re-arm the debounce.
			armDebounce()

		case <-debounce:
			debounce = nil
			if paused() || len(pending) == 0 {
				continue
			}
			if !w.drain(ctx, pending) {
				// Cancelled mid-drain: leave re-arming to shutdown.
				continue
			}
			// Events that arrived during the drain re-arm the debounce.
			armDebounce()
		}
	}
}
