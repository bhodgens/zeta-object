// batch_hardening_test.go — the batchops ?batch hardening tests
// (bughunt 2026-10-05 H4 / M1 / M3), RED-first. Each test pins ONE
// finding on the REAL s3 batch pipeline (bucketLevelDispatch →
// handleBucketBatch → batchops.Runner → s3BatchExecutor), never around it:
//
//   - H4: the per-item ifMatch precheck must not leave an opened source
//     file behind (the discarded-reader leak). Proved with a
//     close-tracking Backend double (portable) plus an OS fd-count
//     sanity check that the leak cannot scale with manifest size.
//   - M1: a batch copy must carry the source's user metadata and tags
//     (the single-op CopyObject parity) and must stamp the requesting
//     principal's user.zeta.owner / user.zeta.writer.* breadcrumbs.
//   - M3: the request context the executor stores must REACH the backend
//     calls (a cancelled client must abort the item), and a
//     traversal-shaped bucket name must be rejected the way the webdav
//     mount rejects it.
//
// The 400-before-any-execution rule and the per-item versioning capture
// stay pinned by batch_endpoint_test.go and the webdav parity suite.
package s3

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// ---- shared helpers ----

// batchPostAs POSTs a manifest to /{bucket}?batch with an AUTHENTICATED
// principal published in the request context — the shape serveHTTP hands
// every real handler (bughunt M1: the principal must reach the write so
// fsbackend stamps the breadcrumbs). An empty principal dispatches with
// no identity, exactly like a direct handler invocation.
func batchPostAs(t *testing.T, principal, bucketName, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/"+bucketName+"?batch", strings.NewReader(body))
	if principal != "" {
		req = req.WithContext(withAuthenticatedIdentity(req.Context(),
			auth.Identity{AccessKeyID: principal, BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}))
	}
	w := httptest.NewRecorder()
	defaultTestFrontend().bucketLevelDispatch(w, req, bucketName)
	return w
}

// batchPutObject writes an object through the REAL PutObject handler (the
// production write path: sidecar + optional tags), optionally carrying a
// principal so the fixture is indistinguishable from a client write.
func batchPutObject(t *testing.T, bucket, key, body string, principal string, headers map[string]string) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+bucket+"/"+key, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if principal != "" {
		req = req.WithContext(withAuthenticatedIdentity(req.Context(),
			auth.Identity{AccessKeyID: principal, BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}))
	}
	w := httptest.NewRecorder()
	defaultTestFrontend().objectLevelDispatch(w, req, bucket, key)
	if w.Code != http.StatusOK {
		t.Fatalf("setup PUT %s/%s: status %d: %s", bucket, key, w.Code, w.Body.String())
	}
}

// batchHead runs the REAL HeadObject handler (metadata headers ride HEAD).
func batchHead(t *testing.T, bucket, key string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/"+bucket+"/"+key, nil)
	defaultTestFrontend().objectLevelDispatch(w, req, bucket, key)
	return w
}

// batchGetTags runs the REAL GetObjectTagging handler and returns the tag
// map (nil when the response was not a 200 TagSet).
func batchGetTags(t *testing.T, bucket, key string) map[string]string {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/"+bucket+"/"+key+"?tagging", nil)
	defaultTestFrontend().objectLevelDispatch(w, req, bucket, key)
	if w.Code != http.StatusOK {
		return nil
	}
	var doc objectmodel.Tagging
	if err := xml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal Tagging XML: %v (%s)", err, w.Body.String())
	}
	out := map[string]string{}
	for _, tag := range doc.TagSet.Tags {
		out[tag.Key] = tag.Value
	}
	return out
}

// readUserXattr reads one user.* xattr by name (the test-side raw read, so
// the assertion pins the contract, not a production helper). ok=false means
// the attribute is absent.
func readUserXattr(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // test-side read-only handle.
	size, err := unix.Fgetxattr(int(f.Fd()), name, nil)
	if err != nil {
		return "", false // absent (ENODATA/ENOATTR) or unreadable: not stamped
	}
	buf := make([]byte, size)
	n, err := unix.Fgetxattr(int(f.Fd()), name, buf)
	if err != nil {
		return "", false
	}
	return string(buf[:n]), true
}

// openFDCount counts this process's open file descriptors via the
// platform's fd listing (/proc/self/fd on Linux, /dev/fd on darwin).
// ok=false when neither directory is readable.
func openFDCount() (int, bool) {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err == nil {
			return len(entries), true
		}
	}
	return 0, false
}

// ---- close-tracking backend double ----

// fdTrackingBackend wraps a real Backend and counts the Get readers it
// hands out and how many were closed. Every reader the backend returns
// MUST be closed by its caller (fsbackend opens an *os.File per Get with
// no explicit lifetime beyond Close).
type fdTrackingBackend struct {
	backend.Backend
	mu       sync.Mutex
	opened   int
	unclosed int
}

func (b *fdTrackingBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	rc, obj, err := b.Backend.Get(ctx, bucket, key, opts)
	if err != nil {
		return rc, obj, err
	}
	b.mu.Lock()
	b.opened++
	b.unclosed++
	b.mu.Unlock()
	return &trackedReader{ReadCloser: rc, b: b}, obj, err
}

func (b *fdTrackingBackend) counts() (opened, unclosed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.opened, b.unclosed
}

// installTrackingBackend installs a close-tracking wrapper over the env's
// real backend for the test's duration.
func installTrackingBackend(t *testing.T) *fdTrackingBackend {
	t.Helper()
	fb := TestBackend()
	if fb == nil {
		t.Fatal("no backend installed — call setupS3TestEnv first")
	}
	tracker := &fdTrackingBackend{Backend: fb}
	prev := installedBackendLookup()
	installBackendLookup(func(bucket string) (backend.Backend, error) {
		return tracker, nil
	})
	t.Cleanup(func() { installBackendLookup(prev) })
	return tracker
}

// trackedReader records its own Close exactly once.
type trackedReader struct {
	io.ReadCloser
	b    *fdTrackingBackend
	once sync.Once
}

func (r *trackedReader) Close() error {
	r.once.Do(func() {
		r.b.mu.Lock()
		r.b.unclosed--
		r.b.mu.Unlock()
	})
	return r.ReadCloser.Close()
}

// ---- H4: the ifMatch precheck must not leak the source reader ----

// TestBatchEndpoint_IfMatchPrecheckClosesSourceReader: a manifest whose
// items all carry ifMatch opens the source twice per item (precheck +
// data read). EVERY one of those readers must be closed, on BOTH exits —
// the satisfying precondition AND the rejected one. Before the fix the
// precheck discarded its reader into `_`, so one fd per item stayed open
// for the life of the process (a 1000-item manifest = 1000 fds).
func TestBatchEndpoint_IfMatchPrecheckClosesSourceReader(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "fd-bkt")

	const items = 20
	var ops []string
	for i := range items {
		key := "src/k" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"
		batchPutObject(t, "fd-bkt", key, "payload", "", nil)
		// ifMatch "*" matches any current representation → the precheck
		// opens the source, passes, and the copy proceeds.
		ops = append(ops, `{"op":"copy","from":"`+key+`","to":"dst/`+key+`","ifMatch":"*"}`)
	}
	// The rejected exit: a stale ETag aborts after the precheck opened the
	// source. That reader must be closed too.
	batchPutObject(t, "fd-bkt", "src/stale.txt", "payload", "", nil)
	ops = append(ops, `{"op":"copy","from":"src/stale.txt","to":"dst/stale.txt","ifMatch":"\"deadbeef\""}`)

	tracker := installTrackingBackend(t)

	fdsBefore, fdsKnown := openFDCount()
	w := batchPost(t, "fd-bkt", `{"operations":[`+strings.Join(ops, ",")+`]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	resp := batchResults(t, w.Body.String())
	if len(resp.Results) != items+1 {
		t.Fatalf("results = %d, want %d", len(resp.Results), items+1)
	}
	if resp.Results[items].Status != StatusConflictWire {
		t.Errorf("last item = %+v, want conflict (stale ETag)", resp.Results[items])
	}

	opened, unclosed := tracker.counts()
	if opened < items {
		t.Fatalf("tracking backend saw %d Gets, want >= %d — the precheck never opened the source", opened, items)
	}
	if unclosed != 0 {
		t.Errorf("batch left %d of %d source readers UNCLOSED (fd leak): every Get reader must be closed", unclosed, opened)
	}
	// Sanity check on the real symptom: the leak used to scale with the
	// manifest. With the fix the descriptor count must NOT grow by one
	// per ifMatch item.
	if fdsKnown {
		if fdsAfter, ok := openFDCount(); ok && fdsAfter-fdsBefore >= items {
			t.Errorf("open fd count grew by %d across %d ifMatch items (before=%d after=%d): a descriptor is leaking per item",
				fdsAfter-fdsBefore, items, fdsBefore, fdsAfter)
		}
	}
}

// ---- M1: batch copy parity with single-op CopyObject ----

// TestBatchEndpoint_CopyPreservesSourceMetadataAndTags: a batch copy is
// the CopyObject it claims to mirror — the destination carries the
// source's user metadata (HEAD) and its tag set (GetObjectTagging).
// Before the fix the Put passed only ContentType, so the destination
// sidecar was rewritten with an empty customMetadata and the tags were
// dropped entirely.
func TestBatchEndpoint_CopyPreservesSourceMetadataAndTags(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "meta-bkt")
	batchPutObject(t, "meta-bkt", "src/report.csv", "a,b,c\n1,2,3\n", "ak-src", map[string]string{
		"Content-Type":     "text/csv",
		"X-Amz-Meta-Owner": "alice",
		"X-Amz-Meta-Team":  "core",
		"x-amz-tagging":    "env=prod&team=core",
	})

	w := batchPost(t, "meta-bkt", `{"operations":[{"op":"copy","from":"src/report.csv","to":"dst/report.csv"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := batchResults(t, w.Body.String()).Results[0].Status; got != StatusOKWire {
		t.Fatalf("copy item = %q, want ok", got)
	}

	// Bytes copied (unchanged behavior).
	got, err := mustGetObject(t, env, "meta-bkt", "dst/report.csv")
	if err != nil || string(got) != "a,b,c\n1,2,3\n" {
		t.Fatalf("copy data = %q err=%v", got, err)
	}

	// User metadata rides HEAD on the destination.
	h := batchHead(t, "meta-bkt", "dst/report.csv")
	if h.Code != http.StatusOK {
		t.Fatalf("HEAD destination = %d", h.Code)
	}
	if v := h.Header().Get("x-amz-meta-owner"); v != "alice" {
		t.Errorf("destination x-amz-meta-owner = %q, want alice (batch copy must preserve source user metadata)", v)
	}
	if v := h.Header().Get("x-amz-meta-team"); v != "core" {
		t.Errorf("destination x-amz-meta-team = %q, want core", v)
	}
	if ct := h.Header().Get("Content-Type"); ct != "text/csv" {
		t.Errorf("destination Content-Type = %q, want text/csv", ct)
	}
	if tc := h.Header().Get("x-amz-tagging-count"); tc != "2" {
		t.Errorf("destination x-amz-tagging-count = %q, want 2", tc)
	}

	// The tag set itself survives the copy.
	tags := batchGetTags(t, "meta-bkt", "dst/report.csv")
	if tags["env"] != "prod" || tags["team"] != "core" {
		t.Errorf("destination tags = %v, want env=prod team=core (batch copy must preserve source tags)", tags)
	}
	// The source is untouched.
	if srcTags := batchGetTags(t, "meta-bkt", "src/report.csv"); srcTags["env"] != "prod" {
		t.Errorf("source tags = %v, want env=prod (a copy must not mutate the source)", srcTags)
	}
}

// TestBatchEndpoint_CopyStampsPrincipalBreadcrumbs: a batch copy is a
// mutating write, so it stamps the forensic attribution breadcrumbs the
// audit charter exists for — user.zeta.owner (set-once at create) and
// user.zeta.writer.<principal>. Before the fix no Principal reached the
// backend Put, so batch-written objects carried no attribution at all.
func TestBatchEndpoint_CopyStampsPrincipalBreadcrumbs(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "crumb-bkt")
	batchPutObject(t, "crumb-bkt", "src/plain.txt", "payload", "", nil)

	body := `{"operations":[{"op":"copy","from":"src/plain.txt","to":"dst/plain.txt"},{"op":"move","from":"src/plain.txt","to":"dst/moved.txt"}]}`
	w := batchPostAs(t, "ak-batch", "crumb-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	for i, res := range batchResults(t, w.Body.String()).Results {
		if res.Status != StatusOKWire {
			t.Errorf("results[%d] = %+v, want ok", i, res)
		}
	}

	for _, key := range []string{"dst/plain.txt", "dst/moved.txt"} {
		path := filepath.Join(env.dataDir, "crumb-bkt", key)
		if owner, ok := readUserXattr(t, path, "user.zeta.owner"); !ok || owner != "ak-batch" {
			t.Errorf("%s user.zeta.owner = %q ok=%v, want ak-batch (batch copy must stamp the principal)", key, owner, ok)
		}
		writer, ok := readUserXattr(t, path, "user.zeta.writer.ak-batch")
		if !ok {
			t.Errorf("%s has no user.zeta.writer.ak-batch breadcrumb", key)
			continue
		}
		if !strings.HasPrefix(writer, "put@") {
			t.Errorf("%s user.zeta.writer.ak-batch = %q, want put@<RFC3339>", key, writer)
		}
	}
}

// ---- M3 (second half): the injected context must reach the backend ----

// TestBatchEndpoint_CancelledRequestContextStopsItemWork: the context the
// executor stores is the client's — a cancelled context must abort the
// item (no destination written) instead of running the whole manifest to
// completion on context.Background().
func TestBatchEndpoint_CancelledRequestContextStopsItemWork(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "ctx-bkt")
	batchPutObject(t, "ctx-bkt", "src.txt", "payload", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client disconnected before the batch ran
	req := httptest.NewRequest("POST", "/ctx-bkt?batch",
		strings.NewReader(`{"operations":[{"op":"copy","from":"src.txt","to":"dst.txt"}]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	defaultTestFrontend().bucketLevelDispatch(w, req, "ctx-bkt")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cancellation is a per-item outcome, not a request failure): %s", w.Code, w.Body.String())
	}
	res := batchResults(t, w.Body.String()).Results[0]
	if res.Status != StatusErrorWire {
		t.Errorf("cancelled context item = %+v, want error (the request context must reach the backend calls)", res)
	}
	if _, err := env.b.Stat(context.Background(), "ctx-bkt", "dst.txt"); err == nil {
		t.Errorf("cancelled context must not write the destination")
	}
	if _, err := env.b.Stat(context.Background(), "ctx-bkt", "src.txt"); err != nil {
		t.Errorf("the source must survive: %v", err)
	}
}

// ---- M3 (first half): the s3 mount validates the bucket name ----

// TestBatchEndpoint_RejectsTraversalBucketName: the s3 ?batch mount must
// apply the SAME bucket-name gate the webdav/h3 mount applies
// (HandleBatchForBucket runs validBucket first) — a traversal-shaped
// bucket name is answered 404 NoSuchBucket and reaches neither the
// exists check's parent directory nor any executor.
func TestBatchEndpoint_RejectsTraversalBucketName(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "traversal-bkt")

	// A real directory OUTSIDE the data dir, named the way a traversal
	// bucket name resolves after filepath.Join.
	escapeDir := filepath.Join(filepath.Dir(env.dataDir), "escape")
	if err := os.MkdirAll(filepath.Join(escapeDir, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating the outside-bucket fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(escapeDir, "outside.txt"), []byte("not yours"), 0o644); err != nil {
		t.Fatalf("seeding the outside-bucket fixture: %v", err)
	}

	w := batchPost(t, "../escape", `{"operations":[{"op":"delete","from":"outside.txt"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (same as the webdav mount's validBucket gate): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("want NoSuchBucket, got: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(escapeDir, "outside.txt")); err != nil {
		t.Errorf("a traversal-shaped bucket name must execute nothing: %v", err)
	}
	// A syntactically invalid (non-traversal) name is rejected the same way.
	w = batchPost(t, "Bad_Name", `{"operations":[{"op":"delete","from":"k"}]}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("invalid bucket name status = %d, want 404: %s", w.Code, w.Body.String())
	}
	// Sanity: a legal name still works (the gate is not over-broad).
	w = batchPost(t, "traversal-bkt", `{"operations":[{"op":"delete","from":"nothing-here.txt"}]}`)
	if w.Code != http.StatusOK {
		t.Errorf("valid bucket name status = %d, want 200: %s", w.Code, w.Body.String())
	}
}
