package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	actionsFileName = ".bucket-actions"
)

// BucketActions represents the configuration from a .bucket-actions file
type BucketActions struct {
	Version           string             `json:"version"`
	AfterUpload       []ActionConfig     `json:"after_upload,omitempty"`
	AfterDownload     []ActionConfig     `json:"after_download,omitempty"`
	AfterDelete       []ActionConfig     `json:"after_delete,omitempty"`
	InactivityTimeout *InactivityConfig  `json:"inactivity_timeout,omitempty"`
	Inheritance       *InheritanceConfig `json:"inheritance,omitempty"`
}

// ActionConfig represents a single action configuration
type ActionConfig struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Patterns    []string `json:"patterns,omitempty"`
	Command     string   `json:"command"`
	Async       *bool    `json:"async,omitempty"`
	Timeout     int      `json:"timeout,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
}

// InactivityConfig represents inactivity timeout configuration
type InactivityConfig struct {
	Duration    string   `json:"duration"`
	Command     string   `json:"command"`
	Description string   `json:"description,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	ResetOn     []string `json:"reset_on,omitempty"`
}

// InheritanceConfig controls how actions are inherited from parent directories
type InheritanceConfig struct {
	Mode string `json:"mode"` // "merge", "override", or "disable"
}

// ActionContext holds context information for action execution
type ActionContext struct {
	FilePath     string
	MetadataPath string
	BucketName   string
	BucketPath   string
	ObjectKey    string
	ContentType  string
	ETag         string
	Size         int64
}

// InactivityTracker tracks activity per bucket for inactivity timeouts
type InactivityTracker struct {
	mu           sync.RWMutex
	lastActivity map[string]time.Time
	timers       map[string]*time.Timer
	configs      map[string]*InactivityConfig
}

var inactivityTracker *InactivityTracker

// InitInactivityTracker initializes the global inactivity tracker.
// Double-init guard: if already initialized, keep the existing tracker.
func InitInactivityTracker() {
	if inactivityTracker != nil {
		return
	}
	inactivityTracker = &InactivityTracker{
		lastActivity: make(map[string]time.Time),
		timers:       make(map[string]*time.Timer),
		configs:      make(map[string]*InactivityConfig),
	}
}

// recordActivity records activity for a bucket and resets inactivity timer
func (t *InactivityTracker) recordActivity(bucketPath, activityType string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastActivity[bucketPath] = time.Now()

	config, exists := t.configs[bucketPath]
	if !exists {
		return
	}

	// Check if this activity type should reset the timer
	if len(config.ResetOn) > 0 {
		shouldReset := slices.Contains(config.ResetOn, activityType)
		if !shouldReset {
			return
		}
	}

	// Reset the timer (stop, then drain the channel if the stop raced a fire)
	if timer, ok := t.timers[bucketPath]; ok {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}

	duration, err := parseDuration(config.Duration)
	if err != nil {
		log.Printf("Error parsing inactivity duration for %s: %v", bucketPath, err)
		return
	}

	t.timers[bucketPath] = time.AfterFunc(duration, func() {
		t.executeInactivityAction(bucketPath, config)
	})
}

// executeInactivityAction runs the inactivity command
func (t *InactivityTracker) executeInactivityAction(bucketPath string, config *InactivityConfig) {
	if config == nil || config.Command == "" {
		return
	}

	if config.Enabled != nil && !*config.Enabled {
		return
	}

	log.Printf("Executing inactivity action for %s: %s", bucketPath, config.Description)
	runCommand("inactivity", config.Command, 0, bucketPath)
}

// initializeForBucket sets up inactivity tracking for a bucket.
// Validation before registration: an invalid duration must not register
// the config entry.
func (t *InactivityTracker) initializeForBucket(bucketPath string, config *InactivityConfig) {
	if config == nil || config.Command == "" {
		return
	}

	if config.Enabled != nil && !*config.Enabled {
		return
	}

	duration, err := parseDuration(config.Duration)
	if err != nil {
		log.Printf("Error parsing inactivity duration for %s: %v", bucketPath, err)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.configs[bucketPath] = config
	t.lastActivity[bucketPath] = time.Now()

	t.timers[bucketPath] = time.AfterFunc(duration, func() {
		t.executeInactivityAction(bucketPath, config)
	})

	log.Printf("Initialized inactivity timer for %s: %s", bucketPath, config.Duration)
}

// parseDuration parses duration strings like "30m", "1h", "7d"
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}

	unit := s[len(s)-1]
	valueStr := s[:len(s)-1]
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("invalid duration value: %s", s)
	}

	switch unit {
	case 's':
		return time.Duration(value) * time.Second, nil
	case 'm':
		return time.Duration(value) * time.Minute, nil
	case 'h':
		return time.Duration(value) * time.Hour, nil
	case 'd':
		return time.Duration(value) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid duration unit: %c", unit)
	}
}

// stripJSON5Comments removes // and /* */ comments from JSON5 content.
// Decomposed into per-state copiers (leaf 1.2) so each stays under the
// gocognit ceiling; behavior is byte-identical to the previous single loop.
func stripJSON5Comments(data []byte) []byte {
	var result bytes.Buffer
	inDouble := false
	inSingle := false
	inLineComment := false
	inBlockComment := false
	i := 0

	for i < len(data) {
		if inLineComment {
			i = copyLineCommentTail(data, i, &result)
			inLineComment = false
			continue
		}

		if inBlockComment {
			var closed bool
			i, closed = copyBlockCommentTail(data, i, &result)
			if closed {
				inBlockComment = false
			}
			continue
		}

		if inDouble {
			var closed bool
			i, closed = copyDoubleQuotedChar(data, i, &result)
			if closed {
				inDouble = false
			}
			continue
		}

		if inSingle {
			var closed bool
			i, closed = copySingleQuotedChar(data, i, &result)
			if closed {
				inSingle = false
			}
			continue
		}

		// Check for start of comments
		if i+1 < len(data) {
			if data[i] == '/' && data[i+1] == '/' {
				inLineComment = true
				i += 2
				continue
			}
			if data[i] == '/' && data[i+1] == '*' {
				inBlockComment = true
				i += 2
				continue
			}
		}

		// Check for string start
		switch data[i] {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		}

		result.WriteByte(data[i])
		i++
	}

	return result.Bytes()
}

// copyLineCommentTail consumes a line comment from i, writing only the
// terminating newline (preserved for line-number accuracy). Returns the
// index just past the newline (or end of data).
func copyLineCommentTail(data []byte, i int, result *bytes.Buffer) int {
	for i < len(data) {
		if data[i] == '\n' {
			result.WriteByte('\n')
			return i + 1
		}
		i++
	}
	return i
}

// copyBlockCommentTail consumes a block comment from i, preserving newlines
// for line-number accuracy. Returns the index past the closing */ (or end
// of data) and whether the comment was closed.
func copyBlockCommentTail(data []byte, i int, result *bytes.Buffer) (int, bool) {
	for i < len(data) {
		if i+1 < len(data) && data[i] == '*' && data[i+1] == '/' {
			return i + 2, true
		}
		if data[i] == '\n' {
			result.WriteByte('\n')
		}
		i++
	}
	return i, false
}

// copyDoubleQuotedChar copies one character of a double-quoted string from i,
// handling backslash escapes. Returns the next index and whether the string
// was closed by this character.
func copyDoubleQuotedChar(data []byte, i int, result *bytes.Buffer) (int, bool) {
	if data[i] == '\\' && i+1 < len(data) {
		result.WriteByte(data[i])
		result.WriteByte(data[i+1])
		return i + 2, false
	}
	if data[i] == '"' {
		result.WriteByte(data[i])
		return i + 1, true
	}
	result.WriteByte(data[i])
	return i + 1, false
}

// copySingleQuotedChar copies one character of a single-quoted JSON5 string
// from i, handling escapes. // and /* inside are string content. Returns the
// next index and whether the string was closed by this character.
func copySingleQuotedChar(data []byte, i int, result *bytes.Buffer) (int, bool) {
	if data[i] == '\\' && i+1 < len(data) {
		result.WriteByte(data[i])
		result.WriteByte(data[i+1])
		return i + 2, false
	}
	if data[i] == '\'' {
		result.WriteByte(data[i])
		return i + 1, true
	}
	result.WriteByte(data[i])
	return i + 1, false
}

// loadActionsFile loads and parses a .bucket-actions file. A missing file is
// not an error (actions are optional per directory): it returns (nil, nil),
// which callers already handle via the rootActions/childActions != nil check.
func loadActionsFile(path string) (*BucketActions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // missing file = no actions configured; intentional and documented.
		}
		return nil, err
	}

	// Strip JSON5 comments
	cleanData := stripJSON5Comments(data)

	var actions BucketActions
	if err := json.Unmarshal(cleanData, &actions); err != nil {
		return nil, fmt.Errorf("error parsing %s: %w", path, err)
	}

	return &actions, nil
}

// loadActionsForPath loads and merges actions from bucket root to object path
func loadActionsForPath(bucketPath, objectKey string) *BucketActions {
	var merged *BucketActions

	// Load bucket root actions
	rootActionsPath := filepath.Join(bucketPath, actionsFileName)
	rootActions, err := loadActionsFile(rootActionsPath)
	if err != nil {
		log.Printf("Error loading actions from %s: %v", rootActionsPath, err)
	} else if rootActions != nil {
		merged = rootActions
	}

	// If no object key, just return bucket root actions
	if objectKey == "" {
		return merged
	}

	// Walk through directory hierarchy
	parts := strings.Split(objectKey, "/")
	currentPath := bucketPath

	for i := range len(parts) - 1 { // Exclude the file name itself
		currentPath = filepath.Join(currentPath, parts[i])
		actionsPath := filepath.Join(currentPath, actionsFileName)

		childActions, err := loadActionsFile(actionsPath)
		if err != nil {
			log.Printf("Error loading actions from %s: %v", actionsPath, err)
			continue
		}

		if childActions != nil {
			merged = mergeActions(merged, childActions)
		}
	}

	return merged
}

// mergeActions merges parent and child actions based on inheritance mode
func mergeActions(parent, child *BucketActions) *BucketActions {
	if child == nil {
		return parent
	}
	if parent == nil {
		return child
	}

	// Determine inheritance mode
	mode := "merge" // default
	if child.Inheritance != nil && child.Inheritance.Mode != "" {
		mode = child.Inheritance.Mode
	}

	switch mode {
	case "override":
		return child
	case "disable":
		// "disable" means no actions run in this subtree: return an EMPTY
		// result so neither parent nor child actions execute.
		return &BucketActions{
			Version:     child.Version,
			Inheritance: child.Inheritance,
		}
	case "merge":
		fallthrough
	default:
		return mergeActionLists(parent, child)
	}
}

// mergeActionLists merges action lists from parent and child
func mergeActionLists(parent, child *BucketActions) *BucketActions {
	result := &BucketActions{
		Version:           child.Version,
		Inheritance:       child.Inheritance,
		InactivityTimeout: child.InactivityTimeout,
	}

	if result.InactivityTimeout == nil {
		result.InactivityTimeout = parent.InactivityTimeout
	}

	// Merge after_upload actions
	result.AfterUpload = mergeActionSlice(parent.AfterUpload, child.AfterUpload)
	result.AfterDownload = mergeActionSlice(parent.AfterDownload, child.AfterDownload)
	result.AfterDelete = mergeActionSlice(parent.AfterDelete, child.AfterDelete)

	return result
}

// mergeActionSlice merges parent and child action slices, child overrides by name
func mergeActionSlice(parent, child []ActionConfig) []ActionConfig {
	if len(child) == 0 {
		return parent
	}
	if len(parent) == 0 {
		return child
	}

	// Create map of child actions by name
	childMap := make(map[string]ActionConfig)
	for _, action := range child {
		if action.Name != "" {
			childMap[action.Name] = action
		}
	}

	// Start with parent actions, override if child has same name
	var result []ActionConfig
	seenNames := make(map[string]bool)

	for _, action := range parent {
		if childAction, exists := childMap[action.Name]; exists {
			result = append(result, childAction)
			seenNames[action.Name] = true
		} else {
			result = append(result, action)
			seenNames[action.Name] = true
		}
	}

	// Add any child actions not in parent
	for _, action := range child {
		if !seenNames[action.Name] {
			result = append(result, action)
		}
	}

	return result
}

// triggerActions triggers actions for a specific event type
func triggerActions(eventType string, ctx ActionContext) {
	actions := loadActionsForPath(ctx.BucketPath, ctx.ObjectKey)
	if actions == nil {
		return
	}

	var actionList []ActionConfig
	switch eventType {
	case "after_upload":
		actionList = actions.AfterUpload
	case "after_download":
		actionList = actions.AfterDownload
	case "after_delete":
		actionList = actions.AfterDelete
	default:
		log.Printf("Unknown action event type: %s", eventType)
		return
	}

	for _, action := range actionList {
		executeAction(action, ctx)
	}

	// Record activity for inactivity tracking
	if inactivityTracker != nil {
		activityType := strings.TrimPrefix(eventType, "after_")
		inactivityTracker.recordActivity(ctx.BucketPath, activityType)
	}
}

// executeAction executes a single action if it matches the object
func executeAction(action ActionConfig, ctx ActionContext) {
	// Check if action is enabled
	if action.Enabled != nil && !*action.Enabled {
		return
	}

	// Check pattern matching
	if len(action.Patterns) > 0 {
		if !matchesAnyPattern(ctx.ObjectKey, action.Patterns) {
			return
		}
	}

	// Substitute variables in command
	cmd := substituteVariables(action.Command, ctx)

	// Determine if async (default true)
	async := true
	if action.Async != nil {
		async = *action.Async
	}

	if async {
		go runCommand(action.Name, cmd, action.Timeout, ctx.BucketPath)
	} else {
		runCommand(action.Name, cmd, action.Timeout, ctx.BucketPath)
	}
}

// shellQuote quotes s for safe POSIX shell use: wraps in single quotes and
// escapes embedded single quotes using the standard quote-arrow sequence.
// An empty string becomes two single quotes. This is
// the real control for command injection (gosec G204's exclusion of
// exec-with-variable is secondary): substituted values can no longer break
// out of quoting, even if the admin-configured command template wraps the
// variable in its own quotes. Command templates should reference the variable
// unquoted; double-quoting it now passes extra literal quote characters
// (the safe direction).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// substituteVariables replaces $VAR placeholders with actual values in a
// SINGLE pass: the command is scanned left to right and matched tokens are
// replaced with their shell-quoted values, so text inside a substituted
// value is never re-scanned (no re-expansion) and results are deterministic
// (fixed longest-name-first match order, not map iteration).
func substituteVariables(cmd string, ctx ActionContext) string {
	type repl struct {
		name  string
		value string
	}
	replacements := []repl{
		{"$METADATA_PATH", ctx.MetadataPath},
		{"$BUCKET_PATH", ctx.BucketPath},
		{"$BUCKET_NAME", ctx.BucketName},
		{"$FILE_PATH", ctx.FilePath},
		{"$OBJECT_KEY", ctx.ObjectKey},
		{"$CONTENT_TYPE", ctx.ContentType},
		{"$ETAG", ctx.ETag},
		{"$SIZE", strconv.FormatInt(ctx.Size, 10)},
	}
	// Longest names first so distinct-prefix overlap can never mis-match.
	sort.Slice(replacements, func(i, j int) bool {
		return len(replacements[i].name) > len(replacements[j].name)
	})

	var b strings.Builder
	b.Grow(len(cmd))
	i := 0
	for i < len(cmd) {
		matched := false
		for _, r := range replacements {
			if strings.HasPrefix(cmd[i:], r.name) {
				b.WriteString(shellQuote(r.value))
				i += len(r.name)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(cmd[i])
			i++
		}
	}
	return b.String()
}

// matchesAnyPattern checks if the object key matches any of the glob patterns
func matchesAnyPattern(objectKey string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := filepath.Match(pattern, objectKey)
		if err != nil {
			log.Printf("Invalid glob pattern '%s': %v", pattern, err)
			continue
		}
		if matched {
			return true
		}

		// Also try matching just the filename
		filename := filepath.Base(objectKey)
		matched, err = filepath.Match(pattern, filename)
		if err == nil && matched {
			return true
		}

		// Try matching with path glob (for patterns like "ephemeral/*")
		if matchPathGlob(objectKey, pattern) {
			return true
		}
	}
	return false
}

// matchPathGlob handles path-based glob patterns like "ephemeral/*"
func matchPathGlob(path, pattern string) bool {
	// Handle ** for recursive matching
	if strings.Contains(pattern, "**") {
		// Split on ** and regexp.QuoteMeta EVERY literal segment so
		// metacharacters like ( ) [ ] . match literally; ** becomes .*
		segments := strings.Split(pattern, "**")
		var b strings.Builder
		for i, seg := range segments {
			b.WriteString(regexp.QuoteMeta(seg))
			if i < len(segments)-1 {
				b.WriteString(".*")
			}
		}
		regexPattern := "^" + b.String() + "$"

		re, err := regexp.Compile(regexPattern)
		if err != nil {
			// With QuoteMeta this is rare; log and fail closed.
			log.Printf("Invalid path glob pattern '%s': %v", pattern, err)
			return false
		}
		return re.MatchString(path)
	}

	// Simple path glob matching: anchored — the pattern must consume the
	// ENTIRE path, so part counts must be equal and every part must match.
	patternParts := strings.Split(pattern, "/")
	pathParts := strings.Split(path, "/")

	if len(patternParts) != len(pathParts) {
		return false
	}

	for i, patternPart := range patternParts {
		matched, err := filepath.Match(patternPart, pathParts[i])
		if err != nil || !matched {
			return false
		}
	}

	return true
}

// limitBuffer is a bytes.Buffer wrapper that caps captured output at capBytes;
// on overflow it appends "\n...[truncated]" and discards the rest.
type limitBuffer struct {
	buf      bytes.Buffer
	capBytes int
}

const commandOutputCap = 1 << 20 // 1MB per stream

func newLimitBuffer() *limitBuffer {
	return &limitBuffer{capBytes: commandOutputCap}
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if l.buf.Len() >= l.capBytes {
		// Discard beyond the cap but report full acceptance so the
		// underlying exec copy does not error out.
		return len(p), nil
	}
	room := l.capBytes - l.buf.Len()
	if len(p) > room {
		l.buf.Write(p[:room])
		l.buf.WriteString("\n...[truncated]")
		return len(p), nil
	}
	return l.buf.Write(p)
}

func (l *limitBuffer) String() string { return l.buf.String() }
func (l *limitBuffer) Len() int       { return l.buf.Len() }

// runCommand executes a shell command with a timeout. A timeout <= 0 uses a
// 30s default (previously unlimited). The command runs in its own process
// group (Setpgid); on timeout the whole group is SIGKILLed so grandchildren
// spawned by sh cannot outlive the timeout.
func runCommand(name, cmd string, timeout int, workDir string) {
	log.Printf("Executing action '%s': %s", name, cmd)

	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	execCmd := exec.CommandContext(ctx, "sh", "-c", cmd)
	execCmd.Dir = workDir
	// New process group; safe on darwin+linux (Setpgid is in syscall for both).
	execCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, stderr := newLimitBuffer(), newLimitBuffer()
	execCmd.Stdout = stdout
	execCmd.Stderr = stderr

	startTime := time.Now()
	err := execCmd.Run()
	elapsed := time.Since(startTime)

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			// Kill the entire process group so shell grandchildren die too.
			if execCmd.Process != nil {
				if killErr := syscall.Kill(-execCmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
					log.Printf("Action '%s': failed to kill process group: %v", name, killErr)
				}
			}
			log.Printf("Action '%s' timed out after %d seconds", name, timeout)
		} else {
			log.Printf("Action '%s' failed after %v: %v\nStderr: %s", name, elapsed, err, stderr.String())
		}
		return
	}

	if stdout.Len() > 0 {
		log.Printf("Action '%s' completed in %v\nStdout: %s", name, elapsed, stdout.String())
	} else {
		log.Printf("Action '%s' completed in %v", name, elapsed)
	}
}

// initializeInactivityTimers scans buckets and initializes inactivity timers
func initializeInactivityTimers() {
	if inactivityTracker == nil {
		return
	}

	// Scan custom buckets
	for bucketName, bucketPath := range serverConfig.Buckets {
		actionsPath := filepath.Join(bucketPath, actionsFileName)
		actions, err := loadActionsFile(actionsPath)
		if err != nil {
			log.Printf("Error loading actions for bucket %s: %v", bucketName, err)
			continue
		}
		if actions != nil && actions.InactivityTimeout != nil {
			inactivityTracker.initializeForBucket(bucketPath, actions.InactivityTimeout)
		}
	}

	// Scan auto-discovered buckets
	dirs, err := os.ReadDir(serverConfig.DataDir)
	if err != nil {
		log.Printf("Error reading data directory for inactivity timers: %v", err)
		return
	}

	for _, dir := range dirs {
		if strings.HasPrefix(dir.Name(), ".") {
			continue
		}

		fullPath := filepath.Join(serverConfig.DataDir, dir.Name())
		info, err := os.Stat(fullPath)
		if err != nil || !info.IsDir() {
			continue
		}

		// Skip if already handled as custom bucket
		if _, exists := serverConfig.Buckets[dir.Name()]; exists {
			continue
		}

		actionsPath := filepath.Join(fullPath, actionsFileName)
		actions, err := loadActionsFile(actionsPath)
		if err != nil {
			log.Printf("Error loading actions for bucket %s: %v", dir.Name(), err)
			continue
		}
		if actions != nil && actions.InactivityTimeout != nil {
			inactivityTracker.initializeForBucket(fullPath, actions.InactivityTimeout)
		}
	}
}
