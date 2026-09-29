package s3

// moved_copy_range_test.go — ports the root package's range/conditional
// suite (range_conditional_test.go) and CopyObject/DeleteObjects suite
// (copy_batch_test.go) onto the s3 package's seams (leaf 6.2).

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mini-s3/internal/objectmodel"
)

// ---- range/conditional (port of range_conditional_test.go) ----

// setupRangeObject creates an env, bucket, and one 10-byte object "0123456789"
// modified at a fixed time. Returns the etag quoted as served by the server.
func setupRangeObject(t *testing.T) (*testS3Env, string, string) {
	t.Helper()
	env := setupS3TestEnv(t)
	env.setupBucket(t, "range-bucket")
	content := "0123456789"
	mod := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	writeRangeObject(t, env, "range-bucket", "data.bin", content, mod)
	etag := fmt.Sprintf("%q", fmt.Sprintf("%x", md5Hash([]byte(content))))
	return env, content, etag
}

// writeRangeObject writes the data file + sidecar at a legacy flat path with
// an explicit LastModified so conditional-request tests control time.
func writeRangeObject(t *testing.T, env *testS3Env, bucketName, objectKey, content string, mod time.Time) {
	t.Helper()
	bucketPath := filepath.Join(env.dataDir, bucketName)
	dataPath := filepath.Join(bucketPath, objectKey)
	metadataPath := filepath.Join(bucketPath, ".metadata", objectKey+".meta")

	if err := os.MkdirAll(filepath.Dir(dataPath), 0o755); err != nil {
		t.Fatalf("Failed to create object dir: %v", err)
	}
	if err := os.WriteFile(dataPath, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write object: %v", err)
	}
	meta := ObjectMetadata{
		ContentType:   "text/plain",
		ContentLength: int64(len(content)),
		ETag:          fmt.Sprintf("%x", md5Hash([]byte(content))),
		LastModified:  mod,
		StoragePath:   dataPath,
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal metadata: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatalf("Failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(metadataPath, metaJSON, 0o644); err != nil {
		t.Fatalf("Failed to write metadata: %v", err)
	}
}

func rangeGet(t *testing.T, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/range-bucket/data.bin", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	getObjectHandler(w, req, "range-bucket", "data.bin")
	return w
}

func rangeHead(t *testing.T, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("HEAD", "/range-bucket/data.bin", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	headObjectHandler(w, req, "range-bucket", "data.bin")
	return w
}

func TestGetObject_RangeStartEnd(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=2-5"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Errorf("expected Content-Range 'bytes 2-5/10', got %q", cr)
	}
	if got := w.Body.String(); got != content[2:6] {
		t.Errorf("expected body %q, got %q", content[2:6], got)
	}
	if cl := w.Header().Get("Content-Length"); cl != "4" {
		t.Errorf("expected Content-Length 4, got %q", cl)
	}
}

func TestGetObject_RangeOpenEnded(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=7-"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 7-9/10" {
		t.Errorf("expected Content-Range 'bytes 7-9/10', got %q", cr)
	}
	if got := w.Body.String(); got != content[7:] {
		t.Errorf("expected body %q, got %q", content[7:], got)
	}
}

func TestGetObject_RangeSuffix(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=-4"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 6-9/10" {
		t.Errorf("expected Content-Range 'bytes 6-9/10', got %q", cr)
	}
	if got := w.Body.String(); got != content[len(content)-4:] {
		t.Errorf("expected body %q, got %q", content[len(content)-4:], got)
	}
}

func TestGetObject_RangeExactEOF(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=9-9"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 9-9/10" {
		t.Errorf("expected Content-Range 'bytes 9-9/10', got %q", cr)
	}
	if got := w.Body.String(); got != content[9:] {
		t.Errorf("expected body %q, got %q", content[9:], got)
	}
}

func TestGetObject_RangeEndClamped(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=5-100"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 5-9/10" {
		t.Errorf("expected Content-Range 'bytes 5-9/10', got %q", cr)
	}
	if got := w.Body.String(); got != content[5:] {
		t.Errorf("expected body %q, got %q", content[5:], got)
	}
}

func TestGetObject_RangeSuffixBeyondSize(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=-50"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 (whole object), got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 0-9/10" {
		t.Errorf("expected Content-Range 'bytes 0-9/10', got %q", cr)
	}
	if got := w.Body.String(); got != content {
		t.Errorf("expected full body, got %q", got)
	}
}

func TestGetObject_RangeStartBeyondEOF416(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=10-"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes */10" {
		t.Errorf("expected Content-Range 'bytes */10', got %q", cr)
	}
	if !strings.Contains(w.Body.String(), "InvalidRange") {
		t.Errorf("expected InvalidRange error code in body, got %q", w.Body.String())
	}
}

func TestGetObject_RangeSuffixZero416(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=-0"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d: %s", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes */10" {
		t.Errorf("expected Content-Range 'bytes */10', got %q", cr)
	}
}

func TestGetObject_RangeMalformedIgnored(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	for _, spec := range []string{"bytes=abc", "chunks=0-5", "bytes=5-2", "bytes=-", "bytes=", "0-5"} {
		w := rangeGet(t, map[string]string{"Range": spec})
		if w.Code != http.StatusOK {
			t.Errorf("Range %q: expected 200 (malformed ignored), got %d", spec, w.Code)
			continue
		}
		if got := w.Body.String(); got != content {
			t.Errorf("Range %q: expected full body, got %q", spec, got)
		}
	}
}

func TestGetObject_RangeMultiRangeFallsBack200(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=0-1,3-4"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for multi-range fallback, got %d", w.Code)
	}
	if got := w.Body.String(); got != content {
		t.Errorf("expected full body, got %q", got)
	}
}

func TestGetObject_AcceptRangesOn200(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ar := w.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("expected Accept-Ranges: bytes, got %q", ar)
	}
}

func TestHeadObject_Range(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeHead(t, map[string]string{"Range": "bytes=2-5"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", w.Code)
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Errorf("expected Content-Range 'bytes 2-5/10', got %q", cr)
	}
	if cl := w.Header().Get("Content-Length"); cl != "4" {
		t.Errorf("expected Content-Length 4, got %q", cl)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD must have no body, got %q", w.Body.String())
	}
}

func TestHeadObject_RangeUnsatisfiable(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeHead(t, map[string]string{"Range": "bytes=99-"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", w.Code)
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes */10" {
		t.Errorf("expected Content-Range 'bytes */10', got %q", cr)
	}
}

func TestHeadObject_AcceptRanges(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeHead(t, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ar := w.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("expected Accept-Ranges: bytes, got %q", ar)
	}
}

func TestGetObject_IfMatch_Hit(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-Match": etag})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on If-Match hit, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetObject_IfMatch_Miss412(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-Match": `"deadbeef"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 on If-Match miss, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PreconditionFailed") {
		t.Errorf("expected PreconditionFailed error code, got %q", w.Body.String())
	}
}

func TestGetObject_IfMatch_Star(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-Match": "*"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on If-Match *, got %d", w.Code)
	}
}

func TestGetObject_IfMatch_WeakTag(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-Match": "W/" + etag})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on weak If-Match, got %d", w.Code)
	}
}

func TestGetObject_IfNoneMatch_Hit304(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 on If-None-Match hit, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 must have no body, got %q", w.Body.String())
	}
	if etag != w.Header().Get("ETag") {
		t.Errorf("304 must carry ETag %s, got %q", etag, w.Header().Get("ETag"))
	}
	lm := w.Header().Get("Last-Modified")
	if _, err := http.ParseTime(lm); err != nil {
		t.Errorf("304 must carry parseable Last-Modified, got %q", lm)
	}
}

func TestGetObject_IfNoneMatch_Miss(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-None-Match": `"deadbeef"`})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on If-None-Match miss, got %d", w.Code)
	}
}

func TestHeadObject_IfNoneMatch_Hit304(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := rangeHead(t, map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 on HEAD If-None-Match hit, got %d", w.Code)
	}
	if w.Header().Get("ETag") != etag {
		t.Errorf("304 must carry ETag, got %q", w.Header().Get("ETag"))
	}
}

func TestGetObject_IfModifiedSince_NotModified304(t *testing.T) {
	_, _, _ = setupRangeObject(t) // modified 2026-09-01T12:00:00Z
	w := rangeGet(t, map[string]string{"If-Modified-Since": "Tue, 01 Sep 2026 13:00:00 GMT"})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("ETag") == "" {
		t.Error("304 must carry ETag")
	}
	if w.Header().Get("Last-Modified") == "" {
		t.Error("304 must carry Last-Modified")
	}
}

func TestGetObject_IfModifiedSince_Modified200(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-Modified-Since": "Tue, 01 Sep 2026 11:00:00 GMT"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetObject_IfModifiedSince_SecondGranularity(t *testing.T) {
	_, _, _ = setupRangeObject(t) // modified exactly 12:00:00
	w := rangeGet(t, map[string]string{"If-Modified-Since": "Tue, 01 Sep 2026 12:00:00 GMT"})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 at same second, got %d", w.Code)
	}
}

func TestGetObject_IfUnmodifiedSince_NotModified200(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-Unmodified-Since": "Tue, 01 Sep 2026 13:00:00 GMT"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetObject_IfUnmodifiedSince_Modified412(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := rangeGet(t, map[string]string{"If-Unmodified-Since": "Tue, 01 Sep 2026 11:00:00 GMT"})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d: %s", w.Code, w.Body.String())
	}
}

// Precedence: If-None-Match evaluated before If-Modified-Since (RFC 7232).
func TestPrecondition_IfNoneMatchOverIfModifiedSince(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := rangeGet(t, map[string]string{
		"If-None-Match":     etag,
		"If-Modified-Since": "Tue, 01 Sep 2026 13:00:00 GMT",
	})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", w.Code)
	}
	// INM misses but IMS would say 304: INM miss must short-circuit IMS → 200.
	w = rangeGet(t, map[string]string{
		"If-None-Match":     `"deadbeef"`,
		"If-Modified-Since": "Tue, 01 Sep 2026 13:00:00 GMT",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when INM misses (IMS ignored), got %d", w.Code)
	}
}

// Precedence: If-Match evaluated before If-Unmodified-Since (RFC 7232).
func TestPrecondition_IfMatchOverIfUnmodifiedSince(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := rangeGet(t, map[string]string{
		"If-Match":            etag,
		"If-Unmodified-Since": "Tue, 01 Sep 2026 11:00:00 GMT",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (IUS ignored on If-Match hit), got %d: %s", w.Code, w.Body.String())
	}
}

// Range interacts with conditional: conditionals first; a 304 short-circuits
// before any Range processing.
func TestPrecondition_BeforeRange(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := rangeGet(t, map[string]string{
		"If-None-Match": etag,
		"Range":         "bytes=0-4",
	})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 (conditional before range), got %d", w.Code)
	}
	w = rangeGet(t, map[string]string{
		"If-Match": `"deadbeef"`,
		"Range":    "bytes=0-4",
	})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 (conditional before range), got %d", w.Code)
	}
	w = rangeGet(t, map[string]string{
		"If-Match": etag,
		"Range":    "bytes=0-4",
	})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 (range applies after conditional pass), got %d", w.Code)
	}
}

// ---- CopyObject / DeleteObjects (port of copy_batch_test.go) ----

// copyObject issues a PUT with x-amz-copy-source through putObjectHandler.
func copyObject(t *testing.T, destBucket, destKey, copySource string, extraHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+destBucket+"/"+destKey, nil)
	req.Header.Set("x-amz-copy-source", copySource)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	putObjectHandler(w, req, destBucket, destKey)
	return w
}

// deleteObjects issues POST /bucket?delete through bucketLevelDispatch.
func deleteObjects(t *testing.T, bucketName, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/"+bucketName+"?delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	defaultTestFrontend().bucketLevelDispatch(w, req, bucketName)
	return w
}

// readObjectMeta reads the stored metadata JSON for an object (legacy flat
// layout, written by writeTestObject/writeRangeObject).
func readObjectMeta(t *testing.T, env *testS3Env, bucketName, key string) ObjectMetadata {
	t.Helper()
	metaPath := filepath.Join(env.dataDir, bucketName, ".metadata", key+".meta")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed reading metadata for %s/%s: %v", bucketName, key, err)
	}
	var meta ObjectMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("failed unmarshalling metadata for %s/%s: %v", bucketName, key, err)
	}
	return meta
}

// mustGetObject reads an object through the backend and returns its bytes.
func mustGetObject(t *testing.T, env *testS3Env, bucket, key string) ([]byte, error) {
	t.Helper()
	rc, _, err := env.b.Get(context.Background(), bucket, key, objectmodel.GetOptions{})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestCopyObject_SameBucket(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "copy-src")
	env.writeTestObject(t, "copy-src", "orig.txt", "hello copy")

	w := copyObject(t, "copy-src", "copied.txt", "copy-src/orig.txt", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<CopyObjectResult") {
		t.Errorf("expected CopyObjectResult in body, got: %s", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/xml" {
		t.Errorf("expected Content-Type application/xml, got %q", ct)
	}

	got, err := mustGetObject(t, env, "copy-src", "copied.txt")
	if err != nil || string(got) != "hello copy" {
		t.Errorf("destination data mismatch: %q err=%v", got, err)
	}
	if _, err := env.b.Stat(context.Background(), "copy-src", "orig.txt"); err != nil {
		t.Errorf("source object must survive the copy: %v", err)
	}
}

func TestCopyObject_CrossBucket(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "copy-from")
	env.setupBucket(t, "copy-to")
	env.writeTestObject(t, "copy-from", "shared.bin", "cross bucket payload")

	w := copyObject(t, "copy-to", "arrived.bin", "copy-from/shared.bin", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got, err := mustGetObject(t, env, "copy-to", "arrived.bin")
	if err != nil || string(got) != "cross bucket payload" {
		t.Errorf("destination data mismatch: %q err=%v", got, err)
	}
	meta := readObjectMeta(t, env, "copy-to", "arrived.bin")
	wantETag := fmt.Sprintf("%x", md5Hash([]byte("cross bucket payload")))
	if meta.ETag != wantETag {
		t.Errorf("expected ETag %s, got %s", wantETag, meta.ETag)
	}
	if meta.ContentLength != int64(len("cross bucket payload")) {
		t.Errorf("expected ContentLength %d, got %d", len("cross bucket payload"), meta.ContentLength)
	}
}

func TestCopyObject_MetaCopyPreserved(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "meta-src")
	env.setupBucket(t, "meta-dst")

	// PUT the source through the real handler so meta headers are stored.
	put := httptest.NewRequest("PUT", "/meta-src/doc.csv", strings.NewReader("a,b,c"))
	put.Header.Set("Content-Type", "text/csv")
	put.Header.Set("X-Amz-Meta-Foo", "bar")
	pw := httptest.NewRecorder()
	putObjectHandler(pw, put, "meta-src", "doc.csv")
	if pw.Code != 200 {
		t.Fatalf("source PUT failed: %d %s", pw.Code, pw.Body.String())
	}

	w := copyObject(t, "meta-dst", "doc-copy.csv", "/meta-src/doc.csv", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	meta := readObjectMeta(t, env, "meta-dst", "doc-copy.csv")
	if meta.ContentType != "text/csv" {
		t.Errorf("COPY directive should preserve Content-Type text/csv, got %q", meta.ContentType)
	}
	if meta.CustomMetadata["X-Amz-Meta-Foo"] != "bar" {
		t.Errorf("COPY directive should preserve x-amz-meta-foo, got %v", meta.CustomMetadata)
	}
}

func TestCopyObject_MetaReplace(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "repl-src")
	env.setupBucket(t, "repl-dst")
	env.writeTestObject(t, "repl-src", "old.txt", "replace me")
	srcMeta := readObjectMeta(t, env, "repl-src", "old.txt")
	srcMeta.ContentType = "text/plain"
	srcMeta.CustomMetadata = map[string]string{"X-Amz-Meta-Stale": "yes"}
	if err := writeFileAtomicJSON(filepath.Join(env.dataDir, "repl-src", ".metadata", "old.txt.meta"), srcMeta, 0o644); err != nil {
		t.Fatalf("failed updating source meta: %v", err)
	}

	w := copyObject(t, "repl-dst", "new.json", "repl-src/old.txt", map[string]string{
		"x-amz-metadata-directive": "REPLACE",
		"Content-Type":             "application/json",
	})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	meta := readObjectMeta(t, env, "repl-dst", "new.json")
	if meta.ContentType != "application/json" {
		t.Errorf("REPLACE should set Content-Type application/json, got %q", meta.ContentType)
	}
	if len(meta.CustomMetadata) != 0 {
		t.Errorf("REPLACE should drop source x-amz-meta-*, got %v", meta.CustomMetadata)
	}
	got, err := mustGetObject(t, env, "repl-dst", "new.json")
	if err != nil || string(got) != "replace me" {
		t.Errorf("destination data mismatch: %q err=%v", got, err)
	}
}

func TestCopyObject_InvalidDirective(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "dir-src")
	env.setupBucket(t, "dir-dst")
	env.writeTestObject(t, "dir-src", "obj", "data")

	w := copyObject(t, "dir-dst", "obj2", "dir-src/obj", map[string]string{
		"x-amz-metadata-directive": "BOGUS",
	})
	if w.Code != 400 {
		t.Fatalf("expected 400 for invalid directive, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("expected InvalidArgument, got: %s", w.Body.String())
	}
}

func TestCopyObject_MissingSource(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "src-missing")
	env.setupBucket(t, "dst-missing")

	w := copyObject(t, "dst-missing", "out", "src-missing/nope.txt", nil)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchKey") {
		t.Errorf("expected NoSuchKey, got: %s", w.Body.String())
	}
}

func TestCopyObject_MissingSourceBucket(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "dst-only")

	w := copyObject(t, "dst-only", "out", "ghost-bucket/key.txt", nil)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("expected NoSuchBucket, got: %s", w.Body.String())
	}
}

func TestCopyObject_SameSrcDst(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "self-copy")
	env.writeTestObject(t, "self-copy", "same.txt", "identical")

	w := copyObject(t, "self-copy", "same.txt", "self-copy/same.txt", nil)
	if w.Code != 400 {
		t.Fatalf("expected 400 for same source and destination, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidRequest") {
		t.Errorf("expected InvalidRequest, got: %s", w.Body.String())
	}
}

func TestCopyObject_VersionIdRejected(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "ver-src")
	env.setupBucket(t, "ver-dst")
	env.writeTestObject(t, "ver-src", "v.txt", "versioned")

	w := copyObject(t, "ver-dst", "v-copy.txt", "ver-src/v.txt?versionId=abc123", nil)
	if w.Code != 501 {
		t.Fatalf("expected 501 for versionId copy source, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NotImplemented") {
		t.Errorf("expected NotImplemented, got: %s", w.Body.String())
	}
}

func TestCopyObject_ResponseXMLShape(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "xml-src")
	env.writeTestObject(t, "xml-src", "shape.txt", "shape")

	w := copyObject(t, "xml-src", "shape-copy.txt", "xml-src/shape.txt", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, s3XMLNamespace) {
		t.Errorf("expected S3 xmlns in response, got: %s", body)
	}
	var result CopyObjectResult
	if err := xml.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("failed to parse CopyObjectResult: %v", err)
	}
	wantETag := fmt.Sprintf("%q", fmt.Sprintf("%x", md5Hash([]byte("shape"))))
	if result.ETag != wantETag {
		t.Errorf("expected quoted ETag %s, got %q", wantETag, result.ETag)
	}
	if result.LastModified == "" {
		t.Errorf("expected non-empty LastModified")
	}
}

func TestDeleteObjects_Batch3(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "batch-bucket")
	env.writeTestObject(t, "batch-bucket", "k1.txt", "one")
	env.writeTestObject(t, "batch-bucket", "k2.txt", "two")

	body := `<Delete><Object><Key>k1.txt</Key></Object><Object><Key>k2.txt</Key></Object><Object><Key>missing.txt</Key></Object></Delete>`
	w := deleteObjects(t, "batch-bucket", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse DeleteResult: %v", err)
	}
	// Missing keys still report Deleted (S3 semantics).
	if len(result.Deleted) != 3 {
		t.Fatalf("expected 3 Deleted entries, got %d: %s", len(result.Deleted), w.Body.String())
	}
	seen := map[string]bool{}
	for _, d := range result.Deleted {
		seen[d.Key] = true
	}
	for _, key := range []string{"k1.txt", "k2.txt", "missing.txt"} {
		if !seen[key] {
			t.Errorf("expected Deleted entry for %q, got: %s", key, w.Body.String())
		}
	}
	if len(result.Error) != 0 {
		t.Errorf("expected no Error entries, got %v", result.Error)
	}
	for _, key := range []string{"k1.txt", "k2.txt"} {
		if _, err := env.b.Stat(context.Background(), "batch-bucket", key); err == nil {
			t.Errorf("expected %s to be deleted from disk", key)
		}
	}
}

func TestDeleteObjects_QuietShape(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "quiet-bucket")
	env.writeTestObject(t, "quiet-bucket", "q1.txt", "one")
	env.writeTestObject(t, "quiet-bucket", "q2.txt", "two")

	body := `<Delete><Quiet>true</Quiet><Object><Key>q1.txt</Key></Object><Object><Key>q2.txt</Key></Object></Delete>`
	w := deleteObjects(t, "quiet-bucket", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	responseBody := w.Body.String()
	if strings.Contains(responseBody, "<Deleted>") {
		t.Errorf("Quiet mode must suppress Deleted entries, got: %s", responseBody)
	}
	if !strings.Contains(responseBody, "<DeleteResult") {
		t.Errorf("expected DeleteResult root element, got: %s", responseBody)
	}
	for _, key := range []string{"q1.txt", "q2.txt"} {
		if _, err := env.b.Stat(context.Background(), "quiet-bucket", key); err == nil {
			t.Errorf("expected %s to be deleted from disk", key)
		}
	}
}

func TestDeleteObjects_MissingKeysReportDeleted(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "ghost-bucket")

	body := `<Delete><Object><Key>never-existed.txt</Key></Object></Delete>`
	w := deleteObjects(t, "ghost-bucket", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse DeleteResult: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Key != "never-existed.txt" {
		t.Errorf("expected Deleted entry for missing key, got: %s", w.Body.String())
	}
}

func TestDeleteObjects_MalformedXML(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "mal-bucket")

	w := deleteObjects(t, "mal-bucket", "this is not xml")
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MalformedXML") {
		t.Errorf("expected MalformedXML, got: %s", w.Body.String())
	}
}

func TestDeleteObjects_EmptyObjectList(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "empty-bucket")

	w := deleteObjects(t, "empty-bucket", "<Delete></Delete>")
	if w.Code != 400 {
		t.Fatalf("expected 400 for empty object list, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MalformedXML") {
		t.Errorf("expected MalformedXML, got: %s", w.Body.String())
	}
}

func TestDeleteObjects_Over1000Keys(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "cap-bucket")

	var sb strings.Builder
	sb.WriteString("<Delete>")
	for i := range 1001 {
		fmt.Fprintf(&sb, "<Object><Key>k-%d</Key></Object>", i)
	}
	sb.WriteString("</Delete>")

	w := deleteObjects(t, "cap-bucket", sb.String())
	if w.Code != 400 {
		t.Fatalf("expected 400 for >1000 keys, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MalformedXML") {
		t.Errorf("expected MalformedXML, got: %s", w.Body.String())
	}
}

func TestDeleteObjects_Exactly1000Keys(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "cap-ok-bucket")

	var sb strings.Builder
	sb.WriteString("<Delete>")
	for i := range 1000 {
		fmt.Fprintf(&sb, "<Object><Key>k-%d</Key></Object>", i)
	}
	sb.WriteString("</Delete>")

	w := deleteObjects(t, "cap-ok-bucket", sb.String())
	if w.Code != 200 {
		t.Fatalf("expected 200 for exactly 1000 keys, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteObjects_NoSuchBucket(t *testing.T) {
	setupS3TestEnv(t)
	body := `<Delete><Object><Key>k</Key></Object></Delete>`
	w := deleteObjects(t, "no-such-bucket", body)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("expected NoSuchBucket, got: %s", w.Body.String())
	}
}

// ---- Copy/Delete edge matrix (leaf 4.5) ----

// A 0-byte object copies to 200 with the canonical empty-MD5 ETag.
func TestCopyObject_ZeroByteObject(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "zb-src")
	env.setupBucket(t, "zb-dst")
	env.writeTestObject(t, "zb-src", "empty.bin", "")

	w := copyObject(t, "zb-dst", "empty-copy.bin", "zb-src/empty.bin", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	const emptyMD5 = "d41d8cd98f00b204e9800998ecf8427e"
	var result CopyObjectResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse CopyObjectResult: %v", err)
	}
	if result.ETag != fmt.Sprintf("%q", emptyMD5) {
		t.Errorf("expected quoted empty-MD5 ETag, got %q", result.ETag)
	}
	got, err := mustGetObject(t, env, "zb-dst", "empty-copy.bin")
	if err != nil {
		t.Fatalf("destination data read failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0-byte destination, got %d bytes", len(got))
	}
	meta := readObjectMeta(t, env, "zb-dst", "empty-copy.bin")
	if meta.ETag != emptyMD5 || meta.ContentLength != 0 {
		t.Errorf("expected ETag %s len 0, got %s len %d", emptyMD5, meta.ETag, meta.ContentLength)
	}
}

// Copying into a nested destination key creates parent data + metadata
// directories and writes both files.
func TestCopyObject_NestedDestination(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "nest-src")
	env.setupBucket(t, "nest-dst")
	env.writeTestObject(t, "nest-src", "src.txt", "nested payload")

	w := copyObject(t, "nest-dst", "a/b/c.txt", "nest-src/src.txt", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data, err := os.Stat(filepath.Join(env.dataDir, "nest-dst", "a", "b", "c.txt"))
	if err != nil {
		t.Fatalf("destination data not created: %v", err)
	}
	if data.Size() != int64(len("nested payload")) {
		t.Errorf("destination size = %d, want %d", data.Size(), len("nested payload"))
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "nest-dst", ".metadata", "a", "b", "c.txt.meta")); err != nil {
		t.Errorf("destination metadata not created: %v", err)
	}
}

// COPY (explicit directive) preserves x-amz-meta-* — asserted via the real
// GET path, not just the stored JSON.
func TestCopyObject_CopyDirectivePreservesMetaHeaders(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "cp-src")
	env.setupBucket(t, "cp-dst")

	put := httptest.NewRequest("PUT", "/cp-src/doc.txt", strings.NewReader("meta me"))
	put.Header.Set("Content-Type", "text/x-custom")
	put.Header.Set("X-Amz-Meta-Color", "blue")
	put.Header.Set("X-Amz-Meta-Shape", "round")
	pw := httptest.NewRecorder()
	putObjectHandler(pw, put, "cp-src", "doc.txt")
	if pw.Code != 200 {
		t.Fatalf("source PUT failed: %d %s", pw.Code, pw.Body.String())
	}

	w := copyObject(t, "cp-dst", "doc-copy.txt", "cp-src/doc.txt",
		map[string]string{"x-amz-metadata-directive": "COPY"})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	get := httptest.NewRequest("GET", "/cp-dst/doc-copy.txt", nil)
	gw := httptest.NewRecorder()
	getObjectHandler(gw, get, "cp-dst", "doc-copy.txt")
	if gw.Code != 200 {
		t.Fatalf("GET failed: %d %s", gw.Code, gw.Body.String())
	}
	if ct := gw.Header().Get("Content-Type"); ct != "text/x-custom" {
		t.Errorf("COPY should preserve Content-Type text/x-custom, got %q", ct)
	}
	if got := gw.Header().Get("X-Amz-Meta-Color"); got != "blue" {
		t.Errorf("COPY should preserve x-amz-meta-color, got %q", got)
	}
	if got := gw.Header().Get("X-Amz-Meta-Shape"); got != "round" {
		t.Errorf("COPY should preserve x-amz-meta-shape, got %q", got)
	}
}

// REPLACE swaps Content-Type on the served response AND the stale source
// meta headers are GONE from a real GET.
func TestCopyObject_ReplaceSwapsServedHeaders(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "rpl-src")
	env.setupBucket(t, "rpl-dst")

	put := httptest.NewRequest("PUT", "/rpl-src/doc.txt", strings.NewReader("replace body"))
	put.Header.Set("Content-Type", "text/x-old")
	put.Header.Set("X-Amz-Meta-Stale", "yes")
	pw := httptest.NewRecorder()
	putObjectHandler(pw, put, "rpl-src", "doc.txt")
	if pw.Code != 200 {
		t.Fatalf("source PUT failed: %d %s", pw.Code, pw.Body.String())
	}

	w := copyObject(t, "rpl-dst", "doc-new.txt", "rpl-src/doc.txt", map[string]string{
		"x-amz-metadata-directive": "REPLACE",
		"Content-Type":             "application/x-new",
	})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	get := httptest.NewRequest("GET", "/rpl-dst/doc-new.txt", nil)
	gw := httptest.NewRecorder()
	getObjectHandler(gw, get, "rpl-dst", "doc-new.txt")
	if gw.Code != 200 {
		t.Fatalf("GET failed: %d %s", gw.Code, gw.Body.String())
	}
	if ct := gw.Header().Get("Content-Type"); ct != "application/x-new" {
		t.Errorf("REPLACE should serve Content-Type application/x-new, got %q", ct)
	}
	if got := gw.Header().Get("X-Amz-Meta-Stale"); got != "" {
		t.Errorf("REPLACE must drop source meta headers, got x-amz-meta-stale=%q", got)
	}
}

// REPLACE with NO request meta/Content-Type still replaces (fresh empty meta,
// not source meta).
func TestCopyObject_ReplaceWithoutNewMeta(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "rw-src")
	env.setupBucket(t, "rw-dst")

	put := httptest.NewRequest("PUT", "/rw-src/doc.txt", strings.NewReader("body"))
	put.Header.Set("Content-Type", "text/x-old")
	put.Header.Set("X-Amz-Meta-Stale", "yes")
	pw := httptest.NewRecorder()
	putObjectHandler(pw, put, "rw-src", "doc.txt")
	if pw.Code != 200 {
		t.Fatalf("source PUT failed: %d %s", pw.Code, pw.Body.String())
	}

	w := copyObject(t, "rw-dst", "doc.txt", "rw-src/doc.txt", map[string]string{
		"x-amz-metadata-directive": "REPLACE",
	})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	meta := readObjectMeta(t, env, "rw-dst", "doc.txt")
	if meta.ContentType != "" {
		t.Errorf("REPLACE without Content-Type should leave it empty, got %q", meta.ContentType)
	}
	if len(meta.CustomMetadata) != 0 {
		t.Errorf("REPLACE must not inherit source meta, got %v", meta.CustomMetadata)
	}
}

// REPLACE that actually sets a new x-amz-meta-* header stores it under the
// original header casing.
func TestCopyObject_ReplaceWithNewMeta(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "rn-src")
	env.setupBucket(t, "rn-dst")

	put := httptest.NewRequest("PUT", "/rn-src/doc.txt", strings.NewReader("body"))
	put.Header.Set("Content-Type", "text/x-old")
	put.Header.Set("X-Amz-Meta-Stale", "yes")
	pw := httptest.NewRecorder()
	putObjectHandler(pw, put, "rn-src", "doc.txt")
	if pw.Code != 200 {
		t.Fatalf("source PUT failed: %d %s", pw.Code, pw.Body.String())
	}

	w := copyObject(t, "rn-dst", "doc.txt", "rn-src/doc.txt", map[string]string{
		"x-amz-metadata-directive": "REPLACE",
		"Content-Type":             "application/x-new",
		"X-Amz-Meta-Fresh":         "value1",
	})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	meta := readObjectMeta(t, env, "rn-dst", "doc.txt")
	if got := meta.CustomMetadata["X-Amz-Meta-Fresh"]; got != "value1" {
		t.Errorf("REPLACE should store the new x-amz-meta-fresh header, got %v", meta.CustomMetadata)
	}
	if _, ok := meta.CustomMetadata["X-Amz-Meta-Stale"]; ok {
		t.Errorf("REPLACE must drop source meta, got %v", meta.CustomMetadata)
	}
	if meta.ContentType != "application/x-new" {
		t.Errorf("REPLACE should set application/x-new, got %q", meta.ContentType)
	}
}

// A copy-source with URL-encoded key (space as %20) resolves to the real
// object.
func TestCopyObject_URLEncodedSourceKey(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "enc-src")
	env.setupBucket(t, "enc-dst")
	env.writeTestObject(t, "enc-src", "dir with space/file name.txt", "encoded src")

	w := copyObject(t, "enc-dst", "out.txt", "enc-src/dir%20with%20space/file%20name.txt", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got, err := mustGetObject(t, env, "enc-dst", "out.txt")
	if err != nil || string(got) != "encoded src" {
		t.Errorf("destination data mismatch: %q err=%v", got, err)
	}
}

// Meta present but data file missing → 404 NoSuchKey through the copy path.
func TestCopyObject_DataFileMissingMetaPresent(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "orphan-src")
	env.setupBucket(t, "orphan-dst")
	// writeTestObject then remove the data file: meta-only object.
	env.writeTestObject(t, "orphan-src", "ghost.txt", "once")
	if err := os.Remove(filepath.Join(env.dataDir, "orphan-src", "ghost.txt")); err != nil {
		t.Fatalf("failed removing data file: %v", err)
	}

	w := copyObject(t, "orphan-dst", "out.txt", "orphan-src/ghost.txt", nil)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchKey") {
		t.Errorf("expected NoSuchKey, got: %s", w.Body.String())
	}
}

// The remaining copy-source parsing guards: malformed URL escape, missing
// key (bucket only), and a traversal-shaped source key all 400
// InvalidArgument before any filesystem access.
func TestCopyObject_MalformedCopySourceVariants(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "mal-src")
	env.writeTestObject(t, "mal-src", "ok.txt", "data")

	tests := []struct {
		name       string
		copySource string
	}{
		{"invalid URL escape", "mal-src/%zz.txt"},
		{"bucket without key", "mal-src-only-bucket"},
		{"traversal source key", "mal-src/../evil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := copyObject(t, "mal-src", "out.txt", tt.copySource, nil)
			if w.Code != 400 {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "InvalidArgument") {
				t.Errorf("expected InvalidArgument, got: %s", w.Body.String())
			}
		})
	}
}

// A traversal-shaped DESTINATION key is 400 InvalidArgument before any
// filesystem work.
func TestCopyObject_InvalidDestinationKey(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "idst-src")
	env.setupBucket(t, "idst-dst")
	env.writeTestObject(t, "idst-src", "src.txt", "data")

	w := copyObject(t, "idst-dst", "../escape", "idst-src/src.txt", nil)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("expected InvalidArgument, got: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "escape")); !os.IsNotExist(err) {
		t.Errorf("something escaped: %v", err)
	}
}

// An UNREADABLE source metadata file (directory where the .meta JSON
// belongs) → 500 InternalError.
func TestCopyObject_UnreadableSourceMeta(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "um-src")
	env.setupBucket(t, "um-dst")
	metaDir := filepath.Join(env.dataDir, "um-src", ".metadata")
	if err := os.MkdirAll(filepath.Join(metaDir, "weird.txt.meta"), 0o755); err != nil {
		t.Fatalf("failed creating directory at meta path: %v", err)
	}

	w := copyObject(t, "um-dst", "out.txt", "um-src/weird.txt", nil)
	if w.Code != 500 {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InternalError") {
		t.Errorf("expected InternalError, got: %s", w.Body.String())
	}
}

// A traversal-shaped key in a batch gets an InvalidArgument error entry,
// NOT a Deleted entry; nothing escapes.
func TestDeleteObjects_TraversalShapedKeyRejectedNoEscape(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "trav-bucket")

	body := `<Delete><Object><Key>../escape</Key></Object></Delete>`
	w := deleteObjects(t, "trav-bucket", body)
	if w.Code != 200 {
		t.Fatalf("expected 200 batch response, got %d: %s", w.Code, w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse DeleteResult: %v", err)
	}
	if len(result.Deleted) != 0 {
		t.Errorf("traversal-shaped key must not be reported Deleted, got: %s", w.Body.String())
	}
	if len(result.Error) != 1 || result.Error[0].Key != "../escape" || result.Error[0].Code != "InvalidArgument" {
		t.Errorf("expected one InvalidArgument error entry for ../escape, got: %s", w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(env.dataDir, "escape")); !os.IsNotExist(err) {
		t.Errorf("something escaped the bucket: %v", err)
	}
	entries, err := os.ReadDir(env.dataDir)
	if err != nil {
		t.Fatalf("dataDir read failed: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "trav-bucket" {
			t.Errorf("unexpected entry %q created in dataDir", e.Name())
		}
	}
	if _, err := os.Stat(bucketPath); err != nil {
		t.Errorf("bucket vanished: %v", err)
	}
}

// Duplicate keys in one batch → 200 with a Deleted entry per occurrence.
func TestDeleteObjects_DuplicateKeys(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "dup-bucket")
	env.writeTestObject(t, "dup-bucket", "dup.txt", "once")

	body := `<Delete><Object><Key>dup.txt</Key></Object><Object><Key>dup.txt</Key></Object></Delete>`
	w := deleteObjects(t, "dup-bucket", body)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse DeleteResult: %v", err)
	}
	if len(result.Deleted) != 2 {
		t.Fatalf("expected 2 Deleted entries for duplicate keys, got %d: %s", len(result.Deleted), w.Body.String())
	}
	for i, d := range result.Deleted {
		if d.Key != "dup.txt" {
			t.Errorf("Deleted[%d].Key = %q, want dup.txt", i, d.Key)
		}
	}
	if _, err := env.b.Stat(context.Background(), "dup-bucket", "dup.txt"); err == nil {
		t.Errorf("object should be deleted from disk: %v", err)
	}
}

// Deleting the last object under a/b/ removes a/b and a; a sibling object
// elsewhere keeps its branch (cleanupEmptyDirs stops at the first non-empty
// directory).
func TestDeleteObject_NestedCleanupStopsAtNonEmpty(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "prune-bucket")
	env.writeTestObject(t, "prune-bucket", "a/b/leaf.txt", "deep")
	env.writeTestObject(t, "prune-bucket", "a/sibling.txt", "keeps a alive")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/prune-bucket/a/b/leaf.txt", nil)
	deleteObjectHandler(w, req, "prune-bucket", "a/b/leaf.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(env.dataDir, "prune-bucket", "a", "b")); !os.IsNotExist(err) {
		t.Errorf("a/b should be pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "prune-bucket", "a")); err != nil {
		t.Errorf("a must be kept (sibling object exists): %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "prune-bucket", "a", "sibling.txt")); err != nil {
		t.Errorf("sibling object must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "prune-bucket", ".metadata", "a", "b")); !os.IsNotExist(err) {
		t.Errorf(".metadata/a/b should be pruned: %v", err)
	}
}

// Deleting a missing key in an EXISTING bucket stays 204 (S3 idempotency).
func TestDeleteObject_MissingKeyInExistingBucket204(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "idem-bucket")
	env.writeTestObject(t, "idem-bucket", "keep.txt", "keep")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/idem-bucket/never-there.txt", nil)
	deleteObjectHandler(w, req, "idem-bucket", "never-there.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for missing key, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "idem-bucket", "keep.txt")); err != nil {
		t.Errorf("decoy sibling must survive: %v", err)
	}
}

// Corrupt metadata JSON falls back to the canonical path, the delete still
// returns 204 and removes the data file.
func TestDeleteObject_CorruptMetaFallback(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "corrupt-bucket")
	dataPath := filepath.Join(bucketPath, "victim.txt")
	if err := os.WriteFile(dataPath, []byte("data"), 0o644); err != nil {
		t.Fatalf("failed writing data: %v", err)
	}
	metaPath := filepath.Join(bucketPath, ".metadata", "victim.txt.meta")
	if err := os.WriteFile(metaPath, []byte("{not valid json!!"), 0o644); err != nil {
		t.Fatalf("failed writing corrupt meta: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/corrupt-bucket/victim.txt", nil)
	deleteObjectHandler(w, req, "corrupt-bucket", "victim.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 with corrupt meta, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Errorf("data file should be deleted via canonical fallback: %v", err)
	}
}

// A batch key whose path is a non-empty DIRECTORY makes deleteObjectCore
// fail on os.Remove → InternalError error entry, and the rest of the batch
// still completes. Also proves the same key through deleteObjectHandler → 500.
func TestDeleteObject_KeyIsNonEmptyDirectory(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "dir-key-bucket")
	dirKeyPath := filepath.Join(bucketPath, "occupied")
	if err := os.MkdirAll(filepath.Join(dirKeyPath, "child"), 0o755); err != nil {
		t.Fatalf("failed creating directory key: %v", err)
	}

	// Single delete: 500 InternalError.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/dir-key-bucket/occupied", nil)
	deleteObjectHandler(w, req, "dir-key-bucket", "occupied")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for non-empty directory key, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InternalError") {
		t.Errorf("expected InternalError, got: %s", w.Body.String())
	}

	// Batch delete: InternalError entry for that key, Deleted for the other.
	env.writeTestObject(t, "dir-key-bucket", "real.txt", "fine")
	body := `<Delete><Object><Key>occupied</Key></Object><Object><Key>real.txt</Key></Object></Delete>`
	bw := deleteObjects(t, "dir-key-bucket", body)
	if bw.Code != 200 {
		t.Fatalf("expected 200 batch response, got %d: %s", bw.Code, bw.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(bw.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse DeleteResult: %v", err)
	}
	if len(result.Error) != 1 || result.Error[0].Key != "occupied" || result.Error[0].Code != "InternalError" {
		t.Errorf("expected InternalError entry for occupied, got: %s", bw.Body.String())
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Key != "real.txt" {
		t.Errorf("expected Deleted entry for real.txt, got: %s", bw.Body.String())
	}
}
