package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// range_conditional_test.go — leaf 3.1: HTTP Range + conditional request tests
// on GET/HEAD object.

// writeTestObjectWithModTime writes an object like testEnv.writeTestObject but
// with an explicit LastModified so conditional-request tests can control time.
func writeTestObjectWithModTime(t *testing.T, env *testEnv, bucketName, objectKey, content string, mod time.Time) {
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
		LastModified:  mod,
		StoragePath:   dataPath,
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal metadata: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0755); err != nil {
		t.Fatalf("Failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(metadataPath, metaJSON, 0644); err != nil {
		t.Fatalf("Failed to write metadata: %v", err)
	}
}

// setupRangeObject creates an env, bucket, and one 10-byte object "0123456789"
// modified at a fixed time. Returns the etag quoted as served by the server.
func setupRangeObject(t *testing.T) (*testEnv, string, string) {
	t.Helper()
	env := setupTestEnv(t)
	env.setupBucket(t, "range-bucket")
	content := "0123456789"
	mod := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	writeTestObjectWithModTime(t, env, "range-bucket", "data.bin", content, mod)
	etag := fmt.Sprintf("%q", fmt.Sprintf("%x", md5.Sum([]byte(content))))
	return env, content, etag
}

func doGet(t *testing.T, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/range-bucket/data.bin", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	getObjectHandler(w, req, "range-bucket", "data.bin")
	return w
}

func doHead(t *testing.T, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("HEAD", "/range-bucket/data.bin", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	headObjectHandler(w, req, "range-bucket", "data.bin")
	return w
}

// ---- Range: GET ----

func TestGetObject_RangeStartEnd(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := doGet(t, map[string]string{"Range": "bytes=2-5"})
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
	w := doGet(t, map[string]string{"Range": "bytes=7-"})
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
	w := doGet(t, map[string]string{"Range": "bytes=-4"})
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
	w := doGet(t, map[string]string{"Range": "bytes=9-9"})
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
	w := doGet(t, map[string]string{"Range": "bytes=5-100"})
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
	w := doGet(t, map[string]string{"Range": "bytes=-50"})
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
	w := doGet(t, map[string]string{"Range": "bytes=10-"})
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
	w := doGet(t, map[string]string{"Range": "bytes=-0"})
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
		w := doGet(t, map[string]string{"Range": spec})
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
	w := doGet(t, map[string]string{"Range": "bytes=0-1,3-4"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for multi-range fallback, got %d", w.Code)
	}
	if got := w.Body.String(); got != content {
		t.Errorf("expected full body, got %q", got)
	}
}

func TestGetObject_AcceptRangesOn200(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := doGet(t, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ar := w.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("expected Accept-Ranges: bytes, got %q", ar)
	}
}

// ---- Range: HEAD ----

func TestHeadObject_Range(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := doHead(t, map[string]string{"Range": "bytes=2-5"})
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
	w := doHead(t, map[string]string{"Range": "bytes=99-"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", w.Code)
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes */10" {
		t.Errorf("expected Content-Range 'bytes */10', got %q", cr)
	}
}

func TestHeadObject_AcceptRanges(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := doHead(t, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ar := w.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("expected Accept-Ranges: bytes, got %q", ar)
	}
}

// ---- Conditional requests ----

func TestGetObject_IfMatch_Hit(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := doGet(t, map[string]string{"If-Match": etag})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on If-Match hit, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetObject_IfMatch_Miss412(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := doGet(t, map[string]string{"If-Match": `"deadbeef"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 on If-Match miss, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PreconditionFailed") {
		t.Errorf("expected PreconditionFailed error code, got %q", w.Body.String())
	}
}

func TestGetObject_IfMatch_Star(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := doGet(t, map[string]string{"If-Match": "*"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on If-Match *, got %d", w.Code)
	}
}

func TestGetObject_IfMatch_WeakTag(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := doGet(t, map[string]string{"If-Match": "W/" + etag})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on weak If-Match, got %d", w.Code)
	}
}

func TestGetObject_IfNoneMatch_Hit304(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := doGet(t, map[string]string{"If-None-Match": etag})
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
	w := doGet(t, map[string]string{"If-None-Match": `"deadbeef"`})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on If-None-Match miss, got %d", w.Code)
	}
}

func TestHeadObject_IfNoneMatch_Hit304(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	w := doHead(t, map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 on HEAD If-None-Match hit, got %d", w.Code)
	}
	if w.Header().Get("ETag") != etag {
		t.Errorf("304 must carry ETag, got %q", w.Header().Get("ETag"))
	}
}

func TestGetObject_IfModifiedSince_NotModified304(t *testing.T) {
	_, _, _ = setupRangeObject(t) // modified 2026-09-01T12:00:00Z
	// Header date after object modification → not modified since → 304.
	w := doGet(t, map[string]string{"If-Modified-Since": "Tue, 01 Sep 2026 13:00:00 GMT"})
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
	// Header date before object modification → modified since → 200.
	w := doGet(t, map[string]string{"If-Modified-Since": "Tue, 01 Sep 2026 11:00:00 GMT"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetObject_IfModifiedSince_SecondGranularity(t *testing.T) {
	_, _, _ = setupRangeObject(t) // modified exactly 12:00:00
	// Same second → not modified → 304.
	w := doGet(t, map[string]string{"If-Modified-Since": "Tue, 01 Sep 2026 12:00:00 GMT"})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 at same second, got %d", w.Code)
	}
}

func TestGetObject_IfUnmodifiedSince_NotModified200(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := doGet(t, map[string]string{"If-Unmodified-Since": "Tue, 01 Sep 2026 13:00:00 GMT"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetObject_IfUnmodifiedSince_Modified412(t *testing.T) {
	_, _, _ = setupRangeObject(t)
	w := doGet(t, map[string]string{"If-Unmodified-Since": "Tue, 01 Sep 2026 11:00:00 GMT"})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d: %s", w.Code, w.Body.String())
	}
}

// Precedence: If-None-Match evaluated before If-Modified-Since (RFC 7232).
func TestPrecondition_IfNoneMatchOverIfModifiedSince(t *testing.T) {
	_, _, etag := setupRangeObject(t)
	// INM matches (→304) and IMS says not-modified too; both agree on 304.
	w := doGet(t, map[string]string{
		"If-None-Match":     etag,
		"If-Modified-Since": "Tue, 01 Sep 2026 13:00:00 GMT",
	})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", w.Code)
	}
	// INM misses but IMS would say 304: INM miss must short-circuit IMS → 200.
	w = doGet(t, map[string]string{
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
	// If-Match hits; stale If-Unmodified-Since alone would 412 but must be
	// ignored when If-Match is present and matches.
	w := doGet(t, map[string]string{
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
	w := doGet(t, map[string]string{
		"If-None-Match": etag,
		"Range":         "bytes=0-4",
	})
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 (conditional before range), got %d", w.Code)
	}
	// If-Match miss + Range → 412, not 206.
	w = doGet(t, map[string]string{
		"If-Match": `"deadbeef"`,
		"Range":    "bytes=0-4",
	})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 (conditional before range), got %d", w.Code)
	}
	// If-Match hit + Range → 206 applies.
	w = doGet(t, map[string]string{
		"If-Match": etag,
		"Range":    "bytes=0-4",
	})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 (range applies after conditional pass), got %d", w.Code)
	}
}
