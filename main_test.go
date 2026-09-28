package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test helper functions

func TestHashSHA256(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"hello", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
		{"test data", "916f0027a575074ce72a331777c3478d6513f786a591bd892da1a577bf2335f9"},
	}

	for _, tt := range tests {
		result := hashSHA256([]byte(tt.input))
		if result != tt.expected {
			t.Errorf("hashSHA256(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestHmacSHA256(t *testing.T) {
	key := []byte("secret")
	data := "message"
	result := hmacSHA256(key, data)
	if len(result) != 32 {
		t.Errorf("hmacSHA256 should return 32 bytes, got %d", len(result))
	}
}

func TestGetSigningKey(t *testing.T) {
	key := getSigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "s3")
	if len(key) != 32 {
		t.Errorf("getSigningKey should return 32 bytes, got %d", len(key))
	}
}

func TestErrorToXML(t *testing.T) {
	result := errorToXML("NoSuchBucket", "The specified bucket does not exist.")

	var s3Err S3Error
	if err := xml.Unmarshal([]byte(result), &s3Err); err != nil {
		t.Fatalf("Failed to unmarshal error XML: %v", err)
	}

	if s3Err.Code != "NoSuchBucket" {
		t.Errorf("Expected error code NoSuchBucket, got %s", s3Err.Code)
	}
	if s3Err.Message != "The specified bucket does not exist." {
		t.Errorf("Unexpected error message: %s", s3Err.Message)
	}
}

func TestParseInt(t *testing.T) {
	tests := []struct {
		input    string
		expected int
		hasError bool
	}{
		{"123", 123, false},
		{"0", 0, false},
		{"-1", -1, false},
		{"abc", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		result, err := parseInt(tt.input, "test")
		if tt.hasError {
			if err == nil {
				t.Errorf("parseInt(%q) should return error", tt.input)
			}
		} else {
			if err != nil {
				t.Errorf("parseInt(%q) returned unexpected error: %v", tt.input, err)
			}
			if result != tt.expected {
				t.Errorf("parseInt(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		}
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	// Test with existing env var
	os.Setenv("TEST_VAR", "test_value")
	defer os.Unsetenv("TEST_VAR")

	result := getEnvOrDefault("TEST_VAR", "default")
	if result != "test_value" {
		t.Errorf("Expected 'test_value', got '%s'", result)
	}

	// Test with non-existing env var
	result = getEnvOrDefault("NON_EXISTING_VAR", "default")
	if result != "default" {
		t.Errorf("Expected 'default', got '%s'", result)
	}
}

func TestValidateBucketName(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"mybucket", true},
		{"my-bucket", true},
		{"my.bucket", true},
		{"my-bucket-123", true},
		{"ab", false},                    // Too short
		{"a", false},                     // Too short
		{strings.Repeat("a", 64), false}, // Too long
		{"MyBucket", false},              // Uppercase
		{"-bucket", false},               // Starts with hyphen
		{"bucket-", false},               // Ends with hyphen
		{"bucket..name", false},          // Consecutive periods
		{"192.168.1.1", false},           // IP address format
	}

	for _, tt := range tests {
		err := validateBucketName(tt.name)
		if tt.valid && err != nil {
			t.Errorf("validateBucketName(%q) should be valid, got error: %v", tt.name, err)
		}
		if !tt.valid && err == nil {
			t.Errorf("validateBucketName(%q) should be invalid", tt.name)
		}
	}
}

func TestValidateObjectKey(t *testing.T) {
	tests := []struct {
		key   string
		valid bool
	}{
		{"mykey", true},
		{"path/to/object", true},
		{"file.txt", true},
		{"", false},                        // Empty
		{strings.Repeat("a", 1025), false}, // Too long
	}

	for _, tt := range tests {
		err := validateObjectKey(tt.key)
		if tt.valid && err != nil {
			t.Errorf("validateObjectKey(%q) should be valid, got error: %v", tt.key, err)
		}
		if !tt.valid && err == nil {
			t.Errorf("validateObjectKey(%q) should be invalid", tt.key)
		}
	}
}

// Integration tests with test server

// Test bucket creation (basic functionality test)
func TestCreateBucketValidation(t *testing.T) {
	tests := []struct {
		name         string
		bucketName   string
		expectStatus int
	}{
		{"valid bucket", "test-bucket", http.StatusOK},
		{"invalid bucket - too short", "ab", http.StatusBadRequest},
		{"invalid bucket - uppercase", "MyBucket", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp test directory
			tmpDir, err := os.MkdirTemp("", "minis3-test-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			// Test bucket name validation directly
			err = validateBucketName(tt.bucketName)
			if tt.expectStatus == http.StatusOK && err != nil {
				t.Errorf("Expected valid bucket name %s, got error: %v", tt.bucketName, err)
			}
			if tt.expectStatus == http.StatusBadRequest && err == nil {
				t.Errorf("Expected invalid bucket name %s to fail validation", tt.bucketName)
			}
		})
	}
}

// Test object metadata structure
func TestObjectMetadataJSON(t *testing.T) {
	meta := ObjectMetadata{
		ContentType:   "text/plain",
		ContentLength: 100,
		ETag:          "abc123",
		CustomMetadata: map[string]string{
			"x-amz-meta-custom": "value",
		},
		StoragePath: "/path/to/object",
	}

	jsonData, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Failed to marshal metadata: %v", err)
	}

	var decoded ObjectMetadata
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal metadata: %v", err)
	}

	if decoded.ContentType != meta.ContentType {
		t.Errorf("ContentType mismatch: got %s, want %s", decoded.ContentType, meta.ContentType)
	}
	if decoded.ContentLength != meta.ContentLength {
		t.Errorf("ContentLength mismatch: got %d, want %d", decoded.ContentLength, meta.ContentLength)
	}
	if decoded.ETag != meta.ETag {
		t.Errorf("ETag mismatch: got %s, want %s", decoded.ETag, meta.ETag)
	}
}

// Test XML response structures
func TestListAllMyBucketsResultXML(t *testing.T) {
	result := ListAllMyBucketsResult{
		Owner: Owner{ID: "test-id", DisplayName: "test-user"},
		Buckets: Buckets{
			Bucket: []Bucket{
				{Name: "bucket1", CreationDate: "2024-01-01T00:00:00.000Z"},
				{Name: "bucket2", CreationDate: "2024-01-02T00:00:00.000Z"},
			},
		},
	}

	xmlData, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal XML: %v", err)
	}

	if !strings.Contains(string(xmlData), "bucket1") {
		t.Error("XML should contain bucket1")
	}
	if !strings.Contains(string(xmlData), "bucket2") {
		t.Error("XML should contain bucket2")
	}
}

func TestListBucketResultXML(t *testing.T) {
	result := ListBucketResult{
		Name:        "test-bucket",
		Prefix:      "prefix/",
		MaxKeys:     1000,
		IsTruncated: false,
		Contents: []Object{
			{Key: "file1.txt", Size: 100, ETag: "\"abc123\"", StorageClass: "STANDARD"},
		},
		CommonPrefixes: []CommonPrefix{
			{Prefix: "folder/"},
		},
	}

	xmlData, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal XML: %v", err)
	}

	if !strings.Contains(string(xmlData), "test-bucket") {
		t.Error("XML should contain bucket name")
	}
	if !strings.Contains(string(xmlData), "file1.txt") {
		t.Error("XML should contain object key")
	}
}

// Test canonical URI generation
func TestGetCanonicalURI(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/", "/"},
		{"/bucket", "/bucket"},
		{"/bucket/object", "/bucket/object"},
		{"", "/"},
	}

	for _, tt := range tests {
		var req *http.Request
		if tt.path == "" {
			// Can't use NewRequest with empty path, so create with "/" and modify
			req = httptest.NewRequest("GET", "/", nil)
			req.URL.Path = ""
		} else {
			req = httptest.NewRequest("GET", tt.path, nil)
		}
		result := getCanonicalURI(req)
		if result != tt.expected {
			t.Errorf("getCanonicalURI(%q) = %q, want %q", tt.path, result, tt.expected)
		}
	}
}

// Test canonical query string generation
func TestGetCanonicalQueryString(t *testing.T) {
	tests := []struct {
		query    string
		expected string
	}{
		{"", ""},
		{"key=value", "key=value"},
		{"b=2&a=1", "a=1&b=2"},   // Should be sorted
		{"key=a%20b", "key=a+b"}, // Go's url.Values.Encode uses + for spaces
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", "/?"+tt.query, nil)
		result := getCanonicalQueryString(req)
		if result != tt.expected {
			t.Errorf("getCanonicalQueryString(%q) = %q, want %q", tt.query, result, tt.expected)
		}
	}
}

// Test multipart upload structures
func TestMultipartUploadJSON(t *testing.T) {
	upload := MultipartUpload{
		UploadID: "test-upload-id",
		Key:      "test-object",
		Parts: map[int]PartMetadata{
			1: {PartNumber: 1, ETag: "abc", Size: 100, StoredPath: "/tmp/part1"},
			2: {PartNumber: 2, ETag: "def", Size: 200, StoredPath: "/tmp/part2"},
		},
	}

	jsonData, err := json.Marshal(upload)
	if err != nil {
		t.Fatalf("Failed to marshal multipart upload: %v", err)
	}

	var decoded MultipartUpload
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal multipart upload: %v", err)
	}

	if len(decoded.Parts) != 2 {
		t.Errorf("Expected 2 parts, got %d", len(decoded.Parts))
	}
	if decoded.Parts[1].ETag != "abc" {
		t.Errorf("Part 1 ETag mismatch")
	}
}

// Test cleanupEmptyDirs helper
func TestCleanupEmptyDirs(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "minis3-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create nested empty directories
	nestedDir := filepath.Join(tmpDir, "a", "b", "c")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatalf("Failed to create nested dirs: %v", err)
	}

	// Clean up from deepest directory
	cleanupEmptyDirs(nestedDir, tmpDir)

	// Verify nested directories are removed
	if _, err := os.Stat(filepath.Join(tmpDir, "a")); !os.IsNotExist(err) {
		t.Error("Expected directory 'a' to be removed")
	}

	// Verify stop directory still exists
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error("Stop directory should not be removed")
	}
}

// Test complete multipart upload XML parsing
func TestCompleteMultipartUploadXML(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUpload>
  <Part>
    <PartNumber>1</PartNumber>
    <ETag>"abc123"</ETag>
  </Part>
  <Part>
    <PartNumber>2</PartNumber>
    <ETag>"def456"</ETag>
  </Part>
</CompleteMultipartUpload>`

	var complete CompleteMultipartUpload
	if err := xml.Unmarshal([]byte(xmlData), &complete); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if len(complete.Parts) != 2 {
		t.Errorf("Expected 2 parts, got %d", len(complete.Parts))
	}
	if complete.Parts[0].PartNumber != 1 {
		t.Errorf("Expected part number 1, got %d", complete.Parts[0].PartNumber)
	}
	if complete.Parts[0].ETag != "\"abc123\"" {
		t.Errorf("Expected ETag \"abc123\", got %s", complete.Parts[0].ETag)
	}
}

// Benchmark tests
func BenchmarkHashSHA256(b *testing.B) {
	data := bytes.Repeat([]byte("test"), 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hashSHA256(data)
	}
}

func BenchmarkHmacSHA256(b *testing.B) {
	key := []byte("secretkey")
	data := "test message"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hmacSHA256(key, data)
	}
}
