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
			w.log.Printf("watch: drain skip %s (get err=%v dirty=%v)", key, err, err == nil && r.Dirty)
			continue // cleaned by sync, deleted, or never tracked dirty
		}
		w.log.Printf("watch: drain upload %s", key)
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
	pauseCh <-chan struct{}, // fires on every pause-state CHANGE
) {
	events, stopWatch, err := w.watchDir(filepath.Join(w.cacheDir, "files"))
	if err != nil {
		w.log.Printf("watch: fsnotify unavailable (%v); dirty polling every %s only", err, pollFallback)
		events = nil
	} else {
		defer stopWatch()
	}

	pending := map[string]bool{}
	var debounce <-chan time.Time
	poll := w.clock.NewTimer(pollFallback)

	armDebounce := func() {
		if debounce == nil && !paused() {
			debounce = w.clock.NewTimer(debounceDelay)
		}
	}

	for {
		var wake <-chan time.Time
		if debounce != nil {
			wake = debounce
		}
		if events == nil {
			wake = poll // no watcher: the poll tick IS the heartbeat
		}
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

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			key := keyOf(w.cacheDir, ev.Name)
			if key == "" {
				continue
			}
			pending[key] = true
			armDebounce()

		case <-wake:
			if wake == debounce {
				debounce = nil
			} else {
				// Poll tick: refresh the whole dirty set (retry path
				// for failed uploads and missed events).
				poll = w.clock.NewTimer(pollFallback)
				if keys, err := w.store.DirtyPaths(ctx); err == nil {
					for _, k := range keys {
						pending[k] = true
					}
				}
				if paused() {
					continue
				}
			}
			if paused() || len(pending) == 0 {
				continue
			}
			if w.drain(ctx, pending) && len(pending) > 0 {
				// Cancelled mid-drain: leave re-arming to shutdown.
				continue
			}
			// Events that arrived during the drain re-arm the debounce.
			armDebounce()
		}
	}
}
