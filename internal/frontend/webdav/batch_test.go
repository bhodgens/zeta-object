// batch_test.go — quic-h3-2026-10 leaf 07 Task 3: the JSON batch
// endpoint on the webdav frontend, mounted through dispatch (POST
// ?batch). The PARITY test (batch_parity_test.go) drives BOTH frontends;
// this file pins the webdav-local behavior: wire shapes, partial
// success, move semantics, malformed-manifest 400 with zero execution,
// auth gating (401 without credentials), GET → 405.
package webdav

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/batchops"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// batchPostDav POSTs a manifest to /?batch through the mode-B handler.
func batchPostDav(t *testing.T, f *Frontend, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/?batch", strings.NewReader(body))
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	return w
}

func newBatchEnv(t *testing.T, bucket string) (*Frontend, string) {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}
	f, err := New(s3.TestBackend(), Config{Bucket: bucket}, WithAuthenticator(newStubAuth()),
		WithBucketPathResolver(func(b string) string { return filepath.Join(dataDir, b) }))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	return f, bucketPath
}

func TestWebdavBatch_CopyMoveDelete(t *testing.T) {
	f, bucketPath := newBatchEnv(t, "dav-batch-bkt")
	be := s3.TestBackend()
	ctx := context.Background()
	for key, body := range map[string]string{"a/x.txt": "copy me", "a/y.bin": "move me", "tmp/junk": "junk"} {
		if _, err := be.Put(ctx, "dav-batch-bkt", key, strings.NewReader(body), int64(len(body)), objectmodel.PutOptions{}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	body := `{"operations":[{"op":"copy","from":"a/x.txt","to":"b/x-copy.txt"},{"op":"move","from":"a/y.bin","to":"archive/y.bin"},{"op":"delete","from":"tmp/junk"}]}`
	w := batchPostDav(t, f, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var resp batchops.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if len(resp.Results) != 3 {
		t.Fatalf("results = %+v, want 3", resp.Results)
	}
	for i, r := range resp.Results {
		if r.Status != batchops.StatusOK || r.Index != i {
			t.Errorf("results[%d] = %+v, want ok at index %d", i, r, i)
		}
	}

	mustRead := func(key string) ([]byte, error) {
		rc, _, err := be.Get(ctx, "dav-batch-bkt", key, objectmodel.GetOptions{})
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		buf := make([]byte, 64)
		n, _ := rc.Read(buf)
		return buf[:n], nil
	}
	if got, err := mustRead("b/x-copy.txt"); err != nil || string(got) != "copy me" {
		t.Errorf("copy destination = %q err=%v", got, err)
	}
	if _, err := be.Stat(ctx, "dav-batch-bkt", "a/x.txt"); err != nil {
		t.Errorf("copy source must survive: %v", err)
	}
	if got, err := mustRead("archive/y.bin"); err != nil || string(got) != "move me" {
		t.Errorf("move destination = %q err=%v", got, err)
	}
	if _, err := be.Stat(ctx, "dav-batch-bkt", "a/y.bin"); err == nil {
		t.Errorf("move source must be GONE (move semantics, not copy+leave)")
	}
	if _, err := be.Stat(ctx, "dav-batch-bkt", "tmp/junk"); err == nil {
		t.Errorf("deleted object must be gone")
	}
	_ = bucketPath
}

// TestWebdavBatch_Malformed400NothingExecutes: a malformed manifest is a
// 400 (the s3 error body the bridge renders) and NOTHING exists or
// changed on disk.
func TestWebdavBatch_Malformed400NothingExecutes(t *testing.T) {
	f, bucketPath := newBatchEnv(t, "dav-guard-bkt")
	be := s3.TestBackend()
	if _, err := be.Put(context.Background(), "dav-guard-bkt", "victim.txt",
		strings.NewReader("stay"), int64(len("stay")), objectmodel.PutOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := batchPostDav(t, f, `{"operations":[{"op":"purge","from":"victim.txt"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if _, err := be.Stat(context.Background(), "dav-guard-bkt", "victim.txt"); err != nil {
		t.Errorf("400 must execute nothing — victim.txt gone: %v", err)
	}
	entries, err := os.ReadDir(bucketPath)
	if err != nil {
		t.Fatalf("read bucket: %v", err)
	}
	for _, e := range entries {
		if e.Name() != ".metadata" && e.Name() != "victim.txt" {
			t.Errorf("unexpected entry %q — something executed", e.Name())
		}
	}
}

// TestWebdavBatch_UnauthenticatedIs401: the mounting frontend's
// authenticator gates the batch request — no credentials, no batch
// (401 + WWW-Authenticate), even before the manifest is parsed.
func TestWebdavBatch_UnauthenticatedIs401(t *testing.T) {
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	bucket := "auth-bkt"
	if err := os.MkdirAll(filepath.Join(dataDir, bucket, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "rw", AccessKey: "ak", SecretKey: "sk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(s3.TestBackend(), Config{Bucket: bucket}, WithAuthenticator(auth.NewBasicAuthenticator(reg)),
		WithBucketPathResolver(func(b string) string { return filepath.Join(dataDir, b) }))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/?batch", strings.NewReader(`{"operations":[{"op":"delete","from":"k"}]}`))
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ?batch status = %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("401 must carry the WWW-Authenticate challenge")
	}
}

// TestWebdavBatch_GETIs405: the batch sub-resource is POST-only.
func TestWebdavBatch_GETIs405(t *testing.T) {
	f, _ := newBatchEnv(t, "dav-405-bkt")
	req := httptest.NewRequest("GET", "/?batch", nil)
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET ?batch status = %d, want 405", w.Code)
	}
}
