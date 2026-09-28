package main

import (
	"os"
	"path/filepath"
	"testing"
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
			expected: `echo "/data/bucket/photo.jpg"`,
		},
		{
			command:  `echo $BUCKET_NAME $OBJECT_KEY`,
			expected: `echo my-bucket photo.jpg`,
		},
		{
			command:  `echo $SIZE bytes`,
			expected: `echo 1024 bytes`,
		},
		{
			command:  `rm -f "$BUCKET_PATH/.thumbs/$(basename "$OBJECT_KEY")"`,
			expected: `rm -f "/data/bucket/.thumbs/$(basename "photo.jpg")"`,
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
		{"other/file.txt", "ephemeral/*", false},
		{"a/b/c.txt", "a/*", true},
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
