package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// multipart_handlers_test.go — tests for storage.go atomic helpers and
// multipart handler data-integrity fixes (leaf 2.3).
//
// Helpers here are deliberately self-contained (mp* prefixes) so this file
// does not couple to helper names in main_handler_test.go.

// mpTestConfig points serverConfig at a temp dir and returns the data dir.
func mpTestConfig(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	orig := serverConfig
	serverConfig = ServerConfig{
		DataDir: tmpDir + "/",
		Buckets: make(map[string]string),
	}
	t.Cleanup(func() { serverConfig = orig })
	return tmpDir
}

// mpTestBucket creates an empty bucket directory under dataDir.
func mpTestBucket(t *testing.T, dataDir, name string) string {
	t.Helper()
	p := filepath.Join(dataDir, name)
	if err := os.MkdirAll(filepath.Join(p, ".metadata"), 0755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}
	return p
}

// mpUploadsDir returns <bucket>/.metadata/.uploads for a bucket path.
func mpUploadsDir(bucketPath string) string {
	return filepath.Join(bucketPath, ".metadata", ".uploads")
}

// newTestUploadID returns a structurally valid (md5 hex) upload ID.
func newTestUploadID(t *testing.T) string {
	t.Helper()
	sum := md5.Sum([]byte(t.Name()))
	return hex.EncodeToString(sum[:])
}

// mpWriteUploadMeta writes a MultipartUpload metadata file for uploadID.
func mpWriteUploadMeta(t *testing.T, bucketPath, uploadID string, mp MultipartUpload) {
	t.Helper()
	if err := os.MkdirAll(mpUploadsDir(bucketPath), 0755); err != nil {
		t.Fatalf("creating .uploads dir: %v", err)
	}
	data, err := json.MarshalIndent(mp, "", "  ")
	if err != nil {
		t.Fatalf("marshalling upload meta: %v", err)
	}
	path := filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("writing upload meta: %v", err)
	}
}

// mpStorePart writes a part data file on disk and returns its PartMetadata.
func mpStorePart(t *testing.T, bucketPath, uploadID string, partNumber int, content string) PartMetadata {
	t.Helper()
	partsDir := filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")
	if err := os.MkdirAll(partsDir, 0755); err != nil {
		t.Fatalf("creating parts dir: %v", err)
	}
	partPath := filepath.Join(partsDir, fmt.Sprintf("part-%d", partNumber))
	if err := os.WriteFile(partPath, []byte(content), 0644); err != nil {
		t.Fatalf("writing part data: %v", err)
	}
	sum := md5.Sum([]byte(content))
	return PartMetadata{
		PartNumber: partNumber,
		ETag:       hex.EncodeToString(sum[:]),
		Size:       int64(len(content)),
		StoredPath: partPath,
	}
}

// mpCompleteBody builds a CompleteMultipartUpload XML body.
func mpCompleteBody(parts ...PartToUpload) *bytes.Buffer {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, p := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", p.PartNumber, p.ETag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	return &b
}

// ---- Part A: storage.go helpers ----

func TestWriteFileAtomic_WritesAndCreatesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	if err := writeFileAtomic(p, []byte("hello"), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
}

func TestWriteFileAtomic_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	if err := os.WriteFile(p, []byte("old-content"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeFileAtomic(p, []byte("new"), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	// No temp files may be left behind in the directory.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "obj" {
		t.Fatalf("unexpected dir entries after write: %v", entries)
	}
}

func TestWriteFileAtomic_RenameFailureCleansTempFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("rename-over-directory succeeds for root")
	}
	dir := t.TempDir()
	// A non-empty directory at the target path makes os.Rename fail
	// (cannot rename a file over a non-empty directory).
	target := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(target, "inner"), 0755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := writeFileAtomic(target, []byte("x"), 0644); err == nil {
		t.Fatal("expected writeFileAtomic to fail renaming over a non-empty dir")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind after failed rename: %s", e.Name())
		}
	}
}

func TestWriteFileAtomicJSON_RoundTrip(t *testing.T) {
	type payload struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	want := payload{A: 42, B: "hello"}
	if err := writeFileAtomicJSON(p, want, 0644); err != nil {
		t.Fatalf("writeFileAtomicJSON: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got payload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// Must be MarshalIndent-formatted (two-space indent).
	if !bytes.Contains(data, []byte("\n  \"a\": 42")) {
		t.Fatalf("expected indented JSON, got: %s", data)
	}
}

func TestLockObject_SerializesConcurrentWriters(t *testing.T) {
	const n = 100
	counter := 0
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			unlock := lockObject("/virtual/path")
			defer unlock()
			c := counter
			counter = c + 1
		})
	}
	wg.Wait()
	if counter != n {
		t.Fatalf("counter = %d, want %d (writes were not serialized)", counter, n)
	}
}

// ---- Part B: uploadID validation gates ----

func TestUploadPartHandler_InvalidUploadIDRejected(t *testing.T) {
	dataDir := mpTestConfig(t)
	mpTestBucket(t, dataDir, "bkt")

	r := httptest.NewRequest(http.MethodPut, "/bkt/obj?partNumber=1&uploadId=../evil", strings.NewReader("data"))
	w := httptest.NewRecorder()
	uploadPartHandler(w, r, "bkt", "obj", "1", "../evil")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("body missing InvalidArgument: %s", w.Body.String())
	}
	// Nothing may be written outside .uploads (no traversal artifact).
	if _, err := os.Stat(filepath.Join(dataDir, "evil.json")); !os.IsNotExist(err) {
		t.Errorf("path traversal artifact created at dataDir root")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bkt", ".metadata", "evil.json")); !os.IsNotExist(err) {
		t.Errorf("path traversal artifact created in .metadata")
	}
}

func TestCompleteMultipartUploadHandler_InvalidUploadIDRejected(t *testing.T) {
	dataDir := mpTestConfig(t)
	bucketPath := mpTestBucket(t, dataDir, "bkt")

	r := httptest.NewRequest(http.MethodPost, "/bkt/obj?uploadId=ZZZZ", mpCompleteBody())
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, r, "bkt", "obj", "ZZZZ")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("body missing InvalidArgument: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(bucketPath, ".metadata", "ZZZZ.json")); !os.IsNotExist(err) {
		t.Errorf("artifact created in .metadata for malformed uploadId")
	}
}

func TestAbortMultipartUploadHandler_InvalidUploadIDRejected(t *testing.T) {
	dataDir := mpTestConfig(t)
	mpTestBucket(t, dataDir, "bkt")

	r := httptest.NewRequest(http.MethodDelete, "/bkt/obj?uploadId=../../evil", nil)
	w := httptest.NewRecorder()
	abortMultipartUploadHandler(w, r, "bkt", "obj", "../../evil")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("body missing InvalidArgument: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "evil.json")); !os.IsNotExist(err) {
		t.Errorf("path traversal artifact created at dataDir root")
	}
}

// ---- Part B: abort key validation ----

func TestAbortMultipartUploadHandler_WrongKeyReturnsNoSuchUpload(t *testing.T) {
	dataDir := mpTestConfig(t)
	bucketPath := mpTestBucket(t, dataDir, "bkt")
	uploadID := newTestUploadID(t)

	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{
		UploadID: uploadID,
		Key:      "other-object",
		Parts:    make(map[int]PartMetadata),
	})

	r := httptest.NewRequest(http.MethodDelete, "/bkt/obj?uploadId="+uploadID, nil)
	w := httptest.NewRecorder()
	abortMultipartUploadHandler(w, r, "bkt", "obj", uploadID)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchUpload") {
		t.Fatalf("body missing NoSuchUpload: %s", w.Body.String())
	}
	// The other upload's metadata must not have been deleted.
	metaPath := filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("upload meta was deleted on key mismatch: %v", err)
	}
}

func TestAbortMultipartUploadHandler_CorrectKeyAborts(t *testing.T) {
	dataDir := mpTestConfig(t)
	bucketPath := mpTestBucket(t, dataDir, "bkt")
	uploadID := newTestUploadID(t)

	mp := MultipartUpload{UploadID: uploadID, Key: "obj", Parts: make(map[int]PartMetadata)}
	mp.Parts[1] = mpStorePart(t, bucketPath, uploadID, 1, "part-one")
	mpWriteUploadMeta(t, bucketPath, uploadID, mp)

	r := httptest.NewRequest(http.MethodDelete, "/bkt/obj?uploadId="+uploadID, nil)
	w := httptest.NewRecorder()
	abortMultipartUploadHandler(w, r, "bkt", "obj", uploadID)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")); !os.IsNotExist(err) {
		t.Errorf("upload meta still exists after abort")
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")); !os.IsNotExist(err) {
		t.Errorf("parts dir still exists after abort")
	}
}

// ---- Part B: complete atomicity + content-type propagation ----

func TestCompleteMultipartUploadHandler_FailurePreservesOldObject(t *testing.T) {
	dataDir := mpTestConfig(t)
	bucketPath := mpTestBucket(t, dataDir, "bkt")
	uploadID := newTestUploadID(t)
	key := "data/obj"

	// Pre-existing object with metadata (as putObjectHandler would leave it).
	if err := os.MkdirAll(filepath.Join(bucketPath, "data"), 0755); err != nil {
		t.Fatalf("seed object dir: %v", err)
	}
	finalObjectPath := filepath.Join(bucketPath, key)
	if err := os.WriteFile(finalObjectPath, []byte("OLD-OBJECT"), 0644); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	oldMeta := ObjectMetadata{ContentType: "text/plain", ContentLength: 11, ETag: "oldetag", StoragePath: finalObjectPath}
	oldMetaJSON, _ := json.MarshalIndent(oldMeta, "", "  ")
	metaPath := filepath.Join(bucketPath, ".metadata", key+".meta")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0755); err != nil {
		t.Fatalf("seed meta dir: %v", err)
	}
	if err := os.WriteFile(metaPath, oldMetaJSON, 0644); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	// Upload references a part whose data file is missing on disk → the copy
	// step fails AFTER the temp assembly file has been created.
	stored := mpStorePart(t, bucketPath, uploadID, 1, "part-one")
	if err := os.Remove(stored.StoredPath); err != nil {
		t.Fatalf("removing part file to simulate failure: %v", err)
	}
	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{
		UploadID: uploadID,
		Key:      key,
		Parts:    map[int]PartMetadata{1: stored},
	})

	r := httptest.NewRequest(http.MethodPost, "/bkt/"+key+"?uploadId="+uploadID, mpCompleteBody(PartToUpload{PartNumber: 1, ETag: stored.ETag}))
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, r, "bkt", key, uploadID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (InternalError)", w.Code)
	}

	// Old object bytes must be intact.
	got, err := os.ReadFile(finalObjectPath)
	if err != nil {
		t.Fatalf("old object missing after failed complete: %v", err)
	}
	if string(got) != "OLD-OBJECT" {
		t.Fatalf("old object corrupted: %q", got)
	}
	// Old metadata must be intact.
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("old meta missing after failed complete: %v", err)
	}
	if !bytes.Contains(metaData, []byte(`"oldetag"`)) {
		t.Fatalf("old meta overwritten after failed complete: %s", metaData)
	}
	// No temp assembly file may be left behind.
	leftovers, _ := filepath.Glob(filepath.Join(bucketPath, "data", "*.tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind after failed complete: %v", leftovers)
	}
}

func TestCompleteMultipartUploadHandler_SuccessAndContentType(t *testing.T) {
	dataDir := mpTestConfig(t)
	bucketPath := mpTestBucket(t, dataDir, "bkt")
	key := "obj"

	// 1. Initiate with a Content-Type header.
	initReq := httptest.NewRequest(http.MethodPost, "/bkt/"+key+"?uploads", nil)
	initReq.Header.Set("Content-Type", "application/x-custom-type")
	initW := httptest.NewRecorder()
	initiateMultipartUploadHandler(initW, initReq, "bkt", key)

	if initW.Code != http.StatusOK {
		t.Fatalf("initiate status = %d, body: %s", initW.Code, initW.Body.String())
	}
	var initResult InitiateMultipartUploadResult
	if err := xml.Unmarshal(initW.Body.Bytes(), &initResult); err != nil {
		t.Fatalf("parsing initiate response: %v", err)
	}
	uploadID := initResult.UploadID
	if len(uploadID) != 32 {
		t.Fatalf("uploadID %q is not 32 hex chars", uploadID)
	}

	// Initiated metadata must carry the Content-Type.
	metaRaw, err := os.ReadFile(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json"))
	if err != nil {
		t.Fatalf("reading initiated upload meta: %v", err)
	}
	var initiated MultipartUpload
	if err := json.Unmarshal(metaRaw, &initiated); err != nil {
		t.Fatalf("parsing initiated upload meta: %v", err)
	}
	if initiated.ContentType != "application/x-custom-type" {
		t.Fatalf("initiated ContentType = %q, want %q", initiated.ContentType, "application/x-custom-type")
	}

	// 2. Upload one part.
	partContent := "part-one-data"
	stored := mpStorePart(t, bucketPath, uploadID, 1, partContent)
	// Bring the handler's bookkeeping in line with what uploadPartHandler would store.
	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{
		UploadID:    uploadID,
		Key:         key,
		ContentType: "application/x-custom-type",
		Parts:       map[int]PartMetadata{1: stored},
	})

	// 3. Complete.
	compReq := httptest.NewRequest(http.MethodPost, "/bkt/"+key+"?uploadId="+uploadID, mpCompleteBody(PartToUpload{PartNumber: 1, ETag: stored.ETag}))
	compW := httptest.NewRecorder()
	completeMultipartUploadHandler(compW, compReq, "bkt", key, uploadID)

	if compW.Code != http.StatusOK {
		t.Fatalf("complete status = %d, body: %s", compW.Code, compW.Body.String())
	}

	// Assembled object content.
	got, err := os.ReadFile(filepath.Join(bucketPath, key))
	if err != nil {
		t.Fatalf("reading completed object: %v", err)
	}
	if string(got) != partContent {
		t.Fatalf("object content = %q, want %q", got, partContent)
	}

	// Final object meta carries the initiated Content-Type.
	metaData, err := os.ReadFile(filepath.Join(bucketPath, ".metadata", key+".meta"))
	if err != nil {
		t.Fatalf("reading final object meta: %v", err)
	}
	var finalMeta ObjectMetadata
	if err := json.Unmarshal(metaData, &finalMeta); err != nil {
		t.Fatalf("parsing final object meta: %v", err)
	}
	if finalMeta.ContentType != "application/x-custom-type" {
		t.Fatalf("final meta ContentType = %q, want %q", finalMeta.ContentType, "application/x-custom-type")
	}
	if finalMeta.ContentLength != int64(len(partContent)) {
		t.Fatalf("final meta ContentLength = %d, want %d", finalMeta.ContentLength, len(partContent))
	}

	// Upload session cleaned up.
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")); !os.IsNotExist(err) {
		t.Errorf("upload meta still exists after complete")
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")); !os.IsNotExist(err) {
		t.Errorf("parts dir still exists after complete")
	}
}
