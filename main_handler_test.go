package main

import (
	"crypto/md5"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// testEnv holds the test server configuration state
type testEnv struct {
	dataDir    string
	origConfig ServerConfig
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	env := &testEnv{dataDir: tmpDir}

	// Save original config
	env.origConfig = serverConfig
	serverConfig = ServerConfig{
		DataDir: tmpDir + "/",
		Buckets: make(map[string]string),
	}

	t.Cleanup(func() {
		serverConfig = env.origConfig
	})

	return env
}

// setupBucket creates a bucket and its directory structure for testing
func (env *testEnv) setupBucket(t *testing.T, bucketName string) string {
	t.Helper()
	bucketPath := filepath.Join(env.dataDir, bucketName)
	metadataPath := filepath.Join(bucketPath, ".metadata")
	if err := os.MkdirAll(metadataPath, 0755); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}
	return bucketPath
}

// writeTestObject writes an object directly to the filesystem for test setup
func (env *testEnv) writeTestObject(t *testing.T, bucketName, objectKey, content string) {
	t.Helper()
	bucketPath := filepath.Join(env.dataDir, bucketName)
	dataPath := filepath.Join(bucketPath, objectKey)
	metadataPath := filepath.Join(bucketPath, ".metadata", objectKey+".meta")

	if err := os.MkdirAll(filepath.Dir(dataPath), 0755); err != nil {
		t.Fatalf("Failed to create object dir: %v", err)
	}
	if err := os.WriteFile(dataPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write object: %v", err)
	}

	meta := ObjectMetadata{
		ContentType:   "text/plain",
		ContentLength: int64(len(content)),
		ETag:          fmt.Sprintf("%x", md5Hash([]byte(content))),
		LastModified:  fileModTime(t, dataPath),
		StoragePath:   dataPath,
	}
	metaJSON, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0755); err != nil {
		t.Fatalf("Failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(metadataPath, metaJSON, 0644); err != nil {
		t.Fatalf("Failed to write metadata: %v", err)
	}
}

func md5Hash(data []byte) [16]byte {
	return md5.Sum(data)
}

// ---- Bucket Handler Tests ----

func TestCreateBucketHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket", nil)

	createBucketHandler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify bucket directory exists
	bucketPath := filepath.Join(env.dataDir, "test-bucket")
	if info, err := os.Stat(bucketPath); err != nil || !info.IsDir() {
		t.Error("bucket directory was not created")
	}
	metadataPath := filepath.Join(bucketPath, ".metadata")
	if info, err := os.Stat(metadataPath); err != nil || !info.IsDir() {
		t.Error("metadata directory was not created")
	}
}

func TestCreateBucketHandler_InvalidName(t *testing.T) {
	setupTestEnv(t)

	tests := []struct {
		name       string
		bucketName string
		wantCode   int
	}{
		{"too short", "ab", http.StatusBadRequest},
		{"uppercase", "MyBucket", http.StatusBadRequest},
		{"starts with hyphen", "-bucket", http.StatusBadRequest},
		{"ends with hyphen", "bucket-", http.StatusBadRequest},
		{"ip address format", "192.168.1.1", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fresh env per subtest to avoid state issues
			env := setupTestEnv(t)
			w := httptest.NewRecorder()
			req := httptest.NewRequest("PUT", "/"+tt.bucketName, nil)
			createBucketHandler(w, req, tt.bucketName)

			if w.Code != tt.wantCode {
				t.Errorf("expected %d, got %d: %s", tt.wantCode, w.Code, w.Body.String())
			}
			_ = env
		})
	}
}

func TestCreateBucketHandler_Idempotent(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket", nil)
	createBucketHandler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for idempotent create, got %d", w.Code)
	}
}

func TestDeleteBucketHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/test-bucket", nil)
	deleteBucketHandler(w, req, "test-bucket")

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}

	// Verify bucket is gone
	if _, err := os.Stat(filepath.Join(env.dataDir, "test-bucket")); !os.IsNotExist(err) {
		t.Error("bucket should be deleted")
	}
}

func TestDeleteBucketHandler_NotEmpty(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// Create an object in the bucket (not .metadata)
	dataPath := filepath.Join(env.dataDir, "test-bucket", "some-file")
	if err := os.WriteFile(dataPath, []byte("hello"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/test-bucket", nil)
	deleteBucketHandler(w, req, "test-bucket")

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 BucketNotEmpty, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteBucketHandler_Nonexistent(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/nonexistent", nil)
	deleteBucketHandler(w, req, "nonexistent")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHeadBucketHandler(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/test-bucket", nil)
	headBucketHandler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHeadBucketHandler_Nonexistent(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/nonexistent", nil)
	headBucketHandler(w, req, "nonexistent")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestGetBucketLocationHandler(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?location", nil)
	getBucketLocationHandler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var loc LocationConstraint
	if err := xml.Unmarshal(w.Body.Bytes(), &loc); err != nil {
		t.Fatalf("Failed to parse response XML: %v", err)
	}
	// Empty location means us-east-1
	if loc.Location != "" {
		t.Errorf("expected empty location, got %q", loc.Location)
	}
}

// ---- Object Handler Tests ----

func TestPutObjectHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	body := "Hello, Mini-S3!"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket/hello.txt", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")

	putObjectHandler(w, req, "test-bucket", "hello.txt")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify ETag header
	etag := w.Header().Get("ETag")
	expectedETag := fmt.Sprintf(`"%x"`, md5Hash([]byte(body)))
	if etag != expectedETag {
		t.Errorf("ETag = %q, want %q", etag, expectedETag)
	}

	// Verify object data written
	dataPath := filepath.Join(env.dataDir, "test-bucket", "hello.txt")
	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("Failed to read object data: %v", err)
	}
	if string(data) != body {
		t.Errorf("object data = %q, want %q", string(data), body)
	}

	// Verify metadata written
	metaPath := filepath.Join(env.dataDir, "test-bucket", ".metadata", "hello.txt.meta")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("Failed to read metadata: %v", err)
	}
	var meta ObjectMetadata
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("Failed to parse metadata: %v", err)
	}
	if meta.ContentType != "text/plain" {
		t.Errorf("metadata ContentType = %q, want %q", meta.ContentType, "text/plain")
	}
	if meta.ContentLength != int64(len(body)) {
		t.Errorf("metadata ContentLength = %d, want %d", meta.ContentLength, len(body))
	}
}

func TestPutObjectHandler_NoSuchBucket(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/nonexistent/obj.txt", strings.NewReader("data"))

	putObjectHandler(w, req, "nonexistent", "obj.txt")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestPutObjectHandler_NestedPath(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	body := "nested content"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket/a/b/c/deep.txt", strings.NewReader(body))

	putObjectHandler(w, req, "test-bucket", "a/b/c/deep.txt")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify nested directories created
	dataPath := filepath.Join(env.dataDir, "test-bucket", "a", "b", "c", "deep.txt")
	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("Failed to read nested object: %v", err)
	}
	if string(data) != body {
		t.Errorf("object data = %q, want %q", string(data), body)
	}
}

func TestPutObjectHandler_InvalidKey(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	tests := []struct {
		name string
		key  string
	}{
		{"empty key", ""},
		{"too long key", strings.Repeat("a", 1025)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("PUT", "/test-bucket/"+tt.key, strings.NewReader("data"))
			putObjectHandler(w, req, "test-bucket", tt.key)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestGetObjectHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	content := "test content for download"
	env.writeTestObject(t, "test-bucket", "download.txt", content)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/download.txt", nil)
	getObjectHandler(w, req, "test-bucket", "download.txt")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != content {
		t.Errorf("body = %q, want %q", w.Body.String(), content)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/plain")
	}
	if cl := w.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(content)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(content))
	}
}

func TestGetObjectHandler_NoSuchKey(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/nonexistent.txt", nil)
	getObjectHandler(w, req, "test-bucket", "nonexistent.txt")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestGetObjectHandler_NoSuchBucket(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/nonexistent/obj.txt", nil)
	getObjectHandler(w, req, "nonexistent", "obj.txt")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHeadObjectHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	content := "head test content"
	env.writeTestObject(t, "test-bucket", "head.txt", content)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/test-bucket/head.txt", nil)
	headObjectHandler(w, req, "test-bucket", "head.txt")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if cl := w.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(content)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(content))
	}
	if w.Body.Len() != 0 {
		t.Error("HEAD response should have no body")
	}
}

func TestHeadObjectHandler_NoSuchKey(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/test-bucket/nonexistent.txt", nil)
	headObjectHandler(w, req, "test-bucket", "nonexistent.txt")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDeleteObjectHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	content := "to be deleted"
	env.writeTestObject(t, "test-bucket", "delete.txt", content)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/test-bucket/delete.txt", nil)
	deleteObjectHandler(w, req, "test-bucket", "delete.txt")

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify data file is gone
	dataPath := filepath.Join(env.dataDir, "test-bucket", "delete.txt")
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Error("object data file should be deleted")
	}
	// Verify metadata file is gone
	metaPath := filepath.Join(env.dataDir, "test-bucket", ".metadata", "delete.txt.meta")
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Error("metadata file should be deleted")
	}
}

func TestDeleteObjectHandler_Idempotent(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// Delete non-existent object should return 204 (S3 spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/test-bucket/nonexistent.txt", nil)
	deleteObjectHandler(w, req, "test-bucket", "nonexistent.txt")

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 for non-existent object, got %d", w.Code)
	}
}

// ---- Listing Tests ----

func TestListObjectsV2Handler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	env.writeTestObject(t, "test-bucket", "file1.txt", "content1")
	env.writeTestObject(t, "test-bucket", "file2.txt", "content2")
	env.writeTestObject(t, "test-bucket", "folder/file3.txt", "content3")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ListBucketResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("Failed to parse response: %v\nBody: %s", err, w.Body.String())
	}

	if result.KeyCount != 3 {
		t.Errorf("KeyCount = %d, want 3", result.KeyCount)
	}
	if len(result.Contents) != 3 {
		t.Errorf("Contents length = %d, want 3", len(result.Contents))
	}

	// Verify keys are sorted
	keys := make([]string, len(result.Contents))
	for i, obj := range result.Contents {
		keys[i] = obj.Key
	}
	if !sort.StringsAreSorted(keys) {
		t.Errorf("keys are not sorted: %v", keys)
	}
}

func TestListObjectsV2Handler_WithPrefix(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	env.writeTestObject(t, "test-bucket", "photos/img1.jpg", "img1")
	env.writeTestObject(t, "test-bucket", "photos/img2.jpg", "img2")
	env.writeTestObject(t, "test-bucket", "docs/doc1.txt", "doc1")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2&prefix=photos/", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result ListBucketResult
	xml.Unmarshal(w.Body.Bytes(), &result)

	if result.KeyCount != 2 {
		t.Errorf("KeyCount = %d, want 2", result.KeyCount)
	}
	for _, obj := range result.Contents {
		if !strings.HasPrefix(obj.Key, "photos/") {
			t.Errorf("unexpected key %q with prefix filter", obj.Key)
		}
	}
}

func TestListObjectsV2Handler_WithDelimiter(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	env.writeTestObject(t, "test-bucket", "folder/file1.txt", "c1")
	env.writeTestObject(t, "test-bucket", "folder/file2.txt", "c2")
	env.writeTestObject(t, "test-bucket", "root-file.txt", "c3")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2&delimiter=/", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result ListBucketResult
	xml.Unmarshal(w.Body.Bytes(), &result)

	// Should have 1 root object and 1 common prefix "folder/"
	if len(result.Contents) != 1 {
		t.Errorf("Contents length = %d, want 1 (root-file.txt)", len(result.Contents))
	}
	if len(result.CommonPrefixes) != 1 {
		t.Errorf("CommonPrefixes length = %d, want 1", len(result.CommonPrefixes))
	}
	if len(result.CommonPrefixes) > 0 && result.CommonPrefixes[0].Prefix != "folder/" {
		t.Errorf("CommonPrefix = %q, want %q", result.CommonPrefixes[0].Prefix, "folder/")
	}
}

func TestListObjectsV2Handler_Pagination(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	// Create 5 objects
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("obj%02d.txt", i)
		env.writeTestObject(t, "test-bucket", key, key)
	}

	// First page: max-keys=2
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2&max-keys=2", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result ListBucketResult
	xml.Unmarshal(w.Body.Bytes(), &result)

	if !result.IsTruncated {
		t.Error("expected IsTruncated=true")
	}
	if result.KeyCount != 2 {
		t.Errorf("first page KeyCount = %d, want 2", result.KeyCount)
	}
	if result.NextContinuationToken == "" {
		t.Error("expected NextContinuationToken")
	}

	// Second page: using continuation-token
	nextToken := result.NextContinuationToken
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET",
		fmt.Sprintf("/test-bucket?list-type=2&max-keys=2&continuation-token=%s", nextToken), nil)
	listObjectsV2Handler(w2, req2, "test-bucket")

	var result2 ListBucketResult
	xml.Unmarshal(w2.Body.Bytes(), &result2)

	if result2.KeyCount != 2 {
		t.Errorf("second page KeyCount = %d, want 2", result2.KeyCount)
	}
	// Should not contain objects from first page
	for _, obj := range result2.Contents {
		for _, firstObj := range result.Contents {
			if obj.Key == firstObj.Key {
				t.Errorf("second page contains duplicate key: %s", obj.Key)
			}
		}
	}
}

func TestListObjectsV2Handler_EmptyBucket(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result ListBucketResult
	xml.Unmarshal(w.Body.Bytes(), &result)

	if result.KeyCount != 0 {
		t.Errorf("KeyCount = %d, want 0", result.KeyCount)
	}
	if len(result.Contents) != 0 {
		t.Errorf("Contents length = %d, want 0", len(result.Contents))
	}
}

func TestListObjectsV2Handler_NoSuchBucket(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/nonexistent?list-type=2", nil)
	listObjectsV2Handler(w, req, "nonexistent")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestListBucketsHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bucket-one")
	env.setupBucket(t, "bucket-two")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	listBucketsHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ListAllMyBucketsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if len(result.Buckets.Bucket) != 2 {
		t.Errorf("bucket count = %d, want 2", len(result.Buckets.Bucket))
	}
	names := []string{result.Buckets.Bucket[0].Name, result.Buckets.Bucket[1].Name}
	sort.Strings(names)
	if names[0] != "bucket-one" || names[1] != "bucket-two" {
		t.Errorf("unexpected buckets: %v", names)
	}
}

// ---- Multipart Upload Tests ----

func TestInitiateMultipartUploadHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test-bucket/large.bin?uploads", nil)
	initiateMultipartUploadHandler(w, req, "test-bucket", "large.bin")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result InitiateMultipartUploadResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if result.UploadID == "" {
		t.Error("expected non-empty UploadID")
	}
	if result.Bucket != "test-bucket" {
		t.Errorf("Bucket = %q, want %q", result.Bucket, "test-bucket")
	}
	if result.Key != "large.bin" {
		t.Errorf("Key = %q, want %q", result.Key, "large.bin")
	}

	// Verify upload metadata on disk
	uploadsDir := filepath.Join(env.dataDir, "test-bucket", ".metadata", ".uploads")
	uploadMetaPath := filepath.Join(uploadsDir, result.UploadID+".json")
	if _, err := os.Stat(uploadMetaPath); err != nil {
		t.Errorf("upload metadata not found: %v", err)
	}
}

func TestUploadPartHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// First initiate upload
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test-bucket/large.bin?uploads", nil)
	initiateMultipartUploadHandler(w, req, "test-bucket", "large.bin")
	var initResult InitiateMultipartUploadResult
	xml.Unmarshal(w.Body.Bytes(), &initResult)

	// Upload part 1
	partData := "part one data here"
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("PUT",
		fmt.Sprintf("/test-bucket/large.bin?partNumber=1&uploadId=%s", initResult.UploadID),
		strings.NewReader(partData))
	uploadPartHandler(w2, req2, "test-bucket", "large.bin", "1", initResult.UploadID)

	if w2.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// Upload part 2
	partData2 := "part two data here - different"
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("PUT",
		fmt.Sprintf("/test-bucket/large.bin?partNumber=2&uploadId=%s", initResult.UploadID),
		strings.NewReader(partData2))
	uploadPartHandler(w3, req3, "test-bucket", "large.bin", "2", initResult.UploadID)

	if w3.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w3.Code, w3.Body.String())
	}

	// Verify upload metadata has both parts
	uploadsDir := filepath.Join(env.dataDir, "test-bucket", ".metadata", ".uploads")
	uploadMetaPath := filepath.Join(uploadsDir, initResult.UploadID+".json")
	metaJSON, _ := os.ReadFile(uploadMetaPath)
	var mpUpload MultipartUpload
	json.Unmarshal(metaJSON, &mpUpload)

	if len(mpUpload.Parts) != 2 {
		t.Errorf("Parts count = %d, want 2", len(mpUpload.Parts))
	}
}

func TestCompleteMultipartUploadHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// Initiate upload
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test-bucket/assembled.txt?uploads", nil)
	initiateMultipartUploadHandler(w, req, "test-bucket", "assembled.txt")
	var initResult InitiateMultipartUploadResult
	xml.Unmarshal(w.Body.Bytes(), &initResult)

	// Upload parts
	for i, partContent := range []string{"Hello ", "World!"} {
		partNum := i + 1
		w2 := httptest.NewRecorder()
		req2 := httptest.NewRequest("PUT",
			fmt.Sprintf("/test-bucket/assembled.txt?partNumber=%d&uploadId=%s", partNum, initResult.UploadID),
			strings.NewReader(partContent))
		uploadPartHandler(w2, req2, "test-bucket", "assembled.txt",
			fmt.Sprintf("%d", partNum), initResult.UploadID)
	}

	// Load part ETags for the complete request
	uploadsDir := filepath.Join(env.dataDir, "test-bucket", ".metadata", ".uploads")
	uploadMetaPath := filepath.Join(uploadsDir, initResult.UploadID+".json")
	metaJSON, _ := os.ReadFile(uploadMetaPath)
	var mpUpload MultipartUpload
	json.Unmarshal(metaJSON, &mpUpload)

	// Complete the upload
	completeXML := `<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUpload>
  <Part><PartNumber>1</PartNumber><ETag>"` + mpUpload.Parts[1].ETag + `"</ETag></Part>
  <Part><PartNumber>2</PartNumber><ETag>"` + mpUpload.Parts[2].ETag + `"</ETag></Part>
</CompleteMultipartUpload>`

	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST",
		fmt.Sprintf("/test-bucket/assembled.txt?uploadId=%s", initResult.UploadID),
		strings.NewReader(completeXML))
	completeMultipartUploadHandler(w3, req3, "test-bucket", "assembled.txt", initResult.UploadID)

	if w3.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w3.Code, w3.Body.String())
	}

	// Verify the assembled object
	dataPath := filepath.Join(env.dataDir, "test-bucket", "assembled.txt")
	assembled, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("Failed to read assembled object: %v", err)
	}
	if string(assembled) != "Hello World!" {
		t.Errorf("assembled content = %q, want %q", string(assembled), "Hello World!")
	}

	// Verify upload metadata cleaned up
	if _, err := os.Stat(uploadMetaPath); !os.IsNotExist(err) {
		t.Error("upload metadata should be cleaned up after completion")
	}
}

func TestAbortMultipartUploadHandler_Success(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// Initiate upload
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test-bucket/abort.txt?uploads", nil)
	initiateMultipartUploadHandler(w, req, "test-bucket", "abort.txt")
	var initResult InitiateMultipartUploadResult
	xml.Unmarshal(w.Body.Bytes(), &initResult)

	// Abort
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE",
		fmt.Sprintf("/test-bucket/abort.txt?uploadId=%s", initResult.UploadID), nil)
	abortMultipartUploadHandler(w2, req2, "test-bucket", "abort.txt", initResult.UploadID)

	if w2.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w2.Code, w2.Body.String())
	}

	// Verify upload metadata cleaned up
	uploadsDir := filepath.Join(env.dataDir, "test-bucket", ".metadata", ".uploads")
	uploadMetaPath := filepath.Join(uploadsDir, initResult.UploadID+".json")
	if _, err := os.Stat(uploadMetaPath); !os.IsNotExist(err) {
		t.Error("upload metadata should be deleted after abort")
	}
}

// ============================================================
// Error response format validation
// ============================================================
func TestErrorXMLFormat(t *testing.T) {
	result := errorToXML("NoSuchBucket", "The specified bucket does not exist.")

	if !strings.Contains(result, "<Code>NoSuchBucket</Code>") {
		t.Errorf("expected Code in XML, got: %s", result)
	}
	if !strings.Contains(result, "<Message>The specified bucket does not exist.</Message>") {
		t.Errorf("expected Message in XML, got: %s", result)
	}
}

// ---- Helper: Validate Bucket Name Edge Cases ----

func TestValidateBucketName_EdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"exactly 3 chars", "abc", true},
		{"exactly 63 chars", strings.Repeat("a", 63), true},
		{"64 chars", strings.Repeat("a", 64), false},
		{"2 chars", "ab", false},
		{"with dots and hyphens", "my-bucket.example.com", true},
		{"consecutive dots", "my..bucket", false},
		{"starts with number", "123-bucket", true},
		{"ends with number", "bucket-123", true},
		{"valid chars only", "my-bucket-1.test-2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBucketName(tt.input)
			if tt.valid && err != nil {
				t.Errorf("validateBucketName(%q) should be valid, got: %v", tt.input, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("validateBucketName(%q) should be invalid", tt.input)
			}
		})
	}
}

// ---- Handler Test Helper ----

func fileModTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime().UTC()
}

// ---- Robustness Tests ----

func TestConcurrentPutObject_DifferentKeys(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	const goroutines = 10
	errCh := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			key := fmt.Sprintf("concurrent-%d.txt", idx)
			body := fmt.Sprintf("goroutine %d", idx)
			w := httptest.NewRecorder()
			req := httptest.NewRequest("PUT", "/test-bucket/"+key, strings.NewReader(body))
			putObjectHandler(w, req, "test-bucket", key)
			if w.Code != http.StatusOK {
				errCh <- fmt.Errorf("goroutine %d: expected 200, got %d: %s", idx, w.Code, w.Body.String())
				return
			}
			errCh <- nil
		}(i)
	}

	for i := 0; i < goroutines; i++ {
		if err := <-errCh; err != nil {
			t.Error(err)
		}
	}

	// Verify all objects exist
	for i := 0; i < goroutines; i++ {
		key := fmt.Sprintf("concurrent-%d.txt", i)
		path := filepath.Join(env.dataDir, "test-bucket", key)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("object %s missing after concurrent upload", key)
		}
	}
}

// ---- DecodeAWSChunked Test ----

func TestDecodeAWSChunked(t *testing.T) {
	// Single chunk, properly terminated with 0\r\n (leaf 2.2: truncated
	// streams now error, so this case must include the final chunk).
	chunked := []byte("A;chunk-signature=abc123\r\nhello worl\r\n0\r\n\r\n") // 10 bytes

	decoded, err := decodeAWSChunked(chunked)
	if err != nil {
		t.Fatalf("decodeAWSChunked error: %v", err)
	}
	if string(decoded) != "hello worl" {
		t.Errorf("decoded = %q, want %q", string(decoded), "hello worl")
	}

	// Multi-chunk
	multiChunk := []byte("5;chunk-signature=abc\r\nhello\r\n5;chunk-signature=def\r\n worl\r\n0\r\n\r\n")
	decoded, err = decodeAWSChunked(multiChunk)
	if err != nil {
		t.Fatalf("decodeAWSChunked multi error: %v", err)
	}
	if string(decoded) != "hello worl" {
		t.Errorf("multi-chunk decoded = %q, want %q", string(decoded), "hello worl")
	}
}

// ---- Benchmark ----

func BenchmarkPutObject(b *testing.B) {
	env := setupTestEnv(&testing.T{})
	_ = env.setupBucket(&testing.T{}, "bench-bucket")
	body := strings.Repeat("x", 1024) // 1KB

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/bench-bucket/bench.txt", strings.NewReader(body))
		putObjectHandler(w, req, "bench-bucket", "bench.txt")
	}
}

func BenchmarkGetObject(b *testing.B) {
	env := setupTestEnv(&testing.T{})
	_ = env.setupBucket(&testing.T{}, "bench-bucket")
	content := strings.Repeat("x", 1024)
	env.writeTestObject(&testing.T{}, "bench-bucket", "bench.txt", content)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/bench-bucket/bench.txt", nil)
		getObjectHandler(w, req, "bench-bucket", "bench.txt")
	}
}
