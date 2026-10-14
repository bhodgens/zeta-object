// bughunt_20261013_test.go — regression pins for the 2026-10-13 bughunt
// findings fixed in the s3 frontend:
//
//   - H8: CompleteMultipartUpload must NOT destroy the previous object's
//     version history or tags (the finalize step rewrites the sidecar
//     wholesale; capture-before, record/restore-after now mirror the
//     plain PutObject gate).
//   - M22: plain HEAD must consult the delete-marker gate exactly like
//     plain GET (404 + x-amz-delete-marker: true on a marker-hidden key).
//   - L39: DeleteObjects caps its manifest body read at
//     bucketBatchMaxBytes exactly like the JSON ?batch surface.
//   - L40: the "!data" and ".uploads" segments are rejected as object-key
//     segments (s3 validator, fsbackend validator, batchops packaged
//     validator) — previously a "!data" segment aliased another key's
//     shadowed data file.
package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/batchops"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// ---------- H8: multipart completion preserves versions + tags ----------

// seedCompleteUpload seeds a 1-part upload session for key and returns
// (uploadID, partETag) ready for a complete call.
func seedCompleteUpload(t *testing.T, bucketPath, key, partBody string) (string, string) {
	t.Helper()
	uploadID := mpSeedUploadWithParts(t, bucketPath, key, map[int]string{1: partBody})
	return uploadID, mpHashETag(partBody)
}

func mpCompleteCall(t *testing.T, bucketName, key, uploadID, partETag string) *httptest.ResponseRecorder {
	t.Helper()
	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: partETag})
	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost, "/"+bucketName+"/"+key+"?uploadId="+uploadID, body), bucketName, key, uploadID)
	return w
}

func TestBughuntH8_MultipartCompletePreservesVersionHistory(t *testing.T) {
	env := newVerEnv(t, "h8ver-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}

	// v1: plain PUT creates the object (a create — records nothing).
	if w := verPutBody(t, env, "doc.txt", "v1-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}

	// v2: multipart-complete overwrites — v1 must survive as one version.
	uploadID, partETag := seedCompleteUpload(t, env.bucketPath, "doc.txt", "v2-part")
	if w := mpCompleteCall(t, env.bucket, "doc.txt", uploadID, partETag); w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}

	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "doc.txt")
	if err != nil {
		t.Fatalf("List after multipart complete: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d entries, want 1 (v1 must survive the completion)", len(versions))
	}
	rc, entry, err := store.Open(env.bucket, "doc.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if string(raw) != "v1-bytes" {
		t.Errorf("recorded version bytes = %q, want v1-bytes", raw)
	}
	if entry.Size != int64(len("v1-bytes")) {
		t.Errorf("recorded version size = %d, want %d", entry.Size, len("v1-bytes"))
	}

	// Current bytes are the assembled multipart body.
	if w := verGet(t, env, "doc.txt"); w.Body.String() != "v2-part" {
		t.Errorf("current bytes = %q, want v2-part", w.Body.String())
	}
}

func TestBughuntH8_MultipartCompletePreservesTags(t *testing.T) {
	env := newVerEnv(t, "h8tag-bucket")
	_ = env // versioning state irrelevant for the tag half; sidecar mode keeps the store uniform

	// PUT the object WITH tags.
	req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/tagged.bin", strings.NewReader("tagged-data"))
	req.Header.Set("x-amz-tagging", "env=prod&team=core")
	w := httptest.NewRecorder()
	putObjectHandler(w, req, env.bucket, "tagged.bin")
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}

	// Multipart-complete over the same key.
	uploadID, partETag := seedCompleteUpload(t, env.bucketPath, "tagged.bin", "mp-part")
	if w := mpCompleteCall(t, env.bucket, "tagged.bin", uploadID, partETag); w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}

	tags, err := tagStoreFor(env.bucketPath).Get("tagged.bin")
	if err != nil {
		t.Fatalf("Get tags after complete: %v", err)
	}
	if len(tags) != 2 || tags["env"] != "prod" || tags["team"] != "core" {
		t.Fatalf("tags after multipart complete = %v, want env=prod team=core (H8: the completion must not wipe tags)", tags)
	}
}

func TestBughuntH8_PlainBucketCompletionUnchanged(t *testing.T) {
	env := newVerEnv(t, "h8plain-bucket") // never versioned: Off
	installZmetadDBForTest(t, nil)

	// Plain PUT (no versioning): plain sidecar, no versions fields.
	if w := verPutBody(t, env, "k.txt", "plain-v1"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	uploadID, partETag := seedCompleteUpload(t, env.bucketPath, "k.txt", "mp-v2")
	if w := mpCompleteCall(t, env.bucket, "k.txt", uploadID, partETag); w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}

	// No version store entries: the Off bucket records nothing.
	store := versionStoreForBucket(env.bucketPath)
	if _, err := store.List(env.bucket, "k.txt"); !isNoSuchKeyErr(err) {
		t.Errorf("Off-bucket completion must record no versions, List err = %v", err)
	}
	// No .versions dir materialized.
	if _, err := os.Stat(filepath.Join(env.bucketPath, ".metadata", ".versions")); !os.IsNotExist(err) {
		t.Errorf(".versions dir exists on Off bucket after completion: %v", err)
	}
	// Current bytes are the assembled body.
	if w := verGet(t, env, "k.txt"); w.Body.String() != "mp-v2" {
		t.Errorf("current bytes = %q, want mp-v2", w.Body.String())
	}
}

// ---------- M22: HEAD consults the delete marker ----------

func TestBughuntM22_HeadDeleteMarkedKey404WithMarkerHeader(t *testing.T) {
	env := newVerEnv(t, "m22head-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "gone.txt", "data"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}
	if w := verDelete(t, env, "gone.txt"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	// HEAD must answer the SAME marker 404 GET answers.
	req := httptest.NewRequest(http.MethodHead, "/"+env.bucket+"/gone.txt", nil)
	w := httptest.NewRecorder()
	headObjectHandler(w, req, env.bucket, "gone.txt")
	if w.Code != http.StatusNotFound {
		t.Fatalf("HEAD delete-marked key = %d, want 404 (M22: HEAD ignored the marker)", w.Code)
	}
	if got := headerGet(w, "x-amz-delete-marker"); got != "true" {
		t.Errorf("HEAD x-amz-delete-marker = %q, want true", got)
	}

	// GET parity: still 404 with the marker header.
	if w := verGet(t, env, "gone.txt"); w.Code != http.StatusNotFound {
		t.Errorf("GET delete-marked key = %d, want 404", w.Code)
	}
}

// ---------- L39: DeleteObjects caps its body read ----------

func TestBughuntL39_DeleteObjectsOversizeBodyRejected(t *testing.T) {
	env := newVerEnv(t, "l39del-bucket")

	// A body over bucketBatchMaxBytes: must be rejected 400 InvalidArgument
	// (the JSON batch surface's oversize error), not buffered whole.
	oversize := strings.Repeat("x", bucketBatchMaxBytes+1024)
	req := httptest.NewRequest(http.MethodPost, "/"+env.bucket+"?delete", strings.NewReader(oversize))
	w := httptest.NewRecorder()
	deleteObjectsHandler(w, req, env.bucket)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize DeleteObjects body = %d, want 400 (L39: unbounded ReadAll)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("oversize body must answer InvalidArgument, got: %s", w.Body.String())
	}

	// Exactly at the cap: parsing proceeds (the garbage XML answers
	// MalformedXML, proving the oversize gate did not fire).
	atCap := strings.Repeat("x", bucketBatchMaxBytes)
	req = httptest.NewRequest(http.MethodPost, "/"+env.bucket+"?delete", strings.NewReader(atCap))
	w = httptest.NewRecorder()
	deleteObjectsHandler(w, req, env.bucket)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "MalformedXML") {
		t.Fatalf("at-cap body = %d %s, want 400 MalformedXML (parse error, not oversize)", w.Code, w.Body.String())
	}
}

// ---------- L40: reserved control segments rejected everywhere ----------

func TestBughuntL40_S3KeyValidatorRejectsShadowAndUploadsSegments(t *testing.T) {
	rejected := []string{
		"b/!data/foo%2Fbar",
		"!data",
		"!data/x",
		"a/b/!data/c",
		".uploads",
		".uploads/sess.json",
		"a/.uploads/b",
	}
	for _, key := range rejected {
		if err := validateObjectKey(key); err == nil {
			t.Errorf("validateObjectKey(%q) accepted, want rejection (L40: aliases a control directory)", key)
		}
	}
	// Lookalikes stay legal (segment-exact matching).
	accepted := []string{"!database/x", "x!data", "a/.uploadsx/b", ".uploadsx"}
	for _, key := range accepted {
		if err := validateObjectKey(key); err != nil {
			t.Errorf("validateObjectKey(%q) rejected: %v (lookalikes must stay legal)", key, err)
		}
	}
}

func TestBughuntL40_PutAndCopyRejectShadowAndUploadsKeys(t *testing.T) {
	env := newVerEnv(t, "l40put-bucket")
	for _, key := range []string{"b/!data/x", "b/.uploads/x"} {
		req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/"+key, strings.NewReader("data"))
		w := httptest.NewRecorder()
		putObjectHandler(w, req, env.bucket, key)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PUT %q = %d, want 400 InvalidArgument", key, w.Code)
		}
	}
	// The aliased control dirs were NOT created by the rejected puts.
	if _, err := os.Stat(filepath.Join(env.bucketPath, "!data")); !os.IsNotExist(err) {
		t.Errorf("bucket-root !data dir created despite rejection: %v", err)
	}
}

func TestBughuntL40_FsbackendValidateKeyRejectsShadowAndUploadsSegments(t *testing.T) {
	// Reaches through to the fsbackend validator via a Put (the same
	// function the seam re-validates with).
	f := envBackendForL40(t)
	ctx := t.Context()
	for _, key := range []string{"b/!data/x", "b/.uploads/x", "!data", ".uploads"} {
		if _, err := f.Put(ctx, "l40fs", key, strings.NewReader("data"), 4, objectmodel.PutOptions{}); err == nil {
			t.Errorf("fsbackend Put(%q) accepted, want InvalidArgument (L40)", key)
		}
	}
}

func TestBughuntL40_BatchManifestRejectsShadowAndUploadsKeys(t *testing.T) {
	env := newVerEnv(t, "l40batch-bucket")

	// JSON batch manifest with a !data key: nothing executes, 400.
	manifest := `{"operations":[{"op":"delete","from":"b/!data/foo%2Fbar"},{"op":"copy","from":"a","to":".uploads/x"}]}`
	req := httptest.NewRequest(http.MethodPost, "/"+env.bucket+"?batch", strings.NewReader(manifest))
	w := httptest.NewRecorder()
	handleBucketBatch(w, req, env.bucket)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("?batch manifest with control segments = %d %s, want 400", w.Code, w.Body.String())
	}

	// The packaged batchops validator carries the same rules (parity pin).
	for _, key := range []string{"b/!data/x", ".uploads/x", "!data", ".uploads"} {
		if err := batchops.DefaultKeyValidator(key); err == nil {
			t.Errorf("batchops.DefaultKeyValidator(%q) accepted, want rejection (L40 parity)", key)
		}
		if err := ValidateObjectKey(key); err == nil {
			t.Errorf("ValidateObjectKey(%q) accepted, want rejection (parity)", key)
		}
	}
}

// ---------- helpers for the fsbackend reach-through ----------

func envBackendForL40(t *testing.T) *fsbackend.FS {
	t.Helper()
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "l40fs")
	return env.b
}
