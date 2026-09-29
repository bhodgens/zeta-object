package s3

// moved_multipart_routing_test.go — ports the root package's multipart
// error-path/atomicity suite (multipart_handlers_test.go) and the routing
// matrix (routing_test.go) onto the s3 package's seams (leaf 6.2).

import (
	"bytes"
	"crypto/md5" //nolint:gosec // G501: MD5 is the S3 ETag/upload-ID algorithm — protocol requirement, not crypto.
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- multipart helpers (port of the mp* helpers) ----

// mpTestBucket creates an empty bucket directory under the env's dataDir.
func (env *testS3Env) mpTestBucket(t *testing.T, name string) string {
	t.Helper()
	return env.setupBucket(t, name)
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

// newTestUploadIDSuffix returns a distinct structurally valid upload ID for
// the test plus a suffix.
func newTestUploadIDSuffix(t *testing.T, suffix string) string {
	t.Helper()
	sum := md5.Sum([]byte(t.Name() + "/" + suffix))
	return hex.EncodeToString(sum[:])
}

// mpWriteUploadMeta writes a MultipartUpload metadata file for uploadID.
func mpWriteUploadMeta(t *testing.T, bucketPath, uploadID string, mp MultipartUpload) {
	t.Helper()
	if err := os.MkdirAll(mpUploadsDir(bucketPath), 0o755); err != nil {
		t.Fatalf("creating .uploads dir: %v", err)
	}
	data, err := json.MarshalIndent(mp, "", "  ")
	if err != nil {
		t.Fatalf("marshalling upload meta: %v", err)
	}
	path := filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing upload meta: %v", err)
	}
}

// mpStorePart writes a part data file on disk and returns its PartMetadata.
func mpStorePart(t *testing.T, bucketPath, uploadID string, partNumber int, content string) PartMetadata {
	t.Helper()
	partsDir := filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		t.Fatalf("creating parts dir: %v", err)
	}
	partPath := filepath.Join(partsDir, fmt.Sprintf("part-%d", partNumber))
	if err := os.WriteFile(partPath, []byte(content), 0o644); err != nil {
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

// mpHashETag returns the md5 hex ETag of content (what uploadPartHandler stores).
func mpHashETag(content string) string {
	sum := md5.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

// mpSeedUploadWithParts creates a session meta plus part files for the given
// part numbers/content and returns the upload id.
func mpSeedUploadWithParts(t *testing.T, bucketPath, key string, parts map[int]string) string {
	t.Helper()
	uploadID := newTestUploadID(t)
	mp := MultipartUpload{UploadID: uploadID, Key: key, Initiated: time.Now().UTC(), Parts: make(map[int]PartMetadata)}
	for num, content := range parts {
		mp.Parts[num] = mpStorePart(t, bucketPath, uploadID, num, content)
	}
	mpWriteUploadMeta(t, bucketPath, uploadID, mp)
	return uploadID
}

// mpCallUploadPart invokes uploadPartHandler with an optional header map.
func mpCallUploadPart(t *testing.T, bucketName, objectName, partNumber, uploadID, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/" + bucketName + "/" + objectName + "?partNumber=" + partNumber + "&uploadId=" + uploadID
	r := httptest.NewRequest(http.MethodPut, url, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	uploadPartHandler(w, r, bucketName, objectName, partNumber, uploadID)
	return w
}

// mpAssertS3Error checks status and error code in an errorToXML response.
func mpAssertS3Error(t *testing.T, w *httptest.ResponseRecorder, wantCode int, wantCodeStr string) {
	t.Helper()
	if w.Code != wantCode {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, wantCode, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), wantCodeStr) {
		t.Fatalf("body missing %s: %s", wantCodeStr, w.Body.String())
	}
}

// mpReadUploadMeta reads and unmarshals a session meta JSON file.
func mpReadUploadMeta(t *testing.T, bucketPath, uploadID string) MultipartUpload {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json"))
	if err != nil {
		t.Fatalf("reading upload meta: %v", err)
	}
	var mp MultipartUpload
	if err := json.Unmarshal(raw, &mp); err != nil {
		t.Fatalf("parsing upload meta: %v", err)
	}
	return mp
}

// mpChunkedBody wraps data in a minimal aws-chunked framing.
func mpChunkedBody(data string) string {
	return fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(data), data)
}

// ---- Part A: storage.go helpers ----

func TestWriteFileAtomic_WritesAndCreatesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	if err := writeFileAtomic(p, []byte("hello"), 0o644); err != nil {
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
	if err := os.WriteFile(p, []byte("old-content"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeFileAtomic(p, []byte("new"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
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
	target := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(target, "inner"), 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := writeFileAtomic(target, []byte("x"), 0o644); err == nil {
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
	if err := writeFileAtomicJSON(p, want, 0o644); err != nil {
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
	env := setupS3TestEnv(t)
	env.mpTestBucket(t, "bkt")

	r := httptest.NewRequest(http.MethodPut, "/bkt/obj?partNumber=1&uploadId=../evil", strings.NewReader("data"))
	w := httptest.NewRecorder()
	uploadPartHandler(w, r, "bkt", "obj", "1", "../evil")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("body missing InvalidArgument: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "evil.json")); !os.IsNotExist(err) {
		t.Errorf("path traversal artifact created at dataDir root")
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "bkt", ".metadata", "evil.json")); !os.IsNotExist(err) {
		t.Errorf("path traversal artifact created in .metadata")
	}
}

func TestCompleteMultipartUploadHandler_InvalidUploadIDRejected(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

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
	env := setupS3TestEnv(t)
	env.mpTestBucket(t, "bkt")

	r := httptest.NewRequest(http.MethodDelete, "/bkt/obj?uploadId=../../evil", nil)
	w := httptest.NewRecorder()
	abortMultipartUploadHandler(w, r, "bkt", "obj", "../../evil")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("body missing InvalidArgument: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "evil.json")); !os.IsNotExist(err) {
		t.Errorf("path traversal artifact created at dataDir root")
	}
}

// ---- Part B: abort key validation ----

func TestAbortMultipartUploadHandler_WrongKeyReturnsNoSuchUpload(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
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
	metaPath := filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("upload meta was deleted on key mismatch: %v", err)
	}
}

func TestAbortMultipartUploadHandler_CorrectKeyAborts(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
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
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := newTestUploadID(t)
	key := "data/obj"

	// Pre-existing object with metadata (as putObjectHandler would leave it).
	if err := os.MkdirAll(filepath.Join(bucketPath, "data"), 0o755); err != nil {
		t.Fatalf("seed object dir: %v", err)
	}
	finalObjectPath := filepath.Join(bucketPath, key)
	if err := os.WriteFile(finalObjectPath, []byte("OLD-OBJECT"), 0o644); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	oldMeta := ObjectMetadata{ContentType: "text/plain", ContentLength: 11, ETag: "oldetag", StoragePath: finalObjectPath}
	oldMetaJSON, _ := json.MarshalIndent(oldMeta, "", "  ")
	metaPath := filepath.Join(bucketPath, ".metadata", key+".meta")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatalf("seed meta dir: %v", err)
	}
	if err := os.WriteFile(metaPath, oldMetaJSON, 0o644); err != nil {
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

	got, err := os.ReadFile(finalObjectPath)
	if err != nil {
		t.Fatalf("old object missing after failed complete: %v", err)
	}
	if string(got) != "OLD-OBJECT" {
		t.Fatalf("old object corrupted: %q", got)
	}
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("old meta missing after failed complete: %v", err)
	}
	if !bytes.Contains(metaData, []byte(`"oldetag"`)) {
		t.Fatalf("old meta overwritten after failed complete: %s", metaData)
	}
	leftovers, _ := filepath.Glob(filepath.Join(bucketPath, "data", "*.tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind after failed complete: %v", leftovers)
	}
}

func TestCompleteMultipartUploadHandler_SuccessAndContentType(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
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

	got, err := os.ReadFile(filepath.Join(bucketPath, key))
	if err != nil {
		t.Fatalf("reading completed object: %v", err)
	}
	if string(got) != partContent {
		t.Fatalf("object content = %q, want %q", got, partContent)
	}

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

	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")); !os.IsNotExist(err) {
		t.Errorf("upload meta still exists after complete")
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")); !os.IsNotExist(err) {
		t.Errorf("parts dir still exists after complete")
	}
}

// ---- Leaf 3.3: ListMultipartUploads ----

// mpCallListUploads invokes listMultipartUploadsHandler with a query string.
func mpCallListUploads(t *testing.T, bucketName, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/" + bucketName
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	listMultipartUploadsHandler(w, r, bucketName)
	return w
}

func TestListMultipartUploadsHandler_EmptyBucket(t *testing.T) {
	env := setupS3TestEnv(t)
	env.mpTestBucket(t, "bkt")

	w := mpCallListUploads(t, "bkt", "uploads")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), xml.Header) {
		t.Errorf("missing xml prolog: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<ListMultipartUploadsResult xmlns="`+s3XMLNamespace+`">`) {
		t.Errorf("missing ListMultipartUploadsResult/ns: %s", w.Body.String())
	}
	var result ListMultipartUploadsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.IsTruncated {
		t.Errorf("IsTruncated = true, want false")
	}
	if result.MaxUploads != 1000 {
		t.Errorf("MaxUploads = %d, want default 1000", result.MaxUploads)
	}
	if len(result.Upload) != 0 {
		t.Errorf("Upload = %+v, want empty", result.Upload)
	}
}

func TestListMultipartUploadsHandler_OrderingByKeyThenUploadID(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	// Insert out of lexicographic order to prove sorting.
	idB := newTestUploadIDSuffix(t, "zeta")
	mpWriteUploadMeta(t, bucketPath, idB, MultipartUpload{UploadID: idB, Key: "zeta", Initiated: time.Now().UTC(), Parts: make(map[int]PartMetadata)})
	idA := newTestUploadIDSuffix(t, "alpha")
	mpWriteUploadMeta(t, bucketPath, idA, MultipartUpload{UploadID: idA, Key: "alpha", Initiated: time.Now().UTC(), Parts: make(map[int]PartMetadata)})
	idC := newTestUploadIDSuffix(t, "zeta2")
	mpWriteUploadMeta(t, bucketPath, idC, MultipartUpload{UploadID: idC, Key: "zeta", Initiated: time.Now().UTC(), Parts: make(map[int]PartMetadata)})
	zetaIDs := []string{idB, idC}
	sort.Strings(zetaIDs)

	w := mpCallListUploads(t, "bkt", "uploads")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var result ListMultipartUploadsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Upload) != 3 {
		t.Fatalf("got %d uploads, want 3: %+v", len(result.Upload), result.Upload)
	}
	wantOrder := []struct{ key, id string }{{"alpha", idA}, {"zeta", zetaIDs[0]}, {"zeta", zetaIDs[1]}}
	for i, want := range wantOrder {
		if result.Upload[i].Key != want.key || result.Upload[i].UploadID != want.id {
			t.Errorf("upload[%d] = (%s,%s), want (%s,%s)", i, result.Upload[i].Key, result.Upload[i].UploadID, want.key, want.id)
		}
		if result.Upload[i].Initiated == "" {
			t.Errorf("upload[%d] missing Initiated", i)
		}
	}
	if result.Bucket != "bkt" {
		t.Errorf("Bucket = %q, want bkt", result.Bucket)
	}
}

func TestListMultipartUploadsHandler_PrefixFilter(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	idMatch := newTestUploadIDSuffix(t, "match")
	mpWriteUploadMeta(t, bucketPath, idMatch, MultipartUpload{UploadID: idMatch, Key: "logs/2026/a", Parts: make(map[int]PartMetadata)})
	idNo := newTestUploadIDSuffix(t, "no")
	mpWriteUploadMeta(t, bucketPath, idNo, MultipartUpload{UploadID: idNo, Key: "photos/x", Parts: make(map[int]PartMetadata)})

	w := mpCallListUploads(t, "bkt", "uploads&prefix=logs/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var result ListMultipartUploadsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Upload) != 1 || result.Upload[0].UploadID != idMatch {
		t.Fatalf("uploads = %+v, want only %s", result.Upload, idMatch)
	}
	if result.Prefix != "logs/" {
		t.Errorf("Prefix = %q, want logs/", result.Prefix)
	}
}

func TestListMultipartUploadsHandler_MarkerPagination(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	ids := make([]string, 3)
	keys := []string{"a", "b", "c"}
	for i, key := range keys {
		ids[i] = newTestUploadIDSuffix(t, key)
		mpWriteUploadMeta(t, bucketPath, ids[i], MultipartUpload{UploadID: ids[i], Key: key, Parts: make(map[int]PartMetadata)})
	}

	// Page 1: max-uploads=1 → only "a", truncated, NextKeyMarker="a".
	w := mpCallListUploads(t, "bkt", "uploads&max-uploads=1")
	var p1 ListMultipartUploadsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &p1); err != nil {
		t.Fatalf("unmarshal page1: %v", err)
	}
	if len(p1.Upload) != 1 || p1.Upload[0].Key != "a" {
		t.Fatalf("page1 uploads = %+v, want [a]", p1.Upload)
	}
	if !p1.IsTruncated {
		t.Errorf("page1 IsTruncated = false, want true")
	}
	if p1.MaxUploads != 1 {
		t.Errorf("page1 MaxUploads = %d, want 1", p1.MaxUploads)
	}
	if p1.NextKeyMarker != "a" {
		t.Errorf("page1 NextKeyMarker = %q, want a", p1.NextKeyMarker)
	}

	// Page 2: key-marker=a → b, c; not truncated.
	w = mpCallListUploads(t, "bkt", "uploads&key-marker=a")
	var p2 ListMultipartUploadsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &p2); err != nil {
		t.Fatalf("unmarshal page2: %v", err)
	}
	if len(p2.Upload) != 2 || p2.Upload[0].Key != "b" || p2.Upload[1].Key != "c" {
		t.Fatalf("page2 uploads = %+v, want [b c]", p2.Upload)
	}
	if p2.IsTruncated {
		t.Errorf("page2 IsTruncated = true, want false")
	}
	if p2.KeyMarker != "a" {
		t.Errorf("page2 KeyMarker = %q, want a", p2.KeyMarker)
	}
}

func TestListMultipartUploadsHandler_NoSuchBucket(t *testing.T) {
	setupS3TestEnv(t)
	w := mpCallListUploads(t, "missing-bucket", "uploads")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Fatalf("body missing NoSuchBucket: %s", w.Body.String())
	}
}

// ---- Leaf 3.3: ListParts ----

func mpCallListParts(t *testing.T, bucketName, objectName, uploadID, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/" + bucketName + "/" + objectName
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	listPartsHandler(w, r, bucketName, objectName, uploadID)
	return w
}

func TestListPartsHandler_PartsSortedWithFields(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := mpSeedUploadWithParts(t, bucketPath, "obj", map[int]string{3: "ccc", 1: "a", 2: "bb"})

	w := mpCallListParts(t, "bkt", "obj", uploadID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var result ListPartsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Part) != 3 {
		t.Fatalf("got %d parts, want 3: %+v", len(result.Part), result.Part)
	}
	for i, p := range result.Part {
		if p.PartNumber != i+1 {
			t.Errorf("part[%d].PartNumber = %d, want %d", i, p.PartNumber, i+1)
		}
		if !strings.HasPrefix(p.ETag, `"`) || !strings.HasSuffix(p.ETag, `"`) {
			t.Errorf("part[%d].ETag = %q, want quoted", i, p.ETag)
		}
		if p.Size <= 0 {
			t.Errorf("part[%d].Size = %d, want > 0", i, p.Size)
		}
		if p.LastModified == "" {
			t.Errorf("part[%d].LastModified empty", i)
		}
	}
	if result.Bucket != "bkt" || result.Key != "obj" || result.UploadID != uploadID {
		t.Errorf("identity = (%q,%q,%q)", result.Bucket, result.Key, result.UploadID)
	}
	if result.MaxParts != 1000 {
		t.Errorf("MaxParts = %d, want default 1000", result.MaxParts)
	}
	if result.PartNumberMarker != 0 {
		t.Errorf("PartNumberMarker = %d, want 0", result.PartNumberMarker)
	}
	if result.IsTruncated {
		t.Errorf("IsTruncated = true, want false")
	}
}

func TestListPartsHandler_MarkerAndMaxPartsPagination(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := mpSeedUploadWithParts(t, bucketPath, "obj", map[int]string{1: "a", 2: "bb", 3: "ccc", 4: "dddd"})

	// Page 1: max-parts=2 → parts 1,2; truncated; NextPartNumberMarker=2.
	w := mpCallListParts(t, "bkt", "obj", uploadID, "max-parts=2")
	var p1 ListPartsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &p1); err != nil {
		t.Fatalf("unmarshal page1: %v", err)
	}
	if len(p1.Part) != 2 || p1.Part[0].PartNumber != 1 || p1.Part[1].PartNumber != 2 {
		t.Fatalf("page1 parts = %+v, want [1 2]", p1.Part)
	}
	if !p1.IsTruncated {
		t.Errorf("page1 IsTruncated = false, want true")
	}
	if p1.NextPartNumberMarker != 2 {
		t.Errorf("page1 NextPartNumberMarker = %d, want 2", p1.NextPartNumberMarker)
	}
	if p1.MaxParts != 2 {
		t.Errorf("page1 MaxParts = %d, want 2", p1.MaxParts)
	}

	// Page 2: part-number-marker=2 → parts after 2.
	w = mpCallListParts(t, "bkt", "obj", uploadID, "part-number-marker=2")
	var p2 ListPartsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &p2); err != nil {
		t.Fatalf("unmarshal page2: %v", err)
	}
	if len(p2.Part) != 2 || p2.Part[0].PartNumber != 3 || p2.Part[1].PartNumber != 4 {
		t.Fatalf("page2 parts = %+v, want [3 4]", p2.Part)
	}
	if p2.IsTruncated {
		t.Errorf("page2 IsTruncated = true, want false")
	}
	if p2.PartNumberMarker != 2 {
		t.Errorf("page2 PartNumberMarker = %d, want 2", p2.PartNumberMarker)
	}
}

func TestListPartsHandler_NoSuchUploadBadID(t *testing.T) {
	env := setupS3TestEnv(t)
	env.mpTestBucket(t, "bkt")

	// Nonexistent but structurally valid id.
	w := mpCallListParts(t, "bkt", "obj", "0123456789abcdef0123456789abcdef", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchUpload") {
		t.Fatalf("body missing NoSuchUpload: %s", w.Body.String())
	}

	// Malformed id → InvalidArgument (validateUploadID gate).
	w = mpCallListParts(t, "bkt", "obj", "../evil", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed id status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("malformed id body missing InvalidArgument: %s", w.Body.String())
	}
}

func TestListPartsHandler_KeyMismatchIsNoSuchUpload(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := mpSeedUploadWithParts(t, bucketPath, "real-key", map[int]string{1: "a"})

	w := mpCallListParts(t, "bkt", "other-key", uploadID, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchUpload") {
		t.Fatalf("body missing NoSuchUpload: %s", w.Body.String())
	}
}

// ---- Leaf 3.3: expiry sweep ----

func TestSweepExpiredUploads_RemovesOldKeepsFresh(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	// Expired session: backdated Initiated, with a parts dir on disk.
	oldID := newTestUploadIDSuffix(t, "old")
	oldMP := MultipartUpload{UploadID: oldID, Key: "obj", Initiated: time.Now().UTC().Add(-multipartUploadExpiry - time.Hour), Parts: make(map[int]PartMetadata)}
	oldMP.Parts[1] = mpStorePart(t, bucketPath, oldID, 1, "stale")
	mpWriteUploadMeta(t, bucketPath, oldID, oldMP)

	// Fresh session must be untouched.
	freshID := newTestUploadIDSuffix(t, "fresh")
	mpWriteUploadMeta(t, bucketPath, freshID, MultipartUpload{UploadID: freshID, Key: "obj", Initiated: time.Now().UTC(), Parts: make(map[int]PartMetadata)})

	removed := sweepExpiredUploads(bucketPath)
	if removed != 1 {
		t.Fatalf("sweepExpiredUploads = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), oldID+".json")); !os.IsNotExist(err) {
		t.Errorf("expired upload meta still exists")
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), oldID+"_parts")); !os.IsNotExist(err) {
		t.Errorf("expired parts dir still exists")
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), freshID+".json")); err != nil {
		t.Errorf("fresh upload meta removed: %v", err)
	}
}

func TestSweepExpiredUploads_EmptyBucketReturnsZero(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	if got := sweepExpiredUploads(bucketPath); got != 0 {
		t.Fatalf("sweepExpiredUploads = %d, want 0", got)
	}
}

// ---- Leaf 3.3: EntityTooSmall on non-final parts ----

func TestCompleteMultipartUploadHandler_EntityTooSmallNonFinalPart(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	key := "obj"

	// Two small parts (both < 5MiB); part 1 is non-final → reject.
	uploadID := mpSeedUploadWithParts(t, bucketPath, key, map[int]string{1: "small-one", 2: "small-two"})

	body := mpCompleteBody(
		PartToUpload{PartNumber: 1, ETag: mpHashETag("small-one")},
		PartToUpload{PartNumber: 2, ETag: mpHashETag("small-two")},
	)
	r := httptest.NewRequest(http.MethodPost, "/bkt/"+key+"?uploadId="+uploadID, body)
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, r, "bkt", key, uploadID)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "EntityTooSmall") {
		t.Fatalf("body missing EntityTooSmall: %s", w.Body.String())
	}
	// The upload session must survive a rejected complete.
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")); err != nil {
		t.Errorf("upload session removed on EntityTooSmall: %v", err)
	}
}

func TestCompleteMultipartUploadHandler_SmallFinalPartAllowed(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	key := "obj"

	// Small non-final part padded to >= minPartSize, small final part → OK.
	big := strings.Repeat("B", minPartSize)
	uploadID := mpSeedUploadWithParts(t, bucketPath, key, map[int]string{1: big, 2: "tiny-last"})

	body := mpCompleteBody(
		PartToUpload{PartNumber: 1, ETag: mpHashETag(big)},
		PartToUpload{PartNumber: 2, ETag: mpHashETag("tiny-last")},
	)
	r := httptest.NewRequest(http.MethodPost, "/bkt/"+key+"?uploadId="+uploadID, body)
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, r, "bkt", key, uploadID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(bucketPath, key))
	if err != nil {
		t.Fatalf("reading completed object: %v", err)
	}
	if string(got) != big+"tiny-last" {
		t.Fatalf("object content wrong: %d bytes", len(got))
	}
}

// ---- Leaf 4.4: multipart error paths ----

func TestInitiateMultipartUploadHandler_InvalidKeyNoUploadsDir(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	w := httptest.NewRecorder()
	initiateMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/a/../evil?uploads", nil), "bkt", "a/../evil")

	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidArgument")
	if _, err := os.Stat(mpUploadsDir(bucketPath)); !os.IsNotExist(err) {
		t.Errorf(".uploads dir created despite invalid key")
	}
}

func TestInitiateMultipartUploadHandler_NoSuchBucket(t *testing.T) {
	env := setupS3TestEnv(t)

	w := httptest.NewRecorder()
	initiateMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/missing/obj?uploads", nil), "missing", "obj")

	mpAssertS3Error(t, w, http.StatusNotFound, "NoSuchBucket")
	if _, err := os.Stat(filepath.Join(env.dataDir, "missing")); !os.IsNotExist(err) {
		t.Errorf("bucket dir created for nonexistent bucket")
	}
}

func TestInitiateMultipartUploadHandler_MetaHeaderCapture(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	r := httptest.NewRequest(http.MethodPost, "/bkt/obj?uploads", nil)
	r.Header.Set("Content-Type", "application/x-test")
	r.Header.Set("X-Amz-Meta-Color", "blue")
	r.Header.Set("X-Amz-Meta-Origin", "deep-space")
	w := httptest.NewRecorder()
	initiateMultipartUploadHandler(w, r, "bkt", "obj")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var result InitiateMultipartUploadResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mp := mpReadUploadMeta(t, bucketPath, result.UploadID)
	if len(mp.CustomMetadata) != 2 {
		t.Fatalf("CustomMetadata = %v, want 2 entries", mp.CustomMetadata)
	}
	got := make(map[string]string, len(mp.CustomMetadata))
	for k, v := range mp.CustomMetadata {
		got[strings.ToLower(k)] = v
	}
	if got["x-amz-meta-color"] != "blue" || got["x-amz-meta-origin"] != "deep-space" {
		t.Fatalf("CustomMetadata = %v, want color=blue origin=deep-space", mp.CustomMetadata)
	}
}

func TestInitiateMultipartUploadHandler_UploadsDirCreateFailure(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	// .metadata as a regular file makes MkdirAll(.metadata/.uploads) fail.
	if err := os.Remove(filepath.Join(bucketPath, ".metadata")); err != nil {
		t.Fatalf("removing .metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bucketPath, ".metadata"), []byte("block"), 0o644); err != nil {
		t.Fatalf("creating .metadata file: %v", err)
	}

	w := httptest.NewRecorder()
	initiateMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/obj?uploads", nil), "bkt", "obj")

	mpAssertS3Error(t, w, http.StatusInternalServerError, "InternalError")
}

// ---- Leaf 4.4: uploadPart error paths ----

func TestUploadPartHandler_PartNumberBoundsAndFormat(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := newTestUploadID(t)
	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{UploadID: uploadID, Key: "obj", Parts: make(map[int]PartMetadata)})

	cases := []struct {
		name string
		pn   string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"over-max", "10001"},
		{"non-numeric", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := mpCallUploadPart(t, "bkt", "obj", tc.pn, uploadID, "data", nil)
			mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidArgument")
		})
	}

	// Malformed uploadId (validateUploadID gate).
	w := mpCallUploadPart(t, "bkt", "obj", "1", "../evil", "data", nil)
	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidArgument")
}

func TestUploadPartHandler_MissingSessionNoSuchUpload(t *testing.T) {
	env := setupS3TestEnv(t)
	env.mpTestBucket(t, "bkt")

	// Structurally valid id, but no session meta on disk.
	w := mpCallUploadPart(t, "bkt", "obj", "1", "0123456789abcdef0123456789abcdef", "data", nil)
	mpAssertS3Error(t, w, http.StatusNotFound, "NoSuchUpload")
}

func TestUploadPartHandler_KeyMismatchNoSuchUpload(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := newTestUploadID(t)
	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{UploadID: uploadID, Key: "real-key", Parts: make(map[int]PartMetadata)})

	w := mpCallUploadPart(t, "bkt", "other-key", "1", uploadID, "data", nil)
	mpAssertS3Error(t, w, http.StatusNotFound, "NoSuchUpload")
	// No part file may be created for the mismatched key.
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")); !os.IsNotExist(err) {
		t.Errorf("parts dir created on key mismatch")
	}
}

func TestUploadPartHandler_AWSChunkedDecodedLengthLies(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := newTestUploadID(t)
	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{UploadID: uploadID, Key: "obj", Parts: make(map[int]PartMetadata)})

	headers := func(decodedLen string) map[string]string {
		return map[string]string{
			"Content-Encoding":             "aws-chunked",
			"x-amz-decoded-content-length": decodedLen,
		}
	}

	// Lying header: body decodes to 4 bytes, header claims 999 → 400.
	w := mpCallUploadPart(t, "bkt", "obj", "1", uploadID, mpChunkedBody("abcd"), headers("999"))
	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidArgument")
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts", "part-1")); !os.IsNotExist(err) {
		t.Errorf("part file stored despite lying decoded length")
	}

	// Honest header → success, ETag matches the decoded body.
	w = mpCallUploadPart(t, "bkt", "obj", "1", uploadID, mpChunkedBody("abcd"), headers("4"))
	if w.Code != http.StatusOK {
		t.Fatalf("honest header status = %d, body: %s", w.Code, w.Body.String())
	}
	wantETag := `"` + mpHashETag("abcd") + `"`
	if got := w.Header().Get("ETag"); got != wantETag {
		t.Errorf("ETag = %s, want %s", got, wantETag)
	}
	mp := mpReadUploadMeta(t, bucketPath, uploadID)
	if pm, ok := mp.Parts[1]; !ok || pm.ETag != mpHashETag("abcd") {
		t.Fatalf("session part 1 = %+v, want ETag of %q", pm, "abcd")
	}
}

func TestUploadPartHandler_ReadOnlyPartsDirNoTempLitter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based rejection does not apply to root")
	}
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := newTestUploadID(t)
	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{UploadID: uploadID, Key: "obj", Parts: make(map[int]PartMetadata)})

	partsDir := filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		t.Fatalf("creating parts dir: %v", err)
	}
	if err := os.Chmod(partsDir, 0o555); err != nil {
		t.Fatalf("chmod parts dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(partsDir, 0o755) })

	w := mpCallUploadPart(t, "bkt", "obj", "1", uploadID, "data", nil)
	mpAssertS3Error(t, w, http.StatusInternalServerError, "InternalError")

	// Atomic write contract: no half-written part, no temp litter.
	entries, err := os.ReadDir(partsDir)
	if err != nil {
		t.Fatalf("reading parts dir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("litter in read-only parts dir after failure: %s", e.Name())
	}
	mp := mpReadUploadMeta(t, bucketPath, uploadID)
	if len(mp.Parts) != 0 {
		t.Fatalf("session Parts = %v, want empty after failed write", mp.Parts)
	}
}

// ---- Leaf 4.4: complete error paths ----

func TestCompleteMultipartUploadHandler_EmptyPartsListInvalidPart(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := mpSeedUploadWithParts(t, bucketPath, "obj", map[int]string{1: "a"})

	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/obj?uploadId="+uploadID, mpCompleteBody()), "bkt", "obj", uploadID)

	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidPart")
	// Session must survive the rejection.
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")); err != nil {
		t.Errorf("session removed on empty parts list: %v", err)
	}
}

func TestCompleteMultipartUploadHandler_UnknownPartNumber(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := mpSeedUploadWithParts(t, bucketPath, "obj", map[int]string{1: "a"})

	body := mpCompleteBody(PartToUpload{PartNumber: 7, ETag: mpHashETag("a")})
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/obj?uploadId="+uploadID, body), "bkt", "obj", uploadID)

	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidPart")
	if !strings.Contains(w.Body.String(), "Part number 7 not found") {
		t.Fatalf("body missing not-found message: %s", w.Body.String())
	}
}

func TestCompleteMultipartUploadHandler_ETagMismatch(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := mpSeedUploadWithParts(t, bucketPath, "obj", map[int]string{1: "a"})

	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: "deadbeefdeadbeefdeadbeefdeadbeef"})
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/obj?uploadId="+uploadID, body), "bkt", "obj", uploadID)

	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidPart")
	if !strings.Contains(w.Body.String(), "ETag mismatch") {
		t.Fatalf("body missing ETag mismatch: %s", w.Body.String())
	}
	// The real part file must be untouched.
	partData, err := os.ReadFile(filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts", "part-1"))
	if err != nil || string(partData) != "a" {
		t.Fatalf("part file disturbed: %q err=%v", partData, err)
	}
}

func TestCompleteMultipartUploadHandler_PartsOutOfOrder(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	// The EntityTooSmall gate runs before the order check for the first
	// entry, so the first-listed part must be >= minPartSize for the order
	// violation to be the failure that trips.
	big := strings.Repeat("B", minPartSize)
	uploadID := mpSeedUploadWithParts(t, bucketPath, "obj", map[int]string{1: "a", 2: big})

	body := mpCompleteBody(
		PartToUpload{PartNumber: 2, ETag: mpHashETag(big)},
		PartToUpload{PartNumber: 1, ETag: mpHashETag("a")},
	)
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/obj?uploadId="+uploadID, body), "bkt", "obj", uploadID)

	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidPartOrder")
	if _, err := os.Stat(filepath.Join(bucketPath, "obj")); !os.IsNotExist(err) {
		t.Errorf("object created despite out-of-order parts")
	}
}

func TestCompleteMultipartUploadHandler_NoSuchUploadUnknownOrForeignID(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")

	// Structurally valid but unknown id.
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w,
		httptest.NewRequest(http.MethodPost, "/bkt/obj?uploadId=0123456789abcdef0123456789abcdef", mpCompleteBody(PartToUpload{PartNumber: 1, ETag: "x"})),
		"bkt", "obj", "0123456789abcdef0123456789abcdef")
	mpAssertS3Error(t, w, http.StatusNotFound, "NoSuchUpload")

	// Valid session, wrong key.
	uploadID := mpSeedUploadWithParts(t, bucketPath, "real-key", map[int]string{1: "a"})
	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: mpHashETag("a")})
	w = httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/other-key?uploadId="+uploadID, body), "bkt", "other-key", uploadID)
	mpAssertS3Error(t, w, http.StatusNotFound, "NoSuchUpload")
}

func TestCompleteMultipartUploadHandler_InvalidObjectKey(t *testing.T) {
	env := setupS3TestEnv(t)
	env.mpTestBucket(t, "bkt")

	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w,
		httptest.NewRequest(http.MethodPost, "/bkt/a/../b?uploadId=0123456789abcdef0123456789abcdef", mpCompleteBody(PartToUpload{PartNumber: 1, ETag: "x"})),
		"bkt", "a/../b", "0123456789abcdef0123456789abcdef")
	mpAssertS3Error(t, w, http.StatusBadRequest, "InvalidArgument")
}

// Pins the finalizeComplete failure ordering: when the final metadata write
// fails (here: the .meta target is a non-empty directory, so the atomic
// rename fails), the assembled temp file must NOT be renamed over the object
// path and must be cleaned up — the object stays invisible and the session
// lives.
func TestCompleteMultipartUploadHandler_MetaWriteFailureObjectNotVisible(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	key := "obj"
	uploadID := mpSeedUploadWithParts(t, bucketPath, key, map[int]string{1: "part-one"})

	metaPath := filepath.Join(bucketPath, ".metadata", key+".meta")
	if err := os.MkdirAll(filepath.Join(metaPath, "inner"), 0o755); err != nil {
		t.Fatalf("seeding blocking meta dir: %v", err)
	}

	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: mpHashETag("part-one")})
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/"+key+"?uploadId="+uploadID, body), "bkt", key, uploadID)

	mpAssertS3Error(t, w, http.StatusInternalServerError, "InternalError")

	if _, err := os.Stat(filepath.Join(bucketPath, key)); !os.IsNotExist(err) {
		t.Errorf("object visible despite meta-write failure")
	}
	leftovers, _ := filepath.Glob(filepath.Join(bucketPath, "*.tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp assembly files left behind: %v", leftovers)
	}
	if _, err := os.Stat(filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")); err != nil {
		t.Errorf("session removed despite meta-write failure: %v", err)
	}
}

// Pins the assembleCompletedObject MkdirAll failure branch: with the key's
// metadata subdirectory blocked by a regular file, complete fails after
// assembly and the object must not appear.
func TestCompleteMultipartUploadHandler_MetaDirCreateFailureObjectNotVisible(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	key := "data/obj"
	uploadID := mpSeedUploadWithParts(t, bucketPath, key, map[int]string{1: "part-one"})

	// .metadata/data as a regular file → MkdirAll(.metadata/data) fails.
	if err := os.WriteFile(filepath.Join(bucketPath, ".metadata", "data"), []byte("block"), 0o644); err != nil {
		t.Fatalf("seeding blocking meta path: %v", err)
	}

	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: mpHashETag("part-one")})
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/bkt/"+key+"?uploadId="+uploadID, body), "bkt", key, uploadID)

	mpAssertS3Error(t, w, http.StatusInternalServerError, "InternalError")

	if _, err := os.Stat(filepath.Join(bucketPath, key)); !os.IsNotExist(err) {
		t.Errorf("object visible despite meta-dir failure")
	}
	leftovers, _ := filepath.Glob(filepath.Join(bucketPath, "data", "*.tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp assembly files left behind: %v", leftovers)
	}
}

// ---- Leaf 4.4: abort error paths ----

func TestAbortMultipartUploadHandler_AlreadyAbortedIs404(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := newTestUploadID(t)

	mp := MultipartUpload{UploadID: uploadID, Key: "obj", Parts: make(map[int]PartMetadata)}
	mp.Parts[1] = mpStorePart(t, bucketPath, uploadID, 1, "part-one")
	mpWriteUploadMeta(t, bucketPath, uploadID, mp)

	req := httptest.NewRequest(http.MethodDelete, "/bkt/obj?uploadId="+uploadID, nil)
	w := httptest.NewRecorder()
	abortMultipartUploadHandler(w, req, "bkt", "obj", uploadID)
	if w.Code != http.StatusNoContent {
		t.Fatalf("first abort status = %d, want 204", w.Code)
	}

	// Second abort: session gone → NoSuchUpload.
	w = httptest.NewRecorder()
	abortMultipartUploadHandler(w, req, "bkt", "obj", uploadID)
	mpAssertS3Error(t, w, http.StatusNotFound, "NoSuchUpload")
}

// ---- Leaf 4.4: concurrent uploadPart lock path ----

func TestUploadPartHandler_ConcurrentPartsSameSession(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.mpTestBucket(t, "bkt")
	uploadID := newTestUploadID(t)
	mpWriteUploadMeta(t, bucketPath, uploadID, MultipartUpload{UploadID: uploadID, Key: "obj", Parts: make(map[int]PartMetadata)})

	const workers = 8
	codes := make([]int, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("part-%d-data", i)
			w := mpCallUploadPart(t, "bkt", "obj", strconv.Itoa(i+1), uploadID, content, nil)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("part %d status = %d, want 200", i+1, c)
		}
	}

	mp := mpReadUploadMeta(t, bucketPath, uploadID)
	if len(mp.Parts) != workers {
		t.Fatalf("session Parts = %d entries, want %d (lost update?)", len(mp.Parts), workers)
	}
	for i := range workers {
		content := fmt.Sprintf("part-%d-data", i)
		pm, ok := mp.Parts[i+1]
		if !ok {
			t.Fatalf("part %d missing from session meta", i+1)
		}
		if pm.ETag != mpHashETag(content) {
			t.Errorf("part %d ETag = %s, want %s", i+1, pm.ETag, mpHashETag(content))
		}
		if pm.Size != int64(len(content)) {
			t.Errorf("part %d Size = %d, want %d", i+1, pm.Size, len(content))
		}
	}
}

// ---- Routing matrix (port of routing_test.go, service/bucket/object) ----

// xmlRootName returns the local name of the first XML start element.
func xmlRootName(t *testing.T, body string) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("no XML root element in body: %q", body)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

// TestMovedRouting_ServiceLevel ports the service-level routing matrix.
func TestMovedRouting_ServiceLevel(t *testing.T) {
	setupS3TestEnv(t)

	t.Run("GET / lists buckets", func(t *testing.T) {
		w := httptest.NewRecorder()
		defaultTestFrontend().serviceLevelDispatch(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListAllMyBucketsResult" {
			t.Errorf("XML root = %q, want ListAllMyBucketsResult (listBucketsHandler)", root)
		}
	})

	for _, method := range []string{"PUT", "POST", "DELETE", "HEAD"} {
		t.Run(method+" / is 405", func(t *testing.T) {
			w := httptest.NewRecorder()
			defaultTestFrontend().serviceLevelDispatch(w, httptest.NewRequest(method, "/", nil))
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", w.Code, w.Body.String())
			}
			if want := "Method Not Allowed at service level"; !strings.Contains(w.Body.String(), want) {
				t.Errorf("body %q missing marker %q", w.Body.String(), want)
			}
		})
	}
}

// TestMovedRouting_BucketLevel ports the bucket-level routing matrix.
func TestMovedRouting_BucketLevel(t *testing.T) {
	env := setupS3TestEnv(t)
	const bkt = "route-bucket"

	call := func(method, target string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, bytes.NewReader(body))
		defaultTestFrontend().bucketLevelDispatch(w, req, bkt)
		return w
	}
	callFor := func(method, target, bucket string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, bytes.NewReader(body))
		defaultTestFrontend().bucketLevelDispatch(w, req, bucket)
		return w
	}

	t.Run("PUT /bkt creates bucket (200)", func(t *testing.T) {
		w := call("PUT", "/"+bkt, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if info, err := os.Stat(filepath.Join(env.dataDir, bkt)); err != nil || !info.IsDir() {
			t.Error("bucket directory was not created (createBucketHandler did not answer)")
		}
	})

	t.Run("PUT /bkt again is 409 BucketAlreadyOwnedByYou", func(t *testing.T) {
		w := call("PUT", "/"+bkt, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "BucketAlreadyOwnedByYou") {
			t.Errorf("want BucketAlreadyOwnedByYou, got: %s", w.Body.String())
		}
	})

	t.Run("HEAD /bkt existing is 200", func(t *testing.T) {
		w := call("HEAD", "/"+bkt, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("HEAD /bkt missing is 404", func(t *testing.T) {
		w := callFor("HEAD", "/route-nope", "route-nope", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})

	t.Run("GET /bkt (no params) is ListBucketResult", func(t *testing.T) {
		w := call("GET", "/"+bkt, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListBucketResult" {
			t.Errorf("XML root = %q, want ListBucketResult (listObjectsV2Handler default route)", root)
		}
	})

	t.Run("GET /bkt missing is 404 NoSuchBucket", func(t *testing.T) {
		w := callFor("GET", "/route-nope", "route-nope", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "NoSuchBucket") {
			t.Errorf("want NoSuchBucket, got: %s", w.Body.String())
		}
	})

	t.Run("GET /bkt?list-type=2 is ListBucketResult", func(t *testing.T) {
		w := call("GET", "/"+bkt+"?list-type=2", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListBucketResult" {
			t.Errorf("XML root = %q, want ListBucketResult", root)
		}
	})

	t.Run("GET /bkt?location is LocationConstraint", func(t *testing.T) {
		w := call("GET", "/"+bkt+"?location", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "LocationConstraint" {
			t.Errorf("XML root = %q, want LocationConstraint (getBucketLocationHandler)", root)
		}
	})

	t.Run("GET /bkt?uploads is ListMultipartUploadsResult", func(t *testing.T) {
		w := call("GET", "/"+bkt+"?uploads", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListMultipartUploadsResult" {
			t.Errorf("XML root = %q, want ListMultipartUploadsResult (listMultipartUploadsHandler)", root)
		}
	})

	t.Run("GET /bkt?versions is ListVersionsResult", func(t *testing.T) {
		w := call("GET", "/"+bkt+"?versions", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListVersionsResult" {
			t.Errorf("XML root = %q, want ListVersionsResult (listObjectVersionsHandler)", root)
		}
	})

	t.Run("GET /bkt?acl is 501 NotImplemented (handleACL stub)", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/"+bkt+"?acl", nil)
		defaultTestFrontend().serveHTTP(w, req)
		// serveHTTP requires auth; drive handleACL directly via the
		// dispatcher's ACL branch instead — use the root handler through a
		// signed request is out of scope here, so assert the stub directly.
		w2 := httptest.NewRecorder()
		handleACL(w2, req, bkt, "")
		if w2.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501: %s", w2.Code, w2.Body.String())
		}
		if !strings.Contains(w2.Body.String(), "NotImplemented") {
			t.Errorf("want NotImplemented, got: %s", w2.Body.String())
		}
	})

	t.Run("POST /bkt is 405", func(t *testing.T) {
		w := call("POST", "/"+bkt, nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", w.Code, w.Body.String())
		}
		if want := "Method Not Allowed for bucket"; !strings.Contains(w.Body.String(), want) {
			t.Errorf("body %q missing marker %q", w.Body.String(), want)
		}
	})

	t.Run("POST /bkt?delete is DeleteResult", func(t *testing.T) {
		// Seed one object, then batch-delete it through the sub-resource.
		env.writeTestObject(t, bkt, "doomed.txt", "delete me")
		body := `<Delete><Object><Key>doomed.txt</Key></Object></Delete>`
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/"+bkt+"?delete", strings.NewReader(body))
		defaultTestFrontend().bucketLevelDispatch(w, req, bkt)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "DeleteResult" {
			t.Errorf("XML root = %q, want DeleteResult (deleteObjectsHandler)", root)
		}
		if !strings.Contains(w.Body.String(), "<Key>doomed.txt</Key>") {
			t.Errorf("DeleteResult does not report deleted key: %s", w.Body.String())
		}
	})

	t.Run("DELETE /bkt with objects is 409 BucketNotEmpty", func(t *testing.T) {
		env.writeTestObject(t, bkt, "keep.txt", "blocker")
		w := call("DELETE", "/"+bkt, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "BucketNotEmpty") {
			t.Errorf("want BucketNotEmpty, got: %s", w.Body.String())
		}
	})

	t.Run("DELETE /bkt empty is 204", func(t *testing.T) {
		// Remove the blocker through the object route first (also proves it).
		if w := callForObject("DELETE", "/"+bkt+"/keep.txt", bkt, "keep.txt"); w.Code != http.StatusNoContent {
			t.Fatalf("setup: DELETE object status = %d, want 204: %s", w.Code, w.Body.String())
		}
		w := call("DELETE", "/"+bkt, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(env.dataDir, bkt)); !os.IsNotExist(err) {
			t.Error("bucket should be gone (deleteBucketHandler did not answer)")
		}
	})
}

// callForObject drives objectLevelDispatch for a bucket/key pair.
func callForObject(method, target, bucket, object string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	defaultTestFrontend().objectLevelDispatch(w, req, bucket, object)
	return w
}

// TestMovedRouting_ObjectLevel ports the object-level routing matrix.
func TestMovedRouting_ObjectLevel(t *testing.T) {
	env := setupS3TestEnv(t)
	const bkt, key = "route-bucket", "routed.txt"
	_ = env.setupBucket(t, bkt)

	t.Run("PUT plain stores object", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/"+bkt+"/"+key, strings.NewReader("plain put body"))
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, key)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Error("missing ETag header (putObjectHandler did not answer)")
		}
		data, err := os.ReadFile(filepath.Join(env.dataDir, bkt, key))
		if err != nil || string(data) != "plain put body" {
			t.Errorf("stored object = %q, err %v; want plain put body", data, err)
		}
	})

	t.Run("HEAD object existing is 200", func(t *testing.T) {
		w := callForObject("HEAD", "/"+bkt+"/"+key, bkt, key)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if w.Header().Get("ETag") == "" {
			t.Error("missing ETag header (headObjectHandler did not answer)")
		}
	})

	t.Run("HEAD object missing is 404", func(t *testing.T) {
		w := callForObject("HEAD", "/"+bkt+"/missing.bin", bkt, "missing.bin")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})

	t.Run("GET object returns body", func(t *testing.T) {
		w := callForObject("GET", "/"+bkt+"/"+key, bkt, key)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != "plain put body" {
			t.Errorf("body = %q, want %q", w.Body.String(), "plain put body")
		}
	})

	t.Run("PUT with x-amz-copy-source is CopyObjectResult", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/"+bkt+"/copied.txt", nil)
		req.Header.Set("x-amz-copy-source", bkt+"/"+key)
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, "copied.txt")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "CopyObjectResult" {
			t.Errorf("XML root = %q, want CopyObjectResult (copyObjectHandler)", root)
		}
		data, err := os.ReadFile(filepath.Join(env.dataDir, bkt, "copied.txt"))
		if err != nil || string(data) != "plain put body" {
			t.Errorf("copied object = %q, err %v; want copied content", data, err)
		}
	})

	t.Run("POST ?uploads is InitiateMultipartUploadResult", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/"+bkt+"/big.bin?uploads", nil)
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, "big.bin")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "InitiateMultipartUploadResult" {
			t.Errorf("XML root = %q, want InitiateMultipartUploadResult (initiateMultipartUploadHandler)", root)
		}
	})

	// Shared in-flight upload for the uploadId-routed cells below.
	initUpload := func(t *testing.T, obj string) string {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/"+bkt+"/"+obj+"?uploads", nil)
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, obj)
		if w.Code != http.StatusOK {
			t.Fatalf("init multipart: status = %d: %s", w.Code, w.Body.String())
		}
		var res InitiateMultipartUploadResult
		if err := xml.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("init multipart: bad XML: %v", err)
		}
		return res.UploadID
	}

	t.Run("PUT ?partNumber&uploadId is UploadPart (ETag, no object)", func(t *testing.T) {
		uploadID := initUpload(t, "big.bin")
		w := httptest.NewRecorder()
		target := fmt.Sprintf("/%s/big.bin?partNumber=1&uploadId=%s", bkt, uploadID)
		req := httptest.NewRequest("PUT", target, strings.NewReader("part one data"))
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, "big.bin")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Error("missing ETag header (uploadPartHandler did not answer)")
		}
		// uploadPart must NOT have created a plain object at big.bin.
		if _, err := os.Stat(filepath.Join(env.dataDir, bkt, "big.bin")); !os.IsNotExist(err) {
			t.Error("UploadPart must not create the object file; putObjectHandler answered instead")
		}
	})

	t.Run("GET ?uploadId is ListPartsResult", func(t *testing.T) {
		uploadID := initUpload(t, "parts.bin")
		// one part so the listing is non-trivial
		w := httptest.NewRecorder()
		target := fmt.Sprintf("/%s/parts.bin?partNumber=1&uploadId=%s", bkt, uploadID)
		req := httptest.NewRequest("PUT", target, strings.NewReader("x"))
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, "parts.bin")
		if w.Code != http.StatusOK {
			t.Fatalf("setup upload part: status = %d: %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		target = fmt.Sprintf("/%s/parts.bin?uploadId=%s", bkt, uploadID)
		req = httptest.NewRequest("GET", target, nil)
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, "parts.bin")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListPartsResult" {
			t.Errorf("XML root = %q, want ListPartsResult (listPartsHandler)", root)
		}
		if !strings.Contains(w.Body.String(), "<PartNumber>1</PartNumber>") {
			t.Errorf("ListPartsResult missing part 1: %s", w.Body.String())
		}
	})

	t.Run("POST ?uploadId routes to CompleteMultipart (MalformedXML 400 marker)", func(t *testing.T) {
		uploadID := initUpload(t, "fin.bin")
		// Non-XML body: only completeMultipartUploadHandler produces this
		// exact 400 MalformedXML, proving the route fired.
		w := httptest.NewRecorder()
		target := fmt.Sprintf("/%s/fin.bin?uploadId=%s", bkt, uploadID)
		req := httptest.NewRequest("POST", target, strings.NewReader("this is not xml"))
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, "fin.bin")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "MalformedXML") {
			t.Errorf("want MalformedXML (completeMultipartUploadHandler), got: %s", w.Body.String())
		}
	})

	t.Run("DELETE ?uploadId is 204 (abort route)", func(t *testing.T) {
		uploadID := initUpload(t, "gone.bin")
		w := httptest.NewRecorder()
		target := fmt.Sprintf("/%s/gone.bin?uploadId=%s", bkt, uploadID)
		req := httptest.NewRequest("DELETE", target, nil)
		defaultTestFrontend().objectLevelDispatch(w, req, bkt, "gone.bin")
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		uploadMeta := filepath.Join(env.dataDir, bkt, ".metadata", ".uploads", uploadID+".json")
		if _, err := os.Stat(uploadMeta); !os.IsNotExist(err) {
			t.Error("abort should have removed the upload session (abortMultipartUploadHandler)")
		}
	})

	t.Run("DELETE object is 204", func(t *testing.T) {
		w := callForObject("DELETE", "/"+bkt+"/"+key, bkt, key)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(env.dataDir, bkt, key)); !os.IsNotExist(err) {
			t.Error("object should be gone (deleteObjectHandler)")
		}
	})

	for _, method := range []string{"PATCH", "OPTIONS"} {
		t.Run(method+" object is 405", func(t *testing.T) {
			w := callForObject(method, "/"+bkt+"/"+key, bkt, key)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", w.Code, w.Body.String())
			}
			if want := "Method Not Allowed for object"; !strings.Contains(w.Body.String(), want) {
				t.Errorf("body %q missing marker %q", w.Body.String(), want)
			}
		})
	}
}
