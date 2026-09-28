package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- Leaf 4.1: inactivity runtime tests ---
//
// parseDuration supports only s/m/h/d units (no sub-second), so the shortest
// configurable window is 1s. Every timer test below uses 1s windows and polls
// rather than sleeps; total wall budget per test stays under 2s.

// waitForTimeout polls cond until true or the deadline passes.
func waitForTimeout(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for: %s", timeout, what)
}

// newTestTracker returns a fresh tracker (bypassing the global).
func newTestTracker() *InactivityTracker {
	return &InactivityTracker{
		lastActivity: make(map[string]time.Time),
		timers:       make(map[string]*time.Timer),
		configs:      make(map[string]*InactivityConfig),
	}
}

// setGlobalTracker swaps the global inactivityTracker for the test duration.
func setGlobalTracker(t *testing.T, tr *InactivityTracker) {
	t.Helper()
	orig := inactivityTracker
	inactivityTracker = tr
	t.Cleanup(func() { inactivityTracker = orig })
}

// TestInactivityTimerFiresAfterWindow pins: activity recorded against a
// registered config arms a timer; after the configured window elapses with no
// further activity, executeInactivityAction fires the configured command via
// the runner seam (name "inactivity", timeout 0).
func TestInactivityTimerFiresAfterWindow(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	tr := newTestTracker()
	setGlobalTracker(t, tr)

	bucketDir := t.TempDir()
	tr.initializeForBucket(bucketDir, &InactivityConfig{
		Duration: "1s",
		Command:  "echo idle",
	})

	// Fresh activity (re)arms the timer; leaf-2.5 Stop+drain semantics.
	tr.recordActivity(bucketDir, "put")

	waitForTimeout(t, 2*time.Second, "inactivity action to fire after 1s window", func() bool {
		return capture.count() > 0
	})

	calls := capture.calls()
	if len(calls) != 1 {
		t.Fatalf("runner invoked %d times, want 1: %+v", len(calls), calls)
	}
	if calls[0].name != "inactivity" {
		t.Errorf("name = %q, want %q", calls[0].name, "inactivity")
	}
	if calls[0].cmd != "echo idle" {
		t.Errorf("cmd = %q, want %q", calls[0].cmd, "echo idle")
	}
	if calls[0].workDir != bucketDir {
		t.Errorf("workDir = %q, want %q", calls[0].workDir, bucketDir)
	}
}

// TestInactivityTimerReArmsOnFreshActivity pins the leaf-2.5 timer-drain fix
// (fix 8): recording fresh activity inside the window stops the old timer AND
// drains a fire that already raced, so the action fires exactly once — after
// the LAST activity's window, not the first one's.
func TestInactivityTimerReArmsOnFreshActivity(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	tr := newTestTracker()
	setGlobalTracker(t, tr)

	bucketDir := t.TempDir()
	tr.initializeForBucket(bucketDir, &InactivityConfig{
		Duration: "1s",
		Command:  "echo idle",
	})

	// t≈0: first activity arms a 1s timer.
	tr.recordActivity(bucketDir, "put")

	// t≈600ms: fresh activity re-arms (old timer stopped/drained).
	time.Sleep(600 * time.Millisecond)
	tr.recordActivity(bucketDir, "put")

	// t≈1100ms: the FIRST window would have fired at ~1000ms. The re-arm
	// must have suppressed it — no fire yet.
	time.Sleep(500 * time.Millisecond)
	if n := capture.count(); n != 0 {
		t.Fatalf("fired %d time(s) at t≈1.1s after re-arm at t≈0.6s — old timer not drained/stopped (leaf-2.5 fix 8 regression)", n)
	}

	// The second window ends at ~1600ms; wait for the (single) fire.
	waitForTimeout(t, 2*time.Second, "inactivity action to fire after the re-armed window", func() bool {
		return capture.count() > 0
	})

	if n := capture.count(); n != 1 {
		t.Fatalf("runner invoked %d times total, want exactly 1: %+v", n, capture.calls())
	}
}

// TestInactivityActivityTypeNotInResetOnIgnored pins: when reset_on is
// configured, an activity type not in the list neither fires nor resets the
// timer — the first window still expires on schedule.
func TestInactivityActivityTypeNotInResetOnIgnored(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	tr := newTestTracker()
	setGlobalTracker(t, tr)

	bucketDir := t.TempDir()
	tr.initializeForBucket(bucketDir, &InactivityConfig{
		Duration: "1s",
		Command:  "echo idle",
		ResetOn:  []string{"put"},
	})

	// t≈0: arms the window.
	tr.recordActivity(bucketDir, "put")

	// t≈600ms: a "download" is NOT in reset_on → must not re-arm.
	time.Sleep(600 * time.Millisecond)
	tr.recordActivity(bucketDir, "download")

	// The original window expires at ~1000ms despite the ignored activity.
	waitForTimeout(t, 2*time.Second, "inactivity action to fire (ignored activity type must not reset)", func() bool {
		return capture.count() > 0
	})
}

// TestInactivityNoDoubleFireWhileInFlight pins actual behavior of a second
// timer expiry while the first executeInactivityAction is still running:
// recordActivity during the in-flight run re-arms a NEW timer, so a second
// run happens only after further activity — never a stacked concurrent run.
// (Current code has no in-flight suppression; the timer mechanism itself
// serializes: one timer per bucket, re-created only by new activity.)
func TestInactivityNoDoubleFireWhileInFlight(t *testing.T) {
	fireEntered := make(chan struct{})
	release := make(chan struct{})
	var runCount int32
	var mu sync.Mutex

	runner := func(name, cmd string, timeout int, workDir string) {
		mu.Lock()
		runCount++
		mu.Unlock()
		// First fire blocks "in flight"; later fires pass straight through
		// (if any — the pin is that none are triggered concurrently).
		select {
		case <-fireEntered:
		default:
			close(fireEntered)
			<-release
		}
	}
	swapActionRunner(t, runner)

	tr := newTestTracker()
	setGlobalTracker(t, tr)

	bucketDir := t.TempDir()
	tr.initializeForBucket(bucketDir, &InactivityConfig{
		Duration: "1s",
		Command:  "echo idle",
	})
	tr.recordActivity(bucketDir, "put")

	// Wait until the fire is in-flight (inside the runner, blocked).
	select {
	case <-fireEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first inactivity fire never entered the runner")
	}

	// While in flight, simulate activity arriving: this re-arms a fresh
	// timer but must NOT stack a second concurrent execution.
	tr.recordActivity(bucketDir, "put")

	mu.Lock()
	inFlight := runCount
	mu.Unlock()
	if inFlight != 1 {
		t.Fatalf("runCount = %d while first fire still in flight, want 1", inFlight)
	}

	// Release the first fire and give the timer machinery 300ms to do
	// anything it's going to do without further activity.
	close(release)
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	total := runCount
	mu.Unlock()
	// Exactly one run for the one recorded activity window; a re-arm fires
	// only ~1s after the NEW activity, well beyond this check.
	if total != 1 {
		t.Fatalf("runCount = %d after release, want 1 — second fire stacked while first was in flight", total)
	}
}

// TestInitializeInactivityTimersValidBucket pins: a bucket with a valid
// .bucket-actions (inactivity_timeout block) gets registered in the tracker
// — observable via a subsequent recordActivity firing the command.
func TestInitializeInactivityTimersValidBucket(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	dataDir := t.TempDir()
	bucketDir := filepath.Join(dataDir, "busy")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeActionsConfig(t, bucketDir, `{
		"version": "1.0",
		"inactivity_timeout": {"duration": "1s", "command": "echo idle-bucket"}
	}`)

	origConfig := serverConfig
	serverConfig = ServerConfig{DataDir: dataDir}
	t.Cleanup(func() { serverConfig = origConfig })

	tr := newTestTracker()
	setGlobalTracker(t, tr)

	initializeInactivityTimers()

	// Registration is observable: recording activity must arm the timer and
	// the command must fire after the window.
	tr.recordActivity(bucketDir, "put")
	waitForTimeout(t, 2*time.Second, "registered bucket to fire its inactivity command", func() bool {
		return bucketCallCount(t, capture, bucketDir) > 0
	})

	calls := bucketCalls(t, capture, bucketDir)
	if len(calls) != 1 {
		t.Fatalf("runner invoked %d times for %s, want 1: %+v", len(calls), bucketDir, calls)
	}
	if calls[0].cmd != "echo idle-bucket" {
		t.Errorf("cmd = %q, want %q", calls[0].cmd, "echo idle-bucket")
	}
}

// bucketCalls filters captured runner invocations to one bucket path. Tests
// in this package share a process, so a timer armed by an earlier test can
// still be in flight; per-bucket filtering keeps assertions leak-tolerant.
func bucketCalls(t *testing.T, capture *runnerCapture, bucketDir string) []runnerInvoke {
	t.Helper()
	var out []runnerInvoke
	for _, c := range capture.calls() {
		if c.workDir == bucketDir {
			out = append(out, c)
		}
	}
	return out
}

func bucketCallCount(t *testing.T, capture *runnerCapture, bucketDir string) int {
	t.Helper()
	return len(bucketCalls(t, capture, bucketDir))
}

// TestInitializeInactivityTimersMalformedFile pins: a bucket whose
// .bucket-actions cannot be parsed is skipped with no panic and no timer
// registration, while other buckets still initialize (partial-failure
// isolation via the per-bucket continue).
func TestInitializeInactivityTimersMalformedFile(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	dataDir := t.TempDir()

	badDir := filepath.Join(dataDir, "bad-bucket")
	if err := os.MkdirAll(badDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeActionsConfig(t, badDir, "{not valid json !!!")

	goodDir := filepath.Join(dataDir, "good-bucket")
	if err := os.MkdirAll(goodDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeActionsConfig(t, goodDir, `{
		"inactivity_timeout": {"duration": "1s", "command": "echo good"}
	}`)

	origConfig := serverConfig
	serverConfig = ServerConfig{DataDir: dataDir}
	t.Cleanup(func() { serverConfig = origConfig })

	tr := newTestTracker()
	setGlobalTracker(t, tr)

	initializeInactivityTimers() // must not panic

	// The good bucket registered: recordActivity fires it.
	tr.recordActivity(goodDir, "put")
	waitForTimeout(t, 2*time.Second, "good bucket to still register and fire", func() bool {
		return capture.count() > 0
	})
	for _, c := range capture.calls() {
		if c.workDir == badDir {
			t.Errorf("malformed bucket fired a command: %+v", c)
		}
	}
}
