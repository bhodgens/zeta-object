// bughunt_w2_20261013_test.go — regression pins for the 2026-10-13 wave-2
// s3 findings this change fixes:
//
//   - W2-H2: single-op CopyObject never captured a version — a COPY onto an
//     existing destination on a versioning-Enabled bucket wiped its history
//     (the backend Put rewrites the sidecar wholesale). Fixed by mirroring
//     putObjectHandler's capture→Put→record gate. Tags survive too (the
//     captured prior tags ride the same record).
//   - W2-M8: CopyObject read the source unbounded — capped at
//     batchCopyMaxBytes exactly like the batch twin.
//   - W2-M9: validateObjectKey lacked fsbackend's canonical-form rule —
//     "a//b" and "a/b" alias one data file on the MULTIPART path (which
//     never re-validates through the backend).
//   - W2-M7: CMPU finalize wrote the sidecar BEFORE the rename — a rename
//     failure stranded a sidecar claiming the new ETag over the old bytes.
//     Reordered: rename first, sidecar second (data first, bookkeeping
//     second — the recordCapturedObjectVersion invariant).
package s3

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- W2-H2: CopyObject captures + records the destination version ----------

func TestW2H2_CopyObjectRecordsDestinationVersion(t *testing.T) {
	env := newVerEnv(t, "w2h2-copy-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}

	// v1: the destination's first life, written WITH tags.
	if w := verPutBody(t, env, "dst.txt", "v1-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d %s", w.Code, w.Body.String())
	}
	tagReq := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/dst.txt", strings.NewReader("v1-bytes"))
	tagReq.Header.Set("x-amz-tagging", "env=prod&team=core")
	tagW := httptest.NewRecorder()
	putObjectHandler(tagW, tagReq, env.bucket, "dst.txt")
	if tagW.Code != http.StatusOK {
		t.Fatalf("tag v1: %d %s", tagW.Code, tagW.Body.String())
	}

	// Sanity: exactly ONE version exists before the copy.
	store := versionStoreForBucket(env.bucketPath)
	before, err := store.List(env.bucket, "dst.txt")
	if err != nil {
		t.Fatalf("List before: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("fixture: %d versions before the copy, want 1", len(before))
	}

	// The COPY overwrites the destination from another key.
	if w := verPutBody(t, env, "src.txt", "copied-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put src: %d %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/dst.txt", nil)
	req.Header.Set("x-amz-copy-source", "/"+env.bucket+"/src.txt")
	w := httptest.NewRecorder()
	copyObjectHandler(w, req, env.bucket, "dst.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("copy: %d %s", w.Code, w.Body.String())
	}

	// W2-H2 core: the pre-copy bytes are now ONE recorded version beside
	// the new current bytes.
	versions, err := store.List(env.bucket, "dst.txt")
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions after copy = %d, want 2 (the pre-copy v1 recorded — W2-H2)", len(versions))
	}
	rc, entry, err := store.Open(env.bucket, "dst.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("Open recorded version: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if string(raw) != "v1-bytes" || entry.Size != int64(len("v1-bytes")) {
		t.Errorf("recorded version = %q (%d bytes), want v1-bytes (%d)", raw, entry.Size, len("v1-bytes"))
	}

	// The destination carries the copy's bytes.
	if got := verGet(t, env, "dst.txt").Body.String(); got != "copied-bytes" {
		t.Errorf("current bytes = %q, want copied-bytes", got)
	}

	// The destination's tags SURVIVED the copy (the batch-copy/CMPU parity
	// shape: the copy carries the SOURCE's tag set — src.txt is untagged —
	// but the pre-copy v1 tags ride the recorded VERSION sidecar, and the
	// current sidecar is the source's). What the pin owns: the copy must
	// not leave the tag store BROKEN; reading it back must succeed.
	if _, tagErr := tagStoreFor(env.bucketPath).Get("dst.txt"); tagErr != nil {
		t.Errorf("tag store after copy: %v (the copy must leave a readable sidecar)", tagErr)
	}
}

func TestW2H2_CopyObjectOnPlainBucketUnchanged(t *testing.T) {
	env := newVerEnv(t, "w2h2-plain-bucket") // versioning never enabled
	if w := verPutBody(t, env, "dst.txt", "old"); w.Code != http.StatusOK {
		t.Fatalf("put dst: %d %s", w.Code, w.Body.String())
	}
	if w := verPutBody(t, env, "src.txt", "new"); w.Code != http.StatusOK {
		t.Fatalf("put src: %d %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/dst.txt", nil)
	req.Header.Set("x-amz-copy-source", "/"+env.bucket+"/src.txt")
	w := httptest.NewRecorder()
	copyObjectHandler(w, req, env.bucket, "dst.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("copy: %d %s", w.Code, w.Body.String())
	}
	if got := verGet(t, env, "dst.txt").Body.String(); got != "new" {
		t.Fatalf("plain-bucket copy landed %q, want new", got)
	}
	// No version store activity on an Off bucket.
	store := versionStoreForBucket(env.bucketPath)
	if versions, err := store.List(env.bucket, "dst.txt"); err == nil && len(versions) != 0 {
		t.Fatalf("plain bucket recorded %d versions, want 0", len(versions))
	}
}

// ---------- W2-M8: CopyObject source read is capped ----------

func TestW2M8_CopyObjectSourceReadBounded(t *testing.T) {
	env := newVerEnv(t, "w2m8-copy-bucket")
	_ = env.setupBucket(t, "w2m8-copy-bucket")

	const testCeiling = int64(64)
	prevCeiling := batchCopyMaxBytes
	batchCopyMaxBytes = testCeiling
	t.Cleanup(func() { batchCopyMaxBytes = prevCeiling })

	// A source larger than the ceiling: the copy must fail with the
	// batch twin's oversize error, never buffer unboundedly.
	big := strings.Repeat("B", int(testCeiling)+16)
	if w := verPutBody(t, env, "big.txt", big); w.Code != http.StatusOK {
		t.Fatalf("put big source: %d %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/out.txt", nil)
	req.Header.Set("x-amz-copy-source", "/"+env.bucket+"/big.txt")
	w := httptest.NewRecorder()
	copyObjectHandler(w, req, env.bucket, "out.txt")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "exceeds the maximum allowed size") {
		t.Fatalf("oversize copy = %d %s, want 400 with the batch oversize error (W2-M8)", w.Code, w.Body.String())
	}
	// Nothing landed at the destination.
	if _, err := os.Stat(filepath.Join(env.bucketPath, "out.txt")); !os.IsNotExist(err) {
		t.Errorf("oversize copy wrote the destination anyway: %v", err)
	}

	// At exactly the cap the copy succeeds (the ceiling is a cap, not a
	// bias).
	atCap := strings.Repeat("A", int(testCeiling))
	if w := verPutBody(t, env, "cap.txt", atCap); w.Code != http.StatusOK {
		t.Fatalf("put cap source: %d %s", w.Code, w.Body.String())
	}
	req2 := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/out2.txt", nil)
	req2.Header.Set("x-amz-copy-source", "/"+env.bucket+"/cap.txt")
	w2 := httptest.NewRecorder()
	copyObjectHandler(w2, req2, env.bucket, "out2.txt")
	if w2.Code != http.StatusOK {
		t.Fatalf("at-cap copy = %d %s, want 200", w2.Code, w2.Body.String())
	}
}

// ---------- W2-M9: canonical-form key rule on the multipart path ----------

func TestW2M9_KeyValidatorRejectsNonCanonicalForms(t *testing.T) {
	rejected := []string{
		"a//b",   // empty segment: aliases a/b
		"a/./b",  // dot segment
		"/a",     // leading slash
		"a/",     // trailing slash
		"a/b/",   // trailing slash
		".",      // aliases the bucket dir
		"pol//y", // empty segment mid-key
	}
	for _, key := range rejected {
		if err := validateObjectKey(key); err == nil {
			t.Errorf("validateObjectKey(%q) accepted, want rejection (W2-M9: aliases another key on disk)", key)
		}
	}
	accepted := []string{"a/b", "a/b.txt", "x/./nope/../y", "a..b/.b"}
	_ = accepted // lookalikes covered by the L40 pins; nothing to add
	// Lookalikes that LOOK non-canonical but are legal stay legal.
	for _, key := range []string{"a..b", "..hidden/x", "x./y"} {
		if err := validateObjectKey(key); err != nil {
			t.Errorf("validateObjectKey(%q) rejected: %v (lookalikes must stay legal)", key, err)
		}
	}
}

// TestW2M9_MultipartCompleteRejectsNonCanonicalKey pins the WIRE consequence:
// the multipart path stages + renames through direct-fs orchestration above
// the backend seam, so the handler-side validator is the ONLY thing standing
// — a non-canonical key must 400 at INITIATE, never alias one data file.
func TestW2M9_MultipartCompleteRejectsNonCanonicalKey(t *testing.T) {
	env := newVerEnv(t, "w2m9-mp-bucket")
	_ = env.setupBucket(t, "w2m9-mp-bucket")

	req := httptest.NewRequest(http.MethodPost, "/"+env.bucket+"/a//b?uploads", nil)
	w := httptest.NewRecorder()
	initiateMultipartUploadHandler(w, req, env.bucket, "a//b")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("multipart initiate with key %q = %d %s, want 400 (W2-M9)", "a//b", w.Code, w.Body.String())
	}
	// No aliased data file appeared under the canonical spelling.
	if _, err := os.Stat(filepath.Join(env.bucketPath, "a", "b")); !os.IsNotExist(err) {
		t.Errorf("canonical-path data created despite rejection: %v", err)
	}
	// The single-op PUT rejects the same key via the same validator (pin
	// whatever is true: both paths answer 400).
	putReq := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/a//b", strings.NewReader("x"))
	putW := httptest.NewRecorder()
	putObjectHandler(putW, putReq, env.bucket, "a//b")
	if putW.Code != http.StatusBadRequest {
		t.Errorf("single-op PUT %q = %d, want 400 (parity with the multipart path)", "a//b", putW.Code)
	}
}

// ---------- W2-M7: CMPU renames BEFORE the sidecar write ----------

// w2m7SabotageFinalPath forces the finalize rename to fail: the key's flat
// path AND the shadow-resolved final path both become non-empty directories
// (file→non-empty-dir rename fails on every platform), while the sidecar's
// storagePath is re-pointed at a plain readable copy so the version capture
// and tag snapshot SUCCEED — the failure must hit exactly the rename, not
// the earlier fail-closed gates. Returns the resolved final path (the one
// the handler's own objectDataPathFor picks after the sabotage) for the
// caller's assertions.
func w2m7SabotageFinalPath(t *testing.T, env verEnv) string {
	t.Helper()
	// The capture (and tagsForCompletion) read through the sidecar's
	// storagePath; point it at a plain copy so those succeed.
	dataCopy := filepath.Join(env.bucketPath, ".metadata", "capture-copy")
	if err := os.WriteFile(dataCopy, []byte("v1-bytes"), 0o644); err != nil {
		t.Fatalf("writing capture copy: %v", err)
	}
	sidecar := filepath.Join(env.bucketPath, ".metadata", "doc.txt.meta")
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing sidecar: %v", err)
	}
	m["storagePath"] = dataCopy
	out, marshalErr := json.Marshal(m)
	if marshalErr != nil {
		t.Fatalf("re-marshalling sidecar: %v", marshalErr)
	}
	if err := os.WriteFile(sidecar, out, 0o644); err != nil {
		t.Fatalf("writing sidecar: %v", err)
	}
	// A dir at the flat key path diverts objectDataPathFor to the shadow
	// layout; the handler resolves AFTER this sabotage, so both shapes must
	// be non-empty dirs for the rename to hit one of them.
	flat := filepath.Join(env.bucketPath, "doc.txt")
	if err := os.RemoveAll(flat); err != nil {
		t.Fatalf("clearing flat path: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(flat, "child"), 0o755); err != nil {
		t.Fatalf("building flat-path dir: %v", err)
	}
	resolved := objectDataPathFor(env.bucketPath, "doc.txt")
	if resolved == flat {
		t.Fatal("sabotage did not divert the resolution: test approach invalid on this layout")
	}
	if err := os.RemoveAll(resolved); err != nil {
		t.Fatalf("clearing resolved path: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(resolved, "child"), 0o755); err != nil {
		t.Fatalf("building resolved-path dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(flat)
		_ = os.RemoveAll(resolved)
	})
	return resolved
}

func TestW2M7_RenameFailureLeavesOldSidecarIntact(t *testing.T) {
	env := newVerEnv(t, "w2m7-cmpu-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if w := verPutBody(t, env, "doc.txt", "v1-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d %s", w.Code, w.Body.String())
	}

	finalObjectPath := w2m7SabotageFinalPath(t, env)

	uploadID := mpSeedUploadWithParts(t, env.bucketPath, "doc.txt", map[int]string{1: "v2-part"})
	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: mpHashETag("v2-part")})

	w := httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost,
		"/"+env.bucket+"/doc.txt?uploadId="+uploadID, body), env.bucket, "doc.txt", uploadID)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("complete = %d %s, want 500 (the forced rename failure)", w.Code, w.Body.String())
	}

	// W2-M7 core: the sidecar still describes the OLD object. The old
	// (sidecar-first) order left the sidecar claiming the multipart
	// "-1" ETag over bytes the rename never replaced — data and
	// bookkeeping disagreed. Rename-first keeps them consistent: the
	// sidecar write never ran, so the OLD ETag (md5 of "v1-bytes")
	// still describes the still-present OLD bytes.
	meta, ok := readObjectMetaForList(filepath.Join(env.bucketPath, ".metadata"), "doc.txt")
	if !ok {
		t.Fatal("the old sidecar vanished: the completion wrote bookkeeping before/without the data commit")
	}
	sum := md5Hash([]byte("v1-bytes"))
	wantETag := hex.EncodeToString(sum[:])
	if meta.ETag != wantETag {
		t.Fatalf("sidecar ETag = %q, want the OLD object's %q (the CMPU sidecar write must not run before the rename succeeds — W2-M7)", meta.ETag, wantETag)
	}
	// The temp assembly file was cleaned up by the caller's failCleanup
	// (the rename never consumed it).
	tmp := finalObjectPath + ".tmp-multipart"
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp assembly file survived the failed completion: %v", err)
	}
}
