// actions_coverage_test.go — leaf 6.4: behavioral tests for actions.go
// residual branches (executeInactivityAction guards, triggerActions event
// branches, initializeInactivityTimers scans, limitBuffer, runCommand
// timeout/output paths, pattern-matching corners). Extends the
// inactivity_runtime_test.go / actions_test.go seam patterns.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// executeInactivityAction guards
// ---------------------------------------------------------------------------

// TestExecuteInactivityActionGuardPins covers every guard branch:
// nil config, empty command, and disabled — none may reach the runner.
func TestExecuteInactivityActionGuardPins(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	tr := newTestTracker()
	disabled := false
	cases := []struct {
		name   string
		config *InactivityConfig
	}{
		{"nil config", nil},
		{"empty command", &InactivityConfig{Duration: "1s", Command: ""}},
		{"explicitly disabled", &InactivityConfig{Duration: "1s", Command: "echo x", Enabled: &disabled}},
	}
	for _, tc := range cases {
		tr.executeInactivityAction(t.TempDir(), tc.config)
	}
	if n := capture.count(); n != 0 {
		t.Fatalf("runner invoked %d times, want 0 (guards must short-circuit)", n)
	}
}

// TestExecuteInactivityActionFiresRunner pins the fire path: an enabled
// config with a command reaches the runner with the frozen inactivity shape
// (name "inactivity", timeout 0, workDir = bucket path).
func TestExecuteInactivityActionFiresRunner(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	bucketDir := t.TempDir()
	newTestTracker().executeInactivityAction(bucketDir, &InactivityConfig{
		Duration:    "1s",
		Command:     "echo idle",
		Description: "gc",
	})

	calls := capture.calls()
	if len(calls) != 1 {
		t.Fatalf("runner invoked %d times, want 1", len(calls))
	}
	if calls[0].name != "inactivity" || calls[0].cmd != "echo idle" || calls[0].workDir != bucketDir || calls[0].timeout != 0 {
		t.Errorf("call = %+v, want {inactivity, echo idle, 0, %s}", calls[0], bucketDir)
	}
}

// TestInitializeForBucketInvalidDurationNotRegistered pins the validation
// rule: an invalid duration must not register the config entry.
func TestInitializeForBucketInvalidDurationNotRegistered(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	tr := newTestTracker()
	bucketDir := t.TempDir()

	tr.initializeForBucket(bucketDir, &InactivityConfig{Duration: "not-a-duration", Command: "echo x"})

	tr.mu.Lock()
	_, registered := tr.configs[bucketDir]
	_, timerArmed := tr.timers[bucketDir]
	tr.mu.Unlock()
	if registered || timerArmed {
		t.Error("invalid duration registered the config entry / armed a timer")
	}

	// recordActivity on the unregistered path must also stay inert.
	tr.recordActivity(bucketDir, "put")
	if n := capture.count(); n != 0 {
		t.Errorf("runner invoked %d times after invalid init, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// recordActivity branches (ResetOn filtering, unknown-activity no-op)
// ---------------------------------------------------------------------------

// TestRecordActivityResetOnFilter pins the ResetOn filter through the
// tracker's observable timer state: the reset path in recordActivity always
// stops the armed timer and replaces it with a freshly armed one, while the
// filter returns BEFORE touching the timer map. So an activity type OUTSIDE
// ResetOn must leave the same *time.Timer armed (pointer unchanged), and a
// listed type must re-arm it (a new timer replaces the old). No sleeping
// needed; the 30m window never fires. Deleting the slices.Contains guard in
// actions.go makes the unlisted-activity assertion fail.
func TestRecordActivityResetOnFilter(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	tr := newTestTracker()
	bucketDir := t.TempDir()
	tr.initializeForBucket(bucketDir, &InactivityConfig{
		Duration: "30m",
		Command:  "echo idle",
		ResetOn:  []string{"put"},
	})

	timerAt := func() *time.Timer {
		tr.mu.RLock()
		defer tr.mu.RUnlock()
		return tr.timers[bucketDir]
	}

	initial := timerAt()
	if initial == nil {
		t.Fatal("initializeForBucket did not arm a timer")
	}

	// "delete" is not in ResetOn: the timer must NOT be stopped or re-armed —
	// the identical timer stays armed with its original deadline.
	tr.recordActivity(bucketDir, "delete")
	if got := timerAt(); got != initial {
		t.Error("unlisted activity 'delete' re-armed the timer: ResetOn filter did not apply")
	}

	// "put" IS in ResetOn: the reset path runs — the old timer is stopped and
	// replaced by a fresh, still-armed timer.
	tr.recordActivity(bucketDir, "put")
	if got := timerAt(); got == initial {
		t.Error("listed activity 'put' did not re-arm the timer (same timer pointer)")
	}
	if got := timerAt(); got == nil {
		t.Error("listed activity left no timer armed")
	}

	if n := capture.count(); n != 0 {
		t.Errorf("runner fired %d times inside the window, want 0", n)
	}
}

// TestRecordActivityUnknownBucketIsNoOp pins the unregistered-bucket branch.
func TestRecordActivityUnknownBucketIsNoOp(t *testing.T) {
	tr := newTestTracker()
	tr.recordActivity(t.TempDir(), "put") // must not panic or arm anything
	tr.mu.Lock()
	count := len(tr.timers)
	tr.mu.Unlock()
	if count != 0 {
		t.Errorf("timers armed for an unknown bucket: %d", count)
	}
}

// ---------------------------------------------------------------------------
// triggerActions event branches
// ---------------------------------------------------------------------------

// TestTriggerActionsUnknownEventType pins the default branch: an unknown
// event type runs nothing (and records no activity).
func TestTriggerActionsUnknownEventType(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)
	setInactivityTrackerNil(t)

	bucketDir := t.TempDir()
	writeActionsFile(t, bucketDir, `{"version":"1.0","after_upload":[{"name":"u","command":"echo up"}]}`)

	triggerActions("after_brew", ActionContext{BucketPath: bucketDir})
	if n := capture.count(); n != 0 {
		t.Errorf("unknown event type ran %d actions, want 0", n)
	}
}

// TestTriggerActionsDownloadsAndDeletesAreDispatched pins the
// after_download / after_delete switch branches end to end through the
// runner seam.
func TestTriggerActionsDownloadsAndDeletesAreDispatched(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)
	setInactivityTrackerNil(t)

	bucketDir := t.TempDir()
	writeActionsFile(t, bucketDir, `{
		"version": "1.0",
		"after_download": [{"name": "d", "command": "echo down", "async": false}],
		"after_delete": [{"name": "x", "command": "echo del", "async": false}]
	}`)

	triggerActions("after_download", ActionContext{BucketPath: bucketDir, ObjectKey: "a.txt"})
	triggerActions("after_delete", ActionContext{BucketPath: bucketDir, ObjectKey: "a.txt"})

	calls := capture.calls()
	if len(calls) != 2 {
		t.Fatalf("runner invoked %d times, want 2: %+v", len(calls), calls)
	}
	if calls[0].cmd != "echo down" || calls[1].cmd != "echo del" {
		t.Errorf("cmds = [%q, %q], want [echo down, echo del]", calls[0].cmd, calls[1].cmd)
	}
}

// TestTriggerActionsRecordsInactivityActivity pins the tracker-record tail:
// with a tracker installed, a dispatched event records "upload" activity.
func TestTriggerActionsRecordsInactivityActivity(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)

	tr := newTestTracker()
	setGlobalTracker(t, tr)
	bucketDir := t.TempDir()
	writeActionsFile(t, bucketDir, `{"version":"1.0","after_upload":[{"name":"u","command":"echo up"}]}`)

	triggerActions("after_upload", ActionContext{BucketPath: bucketDir, ObjectKey: "k"})

	tr.mu.Lock()
	_, tracked := tr.lastActivity[bucketDir]
	tr.mu.Unlock()
	if !tracked {
		t.Error("triggerActions did not record activity for the bucket")
	}
}

// TestTriggerActionsNilActionsFileIsNoOp pins the early return when no
// .bucket-actions file exists on the path.
func TestTriggerActionsNilActionsFileIsNoOp(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)
	setInactivityTrackerNil(t)

	triggerActions("after_upload", ActionContext{BucketPath: t.TempDir(), ObjectKey: "k"})
	if n := capture.count(); n != 0 {
		t.Errorf("runner invoked %d times with no actions file, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// loadActionsForPath / mergeActionSlice residual branches
// ---------------------------------------------------------------------------

// TestLoadActionsForPathUnreadableRootLogsAndContinues pins the root-load
// error branch: an unparsable root file is logged, not fatal, and the
// directory walk still merges child actions.
func TestLoadActionsForPathUnreadableRootLogsAndContinues(t *testing.T) {
	bucket := t.TempDir()
	writeActionsFile(t, bucket, "{not json")

	sub := filepath.Join(bucket, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writeActionsFile(t, sub, `{"version":"1.0","after_upload":[{"name":"child-only","command":"echo c"}]}`)

	actions := loadActionsForPath(bucket, "sub/file.txt")
	if actions == nil {
		t.Fatal("loadActionsForPath returned nil despite a valid child file")
	}
	if len(actions.AfterUpload) != 1 || actions.AfterUpload[0].Name != "child-only" {
		t.Errorf("AfterUpload = %+v, want the child-only action", actions.AfterUpload)
	}
}

// TestLoadActionsForPathNoObjectKeyReturnsRoot pins the objectKey == ""
// early return (bucket-root actions only).
func TestLoadActionsForPathNoObjectKeyReturnsRoot(t *testing.T) {
	bucket := t.TempDir()
	writeActionsFile(t, bucket, `{"version":"1.0","after_upload":[{"name":"root","command":"echo r"}]}`)

	actions := loadActionsForPath(bucket, "")
	if actions == nil || len(actions.AfterUpload) != 1 || actions.AfterUpload[0].Name != "root" {
		t.Fatalf("root-only load = %+v, want the root action", actions)
	}
}

// TestMergeActionSliceChildOnlyAndParentOnly pins mergeActionSlice's
// short-circuit returns.
func TestMergeActionSliceChildOnlyAndParentOnly(t *testing.T) {
	parent := []ActionConfig{{Name: "p", Command: "echo p"}}
	child := []ActionConfig{{Name: "c", Command: "echo c"}}

	if got := mergeActionSlice(parent, nil); len(got) != 1 || got[0].Name != "p" {
		t.Errorf("empty child: got %+v, want parent", got)
	}
	if got := mergeActionSlice(nil, child); len(got) != 1 || got[0].Name != "c" {
		t.Errorf("empty parent: got %+v, want child", got)
	}

	// Both non-empty: child action absent from parent is appended.
	merged := mergeActionSlice(parent, child)
	if len(merged) != 2 {
		t.Fatalf("merged = %+v, want 2 entries", merged)
	}
	if merged[0].Name != "p" || merged[1].Name != "c" {
		t.Errorf("merged order = [%s %s], want [p c]", merged[0].Name, merged[1].Name)
	}
}

// ---------------------------------------------------------------------------
// matchesAnyPattern corners (invalid glob, filename match, path glob)
// ---------------------------------------------------------------------------

// TestMatchesAnyPatternInvalidGlobSkipped pins the invalid-pattern branch:
// a malformed glob is skipped (logged), remaining patterns still match.
func TestMatchesAnyPatternInvalidGlobSkipped(t *testing.T) {
	if matchesAnyPattern("a.txt", []string{"[invalid"}) {
		t.Error("malformed glob matched")
	}
	if !matchesAnyPattern("a.txt", []string{"[invalid", "*.txt"}) {
		t.Error("valid pattern after malformed one did not match")
	}
}

// TestMatchesAnyPatternFilenameFallback pins the basename fallback: a
// bare filename pattern matches a nested object key.
func TestMatchesAnyPatternFilenameFallback(t *testing.T) {
	if !matchesAnyPattern("deep/nested/dir/a.txt", []string{"a.txt"}) {
		t.Error("filename fallback failed for a nested key")
	}
	if matchesAnyPattern("deep/nested/dir/a.txt", []string{"b.txt"}) {
		t.Error("filename fallback matched the wrong name")
	}
}

// TestMatchesAnyPatternPathGlob pins the anchored path-glob fallback
// (part-count equality; ** recursion).
func TestMatchesAnyPatternPathGlob(t *testing.T) {
	cases := []struct {
		key     string
		pattern string
		want    bool
	}{
		// Anchored simple path glob: part counts must be equal.
		{"ephemeral/tmp", "ephemeral/*", true},
		{"ephemeral/tmp/x", "ephemeral/*", false},
		// ** recursion: matches any span (including none).
		{"a/b/c/d.txt", "a/**/d.txt", true},
		{"a/d.txt", "a/**/d.txt", false}, // ** requires the literal /d.txt tail
		{"a/x/d.txt", "a/**/d.txt", true},
	}
	for _, tc := range cases {
		if got := matchesAnyPattern(tc.key, []string{tc.pattern}); got != tc.want {
			t.Errorf("matchesAnyPattern(%q, %q) = %v, want %v", tc.key, tc.pattern, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// limitBuffer
// ---------------------------------------------------------------------------

// TestLimitBufferDiscardsPastCap pins the discard branch: writes past the
// cap are dropped but reported as fully accepted.
func TestLimitBufferDiscardsPastCap(t *testing.T) {
	l := &limitBuffer{capBytes: 8}
	if n, err := l.Write([]byte("12345678")); n != 8 || err != nil {
		t.Fatalf("first write = (%d, %v), want (8, nil)", n, err)
	}
	n, err := l.Write([]byte("ABCD"))
	if n != 4 || err != nil {
		t.Fatalf("past-cap write = (%d, %v), want (4, nil)", n, err)
	}
	if l.String() != "12345678" {
		t.Errorf("buffer = %q, want the first 8 bytes only", l.String())
	}
	if l.Len() != 8 {
		t.Errorf("Len = %d, want 8", l.Len())
	}
}

// TestLimitBufferTruncatesMidWrite pins the partial-room branch: a write
// larger than the remaining room fills the cap and appends the marker.
func TestLimitBufferTruncatesMidWrite(t *testing.T) {
	l := &limitBuffer{capBytes: 10}
	if _, err := l.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	l.buf.Reset() // exercise the partial-room path directly
	l.buf.WriteString("abc")
	n, err := l.Write([]byte("defghijkl")) // 9 bytes into 7 bytes of room
	if n != 9 || err != nil {
		t.Fatalf("partial write = (%d, %v), want (9, nil)", n, err)
	}
	if got := l.String(); got != "abcdefghij\n...[truncated]" {
		t.Errorf("buffer = %q, want truncated form with marker", got)
	}
}

// ---------------------------------------------------------------------------
// runCommand behavioral paths (real sh execution, no seam)
// ---------------------------------------------------------------------------

// TestRunCommandDefaultsTimeoutAndCapturesStdout drives the success path
// with timeout <= 0: the 30s default applies and stdout is captured.
func TestRunCommandDefaultsTimeoutAndCapturesStdout(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCommand("test-success", "echo hello-from-action", 0, t.TempDir())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runCommand did not return (timeout default broken?)")
	}
}

// TestRunCommandNonTimeoutFailureIsLogged pins the failure branch: a
// failing command returns (exit status logged), the stderr is captured.
func TestRunCommandNonTimeoutFailureIsLogged(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCommand("test-fail", "echo boom >&2; exit 3", 5, t.TempDir())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runCommand did not return after a failing command")
	}
}

// TestRunCommandTimeoutKillsProcessGroup pins the timeout branch: a command
// that outlives its timeout is killed (the group SIGKILL path) and
// runCommand returns promptly after the deadline.
func TestRunCommandTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCommand("test-timeout", "sleep 30", 1, t.TempDir())
	}()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("runCommand returned after %v, want ~1s (process group kill)", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runCommand hung past its 1s timeout")
	}
}

// ---------------------------------------------------------------------------
// initializeInactivityTimers scans
// ---------------------------------------------------------------------------

// sweepTrackerSwap swaps the global tracker and restores it on cleanup.
func sweepTrackerSwap(t *testing.T, tr *InactivityTracker) {
	t.Helper()
	orig := inactivityTracker
	t.Cleanup(func() { inactivityTracker = orig })
	inactivityTracker = tr
}

// configSwap swaps serverConfig and restores it on cleanup.
func configSwap(t *testing.T, cfg ServerConfig) {
	t.Helper()
	orig := serverConfig
	t.Cleanup(func() { serverConfig = orig })
	serverConfig = cfg
}

// TestInitializeInactivityTimersNilTrackerIsNoOp pins the nil guard.
func TestInitializeInactivityTimersNilTrackerIsNoOp(t *testing.T) {
	sweepTrackerSwap(t, nil)
	configSwap(t, ServerConfig{DataDir: t.TempDir() + "/", Buckets: map[string]string{}})
	initializeInactivityTimers() // must not panic
}

// TestInitializeInactivityTimersCustomBucketScan pins the custom-bucket
// scan branch: a custom bucket with an actions file gets a timer; one with
// a corrupt file is skipped; one with no inactivity_timeout is skipped.
func TestInitializeInactivityTimersCustomBucketScan(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)
	tr := newTestTracker()
	sweepTrackerSwap(t, tr)

	good := t.TempDir()
	writeActionsFile(t, good, `{"version":"1.0","inactivity_timeout":{"duration":"30m","command":"echo idle-g"}}`)

	corrupt := t.TempDir()
	writeActionsFile(t, corrupt, "{not json")

	noTimeout := t.TempDir()
	writeActionsFile(t, noTimeout, `{"version":"1.0","after_upload":[{"name":"u","command":"echo u"}]}`)

	configSwap(t, ServerConfig{
		DataDir: t.TempDir() + "/",
		Buckets: map[string]string{"good": good, "corrupt": corrupt, "notimeout": noTimeout},
	})

	initializeInactivityTimers()

	tr.mu.Lock()
	_, goodRegistered := tr.configs[good]
	_, corruptRegistered := tr.configs[corrupt]
	_, noTimeoutRegistered := tr.configs[noTimeout]
	tr.mu.Unlock()

	if !goodRegistered {
		t.Error("custom bucket with valid inactivity config was not registered")
	}
	if corruptRegistered || noTimeoutRegistered {
		t.Error("corrupt / no-timeout custom buckets were registered")
	}
}

// TestInitializeInactivityTimersAutoDiscoveredScan pins the dataDir scan:
// buckets are auto-discovered, dotfiles and non-dirs are skipped, custom
// buckets are not double-registered, and an unreadable dataDir returns.
func TestInitializeInactivityTimersAutoDiscoveredScan(t *testing.T) {
	capture := &runnerCapture{}
	swapActionRunner(t, capture.run)
	tr := newTestTracker()
	sweepTrackerSwap(t, tr)

	dataDir := t.TempDir()

	auto := filepath.Join(dataDir, "autobucket")
	if err := os.MkdirAll(auto, 0755); err != nil {
		t.Fatal(err)
	}
	writeActionsFile(t, auto, `{"version":"1.0","inactivity_timeout":{"duration":"30m","command":"echo idle-a"}}`)

	// Dot-prefixed dir and a plain file must be skipped.
	dot := filepath.Join(dataDir, ".hiddendir")
	if err := os.MkdirAll(dot, 0755); err != nil {
		t.Fatal(err)
	}
	writeActionsFile(t, dot, `{"version":"1.0","inactivity_timeout":{"duration":"30m","command":"echo hidden"}}`)
	if err := os.WriteFile(filepath.Join(dataDir, "plainfile"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	// A custom bucket whose path IS under dataDir must not double-register.
	custom := filepath.Join(dataDir, "custombucket")
	if err := os.MkdirAll(custom, 0755); err != nil {
		t.Fatal(err)
	}
	writeActionsFile(t, custom, `{"version":"1.0","inactivity_timeout":{"duration":"30m","command":"echo idle-c"}}`)

	configSwap(t, ServerConfig{
		DataDir: dataDir + "/",
		Buckets: map[string]string{"custombucket": custom},
	})

	initializeInactivityTimers()

	tr.mu.Lock()
	_, autoRegistered := tr.configs[auto]
	_, dotRegistered := tr.configs[dot]
	_, customRegistered := tr.configs[custom]
	tr.mu.Unlock()

	if !autoRegistered {
		t.Error("auto-discovered bucket was not registered")
	}
	if dotRegistered {
		t.Error("dot-prefixed directory was registered")
	}
	if !customRegistered {
		t.Error("custom bucket was not registered via the custom scan")
	}
}

// TestInitializeInactivityTimersUnreadableDataDirReturns pins the
// ReadDir error branch: initializeInactivityTimers returns without panic.
func TestInitializeInactivityTimersUnreadableDataDirReturns(t *testing.T) {
	tr := newTestTracker()
	sweepTrackerSwap(t, tr)
	// DataDir points at a FILE: ReadDir fails, the scan returns cleanly.
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	configSwap(t, ServerConfig{DataDir: file + "/", Buckets: map[string]string{}})
	initializeInactivityTimers()
}

// ---------------------------------------------------------------------------
// shared helper
// ---------------------------------------------------------------------------

// writeActionsFile writes a .bucket-actions file into dir.
func writeActionsFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, actionsFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// silence unused imports when guards evolve
var _ = bytes.MinRead
var _ sync.Mutex
var _ = strings.TrimSpace
