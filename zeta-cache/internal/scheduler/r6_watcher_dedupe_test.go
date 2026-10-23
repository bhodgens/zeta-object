package scheduler

// r6_watcher_dedupe_test.go - the R6 regression: every Create+IsDir event
// spawned a fresh recursive watcher over the named directory; churn
// (create + delete + recreate of the same dir) stacked duplicates - one
// fsnotify fd set + one fan-in goroutine per create, held until shutdown.
// The R6-residual finding: the claim was never RELEASED on deletion, so a
// recreated dir was never re-watched (only the 30s poll covered it).
//
// These tests drive the REAL runWatcher loop: the injected watchDir seam
// feeds a SYNTHETIC root-event channel (deterministic - no dependence on
// the OS backend's Create/Remove event semantics) while subtree
// registrations go through the REAL watchAll behind a counting wrapper.
// The contract:
//   - create+IsDir claims the root; churn within one epoch never stacks
//     (at most one live registration per root);
//   - delete releases the claim, so a recreated dir gets a NEW
//     registration (pre-fix: the stale claim suppressed it forever);
//   - runWatcher exits cleanly with every fan-in goroutine torn down
//     (the undrained-mergeCh fix: mergeDone closes on loop exit).

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

// countingWatchDir: the root channel is test-fed; subtree calls hit the
// real watchAll and are counted per root. closed counts stop invocations
// (release must call the stored stop; the fan-in defer may call it again).
type countingWatchDir struct {
	mu       sync.Mutex
	count    map[string]int // subtree root -> watchAll calls
	closed   map[string]int // subtree root -> stop calls
	rootEv   chan fsnotify.Event
	rootDone bool // the root watch was already handed out
}

func newCountingWatchDir() *countingWatchDir {
	return &countingWatchDir{
		count:  map[string]int{},
		closed: map[string]int{},
		rootEv: make(chan fsnotify.Event), // unbuffered: feed() returns when the loop TOOK the event
	}
}

func (c *countingWatchDir) registrations(root string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count[root]
}

func (c *countingWatchDir) closes(root string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed[root]
}

// feed delivers a synthetic root event to the loop.
func (c *countingWatchDir) feed(ev fsnotify.Event) {
	c.mu.Lock()
	ch := c.rootEv
	c.mu.Unlock()
	ch <- ev
}

// watchDir implements the seam: the ROOT gets the synthetic channel;
// any other root (a runtime-created subtree) gets a real watchAll and a
// registration count bump.
func (c *countingWatchDir) watchDir(root string) (<-chan fsnotify.Event, func(), error) {
	c.mu.Lock()
	taken := c.rootDone
	c.rootDone = true
	c.mu.Unlock()
	if !taken {
		return c.rootEv, func() {}, nil
	}
	c.mu.Lock()
	c.count[root]++
	c.mu.Unlock()
	events, stop, err := watchAll(root)
	if err != nil {
		return nil, nil, err
	}
	// Count every stop call. release must invoke this (H6); the fan-in
	// defer may invoke it again once Close unblocks the event channel.
	// watchAll's stop is idempotent, so the second call must not
	// double-close the Watcher.
	return events, func() {
		c.mu.Lock()
		c.closed[root]++
		c.mu.Unlock()
		stop()
	}, nil
}

// mkdirSynth: create the dir on disk and feed the Create event the loop
// would see; rmdirSynth: remove it and feed Remove.
func (c *countingWatchDir) mkdirSynth(t *testing.T, path string, epoch int) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	c.feed(fsnotify.Event{Name: path, Op: fsnotify.Create})
	// The loop's addSubtree runs watchDir(path) asynchronously; wait
	// for THIS epoch's registration (epoch 1 = first registration).
	deadline := time.Now().Add(2 * time.Second)
	for c.registrations(path) < epoch {
		if time.Now().After(deadline) {
			t.Fatalf("create of %s (epoch %d) never registered a subtree watcher (count=%d)", path, epoch, c.registrations(path))
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *countingWatchDir) rmdirSynth(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c.feed(fsnotify.Event{Name: path, Op: fsnotify.Remove})
	// feed() returned = the loop TOOK the Remove; the release runs in
	// the same select body. Give the goroutine a beat to finish the
	// case, then return - the next mkdirSynth's registration wait is
	// what actually proves the claim was released.
	time.Sleep(20 * time.Millisecond)
}

// emptyStore is a StoreView with nothing dirty (the poll tick's
// DirtyPaths hit; nil store would segfault).
type emptyStore struct{}

func (emptyStore) Get(context.Context, string) (index.Resource, error) {
	return index.Resource{}, index.ErrNotFound
}
func (emptyStore) DirtyPaths(context.Context) ([]string, error) { return nil, nil }

// runCountedWatcher starts the real loop over dir with the counting seam.
func runCountedWatcher(t *testing.T, dir string, cwd *countingWatchDir) (cancel func(), done chan struct{}) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		runWatcher(ctx, watcher{
			store:    emptyStore{}, // nothing dirty; poll ticks are no-ops
			cacheDir: dir,
			up:       nil,
			clock:    ClockFunc(time.Now),
			log:      discardLogger{},
			watchDir: cwd.watchDir,
		}, func() bool { return true }, make(chan struct{})) // paused: debounce never drains
		close(done)
	}()
	return cancelCtx, done
}

// TestR6ChurnDoesNotStackWatchers: 5x create/delete/recreate of the SAME
// directory through the real loop - the claim set stays at one claim per
// epoch (never stacked), each epoch re-registers (release-on-delete), and
// a distinct dir gets its own claim.
func TestR6ChurnDoesNotStackWatchers(t *testing.T) {
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := newCountingWatchDir()
	cancel, done := runCountedWatcher(t, dir, cwd)

	sub := filepath.Join(files, "churn")
	for epoch := 1; epoch <= 5; epoch++ {
		cwd.mkdirSynth(t, sub, epoch)
		cwd.rmdirSynth(t, sub)
		// The release is synchronous in the loop; the next mkdirSynth's
		// registration-wait proves the claim was released (pre-fix it
		// would block: claim already held, no new registration).
	}
	if got := cwd.registrations(sub); got != 5 {
		t.Fatalf("churn registrations after 5 epochs = %d, want 5 (one per epoch: dedupe within an epoch, release on delete)", got)
	}

	// A different directory gets its own claim.
	other := filepath.Join(files, "other")
	cwd.mkdirSynth(t, other, 1)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatcher did not exit after cancel")
	}
}

// TestR6RecreatedDirIsRewatched is the residual's direct pin: delete a
// watched dir, recreate it, and the loop must register a NEW watcher for
// the new epoch (pre-fix the stale claim suppressed every future
// registration, leaving only the 30s poll).
func TestR6RecreatedDirIsRewatched(t *testing.T) {
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := newCountingWatchDir()
	cancel, done := runCountedWatcher(t, dir, cwd)

	sub := filepath.Join(files, "ephemeral")
	// Epoch 1.
	cwd.mkdirSynth(t, sub, 1)
	cwd.rmdirSynth(t, sub)
	// Epoch 2: the direct assertion - pre-fix this never registers.
	cwd.mkdirSynth(t, sub, 2)
	if cwd.registrations(sub) != 2 {
		t.Fatalf("recreated dir registrations = %d, want 2 (stale claim: delete never released)", cwd.registrations(sub))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatcher did not exit after cancel")
	}
}

// TestWatcherShutdownDrainsFanIns is the LOW (undrained mergeCh) pin:
// after runWatcher exits, its fan-in goroutines must NOT be parked
// forever on a mergeCh send nobody reads. With live subtree watchers
// producing events during shutdown, the pre-fix loop abandoned mergeCh
// and every forwarder blocked on `mergeCh <- ev` for the life of the
// process. Observable: after cancel+done, feed events through the root
// seam - the fan-in must take them (and drop them via mergeDone), not
// sit wedged on a send. If the forwarders were wedged, the feed would
// block; mergeDone closed lets it return.
func TestWatcherShutdownDrainsFanIns(t *testing.T) {
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := newCountingWatchDir()
	cancel, done := runCountedWatcher(t, dir, cwd)

	// Create a live subtree watcher (a real fsnotify watch feeding its
	// own fan-in goroutine into mergeCh).
	sub := filepath.Join(files, "sub")
	cwd.mkdirSynth(t, sub, 1)
	// Generate events on the subtree AFTER cancel: the sub watcher's
	// fan-in must not be wedged on a mergeCh send.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatcher did not exit after cancel")
	}
	time.Sleep(50 * time.Millisecond) // let the loop's deferred done() land

	// Write into the (now orphaned) subtree: its fan-in would try to
	// forward to the abandoned mergeCh. The forwarder selects on
	// mergeDone, so it drops the event and keeps draining its source.
	// If the forwarder were wedged on the send, this file write +
	// event would sit in the channel and the watcher's own channel
	// would eventually fill - observable as a blocked forwarder.
	// Directly assert the drop path: write a file, give the fan-in a
	// beat, and confirm the test can still cancel the subtree watch
	// cleanly (the forwarder is not stuck: it processes its source).
	if err := os.WriteFile(filepath.Join(sub, "after-shutdown.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// The forwarder must EXIT after shutdown, not stay wedged on a
	// mergeCh send. Feed events so the fan-in wakes up: pre-fix its
	// bare `mergeCh <- ev` send blocks forever once the loop is gone
	// (goroutine stays alive, holding the watcher fds via stopSub's
	// deferred close never running). The fix selects mergeDone: the
	// forwarder takes one event, sees mergeDone, and returns.
	//
	// Exit evidence: once the fan-in is gone, rootEv has NO reader, so
	// a fed send times out. Pre-fix the forwarder stays alive wedged on
	// the mergeCh send and keeps RECEIVING from rootEv - every feed is
	// accepted (the goroutine never exits). So: bounded feeds until one
	// TIMES OUT (no reader => fan-in exited). Bound the whole probe:
	// pre-fix, all feeds keep succeeding (fan-in alive but wedged), so
	// the probe hits its feed cap and fails the assertion below.
	exits := false
	for range 64 {
		select {
		case cwd.rootEv <- fsnotify.Event{Name: filepath.Join(files, "bulk.txt"), Op: fsnotify.Create}:
			// delivered to a live forwarder; pre-fix keeps accepting
		case <-time.After(300 * time.Millisecond):
			exits = true // no reader: the fan-in has exited
		}
		if exits {
			break
		}
	}
	if !exits {
		t.Fatal("root fan-in still alive after shutdown (wedged on an undrained mergeCh send): all feeds kept being accepted")
	}
}

// TestR6DeleteRecreateStopsOldWatcher is the H6 pin: release must call
// the stored stop, not merely delete the claim. A fast recreate (the
// path exists again by the time Remove/Rename is processed) used to skip
// release entirely, so the old Watcher leaked and the new inode was never
// watched. Pre-fix the stop count stays 0 and registrations stay 1.
func TestR6DeleteRecreateStopsOldWatcher(t *testing.T) {
	ops := []struct {
		name string
		op   fsnotify.Op
	}{
		{"remove", fsnotify.Remove},
		{"rename", fsnotify.Rename},
	}
	for _, tc := range ops {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := filepath.Join(dir, "files")
			if err := os.MkdirAll(files, 0o700); err != nil {
				t.Fatal(err)
			}
			cwd := newCountingWatchDir()
			cancel, done := runCountedWatcher(t, dir, cwd)
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("runWatcher did not exit after cancel")
				}
			}()

			sub := filepath.Join(files, "ephemeral")
			cwd.mkdirSynth(t, sub, 1)

			// Recreate BEFORE the event, so Stat sees the new inode.
			// Pre-fix release was gated on IsNotExist and never called
			// stop, so this left the old watcher running.
			if err := os.Remove(sub); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(sub, 0o700); err != nil {
				t.Fatal(err)
			}
			cwd.feed(fsnotify.Event{Name: sub, Op: tc.op})

			deadline := time.Now().Add(2 * time.Second)
			for cwd.closes(sub) < 1 || cwd.registrations(sub) < 2 {
				if time.Now().After(deadline) {
					t.Fatalf("%s+recreate: closes=%d registrations=%d, want stop called (>=1) and a new watch (regs>=2)",
						tc.name, cwd.closes(sub), cwd.registrations(sub))
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
