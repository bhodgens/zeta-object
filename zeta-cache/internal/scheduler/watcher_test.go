package scheduler

// watcher_test.go - the prompt-upload unit tests: keyOf mapping, drain
// semantics (dirty-only, uploader errors keep dirty, pause boundary) and
// the coalescing map behavior. The full loop with a REAL fsnotify watch
// is exercised by TestShutdownLatencyUnder2s (loops_test.go) which starts
// runWatcher against a temp dir.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

func TestKeyOf(t *testing.T) {
	dir := string(filepath.Separator) + filepath.Join("tmp", "cache")
	cases := []struct {
		path string
		want string
	}{
		{filepath.Join(dir, "files", "a.txt"), "a.txt"},
		{filepath.Join(dir, "files", "d", "b.txt"), "d/b.txt"},
		{filepath.Join(dir, "staging", "uuid"), ""}, // staging: not a key
		{filepath.Join(dir, "index.db"), ""},        // outside files/
		{filepath.Join(dir, "files"), ""},           // the root itself
	}
	for _, tc := range cases {
		if got := keyOf(dir, tc.path); got != tc.want {
			t.Errorf("keyOf(%s) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// recordingUploader counts uploads and can fail selected keys.
type recordingUploader struct {
	mu    sync.Mutex
	seen  []string
	fail  map[string]bool
	block chan struct{} // non-nil: uploads block until closed
}

func (r *recordingUploader) UploadFile(_ context.Context, key string) error {
	r.mu.Lock()
	r.seen = append(r.seen, key)
	up := r.fail[key]
	bl := r.block
	r.mu.Unlock()
	if bl != nil {
		<-bl
	}
	if up {
		return errors.New("upload failed (test)")
	}
	return nil
}

func (r *recordingUploader) uploads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestDrainUploadsOnlyDirty(t *testing.T) {
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	// a: dirty; b: clean-hydrated; c: no row.
	for _, p := range []string{"a", "b"} {
		r := index.Resource{Path: p, Size: 1, Hydrated: true, Dirty: p == "a"}
		if err := store.PutPath(ctx, r, "", "seed"); err != nil {
			t.Fatal(err)
		}
	}
	up := &recordingUploader{}
	w := watcher{store: store, cacheDir: dir, up: up, clock: NewFakeClock(time.Unix(0, 0)), log: discardLogger{}}
	pending := map[string]bool{"a": true, "b": true, "c": true}
	if !w.drain(ctx, pending) {
		t.Fatal("drain reported cancellation")
	}
	got := up.uploads()
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("uploads = %v, want [a] (only the dirty path)", got)
	}
	if len(pending) != 0 {
		t.Fatalf("pending not emptied: %v", pending)
	}
}

func TestDrainFailedUploadStaysDirtyAndRetried(t *testing.T) {
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.PutPath(ctx, index.Resource{Path: "a", Size: 1, Hydrated: true, Dirty: true}, "", "seed"); err != nil {
		t.Fatal(err)
	}
	up := &recordingUploader{fail: map[string]bool{"a": true}}
	w := watcher{store: store, cacheDir: dir, up: up, clock: NewFakeClock(time.Unix(0, 0)), log: discardLogger{}}
	if !w.drain(ctx, map[string]bool{"a": true}) {
		t.Fatal("drain cancelled")
	}
	// Failure leaves the row dirty: a second drain retries it.
	if r, err := store.Get(ctx, "a"); err != nil || !r.Dirty {
		t.Fatalf("row after failed upload: (%+v, %v), want dirty", r, err)
	}
	if !w.drain(ctx, map[string]bool{"a": true}) {
		t.Fatal("second drain cancelled")
	}
	if n := len(up.uploads()); n != 2 {
		t.Fatalf("upload attempts = %d, want 2 (retry after failure)", n)
	}
}

func TestDrainCancelStopsMidSet(t *testing.T) {
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	for _, p := range []string{"a", "b"} {
		if err := store.PutPath(ctx, index.Resource{Path: p, Size: 1, Hydrated: true, Dirty: true}, "", "seed"); err != nil {
			t.Fatal(err)
		}
	}
	up := &recordingUploader{}
	w := watcher{store: store, cacheDir: dir, up: up, clock: NewFakeClock(time.Unix(0, 0)), log: discardLogger{}}
	cancel() // cancel BEFORE the drain: nothing uploads
	if w.drain(ctx, map[string]bool{"a": true, "b": true}) {
		t.Fatal("drain reported success on a cancelled ctx")
	}
	if n := len(up.uploads()); n != 0 {
		t.Fatalf("uploads after cancel = %d, want 0", n)
	}
}

// pauseResponsiveWatcher: runWatcher must stop within 2s of ctx cancel
// even when paused (the pause hold path wakes on ctx.Done).
func TestWatcherLoopStopsWhilePaused(t *testing.T) {
	dir := t.TempDir()
	store, err := index.OpenWithDiskCheck(filepath.Join(dir, "i.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	clock := ClockFunc(time.Now) // real time: bounded poll loop
	ctx, cancel := context.WithCancel(context.Background())
	paused := true
	pauseCh := make(chan struct{}) // never fires: paused forever
	done := make(chan struct{})
	go func() {
		runWatcher(ctx, watcher{
			store:    store,
			cacheDir: dir,
			up:       &recordingUploader{},
			clock:    clock,
			log:      discardLogger{},
			watchDir: watchAll,
		}, func() bool { return paused }, pauseCh)
		close(done)
	}()
	// The poll tick (30s real) would be the only wake in pause mode;
	// ctx cancel must cut through immediately.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-done:
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("paused watcher stop took %s, want < 2s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("paused watcher did not stop within 2s")
	}
}
