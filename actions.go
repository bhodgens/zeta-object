package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// InitInactivityTracker initializes the global inactivity tracker
func InitInactivityTracker() {
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
		shouldReset := false
		for _, resetType := range config.ResetOn {
			if resetType == activityType {
				shouldReset = true
				break
			}
		}
		if !shouldReset {
			return
		}
	}

	// Reset the timer
	if timer, ok := t.timers[bucketPath]; ok {
		timer.Stop()
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

// initializeForBucket sets up inactivity tracking for a bucket
func (t *InactivityTracker) initializeForBucket(bucketPath string, config *InactivityConfig) {
	if config == nil || config.Command == "" {
		return
	}

	if config.Enabled != nil && !*config.Enabled {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.configs[bucketPath] = config
	t.lastActivity[bucketPath] = time.Now()

	duration, err := parseDuration(config.Duration)
	if err != nil {
		log.Printf("Error parsing inactivity duration for %s: %v", bucketPath, err)
		return
	}

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

// stripJSON5Comments removes // and /* */ comments from JSON5 content
func stripJSON5Comments(data []byte) []byte {
	var result bytes.Buffer
	inString := false
	inLineComment := false
	inBlockComment := false
	i := 0

	for i < len(data) {
		if inLineComment {
			if data[i] == '\n' {
				inLineComment = false
				result.WriteByte('\n')
			}
			i++
			continue
		}

		if inBlockComment {
			if i+1 < len(data) && data[i] == '*' && data[i+1] == '/' {
				inBlockComment = false
				i += 2
				continue
			}
			// Preserve newlines in block comments for line number accuracy
			if data[i] == '\n' {
				result.WriteByte('\n')
			}
			i++
			continue
		}

		if inString {
			if data[i] == '\\' && i+1 < len(data) {
				result.WriteByte(data[i])
				result.WriteByte(data[i+1])
				i += 2
				continue
			}
			if data[i] == '"' {
				inString = false
			}
			result.WriteByte(data[i])
			i++
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
		if data[i] == '"' {
			inString = true
		}

		result.WriteByte(data[i])
		i++
	}

	return result.Bytes()
}

// loadActionsFile loads and parses a .bucket-actions file
func loadActionsFile(path string) (*BucketActions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
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

	for i := 0; i < len(parts)-1; i++ { // Exclude the file name itself
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
		return child // Only use child's actions, ignore parent
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

// substituteVariables replaces $VAR placeholders with actual values
func substituteVariables(cmd string, ctx ActionContext) string {
	replacements := map[string]string{
		"$FILE_PATH":     ctx.FilePath,
		"$METADATA_PATH": ctx.MetadataPath,
		"$BUCKET_NAME":   ctx.BucketName,
		"$BUCKET_PATH":   ctx.BucketPath,
		"$OBJECT_KEY":    ctx.ObjectKey,
		"$CONTENT_TYPE":  ctx.ContentType,
		"$ETAG":          ctx.ETag,
		"$SIZE":          strconv.FormatInt(ctx.Size, 10),
	}

	result := cmd
	for varName, value := range replacements {
		result = strings.ReplaceAll(result, varName, value)
	}

	return result
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
		// Escape regex meta chars, then convert glob to regex
		regexPattern := strings.ReplaceAll(pattern, ".", "\\.")
		// Replace ** with placeholder first to avoid double-processing *
		const placeholder = "\x00"
		regexPattern = strings.ReplaceAll(regexPattern, "**", placeholder)
		regexPattern = strings.ReplaceAll(regexPattern, "*", "[^/]*")
		regexPattern = strings.ReplaceAll(regexPattern, placeholder, ".*")
		regexPattern = "^" + regexPattern + "$"

		matched, err := regexp.MatchString(regexPattern, path)
		if err != nil {
			return false
		}
		return matched
	}

	// Simple path glob matching
	patternParts := strings.Split(pattern, "/")
	pathParts := strings.Split(path, "/")

	if len(patternParts) > len(pathParts) {
		return false
	}

	for i, patternPart := range patternParts {
		if i >= len(pathParts) {
			return false
		}
		matched, err := filepath.Match(patternPart, pathParts[i])
		if err != nil || !matched {
			return false
		}
	}

	return true
}

// runCommand executes a shell command with optional timeout
func runCommand(name, cmd string, timeout int, workDir string) {
	log.Printf("Executing action '%s': %s", name, cmd)

	var ctx context.Context
	var cancel context.CancelFunc

	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()
	} else {
		ctx = context.Background()
	}

	execCmd := exec.CommandContext(ctx, "sh", "-c", cmd)
	execCmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	startTime := time.Now()
	err := execCmd.Run()
	elapsed := time.Since(startTime)

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
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
