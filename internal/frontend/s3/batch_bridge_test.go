// batch_bridge_test.go — package-s3 coverage for the batch BRIDGE the
// webdav frontend drives (batch_bridge.go). The webdav package's own
// tests exercise this code cross-package, which does not count toward
// THIS package's coverage floor; these tests drive the bridge directly
// so the s3 package's statements are covered where they are defined.
package s3

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bridgeEnv reuses setupS3TestEnv (the canonical config-view + backend
// seams) so the bridge sees the same wiring production uses.
func bridgeEnv(t *testing.T, bucket string) *testS3Env {
	t.Helper()
	env := setupS3TestEnv(t)
	env.setupBucket(t, bucket)
	return env
}

func TestBatchBridge_HandleBatchForBucket_CopiesAndDeletes(t *testing.T) {
	const bucket = "bridge-bkt"
	env := bridgeEnv(t, bucket)
	env.writeTestObject(t, bucket, "a/src.txt", "copy me")

	manifest := `{"operations":[{"op":"copy","from":"a/src.txt","to":"a/dst.txt"},{"op":"delete","from":"a/src.txt"}]}`
	req := httptest.NewRequest("POST", "/"+bucket+"?batch", strings.NewReader(manifest))
	w := httptest.NewRecorder()
	HandleBatchForBucket(w, req, bucket, env.dataDir, ValidateObjectKey)

	if w.Code != 200 {
		t.Fatalf("status = %d; body=%.300s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, bucket, "a/dst.txt")); err != nil {
		t.Fatalf("copy destination missing after batch: %v", err)
	}
	src := filepath.Join(env.dataDir, bucket, "a/src.txt")
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("delete source still present (err=%v)", err)
	}
}

func TestBatchBridge_MissingBucketIs404(t *testing.T) {
	bridgeEnv(t, "bridge-bkt")
	req := httptest.NewRequest("POST", "/nope-bkt?batch", strings.NewReader(`{"delete":[]}`))
	w := httptest.NewRecorder()
	HandleBatchForBucket(w, req, "nope-bkt", "", ValidateObjectKey)
	if w.Code != 404 {
		t.Fatalf("missing bucket status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Fatalf("body missing NoSuchBucket: %.200s", w.Body.String())
	}
}

func TestBatchBridge_MalformedManifestIs400NothingExecutes(t *testing.T) {
	const bucket = "bridge-bkt"
	env := bridgeEnv(t, bucket)
	seed := filepath.Join(env.dataDir, bucket, "keep.txt")
	if err := os.WriteFile(seed, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/"+bucket+"?batch", strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	HandleBatchForBucket(w, req, bucket, env.dataDir, ValidateObjectKey)
	if w.Code != 400 {
		t.Fatalf("malformed manifest status = %d, want 400", w.Code)
	}
	if _, err := os.Stat(seed); err != nil {
		t.Fatalf("malformed manifest must execute nothing; seed missing: %v", err)
	}
}

func TestBatchBridge_TraversalItemIsRejectedBeforeExecution(t *testing.T) {
	const bucket = "bridge-bkt"
	env := bridgeEnv(t, bucket)
	seed := filepath.Join(env.dataDir, bucket, "survivor.txt")
	if err := os.WriteFile(seed, []byte("survivor"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The key validator runs during manifest processing: a traversal item
	// is rejected BEFORE anything executes (400; nothing on disk changes).
	manifest := `{"operations":[{"op":"delete","from":"../../outside"},{"op":"delete","from":"survivor.txt"}]}`
	req := httptest.NewRequest("POST", "/"+bucket+"?batch", strings.NewReader(manifest))
	w := httptest.NewRecorder()
	HandleBatchForBucket(w, req, bucket, env.dataDir, ValidateObjectKey)
	if w.Code != 400 {
		t.Fatalf("traversal manifest status = %d, want 400; body=%.300s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(seed); err != nil {
		t.Fatalf("nothing must execute on a rejected manifest; survivor missing: %v", err)
	}
}

func TestBatchBridge_EmptyBucketPathFallsBackToOwnResolver(t *testing.T) {
	const bucket = "bridge-bkt"
	env := bridgeEnv(t, bucket)
	env.writeTestObject(t, bucket, "del.txt", "delete me")
	// Empty bucketPath = the documented unwired-seam fallback: getBucketPath.
	manifest := `{"operations":[{"op":"delete","from":"del.txt"}]}`
	req := httptest.NewRequest("POST", "/"+bucket+"?batch", strings.NewReader(manifest))
	w := httptest.NewRecorder()
	HandleBatchForBucket(w, req, bucket, "", ValidateObjectKey)
	if w.Code != 200 {
		t.Fatalf("status = %d; body=%.300s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, bucket, "del.txt")); !os.IsNotExist(err) {
		t.Fatalf("fallback-path delete did not land (err=%v)", err)
	}
}
