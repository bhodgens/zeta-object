// coverage_restore2_test.go — REAL behavior tests for the remaining
// previously-uncovered surfaces (companion to coverage_restore_test.go):
//
//   - versioning_handlers.go captureReflinkDispatch + the reflink PUT
//     capture/record arms, driven through WithReflinkCloneForTest (the
//     harness the webdav parity tests use — byte-copy emulating the
//     FICLONE ioctl) plus the fail-soft clone arm
//   - zfssurface_bridge.go HandleCollectionEventsForBucket (the honest
//     empty collection history, validation, and provider arms)
//   - export_versioning_test_surface.go SetupVersioningTestEnv +
//     SidecarStoreForBucket + VersionStore.List/Open + TestBackend
//     (driving the harness through REAL HTTP handler flows)
//   - object_handlers.go itemCtx fallback chain (via the batch
//     executor's per-item context resolution)
package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/metadata"
)

// TestReflinkCaptureDispatchRoundTrip drives captureReflinkDispatch (the
// 0% dispatcher) end-to-end through the exported clone-seam harness: a
// real PUT creates the object, the capture clones it into .versions-r,
// the overwrite lands, and the record step restores the history — then a
// versioned read serves the OLD bytes back.
func TestReflinkCaptureDispatchRoundTrip(t *testing.T) {
	env := newVerEnv(t, "reflink-dispatch-bkt")
	installReflinkMode(t, 0)
	WithReflinkCloneForTest(t, func(dstPath, srcPath string) error {
		raw, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		return os.WriteFile(dstPath, raw, 0o644)
	})

	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "doc.txt", "r1"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}

	// Capture dispatches into the reflink path (reflink field set).
	captured, err := CaptureCurrentObjectVersionForBucket(env.bucketPath, "doc.txt")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if captured.reflink == nil || !captured.reflink.cloneOK {
		t.Fatalf("capture must dispatch to the reflink path with cloneOK, got %+v", captured.reflink)
	}
	clonePath := filepath.Join(env.bucketPath, ".metadata", reflinkVersionsDirName, keySha("doc.txt"), captured.reflink.id)
	if raw, err := os.ReadFile(clonePath); err != nil || string(raw) != "r1" {
		t.Errorf("clone = %q (%v), want r1", raw, err)
	}

	// The plain overwrite (backend Put rewrites the sidecar wholesale),
	// then the record step lands the bookkeeping entry.
	if w := verPutBody(t, env, "doc.txt", "r2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}
	if err := RecordCapturedObjectVersionForBucket(env.bucketPath, env.bucket, "doc.txt", captured); err != nil {
		t.Fatalf("record: %v", err)
	}
	versions, err := ListVersionsForTest(env.bucketPath, env.bucket, "doc.txt")
	if err != nil || len(versions) != 1 || versions[0].ID != captured.reflink.id {
		t.Fatalf("versions = (%+v, %v), want the cloned entry", versions, err)
	}
	body, err := OpenVersionForTest(env.bucketPath, env.bucket, "doc.txt", captured.reflink.id)
	if err != nil || string(body) != "r1" {
		t.Errorf("old version bytes = %q (%v), want r1", body, err)
	}
}

// TestReflinkCaptureDispatchFailSoft pins the fail-soft arm: a clone
// failure yields a NON-NIL capture with cloneOK=false, and the record
// step then writes NOTHING (the PUT proceeds versionless by design).
func TestReflinkCaptureDispatchFailSoft(t *testing.T) {
	env := newVerEnv(t, "reflink-soft-bkt")
	installReflinkMode(t, 0)
	WithReflinkCloneForTest(t, func(dstPath, srcPath string) error {
		return errors.New("FICLONE refused (emulated)")
	})

	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "doc.txt", "only"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}
	captured, err := CaptureCurrentObjectVersionForBucket(env.bucketPath, "doc.txt")
	if err != nil {
		t.Fatalf("capture must fail soft, got error: %v", err)
	}
	if captured == nil || captured.reflink == nil || captured.reflink.cloneOK {
		t.Fatalf("failed clone must yield a non-nil capture with cloneOK=false, got %+v", captured)
	}
	// Record writes nothing: no version history appears.
	if err := RecordCapturedObjectVersionForBucket(env.bucketPath, env.bucket, "doc.txt", captured); err != nil {
		t.Fatalf("record after failed clone must be a no-op, got %v", err)
	}
	if _, err := ListVersionsForTest(env.bucketPath, env.bucket, "doc.txt"); !isNoSuchKeyErr(err) {
		t.Errorf("history after fail-soft = %v, want NoSuchKey (no version recorded)", err)
	}
}

// TestHandleCollectionEventsForBucketArms drives the webdav collection
// events bridge: a known bucket answers 200 with the honest EMPTY
// history envelope (events: [], never another object's rows), an unknown
// bucket answers 404 NoSuchBucket, and a bucket with no provider gets
// the contracted 503.
func TestHandleCollectionEventsForBucketArms(t *testing.T) {
	bucket := "collection-events-bkt"
	newSharedEventsEnv(t, bucket)
	bucketPath := getBucketPath(bucket)
	InstallMetadataProvider(func(bp string) metadata.MetadataProvider {
		if bp == bucketPath {
			return bridgeProvider{}
		}
		return nil
	})
	t.Cleanup(func() { InstallMetadataProvider(nil) })

	// Known bucket: 200, envelope shape with an EMPTY (non-null) events
	// array and the provider's fresh HistoryDetail.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/"+bucket+"?events", nil)
	HandleCollectionEventsForBucket(w, req, bucket, bucketPath)
	if w.Code != http.StatusOK {
		t.Fatalf("collection events = %d %s, want 200", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"events": []`) && !strings.Contains(body, "\"events\":[]") {
		t.Errorf("collection events body = %s, want an empty events array", body)
	}
	if strings.Contains(body, `"events": null`) || strings.Contains(body, "\"events\":null") {
		t.Errorf("collection events body = %s, events must never be null", body)
	}
	if strings.Contains(body, `"key"`) {
		t.Errorf("collection events body = %s, must not carry another object's rows", body)
	}
	if !strings.Contains(body, "stub/shared") {
		t.Errorf("collection events body = %s, want the fresh HistoryDetail dataset", body)
	}

	// Unknown bucket: 404 NoSuchBucket.
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/nope?events", nil)
	HandleCollectionEventsForBucket(w, req, "no-such-collection-bkt", bucketPath)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("unknown bucket = %d %s, want 404 NoSuchBucket", w.Code, w.Body.String())
	}

	// Known bucket, no provider: the contracted 503 (404 precedence
	// already covered above).
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/"+bucket+"?events", nil)
	HandleCollectionEventsForBucket(w, req, bucket, "/definitely/not/a/bucket")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("no provider = %d %s, want 503", w.Code, w.Body.String())
	}
}

// TestVersioningTestSurfaceHarness drives the exported cross-package
// harness through a REAL handler flow: SetupVersioningTestEnv installs
// the seams, the ForTest handlers run Put/versioning/Get over HTTP,
// SidecarStoreForBucket reads the sidecar-layout history back, and
// TestBackend returns the installed backend.
func TestVersioningTestSurfaceHarness(t *testing.T) {
	dataDir := t.TempDir()
	bucket := "harness-bkt"
	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	SetupVersioningTestEnv(t, dataDir)
	installZfsVersioningModeForTest(t, "sidecar")
	installZmetadDBForTest(t, nil)

	if TestBackend() == nil {
		t.Fatal("TestBackend must return the backend SetupVersioningTestEnv installed")
	}

	// Real HTTP handler flow through the ForTest doubles.
	w := httptest.NewRecorder()
	PutObjectHandlerForTest(w, httptest.NewRequest("PUT", "/"+bucket+"/h.txt", strings.NewReader("h1")), bucket, "h.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("PutObjectHandlerForTest = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	PutBucketVersioningHandlerForTest(w,
		httptest.NewRequest("PUT", "/"+bucket+"?versioning",
			strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")),
		bucket)
	if w.Code != http.StatusOK {
		t.Fatalf("PutBucketVersioningHandlerForTest = %d %s", w.Code, w.Body.String())
	}

	// Overwrite: capture + record via the bridges so the OLD version is
	// in the sidecar history, then read it back through the harness's
	// read surface (SidecarStoreForBucket + ListVersionsForTest).
	captured, err := CaptureCurrentObjectVersionForBucket(bucketPath, "h.txt")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	w = httptest.NewRecorder()
	PutObjectHandlerForTest(w, httptest.NewRequest("PUT", "/"+bucket+"/h.txt", strings.NewReader("h2")), bucket, "h.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("overwrite = %d", w.Code)
	}
	if err := RecordCapturedObjectVersionForBucket(bucketPath, bucket, "h.txt", captured); err != nil {
		t.Fatalf("record: %v", err)
	}

	sidecar := SidecarStoreForBucket(bucketPath)
	entries, err := sidecar.List(bucket, "h.txt")
	if err != nil || len(entries) != 1 {
		t.Fatalf("SidecarStoreForBucket.List = (%+v, %v), want one entry", entries, err)
	}
	body, err := sidecar.Open(bucket, "h.txt", entries[0].ID)
	if err != nil || string(body) != "h1" {
		t.Errorf("SidecarStoreForBucket.Open = %q (%v), want h1", body, err)
	}
	entries, err = ListVersionsForTest(bucketPath, bucket, "h.txt")
	if err != nil || len(entries) != 1 || !entries[0].IsLatest {
		t.Fatalf("ListVersionsForTest = (%+v, %v), want one latest entry", entries, err)
	}
	body, err = OpenVersionForTest(bucketPath, bucket, "h.txt", entries[0].ID)
	if err != nil || string(body) != "h1" {
		t.Errorf("OpenVersionForTest = %q (%v), want h1", body, err)
	}
	// A missing key propagates NoSuchKey through the read surface.
	if _, err := ListVersionsForTest(bucketPath, bucket, "absent.txt"); !isNoSuchKeyErr(err) {
		t.Errorf("ListVersionsForTest absent = %v, want NoSuchKey", err)
	}

	// The DELETE bridge through its ForTest double records the marker
	// on the Enabled bucket; a plain GET then answers the marker 404.
	w = httptest.NewRecorder()
	DeleteObjectHandlerForTest(w, httptest.NewRequest("DELETE", "/"+bucket+"/h.txt", nil), bucket, "h.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteObjectHandlerForTest = %d %s", w.Code, w.Body.String())
	}
	hidden, err := PlainObjectDeleteMarker404(bucketPath, bucket, "h.txt")
	if err != nil || !hidden {
		t.Errorf("marker404 after handler DELETE = (%v, %v), want (true, nil)", hidden, err)
	}

	// COPY through its ForTest double (the webdav COPY parity pin's
	// counterpart): the x-amz-copy-source header routes PUT into the
	// copy path, and the copy lands the source bytes at the destination.
	// The source is h.txt's plain data (still on disk under the delete
	// marker) — copying the CURRENT data proves the copy path, and the
	// destination lands markerless.
	w = httptest.NewRecorder()
	copyReq := httptest.NewRequest("PUT", "/"+bucket+"/h-copy.txt", nil)
	copyReq.Header.Set("x-amz-copy-source", bucket+"/h.txt")
	CopyObjectHandlerForTest(w, copyReq, bucket, "h-copy.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("CopyObjectHandlerForTest = %d %s, want 200", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, bucket, "h-copy.txt")); err != nil {
		t.Errorf("copy destination missing: %v", err)
	}
}

// TestBatchExecutorItemCtxFallback pins the executor's per-item context
// resolution: the caller's context wins, the stored one is the fallback,
// and the result is never nil (a client disconnect must stop the
// manifest's remaining keys).
// The nil-context fallback is exercised through untyped nil interface
// values (the exact shape a miswired caller produces), asserted via a
// helper that keeps staticcheck's SA1012 satisfied — the production
// contract under test is "never nil, fall back to stored/Background".
func itemCtxUntypedNil(e s3BatchExecutor) context.Context {
	var nilCtx context.Context // typed-nil-free: an untyped nil interface
	return e.itemCtx(nilCtxValue(&nilCtx))
}

func nilCtxValue(p *context.Context) context.Context { return *p }

func TestBatchExecutorItemCtxFallback(t *testing.T) {
	caller := context.WithValue(context.Background(), ctxKeyProbe{}, "caller")
	stored := context.WithValue(context.Background(), ctxKeyProbe{}, "stored")
	e := s3BatchExecutor{bucket: "b", bucketPath: "", ctx: stored}

	if got := e.itemCtx(caller).Value(ctxKeyProbe{}); got != "caller" {
		t.Errorf("itemCtx(caller) = %v, want the caller's context", got)
	}
	if got := itemCtxUntypedNil(e).Value(ctxKeyProbe{}); got != "stored" {
		t.Errorf("itemCtx(nil) = %v, want the stored fallback", got)
	}
	if itemCtxUntypedNil(e) == nil {
		t.Error("itemCtx must never return nil")
	}
	bare := s3BatchExecutor{bucket: "b"}
	if itemCtxUntypedNil(bare) == nil {
		t.Error("itemCtx with no stored ctx must still return Background, never nil")
	}
}

type ctxKeyProbe struct{}
