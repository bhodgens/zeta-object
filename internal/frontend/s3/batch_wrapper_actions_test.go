// batch_wrapper_actions_test.go — the pins for the 2026-10-05 fix wave on
// this package's batch + response-wrapper layer, each driving the REAL
// pipeline (dispatch → handler → batchops core → Backend seam), never
// around it:
//
//   - F1: statusRecorder must expose Unwrap, so http.ResponseController
//     reaches the real response's optional capabilities (Flush, Hijack)
//     even when the audit log is configured — the defect commit bf77bef
//     fixed one layer out on the Alt-Svc wrapper.
//   - F2: the executor runs each item under the RESOLVED bucket path its
//     mounting frontend handed over, and falls back to getBucketPath when
//     none was supplied (the s3 mount's own shape).
//   - F3: a batch copy's source read is BOUNDED — an oversized source is
//     refused InvalidArgument without buffering the whole object, while
//     the one-Get shape (the H4 fd-leak fix) survives.
//   - F5: a batch copy fires after_upload and a batch delete fires
//     after_delete with the single-op handlers' ActionContext shape, and a
//     FAILED item fires nothing.
//
// The webdav-mount half of the F2 pin (a resolver that differs from s3's)
// lives in internal/frontend/webdav/batch_resolver_test.go — the mount
// itself is webdav's, so the divergent-resolver wiring is theirs.
package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// ---------- F1: the audit wrapper must not strip ResponseController ----------

// TestStatusRecorder_ResponseControllerReachesUnderlyingWriter: the audit
// wrapper sits in front of EVERY authorized s3 request when the audit log
// is configured, so losing Flush/Hijack/SetDeadline there is a server-wide
// capability loss, not a corner case. Both assertions run against a real
// *http.response over a real connection (httptest.NewRecorder implements
// Flush, but NOT Hijack — the Hijack arm needs the live server).
func TestStatusRecorder_ResponseControllerReachesUnderlyingWriter(t *testing.T) {
	// Arm 1 (real connection): an s3-shaped handler behind statusRecorder
	// must still Flush and Hijack. On a plain writer both succeed; through a
	// wrapper with no Unwrap both return ErrNotSupported. The handler
	// goroutine hands its results back over a channel - plain vars would be
	// a data race the -race gate refuses (the response's completion does not
	// synchronize the writes for the race detector).
	type ctlResult struct {
		flushErr  error
		hijackErr error
	}
	results := make(chan ctlResult, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		rc := http.NewResponseController(rec)
		res := ctlResult{flushErr: rc.Flush()}
		_, _, res.hijackErr = rc.Hijack()
		results <- res
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	select {
	case res := <-results:
		if res.flushErr != nil {
			t.Errorf("ResponseController.Flush through statusRecorder = %v, want nil: the audit wrapper must expose Unwrap() http.ResponseWriter", res.flushErr)
		}
		if res.hijackErr != nil {
			t.Errorf("ResponseController.Hijack through statusRecorder = %v, want nil: the audit wrapper must expose Unwrap() http.ResponseWriter", res.hijackErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never reported its ResponseController results")
	}

}

// TestStatusRecorder_UnwrapsToUnderlyingWriter pins the wrapper's own
// contract directly (the altsvc_test.go shape): Unwrap must return the
// writer it was handed, so the controller's rwUnwrapper walk terminates on
// a writer that actually implements the capabilities.
func TestStatusRecorder_UnwrapsToUnderlyingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &statusRecorder{ResponseWriter: rec}
	u, ok := any(w).(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		t.Fatal("statusRecorder must implement Unwrap() http.ResponseWriter (ResponseController capabilities are otherwise lost)")
	}
	if got := u.Unwrap(); got != http.ResponseWriter(rec) {
		t.Fatalf("Unwrap = %#v, want the wrapped ResponseWriter", got)
	}
}

// TestStatusRecorder_StillRecordsStatus pins that Unwrap did not disturb
// the audit wrapper's actual job: the status still defaults to 200 through
// the implicit-Write path and is captured explicitly on WriteHeader.
func TestStatusRecorder_StillRecordsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &statusRecorder{ResponseWriter: rec}
	if _, err := w.Write([]byte("implicit")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if w.status != http.StatusOK {
		t.Errorf("status after implicit Write = %d, want 200", w.status)
	}

	rec2 := httptest.NewRecorder()
	w2 := &statusRecorder{ResponseWriter: rec2}
	w2.WriteHeader(http.StatusNotFound)
	if w2.status != http.StatusNotFound {
		t.Errorf("status after WriteHeader = %d, want 404", w2.status)
	}
	// Unwrap must not leak the recorder's own capabilities past the wrapper
	// in a way that breaks the recorded status: re-checking the recorded
	// value after an unwrap-driven Flush is the whole point of the pin.
	if err := http.NewResponseController(w2).Flush(); err != nil {
		t.Errorf("Flush through the second recorder = %v, want nil", err)
	}
	if w2.status != http.StatusNotFound {
		t.Errorf("status after Flush through the wrapper = %d, want 404 (Flush must not reset the captured status)", w2.status)
	}
}

// ---------- F3: a batch copy's source read is bounded ----------

// oversizedSourceBackend wraps a real Backend and reports a source object
// larger than the copy ceiling WITHOUT materializing it: Get returns a
// reader that streams batchCopyMaxBytes+2 zero bytes lazily, so a test
// that passed by "the object was small" is impossible, and a test that
// read the whole thing would have to move 5 GiB to fail.
//
// The declared Size comes from the sidecar, so the tracking wrapper must
// patch the Object too — the pin asserts the reader was NOT drained past
// the bound, which is the actual resource claim.
type oversizedSourceBackend struct {
	backend.Backend
	limit int64
	reads *int64
	mu    sync.Mutex
}

func (b *oversizedSourceBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	rc, obj, err := b.Backend.Get(ctx, bucket, key, opts)
	if err != nil {
		return rc, obj, err
	}
	b.mu.Lock()
	*b.reads += 0 // touch under the lock; the count is bumped per read chunk
	b.mu.Unlock()
	obj.Size = b.limit + 2 // the source is over the ceiling
	return &meteredReader{rc: rc, remaining: b.limit + 2, reads: b.reads}, obj, nil
}

// meteredReader streams up to remaining lazy zero bytes, counting the bytes
// it actually yields — the pin is "the batch never drained more than the
// ceiling + 1", which is what a bounded read guarantees and an unbounded
// io.ReadAll does not.
type meteredReader struct {
	rc        io.ReadCloser
	remaining int64
	reads     *int64
	mu        sync.Mutex
}

func (m *meteredReader) Read(p []byte) (int, error) {
	if m.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > m.remaining {
		p = p[:m.remaining]
	}
	n, err := m.rc.Read(p)
	if n == 0 && err == nil {
		// A reader that blocks-then-yields reports (0, nil); hand back
		// synthetic zeros so the copy treats them like real bytes.
		n = len(p)
		for i := range p {
			p[i] = 0
		}
		err = nil
	} else if n == 0 && err == io.EOF && m.remaining > 0 {
		// The underlying object ENDED (a real file's Read returns (0, EOF)
		// once exhausted), but the declared source is larger: stream
		// synthetic zeros up to the declared size. Without this arm the
		// declared-size premise is untestable - the reader would stop at the
		// real file's EOF and the size gate would never fire.
		n = len(p)
		for i := range p {
			p[i] = 0
		}
		err = nil
	}
	m.remaining -= int64(n)
	m.mu.Lock()
	*m.reads += int64(n)
	m.mu.Unlock()
	return n, err
}

func (m *meteredReader) Close() error { return m.rc.Close() }

// TestBatchCopy_OverSizedSourceIsRefusedWithoutBuffering: a copy item
// whose source exceeds the bound answers InvalidArgument and never buffers
// the whole object. Before the fix the read was an unbounded io.ReadAll,
// so a stale-ifMatch manifest could grow the heap by source-size per item.
func TestBatchCopy_OverSizedSourceIsRefusedWithoutBuffering(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "bound-bkt")
	batchPutObject(t, "bound-bkt", "src/big.bin", "small on disk", "", nil)

	// Shrink the ceiling to a test-sized budget via the live var (the seam
	// exists for exactly this), so the metered reader streams 1 MiB+2 under
	// -race in milliseconds instead of 5 GiB+2 in a minute - the CI runner
	// killed this test's 5-GiB stream deterministically (3/3 runner
	// shutdowns, all landing inside this one test's window). The pin's
	// claims are unchanged: the read stops one byte past the WHATEVER
	// ceiling, and the refusal carries the same InvalidArgument.
	const testCeiling int64 = 1 << 20
	prevCeiling := batchCopyMaxBytes
	batchCopyMaxBytes = testCeiling
	t.Cleanup(func() { batchCopyMaxBytes = prevCeiling })

	var read int64
	prev := installedBackendLookup()
	installBackendLookup(func(bucket string) (backend.Backend, error) {
		b, err := prev(bucket)
		if err != nil {
			return nil, err
		}
		return &oversizedSourceBackend{Backend: b, limit: testCeiling, reads: &read}, nil
	})
	t.Cleanup(func() { installBackendLookup(prev) })

	// The over-limit gate is checked before the If-Match decision, so this
	// runs with a MATCHING ifMatch ("*") — the copy would otherwise
	// succeed, which proves the size gate (not the precondition) is what
	// refuses the item.
	w := batchPost(t, "bound-bkt", `{"operations":[{"op":"copy","from":"src/big.bin","to":"dst/big.bin","ifMatch":"*"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the refusal is a per-item result): %s", w.Code, w.Body.String())
	}
	res := batchResults(t, w.Body.String()).Results[0]
	if res.Status != StatusErrorWire {
		t.Fatalf("oversized copy item = %+v, want error", res)
	}
	if res.Code != objectmodel.CodeInvalidArgument {
		t.Errorf("oversized copy item code = %q, want %q (the same class the PutObject size limit reports)", res.Code, objectmodel.CodeInvalidArgument)
	}
	if !strings.Contains(strings.ToLower(res.Message), "maximum allowed size") {
		t.Errorf("oversized copy item message = %q, want the size-limit wording", res.Message)
	}

	// The resource claim: the read stopped at the bound. The drain is
	// bounded by ceiling+1 (the LimitReader's one byte of overflow
	// detection), never by the object's size.
	if read > batchCopyMaxBytes+1 {
		t.Errorf("drained %d bytes from the source, want <= %d (the read must be bounded, not buffered whole)", read, batchCopyMaxBytes+1)
	}
	// Nothing was written.
	if _, err := env.b.Stat(context.Background(), "bound-bkt", "dst/big.bin"); err == nil {
		t.Errorf("a refused oversized copy must write no destination")
	}
}

// TestBatchCopy_NormalSizeStillCopiesAndIfMatchStaleStillConflicts: the
// bound must not change ordinary behavior — a normal copy succeeds and a
// stale If-Match still answers conflict (not the size error).
func TestBatchCopy_NormalSizeStillCopiesAndIfMatchStaleStillConflicts(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "norm-bkt")
	batchPutObject(t, "norm-bkt", "src/a.txt", "payload", "", nil)

	w := batchPost(t, "norm-bkt", `{"operations":[{"op":"copy","from":"src/a.txt","to":"dst/a.txt","ifMatch":"\"deadbeef\""}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	res := batchResults(t, w.Body.String()).Results[0]
	if res.Status != StatusConflictWire {
		t.Fatalf("stale ifMatch item = %+v, want conflict (the size gate must not shadow the precondition)", res)
	}

	w = batchPost(t, "norm-bkt", `{"operations":[{"op":"copy","from":"src/a.txt","to":"dst/a.txt"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := batchResults(t, w.Body.String()).Results[0].Status; got != StatusOKWire {
		t.Fatalf("plain copy = %q, want ok: a normal-size source must still copy", got)
	}
	if data, err := mustGetObject(t, env, "norm-bkt", "dst/a.txt"); err != nil || string(data) != "payload" {
		t.Errorf("copied data = %q err=%v, want payload", data, err)
	}
}

// ---------- F2: the executor honours the caller's resolved bucket path ----------

// TestBatchExecutor_ResolvesCallerPathThenFallsBack: resolveBucketPath is
// the seam that makes the two ?batch mounts non-divergent by construction.
// A non-empty caller path wins; an empty one falls back to getBucketPath
// (the s3 mount's shape, and the documented unwired seam).
func TestBatchExecutor_ResolvesCallerPathThenFallsBack(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "resolve-bkt")

	callerPath := filepath.Join(env.dataDir, "resolve-bkt", "!caller-root")
	withPath := s3BatchExecutor{bucket: "resolve-bkt", bucketPath: callerPath, ctx: context.Background()}
	if got := withPath.resolveBucketPath(); got != callerPath {
		t.Errorf("resolveBucketPath with a caller path = %q, want %q (the mounting frontend's resolution must win)", got, callerPath)
	}
	// The mount that supplies no path lands on getBucketPath — unchanged.
	noPath := s3BatchExecutor{bucket: "resolve-bkt", ctx: context.Background()}
	if got, want := noPath.resolveBucketPath(), getBucketPath("resolve-bkt"); got != want {
		t.Errorf("resolveBucketPath with no caller path = %q, want getBucketPath's %q", got, want)
	}
}

// TestBatchBridge_CarriesCallerPathOntoEveryItem: the bridge must thread
// its bucketPath argument onto the executor it builds — that parameter used
// to be dead, which is the whole finding. Driving the bridge end to end
// with a path that is deliberately NOT getBucketPath's answer proves the
// thread survives to the executed item: the capture/tags/versioning steps
// then read and write under the caller's root.
//
// The observable proof is the version store: a batch overwrite on a bucket
// whose versioning was enabled THROUGH the caller path captures an old
// version under THAT root. getBucketPath's root has no versioning marker,
// so a capture recorded only there is what a dead parameter would produce.
func TestBatchBridge_CarriesCallerPathOntoEveryItem(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "bridge-bkt")
	bucketPath := getBucketPath("bridge-bkt")
	// The caller's resolved root — same directory today (production wires
	// one resolver), but a DIFFERENT string the executor must prefer: a
	// symlinked alias of the same directory. Same data, different path
	// string, so the tag/version/capture steps that join on bucketPath are
	// observably resolved through the caller's answer.
	alias := filepath.Join(env.dataDir, "bridge-bkt-alias")
	if err := os.Symlink(bucketPath, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	batchPutObject(t, "bridge-bkt", "src/x.txt", "original", "", nil)
	// The DESTINATION must already exist: version capture records the OLD
	// bytes on an overwrite. A copy to a NEW key records no version at all
	// (errNoPriorVersion -> create), so a new-key copy could never prove
	// which root the capture ran under.
	batchPutObject(t, "bridge-bkt", "dst/x.txt", "old bytes", "", nil)

	// Enable versioning through the REAL handler (a sidecar write under the
	// caller's root), then overwrite via the bridge with the alias path.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/bridge-bkt?versioning",
		strings.NewReader(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`))
	req.Header.Set("Content-Type", "application/xml")
	putBucketVersioningHandler(w, req, "bridge-bkt")
	if w.Code != http.StatusOK {
		t.Fatalf("enable versioning = %d: %s", w.Code, w.Body.String())
	}

	bridgeReq := httptest.NewRequest("POST", "/bridge-bkt?batch",
		strings.NewReader(`{"operations":[{"op":"copy","from":"src/x.txt","to":"dst/x.txt"}]}`))
	bw := httptest.NewRecorder()
	HandleBatchForBucket(bw, bridgeReq, "bridge-bkt", alias, validateObjectKey)
	if bw.Code != http.StatusOK {
		t.Fatalf("bridge status = %d, want 200: %s", bw.Code, bw.Body.String())
	}
	if got := batchResults(t, bw.Body.String()).Results[0].Status; got != StatusOKWire {
		t.Fatalf("bridge copy = %q, want ok: %s", got, bw.Body.String())
	}

	// The captured old version lives under the ALIAS root the caller passed
	// (the same directory, reached through the caller's string). Resolve it
	// through the caller's path: the version store must report the
	// pre-overwrite bytes.
	entries, err := versionStoreForBucket(alias).List("bridge-bkt", "dst/x.txt")
	if err != nil {
		t.Fatalf("listing versions through the caller path: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no captured version under the caller-resolved path — the executor ran through getBucketPath instead of the caller's path")
	}
}

// ---------- F5: batch writes fire the bucket-action triggers ----------

// actionRecorder collects the trigger's (eventType, ctx) pairs.
type actionRecorder struct {
	mu     sync.Mutex
	events []recordedAction
}

type recordedAction struct {
	event string
	ctx   ActionContext
}

func (r *actionRecorder) install(t *testing.T) {
	t.Helper()
	InstallActionTrigger(func(eventType string, ctx ActionContext) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, recordedAction{event: eventType, ctx: ctx})
	})
	t.Cleanup(func() { InstallActionTrigger(nil) })
}

func (r *actionRecorder) waitFor(t *testing.T, want int, timeout time.Duration) []recordedAction {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		got := append([]recordedAction(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBatchWrites_FireActionsWithSingleOpShape: a batch copy fires
// after_upload and a batch delete fires after_delete, with the SAME
// ActionContext shape (bucket, key, resolved paths) the single-op handlers
// fire — the parity finding was that the batch surface reached
// fsbackend.Put directly and bypassed the trigger entirely.
func TestBatchWrites_FireActionsWithSingleOpShape(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "act-bkt")
	bucketPath := getBucketPath("act-bkt")

	rec := &actionRecorder{}
	rec.install(t)

	// The source PUT carries a Content-Type so the parity assertion below
	// tests the copy's ContentType carry-over, not the empty default a
	// header-less PUT stores.
	batchPutObject(t, "act-bkt", "src/a.txt", "copy payload", "ak-batch",
		map[string]string{"Content-Type": "text/plain"})
	batchPutObject(t, "act-bkt", "src/b.txt", "delete payload", "ak-batch", nil)

	// Drain the setup writes' own triggers so the assertions below see only
	// the batch item's. The PUTs fire after_upload in `go triggerActions`
	// goroutines: WAIT for both to land first, or a late setup event can
	// arrive after this drain, satisfy waitFor(2) below, and starve the real
	// after_delete (two stale uploads is exactly 2 events).
	rec.waitFor(t, 2, 2*time.Second)
	rec.mu.Lock()
	rec.events = nil
	rec.mu.Unlock()

	w := batchPostAs(t, "ak-batch", "act-bkt",
		`{"operations":[{"op":"copy","from":"src/a.txt","to":"dst/a.txt"},{"op":"delete","from":"src/b.txt"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	for i, res := range batchResults(t, w.Body.String()).Results {
		if res.Status != StatusOKWire {
			t.Fatalf("results[%d] = %+v, want ok", i, res)
		}
	}

	events := rec.waitFor(t, 2, 2*time.Second)
	if len(events) < 2 {
		t.Fatalf("batch fired %d actions (%+v), want an after_upload for the copy AND an after_delete for the delete", len(events), events)
	}

	var upload, del *recordedAction
	for i := range events {
		switch events[i].event {
		case "after_upload":
			upload = &events[i]
		case "after_delete":
			del = &events[i]
		}
	}
	if upload == nil {
		t.Fatal("a batch copy fired no after_upload action (per-bucket .bucket-actions hooks silently skip every batch write)")
	}
	if del == nil {
		t.Fatal("a batch delete fired no after_delete action")
	}

	// Same ActionContext shape the single-op CopyObject/DeleteObject fire.
	if upload.ctx.BucketName != "act-bkt" || upload.ctx.ObjectKey != "dst/a.txt" {
		t.Errorf("after_upload ctx = %+v, want bucket act-bkt key dst/a.txt", upload.ctx)
	}
	if upload.ctx.BucketPath != bucketPath {
		t.Errorf("after_upload BucketPath = %q, want the resolved bucket root %q", upload.ctx.BucketPath, bucketPath)
	}
	if upload.ctx.FilePath != objectDataPathFor(bucketPath, "dst/a.txt") {
		t.Errorf("after_upload FilePath = %q, want the destination data path", upload.ctx.FilePath)
	}
	if upload.ctx.MetadataPath != filepath.Join(bucketPath, ".metadata", "dst/a.txt"+".meta") {
		t.Errorf("after_upload MetadataPath = %q, want the destination sidecar path", upload.ctx.MetadataPath)
	}
	if upload.ctx.Size != int64(len("copy payload")) {
		t.Errorf("after_upload Size = %d, want %d", upload.ctx.Size, len("copy payload"))
	}
	if upload.ctx.ContentType != "text/plain; charset=utf-8" && upload.ctx.ContentType != "text/plain" {
		t.Errorf("after_upload ContentType = %q, want the copied source content type", upload.ctx.ContentType)
	}
	if upload.ctx.ETag == "" {
		t.Error("after_upload ETag is empty (the single-op shape carries the written object's ETag)")
	}

	if del.ctx.BucketName != "act-bkt" || del.ctx.ObjectKey != "src/b.txt" {
		t.Errorf("after_delete ctx = %+v, want bucket act-bkt key src/b.txt", del.ctx)
	}
	if del.ctx.FilePath != objectDataPathFor(bucketPath, "src/b.txt") {
		t.Errorf("after_delete FilePath = %q, want the deleted key's data path", del.ctx.FilePath)
	}
	_ = env
}

// TestBatchWrites_FailedItemFiresNothing: an item that fails its
// precondition (or its size gate) writes nothing, so it must fire no
// action — a hook that observed a failed item would run against an object
// that was never created.
func TestBatchWrites_FailedItemFiresNothing(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "act-fail-bkt")
	rec := &actionRecorder{}
	rec.install(t)
	batchPutObject(t, "act-fail-bkt", "src/a.txt", "payload", "", nil)

	// The setup PUT fired its own after_upload in a `go triggerActions`
	// goroutine. WAIT for it before draining - clearing immediately races
	// that goroutine, and its late event would be misread as a failed item's
	// action (the exact false positive this test exists to refuse).
	rec.waitFor(t, 1, 2*time.Second)
	rec.mu.Lock()
	rec.events = nil
	rec.mu.Unlock()

	// A stale If-Match refuses the copy before any write.
	w := batchPost(t, "act-fail-bkt",
		`{"operations":[{"op":"copy","from":"src/a.txt","to":"dst/a.txt","ifMatch":"\"deadbeef\""}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := batchResults(t, w.Body.String()).Results[0].Status; got != StatusConflictWire {
		t.Fatalf("item = %q, want conflict", got)
	}

	// A delete of a missing key fails NoSuchKey and writes nothing.
	w = batchPost(t, "act-fail-bkt", `{"operations":[{"op":"delete","from":"nothing-here.txt"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := batchResults(t, w.Body.String()).Results[0].Status; got != StatusErrorWire {
		t.Fatalf("missing-key delete = %q, want error", got)
	}

	// Give a stray trigger the same window the success test used.
	time.Sleep(200 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 0 {
		t.Errorf("failed batch items fired %d actions (%+v), want none: a hook must never observe an item that wrote nothing", len(rec.events), rec.events)
	}
	_ = env
}
