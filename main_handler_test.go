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
	env.origConfig = *serverConfig()
	setServerConfig(ServerConfig{
		DataDir: tmpDir + "/",
		Buckets: make(map[string]string),
	})

	t.Cleanup(func() {
		setServerConfig(env.origConfig)
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

// Leaf 2.4: an existing directory at the bucket path is a bucket we already
// own — single-user server returns 409 BucketAlreadyOwnedByYou (real S3
// returns this outside us-east-1).
func TestCreateBucketHandler_Idempotent(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket", nil)
	createBucketHandler(w, req, "test-bucket")

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 BucketAlreadyOwnedByYou for existing bucket, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketAlreadyOwnedByYou") {
		t.Errorf("expected BucketAlreadyOwnedByYou in body, got: %s", w.Body.String())
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
	for i := range 5 {
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

	// Upload parts. Part 1 is padded to the 5MiB non-final minimum
	// (EntityTooSmall rule, leaf 3.3); part 2 is the small final part.
	bigPart := "Hello " + strings.Repeat("x", minPartSize)
	for i, partContent := range []string{bigPart, "World!"} {
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
	wantAssembled := bigPart + "World!"
	if string(assembled) != wantAssembled {
		t.Errorf("assembled content = %d bytes, want %d bytes", len(assembled), len(wantAssembled))
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

	for i := range goroutines {
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

	for range goroutines {
		if err := <-errCh; err != nil {
			t.Error(err)
		}
	}

	// Verify all objects exist
	for i := range goroutines {
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
	for b.Loop() {
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
	for b.Loop() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/bench-bucket/bench.txt", nil)
		getObjectHandler(w, req, "bench-bucket", "bench.txt")
	}
}

// ============================================================
// Leaf 2.4 — handler semantics + XML conformance tests
// ============================================================

// ---- Fix 2: validateObjectKey traversal rejection ----

func TestValidateObjectKey_Traversal(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"dotdot escape", "../escape"},
		{"dotdot mid-path", "a/../../x"},
		{"bare dotdot", ".."},
		{"trailing dotdot", "a/b/.."},
		{"metadata segment", ".metadata/steal"},
		{"metadata mid-path", "a/.metadata/b"},
		{"metadata suffix segment", ".metadata/foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateObjectKey(tt.key); err == nil {
				t.Errorf("validateObjectKey(%q) = nil, want traversal rejection", tt.key)
			}
		})
	}

	valid := []string{"normal/key.txt", "a..b", "..hidden", "x.metadata"}
	for _, key := range valid {
		if err := validateObjectKey(key); err != nil {
			t.Errorf("validateObjectKey(%q) = %v, want nil (safe key)", key, err)
		}
	}
	// Leaf-spec divergence: "a/../b" normalizes inside the bucket, but the
	// spec's rule is "reject any segment == '..'" — safety wins.
	if err := validateObjectKey("a/../b"); err == nil {
		t.Error(`validateObjectKey("a/../b") = nil, want rejection (any ".." segment is rejected)`)
	}
}

func TestPutObjectHandler_TraversalRejected(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	for _, key := range []string{"../escape", "a/../../x", ".metadata/steal"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/test-bucket/"+key, strings.NewReader("data"))
		putObjectHandler(w, req, "test-bucket", key)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PUT %q: expected 400, got %d: %s", key, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "InvalidArgument") {
			t.Errorf("PUT %q: expected InvalidArgument, got: %s", key, w.Body.String())
		}
	}
	// Verify nothing escaped the bucket
	if _, err := os.Stat(filepath.Join(env.dataDir, "escape")); !os.IsNotExist(err) {
		t.Error("traversal key escaped the bucket directory")
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "test-bucket", ".metadata", "steal.meta")); !os.IsNotExist(err) {
		t.Error("traversal key wrote into .metadata")
	}
}

// ---- Fix 3: deleteObjectHandler missing bucket → 404 NoSuchBucket ----

func TestDeleteObjectHandler_NoSuchBucket(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/nonexistent/obj.txt", nil)
	deleteObjectHandler(w, req, "nonexistent", "obj.txt")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for delete on missing bucket, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("expected NoSuchBucket code, got: %s", w.Body.String())
	}
}

// ---- Fix 4: default Content-Type binary/octet-stream ----

func writeTestObjectRawMeta(t *testing.T, env *testEnv, bucket, key, content string, meta ObjectMetadata) {
	t.Helper()
	bucketPath := filepath.Join(env.dataDir, bucket)
	dataPath := filepath.Join(bucketPath, key)
	metadataPath := filepath.Join(bucketPath, ".metadata", key+".meta")
	if err := os.MkdirAll(filepath.Dir(dataPath), 0755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0755); err != nil {
		t.Fatalf("mkdir meta dir: %v", err)
	}
	if err := os.WriteFile(dataPath, []byte(content), 0644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	metaJSON, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metadataPath, metaJSON, 0644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}

func TestGetObjectHandler_DefaultContentType(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	writeTestObjectRawMeta(t, env, "test-bucket", "blob.bin", "raw bytes", ObjectMetadata{
		ContentType:   "",
		ContentLength: 9,
		ETag:          "abc",
		LastModified:  time.Now().UTC(),
		StoragePath:   filepath.Join(env.dataDir, "test-bucket", "blob.bin"),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/blob.bin", nil)
	getObjectHandler(w, req, "test-bucket", "blob.bin")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "binary/octet-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "binary/octet-stream")
	}
}

func TestHeadObjectHandler_DefaultContentType(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	writeTestObjectRawMeta(t, env, "test-bucket", "blob2.bin", "raw bytes", ObjectMetadata{
		ContentType:   "",
		ContentLength: 9,
		ETag:          "abc",
		LastModified:  time.Now().UTC(),
		StoragePath:   filepath.Join(env.dataDir, "test-bucket", "blob2.bin"),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/test-bucket/blob2.bin", nil)
	headObjectHandler(w, req, "test-bucket", "blob2.bin")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "binary/octet-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "binary/octet-stream")
	}
}

// ---- Fix 5: Content-Length serves actual file size ----

func TestGetObjectHandler_ContentLengthActualSize(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	// Data file on disk is 5 bytes but meta claims 100 (meta lie)
	content := "12345"
	writeTestObjectRawMeta(t, env, "test-bucket", "liar.bin", content, ObjectMetadata{
		ContentType:   "application/octet-stream",
		ContentLength: 100,
		ETag:          "abc",
		LastModified:  time.Now().UTC(),
		StoragePath:   filepath.Join(env.dataDir, "test-bucket", "liar.bin"),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/liar.bin", nil)
	getObjectHandler(w, req, "test-bucket", "liar.bin")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if cl := w.Header().Get("Content-Length"); cl != "5" {
		t.Errorf("Content-Length = %q, want %q (actual file size)", cl, "5")
	}
}

func TestHeadObjectHandler_ContentLengthActualSize(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	content := "1234567890"
	writeTestObjectRawMeta(t, env, "test-bucket", "liar2.bin", content, ObjectMetadata{
		ContentType:   "application/octet-stream",
		ContentLength: 1,
		ETag:          "abc",
		LastModified:  time.Now().UTC(),
		StoragePath:   filepath.Join(env.dataDir, "test-bucket", "liar2.bin"),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/test-bucket/liar2.bin", nil)
	headObjectHandler(w, req, "test-bucket", "liar2.bin")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if cl := w.Header().Get("Content-Length"); cl != "10" {
		t.Errorf("Content-Length = %q, want %q (actual file size)", cl, "10")
	}
}

// ---- Fix 6: corrupt/empty StoragePath fallback ----

func TestGetObjectHandler_EmptyStoragePathFallback(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	content := "fallback content"
	writeTestObjectRawMeta(t, env, "test-bucket", "fallback.txt", content, ObjectMetadata{
		ContentType:   "text/plain",
		ContentLength: int64(len(content)),
		ETag:          "abc",
		LastModified:  time.Now().UTC(),
		StoragePath:   "", // corrupt: empty → must fall back to bucketPath/key
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/fallback.txt", nil)
	getObjectHandler(w, req, "test-bucket", "fallback.txt")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with fallback path, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != content {
		t.Errorf("body = %q, want %q", w.Body.String(), content)
	}
}

func TestGetObjectHandler_EmptyStoragePathMissingData(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	// Meta has empty StoragePath AND no data file → 404 NoSuchKey, not 500
	writeTestObjectRawMeta(t, env, "test-bucket", "ghost.txt", "", ObjectMetadata{
		ContentType:   "text/plain",
		ContentLength: 0,
		ETag:          "abc",
		LastModified:  time.Now().UTC(),
		StoragePath:   "",
	})
	// Remove the data file so the fallback also misses
	if err := os.Remove(filepath.Join(env.dataDir, "test-bucket", "ghost.txt")); err != nil {
		t.Fatalf("remove data: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/ghost.txt", nil)
	getObjectHandler(w, req, "test-bucket", "ghost.txt")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 NoSuchKey, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchKey") {
		t.Errorf("expected NoSuchKey code, got: %s", w.Body.String())
	}
}

// ---- Fix 7: validBucket gate on bucket-level handlers ----

func TestValidBucket(t *testing.T) {
	setupTestEnv(t)
	if validBucket("../escape") {
		t.Error("validBucket('../escape') = true, want false")
	}
	if validBucket(".") {
		t.Error("validBucket('.') = true, want false")
	}
	if validBucket("") {
		t.Error("validBucket('') = true, want false")
	}
}

func TestDeleteBucketHandler_InvalidName(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/..%2Fescape", nil)
	deleteBucketHandler(w, req, "../escape")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid bucket name on delete, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("expected InvalidArgument code, got: %s", w.Body.String())
	}
}

func TestHeadBucketHandler_InvalidName(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/..%2Fescape", nil)
	headBucketHandler(w, req, "../escape")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid bucket name on head, got %d", w.Code)
	}
}

func TestGetBucketLocationHandler_InvalidName(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/..%2Fescape?location", nil)
	getBucketLocationHandler(w, req, "../escape")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid bucket name on location, got %d", w.Code)
	}
}

func TestHeadBucketHandler_FileAtBucketPath(t *testing.T) {
	env := setupTestEnv(t)
	// A FILE (not dir) at the bucket path — headBucket must 404
	filePath := filepath.Join(env.dataDir, "not-a-bucket")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/not-a-bucket", nil)
	headBucketHandler(w, req, "not-a-bucket")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for file-at-bucket-path, got %d", w.Code)
	}
	_ = env
}

// ---- Fix 8: createBucket error semantics ----

func TestCreateBucketHandler_PathIsFile(t *testing.T) {
	env := setupTestEnv(t)
	// A FILE exists where the bucket would go
	filePath := filepath.Join(env.dataDir, "occupied")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/occupied", nil)
	createBucketHandler(w, req, "occupied")

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 BucketAlreadyExists for file at path, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketAlreadyExists") {
		t.Errorf("expected BucketAlreadyExists code, got: %s", w.Body.String())
	}
}

func TestCreateBucketHandler_CustomBucketMissingPath(t *testing.T) {
	env := setupTestEnv(t)
	env.serverConfigBuckets(t, map[string]string{"custom-bucket": filepath.Join(env.dataDir, "missing-dir")})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/custom-bucket", nil)
	createBucketHandler(w, req, "custom-bucket")

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for custom bucket with missing path, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketAlreadyExists") {
		t.Errorf("expected BucketAlreadyExists code, got: %s", w.Body.String())
	}
}

// ---- Fix 10: deleteBucket with in-flight multipart → 409 ----

func TestDeleteBucketHandler_InFlightMultipart(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// Simulate an in-flight multipart upload session
	uploadsDir := filepath.Join(env.dataDir, "test-bucket", ".metadata", ".uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatalf("mkdir uploads: %v", err)
	}
	uploadMeta := MultipartUpload{
		UploadID:  "test-upload-id",
		Key:       "big.bin",
		Initiated: time.Now().UTC(),
		Parts:     map[int]PartMetadata{},
	}
	data, _ := json.MarshalIndent(uploadMeta, "", "  ")
	if err := os.WriteFile(filepath.Join(uploadsDir, "test-upload-id.json"), data, 0644); err != nil {
		t.Fatalf("write upload meta: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/test-bucket", nil)
	deleteBucketHandler(w, req, "test-bucket")

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for bucket with in-flight multipart, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketNotEmpty") {
		t.Errorf("expected BucketNotEmpty code, got: %s", w.Body.String())
	}
}

// ---- Fix 11: XML conformance — prolog + xmlns ----

func TestErrorXMLHeaderAndXMLNS(t *testing.T) {
	w := httptest.NewRecorder()
	writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)

	body := w.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Errorf("error response missing xml.Header prolog, got: %q", body[:min(len(body), 80)])
	}
	// Real S3 error documents carry NO xmlns: botocore's S3 error parser
	// requires a bare <Error> root (leaf-3.6 e2e finding). All other
	// response documents keep the namespace.
	if strings.Contains(body, `xmlns=`) {
		t.Errorf("error response must NOT carry xmlns (breaks botocore parsing), got: %s", body)
	}
	if !strings.Contains(body, "<Error>") {
		t.Errorf("error response root must be bare <Error>, got: %s", body)
	}
}

func TestListObjectsV2_XMLHeaderAndXMLNS(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	env.writeTestObject(t, "test-bucket", "x.txt", "x")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	body := w.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Errorf("list response missing xml.Header prolog, got: %q", body[:min(len(body), 80)])
	}
	if !strings.Contains(body, `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`) {
		t.Errorf("list response missing S3 xmlns, got: %s", body)
	}
}

func TestListBuckets_XMLHeaderAndXMLNS(t *testing.T) {
	setupTestEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	listBucketsHandler(w, req)

	body := w.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Errorf("list-buckets response missing xml.Header prolog, got: %q", body[:min(len(body), 80)])
	}
	if !strings.Contains(body, `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`) {
		t.Errorf("list-buckets response missing S3 xmlns, got: %s", body)
	}
}

func TestGetBucketLocation_XMLHeaderAndXMLNS(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?location", nil)
	getBucketLocationHandler(w, req, "test-bucket")

	body := w.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Errorf("location response missing xml.Header prolog, got: %q", body[:min(len(body), 80)])
	}
	if !strings.Contains(body, `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`) {
		t.Errorf("location response missing S3 xmlns, got: %s", body)
	}
}

// ---- Fix 12: delimiter roll-ups count toward maxKeys ----

func TestListObjectsV2_DelimiterCountsTowardMaxKeys(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	// 1 root object + 3 distinct prefixes
	env.writeTestObject(t, "test-bucket", "root.txt", "r")
	env.writeTestObject(t, "test-bucket", "a/1.txt", "1")
	env.writeTestObject(t, "test-bucket", "b/2.txt", "2")
	env.writeTestObject(t, "test-bucket", "c/3.txt", "3")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2&delimiter=/&max-keys=2", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ListBucketResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// KeyCount = entries emitted (contents + prefixes) must respect maxKeys=2
	if result.KeyCount > 2 {
		t.Errorf("KeyCount = %d, want <= 2 (delimiter roll-ups count toward maxKeys)", result.KeyCount)
	}
	total := len(result.Contents) + len(result.CommonPrefixes)
	if total > 2 {
		t.Errorf("emitted %d entries (contents=%d prefixes=%d), want <= 2", total, len(result.Contents), len(result.CommonPrefixes))
	}
	// With maxKeys=2 and 4 available entries, must be truncated with a token
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true")
	}
	if result.NextContinuationToken == "" {
		t.Error("expected NextContinuationToken when truncated")
	}
}

// ---- Fix 13: max-keys=0 → empty result ----

func TestListObjectsV2_MaxKeysZero(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	env.writeTestObject(t, "test-bucket", "a.txt", "a")
	env.writeTestObject(t, "test-bucket", "b/1.txt", "b1")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2&max-keys=0", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result ListBucketResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Contents) != 0 {
		t.Errorf("Contents length = %d, want 0", len(result.Contents))
	}
	if len(result.CommonPrefixes) != 0 {
		t.Errorf("CommonPrefixes length = %d, want 0", len(result.CommonPrefixes))
	}
	if result.KeyCount != 0 {
		t.Errorf("KeyCount = %d, want 0", result.KeyCount)
	}
	if result.IsTruncated {
		t.Error("IsTruncated = true, want false for max-keys=0")
	}
}

// ---- Fix 14: IsTruncated=true requires non-empty token ----

func TestListObjectsV2_IsTruncatedRequiresToken(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	// Exactly maxKeys objects: loop consumes all, no next page
	for i := range 3 {
		env.writeTestObject(t, "test-bucket", fmt.Sprintf("obj%d.txt", i), "x")
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2&max-keys=3", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	var result ListBucketResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.IsTruncated && result.NextContinuationToken == "" {
		t.Error("IsTruncated=true with empty NextContinuationToken is invalid; want IsTruncated=false")
	}
	if result.IsTruncated {
		t.Error("expected IsTruncated=false when all keys fit in maxKeys")
	}
}

// ---- Fix 15: encoding-type=url ----

func TestListObjectsV2_EncodingTypeURL(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")
	env.writeTestObject(t, "test-bucket", "file with space+plus.txt", "x")
	env.writeTestObject(t, "test-bucket", "dir with space/k.txt", "k")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?list-type=2&encoding-type=url&delimiter=/", nil)
	listObjectsV2Handler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ListBucketResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.EncodingType != "url" {
		t.Errorf("EncodingType = %q, want %q", result.EncodingType, "url")
	}
	foundKey := false
	for _, obj := range result.Contents {
		// S3 encoding-type=url uses RFC 3986 unreserved encoding: space is
		// %20 (never '+'), literal + is %2B, '/' stays literal (leaf-3.6
		// e2e finding).
		if obj.Key == "file%20with%20space%2Bplus.txt" {
			foundKey = true
		}
		if obj.Key == "file with space+plus.txt" {
			t.Error("Key not URL-encoded despite encoding-type=url")
		}
	}
	if !foundKey {
		t.Errorf("expected URL-encoded key 'file%%20with%%20space%%2Bplus.txt' in Contents, got: %+v", result.Contents)
	}
	if len(result.CommonPrefixes) != 1 {
		t.Fatalf("CommonPrefixes = %+v, want 1 entry", result.CommonPrefixes)
	}
	if result.CommonPrefixes[0].Prefix != "dir%20with%20space/" {
		t.Errorf("CommonPrefix = %q, want %q", result.CommonPrefixes[0].Prefix, "dir%20with%20space/")
	}
}

// ---- Fix 1: PUT uses atomic helpers (behavioral check: no tmp-file litter
// and data survives; helpers are exercised indirectly) ----

func TestPutObjectHandler_LeavesNoTempFiles(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket/atomic.txt", strings.NewReader("atomic data"))
	putObjectHandler(w, req, "test-bucket", "atomic.txt")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(env.dataDir, "test-bucket"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file litter left behind: %s", e.Name())
		}
	}
	// Metadata should exist and parse
	metaPath := filepath.Join(env.dataDir, "test-bucket", ".metadata", "atomic.txt.meta")
	metaJSON, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var meta ObjectMetadata
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		t.Fatalf("parse meta: %v", err)
	}
	if meta.StoragePath == "" {
		t.Error("meta.StoragePath empty after PUT")
	}
}

// ---- Cross-leaf wiring: VerifyDecodedLength in putObjectHandler ----

func TestPutObjectHandler_DecodedLengthMismatch(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// Chunked body decodes to 10 bytes; header claims 99 → 400
	chunked := "A;chunk-signature=abc123\r\nhello worl\r\n0\r\n\r\n"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket/chunked.bin", strings.NewReader(chunked))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", "99")
	putObjectHandler(w, req, "test-bucket", "chunked.bin")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for decoded-length mismatch, got %d: %s", w.Code, w.Body.String())
	}

	// Matching header → 200
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("PUT", "/test-bucket/chunked.bin", strings.NewReader(chunked))
	req2.Header.Set("Content-Encoding", "aws-chunked")
	req2.Header.Set("x-amz-decoded-content-length", "10")
	putObjectHandler(w2, req2, "test-bucket", "chunked.bin")

	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 for matching decoded-length, got %d: %s", w2.Code, w2.Body.String())
	}
	_ = env
}

// ---- testEnv helper for custom buckets ----

func (env *testEnv) serverConfigBuckets(t *testing.T, buckets map[string]string) {
	t.Helper()
	setServerConfigField(func(c *ServerConfig) { c.Buckets = buckets })
	_ = env
}

// TestXMLNamespaceConstantPinned pins s3XMLNamespace to the value hard-coded
// in the types.go XMLName tags, so the constant and the wire format cannot
// drift apart silently.
func TestXMLNamespaceConstantPinned(t *testing.T) {
	if s3XMLNamespace != "http://s3.amazonaws.com/doc/2006-03-01/" {
		t.Fatalf("s3XMLNamespace changed: %q — update types.go XMLName tags in lockstep", s3XMLNamespace)
	}
}
