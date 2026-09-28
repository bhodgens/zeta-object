package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStripJSON5Comments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no comments",
			input:    `{"key": "value"}`,
			expected: `{"key": "value"}`,
		},
		{
			name:     "line comment at end",
			input:    `{"key": "value"} // comment`,
			expected: `{"key": "value"} `,
		},
		{
			name:     "line comment on own line",
			input:    "// comment\n{\"key\": \"value\"}",
			expected: "\n{\"key\": \"value\"}",
		},
		{
			name:     "block comment",
			input:    `{"key": /* comment */ "value"}`,
			expected: `{"key":  "value"}`,
		},
		{
			name:     "multiline block comment",
			input:    "{\n/* multi\nline\ncomment */\n\"key\": \"value\"\n}",
			expected: "{\n\n\n\n\"key\": \"value\"\n}",
		},
		{
			name:     "comment-like string in quotes",
			input:    `{"url": "http://example.com"}`,
			expected: `{"url": "http://example.com"}`,
		},
		{
			name:     "double slash in string preserved",
			input:    `{"path": "//server/share"}`,
			expected: `{"path": "//server/share"}`,
		},
		{
			name:     "mixed comments",
			input:    "// header comment\n{\"key\": \"value\" /* inline */} // trailing",
			expected: "\n{\"key\": \"value\" } ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(stripJSON5Comments([]byte(tt.input)))
			if result != tt.expected {
				t.Errorf("stripJSON5Comments(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input   string
		wantSec int
		wantErr bool
	}{
		{"30s", 30, false},
		{"5m", 300, false},
		{"2h", 7200, false},
		{"1d", 86400, false},
		{"", 0, true},
		{"30", 0, true},
		{"abc", 0, true},
		{"30x", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			duration, err := parseDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && int(duration.Seconds()) != tt.wantSec {
				t.Errorf("parseDuration(%q) = %v seconds, want %v seconds", tt.input, duration.Seconds(), tt.wantSec)
			}
		})
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	tests := []struct {
		objectKey string
		patterns  []string
		want      bool
	}{
		// Simple extension patterns
		{"photo.jpg", []string{"*.jpg"}, true},
		{"photo.png", []string{"*.jpg"}, false},
		{"photo.jpg", []string{"*.jpg", "*.png"}, true},
		{"photo.png", []string{"*.jpg", "*.png"}, true},

		// Path patterns
		{"images/photo.jpg", []string{"*.jpg"}, true},
		{"ephemeral/file.txt", []string{"ephemeral/*"}, true},
		{"other/file.txt", []string{"ephemeral/*"}, false},

		// Nested paths
		{"a/b/c/file.jpg", []string{"*.jpg"}, true},

		// Empty patterns (should match nothing - patterns required)
		{"file.txt", []string{}, false},

		// Wildcard
		{"anything.txt", []string{"*"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.objectKey+"_"+patternString(tt.patterns), func(t *testing.T) {
			got := matchesAnyPattern(tt.objectKey, tt.patterns)
			if got != tt.want {
				t.Errorf("matchesAnyPattern(%q, %v) = %v, want %v", tt.objectKey, tt.patterns, got, tt.want)
			}
		})
	}
}

func patternString(patterns []string) string {
	if len(patterns) == 0 {
		return "empty"
	}
	return patterns[0]
}

func TestSubstituteVariables(t *testing.T) {
	ctx := ActionContext{
		FilePath:     "/data/bucket/photo.jpg",
		MetadataPath: "/data/bucket/.metadata/photo.jpg.meta",
		BucketName:   "my-bucket",
		BucketPath:   "/data/bucket",
		ObjectKey:    "photo.jpg",
		ContentType:  "image/jpeg",
		ETag:         "abc123",
		Size:         1024,
	}

	tests := []struct {
		command  string
		expected string
	}{
		{
			command:  `echo "$FILE_PATH"`,
			expected: `echo "'/data/bucket/photo.jpg'"`,
		},
		{
			command:  `echo $BUCKET_NAME $OBJECT_KEY`,
			expected: `echo 'my-bucket' 'photo.jpg'`,
		},
		{
			command:  `echo $SIZE bytes`,
			expected: `echo '1024' bytes`,
		},
		{
			command:  `rm -f "$BUCKET_PATH/.thumbs/$(basename "$OBJECT_KEY")"`,
			expected: `rm -f "'/data/bucket'/.thumbs/$(basename "'photo.jpg'")"`,
		},
		{
			command:  `no-variables-here`,
			expected: `no-variables-here`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			result := substituteVariables(tt.command, ctx)
			if result != tt.expected {
				t.Errorf("substituteVariables(%q) = %q, want %q", tt.command, result, tt.expected)
			}
		})
	}
}

// TestSubstituteVariablesQuoting pins the leaf-2.5 shell-quoting behavior:
// every substituted value is POSIX single-quote escaped.
func TestSubstituteVariablesQuoting(t *testing.T) {
	ctx := ActionContext{
		FilePath:   "/data/bucket/photo.jpg",
		BucketName: "my-bucket",
		BucketPath: "/data/bucket",
		ObjectKey:  "photo.jpg",
		Size:       1024,
	}

	tests := []struct {
		command  string
		expected string
	}{
		{
			command:  `echo "$FILE_PATH"`,
			expected: `echo "'/data/bucket/photo.jpg'"`,
		},
		{
			command:  `echo $BUCKET_NAME $OBJECT_KEY`,
			expected: `echo 'my-bucket' 'photo.jpg'`,
		},
		{
			command:  `echo $SIZE bytes`,
			expected: `echo '1024' bytes`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			result := substituteVariables(tt.command, ctx)
			if result != tt.expected {
				t.Errorf("substituteVariables(%q) = %q, want %q", tt.command, result, tt.expected)
			}
		})
	}
}

func TestLoadActionsFile(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "actions-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Test: valid JSON5 config
	validConfig := `{
		// This is a comment
		"version": "1.0",
		"after_upload": [
			{
				"name": "test-action",
				"patterns": ["*.jpg"],
				"command": "echo hello"
			}
		]
	}`

	validPath := filepath.Join(tmpDir, ".bucket-actions")
	if err := os.WriteFile(validPath, []byte(validConfig), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	actions, err := loadActionsFile(validPath)
	if err != nil {
		t.Errorf("loadActionsFile() error = %v", err)
	}
	if actions == nil {
		t.Fatal("loadActionsFile() returned nil")
	}
	if actions.Version != "1.0" {
		t.Errorf("Version = %q, want %q", actions.Version, "1.0")
	}
	if len(actions.AfterUpload) != 1 {
		t.Errorf("AfterUpload length = %d, want 1", len(actions.AfterUpload))
	}
	if actions.AfterUpload[0].Name != "test-action" {
		t.Errorf("Action name = %q, want %q", actions.AfterUpload[0].Name, "test-action")
	}

	// Test: non-existent file
	actions, err = loadActionsFile(filepath.Join(tmpDir, "nonexistent"))
	if err != nil {
		t.Errorf("loadActionsFile() for nonexistent should not error: %v", err)
	}
	if actions != nil {
		t.Error("loadActionsFile() for nonexistent should return nil")
	}

	// Test: invalid JSON
	invalidPath := filepath.Join(tmpDir, "invalid.json")
	if err := os.WriteFile(invalidPath, []byte("{invalid json}"), 0644); err != nil {
		t.Fatalf("Failed to write invalid config: %v", err)
	}
	_, err = loadActionsFile(invalidPath)
	if err == nil {
		t.Error("loadActionsFile() should error for invalid JSON")
	}
}

func TestMergeActions(t *testing.T) {
	boolTrue := true
	boolFalse := false

	parent := &BucketActions{
		Version: "1.0",
		AfterUpload: []ActionConfig{
			{Name: "parent-action", Command: "echo parent"},
			{Name: "shared-action", Command: "echo from parent"},
		},
	}

	child := &BucketActions{
		Version: "1.0",
		AfterUpload: []ActionConfig{
			{Name: "child-action", Command: "echo child"},
			{Name: "shared-action", Command: "echo from child"},
		},
	}

	// Test: merge mode (default)
	merged := mergeActions(parent, child)
	if len(merged.AfterUpload) != 3 {
		t.Errorf("Merged AfterUpload length = %d, want 3", len(merged.AfterUpload))
	}

	// Verify shared-action was overridden by child
	foundShared := false
	for _, action := range merged.AfterUpload {
		if action.Name == "shared-action" {
			foundShared = true
			if action.Command != "echo from child" {
				t.Errorf("shared-action command = %q, want %q", action.Command, "echo from child")
			}
		}
	}
	if !foundShared {
		t.Error("shared-action not found in merged result")
	}

	// Test: override mode
	childOverride := &BucketActions{
		Version: "1.0",
		AfterUpload: []ActionConfig{
			{Name: "only-child", Command: "echo only"},
		},
		Inheritance: &InheritanceConfig{Mode: "override"},
	}
	merged = mergeActions(parent, childOverride)
	if len(merged.AfterUpload) != 1 {
		t.Errorf("Override mode: AfterUpload length = %d, want 1", len(merged.AfterUpload))
	}
	if merged.AfterUpload[0].Name != "only-child" {
		t.Errorf("Override mode: action name = %q, want %q", merged.AfterUpload[0].Name, "only-child")
	}

	// Test: nil handling
	if mergeActions(nil, child) != child {
		t.Error("mergeActions(nil, child) should return child")
	}
	if mergeActions(parent, nil) != parent {
		t.Error("mergeActions(parent, nil) should return parent")
	}

	// Test: enabled flags
	_ = boolTrue
	_ = boolFalse
}

func TestLoadActionsForPath(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "actions-path-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	bucketPath := filepath.Join(tmpDir, "bucket")
	subDir := filepath.Join(bucketPath, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}

	// Create bucket root actions
	bucketActions := `{
		"version": "1.0",
		"after_upload": [{"name": "root-action", "command": "echo root"}]
	}`
	if err := os.WriteFile(filepath.Join(bucketPath, ".bucket-actions"), []byte(bucketActions), 0644); err != nil {
		t.Fatalf("Failed to write bucket actions: %v", err)
	}

	// Create subdir actions
	subdirActions := `{
		"version": "1.0",
		"after_upload": [{"name": "subdir-action", "command": "echo subdir"}]
	}`
	if err := os.WriteFile(filepath.Join(subDir, ".bucket-actions"), []byte(subdirActions), 0644); err != nil {
		t.Fatalf("Failed to write subdir actions: %v", err)
	}

	// Test: load for file in root
	actions := loadActionsForPath(bucketPath, "file.txt")
	if actions == nil {
		t.Fatal("loadActionsForPath() returned nil for root")
	}
	if len(actions.AfterUpload) != 1 {
		t.Errorf("Root AfterUpload length = %d, want 1", len(actions.AfterUpload))
	}
	if actions.AfterUpload[0].Name != "root-action" {
		t.Errorf("Root action name = %q, want %q", actions.AfterUpload[0].Name, "root-action")
	}

	// Test: load for file in subdir (should merge)
	actions = loadActionsForPath(bucketPath, "subdir/file.txt")
	if actions == nil {
		t.Fatal("loadActionsForPath() returned nil for subdir")
	}
	if len(actions.AfterUpload) != 2 {
		t.Errorf("Subdir AfterUpload length = %d, want 2", len(actions.AfterUpload))
	}

	// Verify both actions present
	names := make(map[string]bool)
	for _, action := range actions.AfterUpload {
		names[action.Name] = true
	}
	if !names["root-action"] || !names["subdir-action"] {
		t.Error("Expected both root-action and subdir-action in merged result")
	}
}

func TestMatchPathGlob(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		{"ephemeral/file.txt", "ephemeral/*", true},
		{"ephemeral/a/b.txt", "ephemeral/*", false},
		{"other/file.txt", "ephemeral/*", false},
		{"a/b/c.txt", "a/*", false},
		{"a/b/c.txt", "a/b/*", true},
		{"file.txt", "*", true},
		// Note: ** pattern requires full path matching; direct subdirectories work
		{"deep/file.txt", "deep/**", true},
	}

	for _, tt := range tests {
		t.Run(tt.path+"_"+tt.pattern, func(t *testing.T) {
			got := matchPathGlob(tt.path, tt.pattern)
			if got != tt.want {
				t.Errorf("matchPathGlob(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
			}
		})
	}
}

// --- Leaf 2.5 new tests ---

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "''"},
		{"simple", "'simple'"},
		{"it's", `'it'\''s'`},
		{"a b;c", `'a b;c'`},
		{"$(touch x)", `'$(touch x)'`},
	}

	for _, tt := range tests {
		if got := shellQuote(tt.input); got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSubstituteVariablesInjection(t *testing.T) {
	tmpDir := t.TempDir()
	sentinel := filepath.Join(tmpDir, "pwned")

	ctx := ActionContext{
		BucketName: "b",
		BucketPath: tmpDir,
		ObjectKey:  "x'; touch " + sentinel + "; '",
	}

	got := substituteVariables("touch $OBJECT_KEY", ctx)
	// The embedded single quote must appear in escaped POSIX form.
	if !strings.Contains(got, `'\''`) {
		t.Fatalf("substituteVariables output not shell-escaped: %q", got)
	}

	runCommand("injection-test", got, 10, tmpDir)

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("sentinel file %s was created — command injection not blocked", sentinel)
	}
}

func TestSubstituteVariablesNoReExpansion(t *testing.T) {
	ctx := ActionContext{
		BucketName: "$OBJECT_KEY",
		ObjectKey:  "real.txt",
	}

	got := substituteVariables("echo $BUCKET_NAME $OBJECT_KEY", ctx)
	want := "echo '$OBJECT_KEY' 'real.txt'"
	if got != want {
		t.Errorf("substituteVariables = %q, want %q (values must not re-expand)", got, want)
	}
}

func TestMergeActionsDisableMode(t *testing.T) {
	parent := &BucketActions{
		Version: "1.0",
		AfterUpload: []ActionConfig{
			{Name: "parent-action", Command: "echo parent"},
		},
		AfterDelete: []ActionConfig{
			{Name: "parent-delete", Command: "echo parent-del"},
		},
	}
	child := &BucketActions{
		Version:     "1.0",
		AfterUpload: []ActionConfig{{Name: "child-action", Command: "echo child"}},
		Inheritance: &InheritanceConfig{Mode: "disable"},
	}

	merged := mergeActions(parent, child)
	if merged == nil {
		t.Fatal("mergeActions returned nil for disable mode")
	}
	if len(merged.AfterUpload) != 0 {
		t.Errorf("disable mode: AfterUpload length = %d, want 0 (no actions run in subtree)", len(merged.AfterUpload))
	}
	if len(merged.AfterDownload) != 0 {
		t.Errorf("disable mode: AfterDownload length = %d, want 0", len(merged.AfterDownload))
	}
	if len(merged.AfterDelete) != 0 {
		t.Errorf("disable mode: AfterDelete length = %d, want 0 (parent actions must not leak)", len(merged.AfterDelete))
	}
}

func TestMatchPathGlobAnchored(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		// Simple branch must anchor: pattern must consume the ENTIRE path
		{"logs/a/b", "logs/*", false},
		{"logs/a/b", "logs/**", true},
		{"logs/file.txt", "logs/*", true},
		{"logs/file.txt", "logs", false},
		// ** branch: regexp metacharacters must be escaped (literal match)
		{"a(b)/c.txt", "a(b)/**", true},
		{"a(b)/x/y", "a(b)/**", true},
		{"axb/c", "a(b)/**", false},
	}

	for _, tt := range tests {
		t.Run(tt.path+"_"+tt.pattern, func(t *testing.T) {
			got := matchPathGlob(tt.path, tt.pattern)
			if got != tt.want {
				t.Errorf("matchPathGlob(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestStripJSON5CommentsSingleQuotedStrings(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "URL in single-quoted string survives",
			input: `{"command": 'echo "http://x"'}`,
			want:  `{"command": 'echo "http://x"'}`,
		},
		{
			name:  "line-comment marker inside single quotes preserved",
			input: `{"command": 'echo // not a comment'}`,
			want:  `{"command": 'echo // not a comment'}`,
		},
		{
			name:  "block-comment marker inside single quotes preserved",
			input: `{"command": 'echo /* not a comment */'}`,
			want:  `{"command": 'echo /* not a comment */'}`,
		},
		{
			name:  "escaped quote in single-quoted string",
			input: `{"command": 'echo it\'s // fine', "a": 1} // real comment`,
			want:  `{"command": 'echo it\'s // fine', "a": 1} `,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(stripJSON5Comments([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("stripJSON5Comments(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRunCommandTimeout(t *testing.T) {
	// Budget: must return well under 3s — sleep 5 killed by 1s timeout.
	start := time.Now()
	runCommand("timeout-test", "sleep 5", 1, "")
	elapsed := time.Since(start)
	if elapsed >= 3*time.Second {
		t.Errorf("runCommand with 1s timeout took %v (want <3s) — process-group kill not effective", elapsed)
	}
}

func TestInitializeForBucketInvalidDuration(t *testing.T) {
	tr := &InactivityTracker{
		lastActivity: make(map[string]time.Time),
		timers:       make(map[string]*time.Timer),
		configs:      make(map[string]*InactivityConfig),
	}

	tr.initializeForBucket("/tmp/bucket", &InactivityConfig{
		Duration: "not-a-duration",
		Command:  "echo hi",
	})

	if len(tr.configs) != 0 {
		t.Errorf("configs length = %d, want 0 — invalid duration must not register the entry", len(tr.configs))
	}
	if len(tr.timers) != 0 {
		t.Errorf("timers length = %d, want 0", len(tr.timers))
	}
}

func TestInitializeForBucketValidDuration(t *testing.T) {
	tr := &InactivityTracker{
		lastActivity: make(map[string]time.Time),
		timers:       make(map[string]*time.Timer),
		configs:      make(map[string]*InactivityConfig),
	}

	tr.initializeForBucket("/tmp/bucket-ok", &InactivityConfig{
		Duration: "1h",
		Command:  "echo hi",
	})

	if len(tr.configs) != 1 {
		t.Fatalf("configs length = %d, want 1", len(tr.configs))
	}
	if tr.timers["/tmp/bucket-ok"] == nil {
		t.Error("expected inactivity timer to be registered for valid duration")
	}
}

func TestInitInactivityTrackerGuard(t *testing.T) {
	// Clean up global state for other tests.
	defer func() { inactivityTracker = nil }()

	inactivityTracker = nil
	InitInactivityTracker()
	first := inactivityTracker
	if first == nil {
		t.Fatal("InitInactivityTracker did not initialize the tracker")
	}
	InitInactivityTracker()
	if inactivityTracker != first {
		t.Error("second InitInactivityTracker call re-assigned the global tracker (double-init guard missing)")
	}
}

// --- Leaf 4.1: triggerActions / executeAction runtime tests ---

// runnerCapture is a thread-safe recorder swapped in for actionCommandRunner.
type runnerCapture struct {
	mu      sync.Mutex
	invokes []runnerInvoke
}

type runnerInvoke struct {
	name    string
	cmd     string
	timeout int
	workDir string
}

func (r *runnerCapture) run(name, cmd string, timeout int, workDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invokes = append(r.invokes, runnerInvoke{name: name, cmd: cmd, timeout: timeout, workDir: workDir})
}

func (r *runnerCapture) calls() []runnerInvoke {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runnerInvoke(nil), r.invokes...)
}

func (r *runnerCapture) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.invokes)
}

// swapActionRunner replaces actionCommandRunner for the duration of the test
// (restored via t.Cleanup). Frozen-seam swap per docs/plans/test-gaps-2026-09/.
func swapActionRunner(t *testing.T, runner func(name, cmd string, timeout int, workDir string)) {
	t.Helper()
	orig := actionCommandRunner
	actionCommandRunner = runner
	t.Cleanup(func() { actionCommandRunner = orig })
}

// setInactivityTrackerNil isolates triggerActions tests from the global
// tracker (restored via t.Cleanup).
func setInactivityTrackerNil(t *testing.T) {
	t.Helper()
	orig := inactivityTracker
	inactivityTracker = nil
	t.Cleanup(func() { inactivityTracker = orig })
}

func writeActionsConfig(t *testing.T, dir, content string) {
	t.Helper()
	path := filepath.Join(dir, actionsFileName)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func TestTriggerActions(t *testing.T) {
	boolFalse := false

	tests := []struct {
		name      string
		config    string // .bucket-actions content ("" = no file at all)
		objectKey string
		wantCalls int
		wantNames []string // expected runner invocations, in order
		wantCmds  []string // expected (substituted) commands, aligned with wantNames
	}{
		{
			// Pins: pattern match → action runs with substituted AND
			// shell-quoted values arriving at the runner.
			name: "matching pattern runs with substituted quoted values",
			config: `{
				"version": "1.0",
				"after_upload": [
					{"name": "thumb", "patterns": ["*.jpg"], "command": "echo $OBJECT_KEY"}
				]
			}`,
			objectKey: "photo.jpg",
			wantCalls: 1,
			wantNames: []string{"thumb"},
			wantCmds:  []string{"echo 'photo.jpg'"},
		},
		{
			// Pins: non-matching pattern runs nothing.
			name: "non-matching pattern runs nothing",
			config: `{
				"after_upload": [
					{"name": "thumb", "patterns": ["*.png"], "command": "echo $OBJECT_KEY"}
				]
			}`,
			objectKey: "photo.jpg",
			wantCalls: 0,
		},
		{
			// Pins: enabled:false action never runs even when pattern matches.
			name: "enabled false never runs",
			config: `{
				"after_upload": [
					{"name": "off", "patterns": ["*.jpg"], "command": "echo hi", "enabled": false}
				]
			}`,
			objectKey: "photo.jpg",
			wantCalls: 0,
		},
		{
			// Pins: multiple matching actions run in config order (async
			// disabled so the runs are serialized through the runner).
			name: "multiple matches run in config order",
			config: `{
				"after_upload": [
					{"name": "first", "patterns": ["*.log"], "command": "echo 1", "async": false},
					{"name": "second", "patterns": ["*.log"], "command": "echo 2", "async": false},
					{"name": "third", "patterns": ["*.log"], "command": "echo 3", "async": false}
				]
			}`,
			objectKey: "a.log",
			wantCalls: 3,
			wantNames: []string{"first", "second", "third"},
			wantCmds:  []string{"echo 1", "echo 2", "echo 3"},
		},
		{
			// Pins: missing .bucket-actions file → no panic, no run
			// (covers the loadActionsForPath nil-return path).
			name:      "missing config file no panic no run",
			config:    "",
			objectKey: "photo.jpg",
			wantCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setInactivityTrackerNil(t)

			bucketDir := t.TempDir()
			if tt.config != "" {
				writeActionsConfig(t, bucketDir, tt.config)
			}

			// Channel-ordered capture: default async actions run in goroutines, so a
			// mutex-protected slice does not pin arrival order — the channel does.
			ordered := make(chan runnerInvoke, 8)
			swapActionRunner(t, func(name, cmd string, timeout int, workDir string) {
				ordered <- runnerInvoke{name: name, cmd: cmd, timeout: timeout, workDir: workDir}
			})

			ctx := ActionContext{
				BucketName: "b",
				BucketPath: bucketDir,
				ObjectKey:  tt.objectKey,
			}
			triggerActions("after_upload", ctx)

			for i := range tt.wantCalls {
				select {
				case inv := <-ordered:
					if inv.name != tt.wantNames[i] {
						t.Errorf("call %d name = %q, want %q", i, inv.name, tt.wantNames[i])
					}
					if inv.cmd != tt.wantCmds[i] {
						t.Errorf("call %d cmd = %q, want %q (substituted+quoted)", i, inv.cmd, tt.wantCmds[i])
					}
					if inv.workDir != bucketDir {
						t.Errorf("call %d workDir = %q, want %q", i, inv.workDir, bucketDir)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("only %d of %d expected runner invocations arrived within 2s", i, tt.wantCalls)
				}
			}
			// Extra invocations beyond the expectation would arrive on the buffered
			// channel; for wantCalls==0 assert none arrived promptly.
			if tt.wantCalls == 0 {
				select {
				case inv := <-ordered:
					t.Errorf("unexpected runner invocation: %+v", inv)
				case <-time.After(50 * time.Millisecond):
				}
			}
		})
	}

	// executeAction enabled:false is also reachable directly (no patterns):
	// keeps the guard pinned independent of triggerActions wiring.
	t.Run("executeAction enabled false direct", func(t *testing.T) {
		capture := &runnerCapture{}
		swapActionRunner(t, capture.run)
		executeAction(ActionConfig{Name: "off", Command: "echo hi", Enabled: &boolFalse}, ActionContext{})
		if capture.count() != 0 {
			t.Errorf("disabled action ran: %+v", capture.calls())
		}
	})
}

func TestExecuteActionAsyncDoesNotBlock(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once

	swapActionRunner(t, func(name, cmd string, timeout int, workDir string) {
		once.Do(func() { close(started) })
		<-release // runner blocked here: a sync executeAction could NOT return
		close(finished)
	})

	done := make(chan struct{})
	go func() {
		// Async is the default (Async == nil): must return without waiting.
		executeAction(ActionConfig{Name: "async-action", Command: "echo hi"}, ActionContext{BucketPath: t.TempDir()})
		close(done)
	}()

	select {
	case <-done:
		// executeAction returned while the runner is still blocked — proves
		// the async path does not wait for command completion.
	case <-time.After(2 * time.Second):
		t.Fatal("executeAction did not return within 2s — async:true blocked the caller")
	}

	select {
	case <-finished:
		t.Fatal("runner finished before release — executeAction waited for it (not async)")
	case <-time.After(50 * time.Millisecond):
	}

	// Release the runner and prove the goroutine eventually ran.
	close(release)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async goroutine never invoked the runner")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("runner never finished after release")
	}
}

func TestExecuteActionSyncBlocks(t *testing.T) {
	var counter atomic.Int32

	swapActionRunner(t, func(name, cmd string, timeout int, workDir string) {
		counter.Add(1)
	})

	asyncFalse := false
	// Sync (async:false): when executeAction returns, the runner MUST have
	// completed — no goroutine deferral.
	executeAction(ActionConfig{Name: "sync-action", Command: "echo hi", Async: &asyncFalse}, ActionContext{BucketPath: t.TempDir()})

	if got := counter.Load(); got != 1 {
		t.Fatalf("counter = %d at executeAction return, want 1 — async:false did not block until completion", got)
	}
}
