package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copy_batch_test.go — leaf 3.5: CopyObject (x-amz-copy-source) and
// DeleteObjects (POST /bucket?delete) tests.

// doCopyObject issues a PUT with x-amz-copy-source through putObjectHandler
// (the copy dispatch site) and returns the recorder.
func doCopyObject(t *testing.T, destBucket, destKey, copySource string, extraHeaders map[string]string) *httptest.ResponseRecorder {
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

func doDeleteObjects(t *testing.T, bucketName, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/"+bucketName+"?delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	bucketLevelDispatch(w, req, bucketName)
	return w
}

// readObjectMeta reads the stored metadata JSON for an object.
func readObjectMeta(t *testing.T, env *testEnv, bucketName, key string) ObjectMetadata {
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

// ---- CopyObject ----

func TestCopyObject_SameBucket(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "copy-src")
	env.writeTestObject(t, "copy-src", "orig.txt", "hello copy")

	w := doCopyObject(t, "copy-src", "copied.txt", "copy-src/orig.txt", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<CopyObjectResult") {
		t.Errorf("expected CopyObjectResult in body, got: %s", body)
	}
	// S3 quirk: 200 with XML body
	if ct := w.Header().Get("Content-Type"); ct != "application/xml" {
		t.Errorf("expected Content-Type application/xml, got %q", ct)
	}

	// Destination data must match, source must still exist.
	got, err := os.ReadFile(filepath.Join(env.dataDir, "copy-src", "copied.txt"))
	if err != nil || string(got) != "hello copy" {
		t.Errorf("destination data mismatch: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "copy-src", "orig.txt")); err != nil {
		t.Errorf("source object must survive the copy: %v", err)
	}
}

func TestCopyObject_CrossBucket(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "copy-from")
	env.setupBucket(t, "copy-to")
	env.writeTestObject(t, "copy-from", "shared.bin", "cross bucket payload")

	w := doCopyObject(t, "copy-to", "arrived.bin", "copy-from/shared.bin", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(env.dataDir, "copy-to", "arrived.bin"))
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
	env := setupTestEnv(t)
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

	w := doCopyObject(t, "meta-dst", "doc-copy.csv", "/meta-src/doc.csv", nil)
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
	env := setupTestEnv(t)
	env.setupBucket(t, "repl-src")
	env.setupBucket(t, "repl-dst")
	env.writeTestObject(t, "repl-src", "old.txt", "replace me")
	// Enrich source meta beyond the test writer defaults.
	srcMeta := readObjectMeta(t, env, "repl-src", "old.txt")
	srcMeta.ContentType = "text/plain"
	srcMeta.CustomMetadata = map[string]string{"X-Amz-Meta-Stale": "yes"}
	if err := writeFileAtomicJSON(filepath.Join(env.dataDir, "repl-src", ".metadata", "old.txt.meta"), srcMeta, 0644); err != nil {
		t.Fatalf("failed updating source meta: %v", err)
	}

	w := doCopyObject(t, "repl-dst", "new.json", "repl-src/old.txt", map[string]string{
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
	// Data must still be copied even though metadata was replaced.
	got, err := os.ReadFile(filepath.Join(env.dataDir, "repl-dst", "new.json"))
	if err != nil || string(got) != "replace me" {
		t.Errorf("destination data mismatch: %q err=%v", got, err)
	}
}

func TestCopyObject_InvalidDirective(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "dir-src")
	env.setupBucket(t, "dir-dst")
	env.writeTestObject(t, "dir-src", "obj", "data")

	w := doCopyObject(t, "dir-dst", "obj2", "dir-src/obj", map[string]string{
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
	env := setupTestEnv(t)
	env.setupBucket(t, "src-missing")
	env.setupBucket(t, "dst-missing")

	w := doCopyObject(t, "dst-missing", "out", "src-missing/nope.txt", nil)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchKey") {
		t.Errorf("expected NoSuchKey, got: %s", w.Body.String())
	}
}

func TestCopyObject_MissingSourceBucket(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "dst-only")

	w := doCopyObject(t, "dst-only", "out", "ghost-bucket/key.txt", nil)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("expected NoSuchBucket, got: %s", w.Body.String())
	}
}

func TestCopyObject_SameSrcDst(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "self-copy")
	env.writeTestObject(t, "self-copy", "same.txt", "identical")

	w := doCopyObject(t, "self-copy", "same.txt", "self-copy/same.txt", nil)
	if w.Code != 400 {
		t.Fatalf("expected 400 for same source and destination, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidRequest") {
		t.Errorf("expected InvalidRequest, got: %s", w.Body.String())
	}
}

func TestCopyObject_VersionIdRejected(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "ver-src")
	env.setupBucket(t, "ver-dst")
	env.writeTestObject(t, "ver-src", "v.txt", "versioned")

	w := doCopyObject(t, "ver-dst", "v-copy.txt", "ver-src/v.txt?versionId=abc123", nil)
	if w.Code != 501 {
		t.Fatalf("expected 501 for versionId copy source, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NotImplemented") {
		t.Errorf("expected NotImplemented, got: %s", w.Body.String())
	}
}

func TestCopyObject_ResponseXMLShape(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "xml-src")
	env.writeTestObject(t, "xml-src", "shape.txt", "shape")

	w := doCopyObject(t, "xml-src", "shape-copy.txt", "xml-src/shape.txt", nil)
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

// ---- DeleteObjects ----

func TestDeleteObjects_Batch3(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "batch-bucket")
	env.writeTestObject(t, "batch-bucket", "k1.txt", "one")
	env.writeTestObject(t, "batch-bucket", "k2.txt", "two")

	body := `<Delete><Object><Key>k1.txt</Key></Object><Object><Key>k2.txt</Key></Object><Object><Key>missing.txt</Key></Object></Delete>`
	w := doDeleteObjects(t, "batch-bucket", body)
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
		if _, err := os.Stat(filepath.Join(env.dataDir, "batch-bucket", key)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted from disk", key)
		}
	}
}

func TestDeleteObjects_QuietShape(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "quiet-bucket")
	env.writeTestObject(t, "quiet-bucket", "q1.txt", "one")
	env.writeTestObject(t, "quiet-bucket", "q2.txt", "two")

	body := `<Delete><Quiet>true</Quiet><Object><Key>q1.txt</Key></Object><Object><Key>q2.txt</Key></Object></Delete>`
	w := doDeleteObjects(t, "quiet-bucket", body)
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
		if _, err := os.Stat(filepath.Join(env.dataDir, "quiet-bucket", key)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted from disk", key)
		}
	}
}

func TestDeleteObjects_MissingKeysReportDeleted(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "ghost-bucket")

	body := `<Delete><Object><Key>never-existed.txt</Key></Object></Delete>`
	w := doDeleteObjects(t, "ghost-bucket", body)
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
	env := setupTestEnv(t)
	env.setupBucket(t, "mal-bucket")

	w := doDeleteObjects(t, "mal-bucket", "this is not xml")
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MalformedXML") {
		t.Errorf("expected MalformedXML, got: %s", w.Body.String())
	}
}

func TestDeleteObjects_EmptyObjectList(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "empty-bucket")

	w := doDeleteObjects(t, "empty-bucket", "<Delete></Delete>")
	if w.Code != 400 {
		t.Fatalf("expected 400 for empty object list, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MalformedXML") {
		t.Errorf("expected MalformedXML, got: %s", w.Body.String())
	}
}

func TestDeleteObjects_Over1000Keys(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "cap-bucket")

	var sb strings.Builder
	sb.WriteString("<Delete>")
	for i := range 1001 {
		fmt.Fprintf(&sb, "<Object><Key>k-%d</Key></Object>", i)
	}
	sb.WriteString("</Delete>")

	w := doDeleteObjects(t, "cap-bucket", sb.String())
	if w.Code != 400 {
		t.Fatalf("expected 400 for >1000 keys, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MalformedXML") {
		t.Errorf("expected MalformedXML, got: %s", w.Body.String())
	}
}

func TestDeleteObjects_Exactly1000Keys(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "cap-ok-bucket")

	var sb strings.Builder
	sb.WriteString("<Delete>")
	for i := range 1000 {
		fmt.Fprintf(&sb, "<Object><Key>k-%d</Key></Object>", i)
	}
	sb.WriteString("</Delete>")

	w := doDeleteObjects(t, "cap-ok-bucket", sb.String())
	if w.Code != 200 {
		t.Fatalf("expected 200 for exactly 1000 keys, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteObjects_NoSuchBucket(t *testing.T) {
	setupTestEnv(t)
	body := `<Delete><Object><Key>k</Key></Object></Delete>`
	w := doDeleteObjects(t, "no-such-bucket", body)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("expected NoSuchBucket, got: %s", w.Body.String())
	}
}
