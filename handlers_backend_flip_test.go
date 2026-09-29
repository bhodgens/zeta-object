package main

// handlers_backend_flip_test.go — leaf-02 Task 0/Task 4 handler-level pins:
//
//  1. The handler-level RESIDUAL WINDOW pin (Task 0's optional but
//     recommended pin, kept green through the flip): a PUT whose metadata
//     write fails leaves the data file in place.
//  2. Task 4's flip proof: with a recording Backend installed via the
//     backendFor indirection, PUT/GET/HEAD/DELETE/LIST route through the
//     seam (the wrapper observes the calls), and the wire responses carry
//     the pre-flip fixture values (status, ETag, Content-Type, body).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// recordingBackend records the operations observed through the seam.
type recordingBackend struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingBackend) record(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, op)
}

func (r *recordingBackend) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// TestPutObjectHandler_ResidualWindowPinned pins the RESIDUAL WINDOW at the
// handler level: metadata-write failure ⇒ 500 AND the new data file still
// exists (never removed — the historical data-loss bug fix).
func TestPutObjectHandler_ResidualWindowPinned(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "resid-bkt")

	put := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/resid-bkt/k.txt", strings.NewReader("body-v2"))
		w := httptest.NewRecorder()
		putObjectHandler(w, req, "resid-bkt", "k.txt")
		return w
	}

	// First PUT succeeds.
	if w := put(); w.Code != http.StatusOK {
		t.Fatalf("first PUT: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Break the metadata dir: sidecar writes now fail after the data write.
	metaDir := filepath.Join(env.dataDir, "resid-bkt", ".metadata")
	if err := os.RemoveAll(metaDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaDir, []byte("not a dir"), 0644); err != nil {
		t.Fatal(err)
	}

	w := put()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("residual-window PUT: expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// The data file must STILL exist (left in place; not removed).
	data, err := os.ReadFile(filepath.Join(env.dataDir, "resid-bkt", "k.txt"))
	if err != nil {
		t.Fatalf("residual window violated: data file missing after metadata failure: %v", err)
	}
	if string(data) != "body-v2" {
		t.Errorf("data file = %q, want the new body left in place", data)
	}
}

// TestHandlersRouteThroughBackend installs a recording Backend via the
// backendFor indirection, drives PUT/GET/HEAD/DELETE/LIST through the
// handlers, and asserts (a) the wrapper observed the calls — proving the
// handlers no longer short-circuit to direct fs — and (b) the wire
// responses carry the pre-flip fixture values.
func TestHandlersRouteThroughBackend(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "flip-bkt")

	rec := &recordingBackend{}
	origFor := backendFor
	backendFor = func(bucket string) (backend.Backend, error) {
		f, err := fsbackend.New(env.dataDir)
		if err != nil {
			return nil, err
		}
		return &routeRecorder{rec: rec, real: f}, nil
	}
	t.Cleanup(func() { backendFor = origFor })

	const body = "flip-payload-123"
	const ct = "text/x-flip"

	// PUT through the handler.
	putReq := httptest.NewRequest("PUT", "/flip-bkt/obj.bin", strings.NewReader(body))
	putReq.Header.Set("Content-Type", ct)
	putReq.Header.Set("X-Amz-Meta-Color", "blue")
	putW := httptest.NewRecorder()
	putObjectHandler(putW, putReq, "flip-bkt", "obj.bin")
	if putW.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", putW.Code, putW.Body.String())
	}
	wantETag := fmt.Sprintf("%x", md5Hash([]byte(body)))
	if got := putW.Header().Get("ETag"); got != `"`+wantETag+`"` {
		t.Errorf("PUT ETag = %q, want quoted %q", got, wantETag)
	}

	// GET through the handler: fixture response surface.
	getReq := httptest.NewRequest("GET", "/flip-bkt/obj.bin", nil)
	getW := httptest.NewRecorder()
	getObjectHandler(getW, getReq, "flip-bkt", "obj.bin")
	if getW.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getW.Code, getW.Body.String())
	}
	if got := getW.Body.String(); got != body {
		t.Errorf("GET body = %q, want %q", got, body)
	}
	if got := getW.Header().Get("Content-Type"); got != ct {
		t.Errorf("GET Content-Type = %q, want %q", got, ct)
	}
	if got := getW.Header().Get("ETag"); got != `"`+wantETag+`"` {
		t.Errorf("GET ETag = %q, want %q", got, wantETag)
	}
	if got := getW.Header().Get("X-Amz-Meta-Color"); got != "blue" {
		t.Errorf("GET custom meta = %q, want blue", got)
	}
	if got := getW.Header().Get("Content-Length"); got != fmt.Sprintf("%d", len(body)) {
		t.Errorf("GET Content-Length = %q, want %d", got, len(body))
	}

	// HEAD through the handler.
	headReq := httptest.NewRequest("HEAD", "/flip-bkt/obj.bin", nil)
	headW := httptest.NewRecorder()
	headObjectHandler(headW, headReq, "flip-bkt", "obj.bin")
	if headW.Code != http.StatusOK {
		t.Fatalf("HEAD: expected 200, got %d", headW.Code)
	}
	if headW.Body.Len() != 0 {
		t.Errorf("HEAD must not carry a body, got %d bytes", headW.Body.Len())
	}

	// LIST through the handler.
	listReq := httptest.NewRequest("GET", "/flip-bkt?list-type=2", nil)
	listW := httptest.NewRecorder()
	listObjectsV2Handler(listW, listReq, "flip-bkt")
	if listW.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d", listW.Code)
	}
	listBody := listW.Body.String()
	if !strings.Contains(listBody, "<Key>obj.bin</Key>") {
		t.Errorf("LIST response missing obj.bin: %s", listBody)
	}
	quotedETagXML := "&#34;" + wantETag + "&#34;" // xml-escaped quotes
	if !strings.Contains(listBody, quotedETagXML) {
		t.Errorf("LIST response missing quoted ETag %s: %s", quotedETagXML, listBody)
	}

	// DELETE through the handler.
	delReq := httptest.NewRequest("DELETE", "/flip-bkt/obj.bin", nil)
	delW := httptest.NewRecorder()
	deleteObjectHandler(delW, delReq, "flip-bkt", "obj.bin")
	if delW.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d: %s", delW.Code, delW.Body.String())
	}

	// The recording wrapper MUST have observed the data-plane ops (proving
	// the handlers route through the seam instead of touching the fs).
	observed := rec.snapshot()
	for _, op := range []string{"Put", "Get", "Stat", "List", "Delete"} {
		if !slices.Contains(observed, op) {
			t.Errorf("handlers did not route %s through the Backend seam (observed: %v)", op, observed)
		}
	}

	// The object is really gone from disk (the delete took effect).
	if _, err := os.Stat(filepath.Join(env.dataDir, "flip-bkt", "obj.bin")); !os.IsNotExist(err) {
		t.Errorf("object still on disk after DELETE: %v", err)
	}
}

// routeRecorder records op names and delegates to the real fs backend.
type routeRecorder struct {
	rec  *recordingBackend
	real backend.Backend
}

func (r *routeRecorder) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	r.rec.record("Get")
	return r.real.Get(ctx, bucket, key, opts)
}

func (r *routeRecorder) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	r.rec.record("Put")
	return r.real.Put(ctx, bucket, key, data, size, opts)
}

func (r *routeRecorder) Delete(ctx context.Context, bucket, key string) error {
	r.rec.record("Delete")
	return r.real.Delete(ctx, bucket, key)
}

func (r *routeRecorder) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	r.rec.record("Stat")
	return r.real.Stat(ctx, bucket, key)
}

func (r *routeRecorder) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	r.rec.record("List")
	return r.real.List(ctx, bucket, p)
}

func (r *routeRecorder) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	r.rec.record("Buckets")
	return r.real.Buckets(ctx)
}

func (r *routeRecorder) Capabilities() objectmodel.CapabilitySet {
	return r.real.Capabilities()
}
