// batch_endpoint_test.go — quic-h3-2026-10 leaf 07 Task 3: the JSON batch
// endpoint (POST /{bucket}?batch) on the s3 frontend. Wire shapes are
// pinned, the 400-before-execution rule is asserted through the REAL
// pipeline (no object is created or deleted by any malformed manifest),
// and the per-item code mapping (NoSuchKey error, PreconditionFailed
// conflict) is exercised end to end.
//
// The s3↔webdav PARITY test lives in the webdav package
// (batch_parity_test.go) following the established leaf-05/06 pattern.
package s3

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/batchops"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// Wire-status aliases keep the tests honest about the response vocabulary
// (they alias the batchops package constants; test-only).
const (
	StatusOKWire       = batchops.StatusOK
	StatusErrorWire    = batchops.StatusError
	StatusConflictWire = batchops.StatusConflict
)

// batchPost drives POST /{bucket}?batch through bucketLevelDispatch.
func batchPost(t *testing.T, bucketName, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/"+bucketName+"?batch", strings.NewReader(body))
	w := httptest.NewRecorder()
	defaultTestFrontend().bucketLevelDispatch(w, req, bucketName)
	return w
}

// batchResults parses the JSON envelope.
func batchResults(t *testing.T, body string) batchops.Response {
	t.Helper()
	var resp batchops.Response
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal batch response %q: %v", body, err)
	}
	return resp
}

func TestBatchEndpoint_CopyMoveDelete(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "batch-bkt")
	env.writeTestObject(t, "batch-bkt", "a/x.txt", "copy me")
	env.writeTestObject(t, "batch-bkt", "a/y.bin", "move me")
	env.writeTestObject(t, "batch-bkt", "tmp/junk", "junk")

	body := `{"operations":[{"op":"copy","from":"a/x.txt","to":"b/x-copy.txt"},{"op":"move","from":"a/y.bin","to":"archive/y.bin"},{"op":"delete","from":"tmp/junk"}]}`
	w := batchPost(t, "batch-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	resp := batchResults(t, w.Body.String())
	if len(resp.Results) != 3 {
		t.Fatalf("results = %+v, want 3 entries", resp.Results)
	}
	for i, want := range []string{StatusOKWire, StatusOKWire, StatusOKWire} {
		if resp.Results[i].Status != want {
			t.Errorf("results[%d].Status = %q, want %q (%+v)", i, resp.Results[i].Status, want, resp.Results[i])
		}
		if resp.Results[i].Index != i {
			t.Errorf("results[%d].Index = %d, want %d", i, resp.Results[i].Index, i)
		}
	}

	// Copy: destination exists with the source bytes, source survives.
	got, err := mustGetObject(t, env, "batch-bkt", "b/x-copy.txt")
	if err != nil || string(got) != "copy me" {
		t.Errorf("copy destination = %q err=%v, want %q", got, err, "copy me")
	}
	if _, err := env.b.Stat(context.Background(), "batch-bkt", "a/x.txt"); err != nil {
		t.Errorf("copy source must survive: %v", err)
	}
	// Move: MOVE semantics — destination has the bytes, NO source copy left.
	got, err = mustGetObject(t, env, "batch-bkt", "archive/y.bin")
	if err != nil || string(got) != "move me" {
		t.Errorf("move destination = %q err=%v, want %q", got, err, "move me")
	}
	if _, err := env.b.Stat(context.Background(), "batch-bkt", "a/y.bin"); err == nil {
		t.Errorf("move source must be GONE (move semantics, not copy+leave)")
	}
	// Delete: gone.
	if _, err := env.b.Stat(context.Background(), "batch-bkt", "tmp/junk"); err == nil {
		t.Errorf("deleted object must be gone")
	}
}

// TestBatchEndpoint_PartialSuccess_NoAtomicity: a mid-manifest
// NoSuchKey does not stop later items and does not roll back the earlier
// copy — per-item results say exactly which items succeeded.
func TestBatchEndpoint_PartialSuccess_NoAtomicity(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "partial-bkt")
	env.writeTestObject(t, "partial-bkt", "src.txt", "keep")

	body := `{"operations":[{"op":"copy","from":"src.txt","to":"dst.txt"},{"op":"delete","from":"missing.txt"},{"op":"delete","from":"src.txt"}]}`
	w := batchPost(t, "partial-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (item failures are per-item, not request failures): %s", w.Code, w.Body.String())
	}
	resp := batchResults(t, w.Body.String())
	if resp.Results[0].Status != StatusOKWire {
		t.Errorf("results[0] = %+v, want ok", resp.Results[0])
	}
	if resp.Results[1].Status != StatusErrorWire || resp.Results[1].Code != objectmodel.CodeNoSuchKey {
		t.Errorf("results[1] = %+v, want error/NoSuchKey", resp.Results[1])
	}
	if resp.Results[2].Status != StatusOKWire {
		t.Errorf("results[2] = %+v, want ok (item 3 ran after item 2 failed)", resp.Results[2])
	}
	// dst.txt survived (no rollback) and src.txt is gone.
	if _, err := env.b.Stat(context.Background(), "partial-bkt", "dst.txt"); err != nil {
		t.Errorf("earlier success must NOT be rolled back: %v", err)
	}
	if _, err := env.b.Stat(context.Background(), "partial-bkt", "src.txt"); err == nil {
		t.Errorf("item 3 (delete src.txt) must have executed")
	}
}

// TestBatchEndpoint_MalformedManifest400NothingExecutes: the
// 400-before-execution rule through the real dispatch — every
// malformed class answers 400 with an error body naming the problem,
// and NOTHING exists on disk afterward that didn't before.
func TestBatchEndpoint_MalformedManifest400NothingExecutes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad JSON", `{"operations": [`},
		{"unknown op", `{"operations":[{"op":"purge","from":"a"}]}`},
		{"metadata segment", `{"operations":[{"op":"delete","from":".metadata/x"}]}`},
		{"zfs segment", `{"operations":[{"op":"delete","from":"a/.zfs/s"}]}`},
		{"over limit", `{"operations":` + overLimitOpsJSON(1001) + `}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupS3TestEnv(t)
			env.setupBucket(t, "guard-bkt")
			env.writeTestObject(t, "guard-bkt", "victim.txt", "must survive")

			w := batchPost(t, "guard-bkt", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			// Nothing executed: the seeded object is untouched and no
			// new object appeared in the bucket directory.
			if _, err := env.b.Stat(context.Background(), "guard-bkt", "victim.txt"); err != nil {
				t.Errorf("victim.txt must survive a 400 manifest: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(env.dataDir, "guard-bkt"))
			if err != nil {
				t.Fatalf("read bucket dir: %v", err)
			}
			for _, e := range entries {
				if e.Name() != ".metadata" && e.Name() != "victim.txt" {
					t.Errorf("unexpected entry %q — something executed", e.Name())
				}
			}
		})
	}
}

// TestBatchEndpoint_IfMatchConflict: an item carrying ifMatch enforces
// the precondition — a stale ETag yields a conflict result, and the
// object is NOT deleted.
func TestBatchEndpoint_IfMatchConflict(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "cond-bkt")
	env.writeTestObject(t, "cond-bkt", "k.txt", "payload")

	body := `{"operations":[{"op":"delete","from":"k.txt","ifMatch":"\"stale-etag\""}]}`
	w := batchPost(t, "cond-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	resp := batchResults(t, w.Body.String())
	if resp.Results[0].Status != StatusConflictWire || resp.Results[0].Code != objectmodel.CodePreconditionFailed {
		t.Fatalf("results[0] = %+v, want conflict/PreconditionFailed", resp.Results[0])
	}
	if _, err := env.b.Stat(context.Background(), "cond-bkt", "k.txt"); err != nil {
		t.Errorf("a failed precondition must NOT delete the object: %v", err)
	}
}

// TestBatchEndpoint_IfMatchSatisfied: a matching ETag lets the op
// execute.
func TestBatchEndpoint_IfMatchSatisfied(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "cond-ok-bkt")
	env.writeTestObject(t, "cond-ok-bkt", "k.txt", "payload")

	rc, obj, err := env.b.Get(context.Background(), "cond-ok-bkt", "k.txt", objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("setup get: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()

	body := `{"operations":[{"op":"delete","from":"k.txt","ifMatch":"` + obj.ETag + `"}]}`
	w := batchPost(t, "cond-ok-bkt", body)
	resp := batchResults(t, w.Body.String())
	if resp.Results[0].Status != StatusOKWire {
		t.Fatalf("results[0] = %+v, want ok (matching ETag executes)", resp.Results[0])
	}
	if _, err := env.b.Stat(context.Background(), "cond-ok-bkt", "k.txt"); err == nil {
		t.Errorf("object should be deleted")
	}
}

// TestBatchEndpoint_GETIs405: a GET ?batch is method-not-allowed — the
// batch sub-resource is POST-only.
func TestBatchEndpoint_GETIs405(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "m405-bkt")

	req := httptest.NewRequest("GET", "/m405-bkt?batch", nil)
	w := httptest.NewRecorder()
	defaultTestFrontend().bucketLevelDispatch(w, req, "m405-bkt")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET ?batch status = %d, want 405: %s", w.Code, w.Body.String())
	}
}

// TestBatchEndpoint_NoSuchBucket: 404, nothing executed.
func TestBatchEndpoint_NoSuchBucket(t *testing.T) {
	setupS3TestEnv(t)
	w := batchPost(t, "ghost-bkt", `{"operations":[{"op":"delete","from":"k"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("want NoSuchBucket, got: %s", w.Body.String())
	}
}

// overLimitOpsJSON builds `[{...},...]` with n delete operations.
func overLimitOpsJSON(n int) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"op":"delete","from":"k"}`)
	}
	sb.WriteByte(']')
	return sb.String()
}
