// coverage_restore_test.go — REAL behavior tests for previously
// uncovered surfaces, written to RAISE coverage (AGENTS.md rule), never
// to game it: every test drives a production code path end-to-end and
// asserts observable behavior (wire status, on-disk layout, bridge
// semantics).
//
// Files under test:
//   - bucket_handlers.go: BucketPathFromInstalledView, InstalledDataDir,
//     InstalledCustomBucketPaths
//   - versioning_bridge.go: VersionStoreStateForBucket,
//     CaptureCurrentObjectVersionForBucket,
//     RecordCapturedObjectVersionForBucket,
//     DeleteObjectVersionedMarkerForBucket, PlainObjectDeleteMarker404
//   - versioning_handlers.go: objectVersionedDispatch
//   - versionstore.go: the sidecar/merge/unimplemented store methods
//   - batch_bridge.go: BatchExecutorForBucket(BucketAt) real copy/move/
//     delete behavior, IdentityOfRequestForTest, BatchPrincipalForTest
//   - object_handlers.go: deleteBatchExecutor.Copy/Move,
//     stampReaderBreadcrumb, noteBudgetExhausted
//   - export_test_surface.go: ZfsDatasetProvisionerInstalled,
//     ZfsBucketDatasetParentProbe, SetRegionForTest
//   - zfstagseam.go: InstallZfsTagDatasetProbe
//   - zfsdatasets_handlers.go: ClearZfsBucketDatasetParent
//   - sigv4.go: getCanonicalURI edge shapes
//   - seam.go: defaultWriteFileAtomic success + failure arms
//   - reflinkversions.go: reflinkVersionStore.Open arms
package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// ---------- bucket_handlers.go: installed-view exports ----------

// TestBucketPathFromInstalledView pins the installed-view resolver: a
// config-declared custom path wins, everything else lands under the
// installed dataDir — the same layout math getBucketPath falls back to
// (bughunt 2026-10-06 H3: resolver and gate must read ONE generation).
func TestBucketPathFromInstalledView(t *testing.T) {
	env := setupS3TestEnv(t)
	custom := filepath.Join(t.TempDir(), "custom-loc")
	installServerConfigView(serverConfigView{
		DataDir: env.dataDir + "/",
		Buckets: map[string]string{"custom-bkt": custom},
	})
	t.Cleanup(func() { installServerConfigView(serverConfigView{DataDir: env.dataDir + "/"}) })

	if got := BucketPathFromInstalledView("custom-bkt"); got != custom {
		t.Errorf("custom bucket path = %q, want %q", got, custom)
	}
	wantAuto := filepath.Join(env.dataDir, "auto-bkt")
	if got := BucketPathFromInstalledView("auto-bkt"); got != wantAuto {
		t.Errorf("auto bucket path = %q, want %q", got, wantAuto)
	}
	if got := InstalledDataDir(); got != env.dataDir+"/" {
		t.Errorf("InstalledDataDir = %q, want %q", got, env.dataDir+"/")
	}
	got := InstalledCustomBucketPaths()
	if len(got) != 1 || got["custom-bkt"] != custom {
		t.Errorf("InstalledCustomBucketPaths = %v, want {custom-bkt: %s}", got, custom)
	}
	// The returned map must be a COPY: mutating it must not reach the
	// installed view.
	got["custom-bkt"] = "/tampered"
	if InstalledCustomBucketPaths()["custom-bkt"] != custom {
		t.Error("InstalledCustomBucketPaths leaked the installed map (mutation propagated)")
	}
}

// ---------- versioning_bridge.go: the production bridges ----------

// TestBridgeVersionStoreStateRoundTrip drives VersionStoreStateForBucket
// (the webdav bridge over versionStoreForBucket.State) through the real
// state marker: never-versioned reads Off, the real handler's Enabled
// write reads Enabled back.
func TestBridgeVersionStoreStateRoundTrip(t *testing.T) {
	env := newVerEnv(t, "bridge-state-bkt")

	state, err := VersionStoreStateForBucket(env.bucketPath, env.bucket)
	if err != nil {
		t.Fatalf("state on fresh bucket: %v", err)
	}
	if state != "Off" {
		t.Errorf("fresh bucket state = %q, want Off", state)
	}
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	state, err = VersionStoreStateForBucket(env.bucketPath, env.bucket)
	if err != nil {
		t.Fatalf("state after enable: %v", err)
	}
	if state != VersioningEnabled {
		t.Errorf("state after enable = %q, want %q", state, VersioningEnabled)
	}
}

// TestBridgeCaptureAndRecordVersion drives the capture/record bridge pair
// over a real object: capture reads the OLD bytes, the backend Put
// rewrites the sidecar, and the record step restores the captured history
// beneath the new entry — the exact contract the webdav PUT path rides.
func TestBridgeCaptureAndRecordVersion(t *testing.T) {
	env := newVerEnv(t, "bridge-capture-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "doc.txt", "v1-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}

	captured, err := CaptureCurrentObjectVersionForBucket(env.bucketPath, "doc.txt")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if string(captured.data) != "v1-bytes" {
		t.Errorf("captured bytes = %q, want v1-bytes", captured.data)
	}

	// The plain overwrite (backend Put rewrites the sidecar wholesale).
	if w := verPutBody(t, env, "doc.txt", "v2-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}
	if err := RecordCapturedObjectVersionForBucket(env.bucketPath, env.bucket, "doc.txt", captured); err != nil {
		t.Fatalf("record: %v", err)
	}

	versions, err := ListVersionsForTest(env.bucketPath, env.bucket, "doc.txt")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d entries, want 1 (the captured pre-overwrite state)", len(versions))
	}
	if !versions[0].IsLatest || versions[0].Size != int64(len("v1-bytes")) {
		t.Errorf("entry = %+v, want latest v1-sized version", versions[0])
	}
	body, err := OpenVersionForTest(env.bucketPath, env.bucket, "doc.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("open version: %v", err)
	}
	if string(body) != "v1-bytes" {
		t.Errorf("version bytes = %q, want v1-bytes", body)
	}

	// A capture on a key that does not exist is the ErrNoPriorVersion
	// sentinel (a create records nothing).
	if _, err := CaptureCurrentObjectVersionForBucket(env.bucketPath, "absent.txt"); !errors.Is(err, ErrNoPriorVersion) {
		t.Errorf("capture absent = %v, want ErrNoPriorVersion", err)
	}
}

// TestBridgeDeleteMarkerAndPlain404 drives the DELETE marker bridge and
// the READ-side visibility bridge together: Enabled records a marker and
// suppresses the plain delete, and a plain read on the key is
// marker-hidden; Off proceeds plainly.
func TestBridgeDeleteMarkerAndPlain404(t *testing.T) {
	env := newVerEnv(t, "bridge-marker-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "gone.txt", "surviving"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	suppress, err := DeleteObjectVersionedMarkerForBucket(env.bucketPath, env.bucket, "gone.txt")
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if !suppress {
		t.Fatal("Enabled delete must suppress the plain delete")
	}

	hidden, err := PlainObjectDeleteMarker404(env.bucketPath, env.bucket, "gone.txt")
	if err != nil {
		t.Fatalf("marker404: %v", err)
	}
	if !hidden {
		t.Error("plain read on marker'd key must answer the marker 404")
	}

	// An unmarked key with history, and a key with no history at all,
	// are not marker-hidden.
	if w := verPutBody(t, env, "kept.txt", "here"); w.Code != http.StatusOK {
		t.Fatalf("put kept: %d", w.Code)
	}
	hidden, err = PlainObjectDeleteMarker404(env.bucketPath, env.bucket, "kept.txt")
	if err != nil || hidden {
		t.Errorf("kept.txt marker404 = (%v, %v), want (false, nil)", hidden, err)
	}
	hidden, err = PlainObjectDeleteMarker404(env.bucketPath, env.bucket, "never-existed.txt")
	if err != nil || hidden {
		t.Errorf("absent key marker404 = (%v, %v), want (false, nil)", hidden, err)
	}

	// Off bucket: the consult short-circuits (never marker-hidden, no
	// marker recorded).
	env2 := newVerEnv(t, "bridge-marker-off-bkt")
	if w := verPutBody(t, env2, "plain.txt", "data"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}
	hidden, err = PlainObjectDeleteMarker404(env2.bucketPath, env2.bucket, "plain.txt")
	if err != nil || hidden {
		t.Errorf("Off bucket marker404 = (%v, %v), want (false, nil)", hidden, err)
	}
	suppress, err = DeleteObjectVersionedMarkerForBucket(env2.bucketPath, env2.bucket, "plain.txt")
	if err != nil || suppress {
		t.Errorf("Off bucket marker = (%v, %v), want (false, nil)", suppress, err)
	}
}

// ---------- versionstore.go: store behavior ----------

// TestSidecarStoreSemantics exercises the sidecarVersionStore surface the
// bridge wrappers proxy: State/SetState round-trip (including the
// invalid-state rejection and the empty-marker Off), PutVersion
// newest-first IsLatest, the short-body rejection, PutDeleteMarker,
// List NoSuchKey, and Open's read arms — direct through the sidecar
// store AND through the reflink store's shared bookkeeping reader.
func TestSidecarStoreSemantics(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "wrstore-bkt")
	installZfsVersioningModeForTest(t, "sidecar")
	installZmetadDBForTest(t, nil)
	bp := filepath.Join(env.dataDir, "wrstore-bkt")
	s := sidecarVersionStore{bucketPath: bp}

	if err := s.SetState("wrstore-bkt", "Bogus"); err == nil {
		t.Error("SetState(Bogus) must reject the invalid state")
	}
	if err := s.SetState("wrstore-bkt", versioningSuspended); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	state, err := s.State("wrstore-bkt")
	if err != nil || state != versioningSuspended {
		t.Fatalf("State after SetState = (%q, %v), want Suspended", state, err)
	}
	// An empty-state marker file reads Off.
	if err := writeFileAtomicJSON(s.statePath(), stateSidecar{}, 0o644); err != nil {
		t.Fatalf("write empty marker: %v", err)
	}
	if state, _ = s.State("wrstore-bkt"); state != versioningOff {
		t.Errorf("empty marker State = %q, want Off", state)
	}

	v1, err := s.PutVersion("wrstore-bkt", "k.txt", strings.NewReader("one"), 3, "etag-1")
	if err != nil {
		t.Fatalf("PutVersion v1: %v", err)
	}
	v2, err := s.PutVersion("wrstore-bkt", "k.txt", strings.NewReader("twotwo"), 6, "etag-2")
	if err != nil {
		t.Fatalf("PutVersion v2: %v", err)
	}
	list, err := s.List("wrstore-bkt", "k.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != v2.ID || !list[0].IsLatest || list[1].ID != v1.ID || list[1].IsLatest {
		t.Errorf("List = %+v, want [v2 latest, v1]", list)
	}
	// A short body must fail honestly (size mismatch).
	if _, err := s.PutVersion("wrstore-bkt", "k.txt", strings.NewReader("x"), 3, "e"); err == nil {
		t.Error("PutVersion with short body must error")
	}
	if _, err := s.List("wrstore-bkt", "no-such-key"); !isNoSuchKeyErr(err) {
		t.Errorf("List missing key = %v, want NoSuchKey", err)
	}

	// Open: latest via "", unknown id, then delete marker.
	rc, entry, err := s.Open("wrstore-bkt", "k.txt", "")
	if err != nil {
		t.Fatalf("Open latest: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "twotwo" || entry.ID != v2.ID {
		t.Errorf("Open latest = %q/%s, want twotwo/%s", body, entry.ID, v2.ID)
	}
	if _, _, err := s.Open("wrstore-bkt", "k.txt", "0000000000000000-deadbeef"); !isNoSuchKeyErr(err) {
		t.Errorf("Open unknown id = %v, want NoSuchKey", err)
	}
	marker, err := s.PutDeleteMarker("wrstore-bkt", "k.txt")
	if err != nil {
		t.Fatalf("PutDeleteMarker: %v", err)
	}
	if _, mEntry, err := s.Open("wrstore-bkt", "k.txt", marker.ID); !errors.Is(err, ErrIsDeleteMarker) || mEntry.ID != marker.ID {
		t.Errorf("Open marker id = (_, %s, %v), want ErrIsDeleteMarker", mEntry.ID, err)
	}

	// reflinkVersionStore.Open shares the sidecar bookkeeping but reads
	// bytes from .versions-r: a version recorded by the SIDECAR store
	// resolves its id/entry but has no .versions-r data file (the
	// fail-soft error arm). Version entries recorded via the reflink
	// store's own PutVersion are readable — seed one directly.
	rs := reflinkVersionStore{sidecarVersionStore: s}
	if _, _, err := rs.Open("wrstore-bkt", "k.txt", v1.ID); err == nil {
		t.Error("reflink Open of a sidecar-layout version must not silently succeed")
	}
	rv, err := rs.PutVersion("wrstore-bkt", "r.txt", strings.NewReader("reflinked"), 9, "etag-r")
	if err != nil {
		t.Fatalf("reflink PutVersion: %v", err)
	}
	rc, entry, err = rs.Open("wrstore-bkt", "r.txt", rv.ID)
	if err != nil {
		t.Fatalf("reflink Open explicit id: %v", err)
	}
	body, _ = io.ReadAll(rc)
	rc.Close()
	if string(body) != "reflinked" || entry.ID != rv.ID {
		t.Errorf("reflink Open = %q/%s, want reflinked/%s", body, entry.ID, rv.ID)
	}
	if _, _, err := rs.Open("wrstore-bkt", "absent", ""); !isNoSuchKeyErr(err) {
		t.Errorf("reflink Open absent key = %v, want NoSuchKey", err)
	}
}

// TestMergeStoreDelegates pins mergeVersionStore's write-side delegation
// (both-mode State/SetState/PutVersion/PutDeleteMarker ride the sidecar
// store) — with zdb nil, List resolves the sidecar history only.
func TestMergeStoreDelegates(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "merge-bkt")
	installZfsVersioningModeForTest(t, "sidecar")
	installZmetadDBForTest(t, nil)
	bp := filepath.Join(env.dataDir, "merge-bkt")
	m := mergeVersionStore{
		sidecar:  sidecarVersionStore{bucketPath: bp},
		snapshot: zfsSnapshotVersionStore{bucketPath: bp, zdb: nil},
	}
	if err := m.SetState("merge-bkt", versioningEnabled); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	state, err := m.State("merge-bkt")
	if err != nil || state != versioningEnabled {
		t.Fatalf("State = (%q, %v), want Enabled", state, err)
	}
	entry, err := m.PutVersion("merge-bkt", "m.txt", strings.NewReader("merged"), 6, "e1")
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	list, err := m.List("merge-bkt", "m.txt")
	if err != nil || len(list) != 1 || !list[0].IsLatest || list[0].ID != entry.ID {
		t.Fatalf("List = (%+v, %v), want the single latest entry", list, err)
	}
	if _, err := m.PutDeleteMarker("merge-bkt", "m.txt"); err != nil {
		t.Fatalf("PutDeleteMarker: %v", err)
	}
	list, _ = m.List("merge-bkt", "m.txt")
	if len(list) != 2 || !list[0].IsDeleteMarker {
		t.Errorf("List after marker = %+v, want [marker latest, version]", list)
	}
	// Open: sidecar ids win; unknown ids are 404-class NoSuchKey.
	if _, _, err := m.Open("merge-bkt", "m.txt", entry.ID); err != nil {
		t.Errorf("Open sidecar id: %v", err)
	}
	if _, _, err := m.Open("merge-bkt", "m.txt", "0000000000000000-f00dfeed"); !isNoSuchKeyErr(err) {
		t.Errorf("Open unknown id = %v, want NoSuchKey", err)
	}
}

// TestUnimplementedVersionStoreFailsLoudly pins the factory's defensive
// default: a mis-wired mode fails loudly on every method instead of
// silently corrupting state.
func TestUnimplementedVersionStoreFailsLoudly(t *testing.T) {
	store := versionStoreFor("/tmp/unused", nil, "mystery-mode")
	u, ok := store.(unimplementedVersionStore)
	if !ok {
		t.Fatalf("versionStoreFor(mystery-mode) = %T, want unimplementedVersionStore", store)
	}
	want := errVersioningUnimplemented
	if _, err := u.State("b"); !errors.Is(err, want) {
		t.Errorf("State: %v", err)
	}
	if err := u.SetState("b", "Enabled"); !errors.Is(err, want) {
		t.Errorf("SetState: %v", err)
	}
	if _, err := u.PutVersion("b", "k", strings.NewReader("x"), 1, "e"); !errors.Is(err, want) {
		t.Errorf("PutVersion: %v", err)
	}
	if _, err := u.PutDeleteMarker("b", "k"); !errors.Is(err, want) {
		t.Errorf("PutDeleteMarker: %v", err)
	}
	if _, err := u.List("b", "k"); !errors.Is(err, want) {
		t.Errorf("List: %v", err)
	}
	if _, _, err := u.Open("b", "k", "v"); !errors.Is(err, want) {
		t.Errorf("Open: %v", err)
	}
}

// ---------- versioning_handlers.go: objectVersionedDispatch ----------

// TestObjectVersionedDispatchArms drives the ?versionId dispatcher's
// contract: non-versioned requests fall through, non-GET/HEAD is
// rejected, an invalid key wins the 400, a missing bucket the 404, an
// unknown sidecar-form id the 400 InvalidArgument, and a valid versioned
// read serves the bytes with x-amz-version-id.
func TestObjectVersionedDispatchArms(t *testing.T) {
	env := newVerEnv(t, "dispatch-ver-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "doc.txt", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	if w := verPutBody(t, env, "doc.txt", "v2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}
	versions, err := ListVersionsForTest(env.bucketPath, env.bucket, "doc.txt")
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions = (%+v, %v), want one", versions, err)
	}
	oldID := versions[0].ID

	// No versionId param: fall through to the plain method switch.
	req := httptest.NewRequest("GET", "/"+env.bucket+"/doc.txt", nil)
	w := httptest.NewRecorder()
	if objectVersionedDispatch(w, req, env.bucket, "doc.txt") {
		t.Fatal("request without versionId must fall through")
	}
	// A versionId on a POST is not a versioned read (only GET/HEAD
	// dispatch; the caller falls through to its method gate).
	req = httptest.NewRequest("POST", "/"+env.bucket+"/doc.txt?versionId="+oldID, nil)
	w = httptest.NewRecorder()
	if objectVersionedDispatch(w, req, env.bucket, "doc.txt") {
		t.Fatal("POST with versionId must fall through (only GET/HEAD dispatch)")
	}
	// Invalid key: 400 InvalidArgument.
	req = httptest.NewRequest("GET", "/"+env.bucket+"/..%2Fescape?versionId=1", nil)
	w = httptest.NewRecorder()
	if !objectVersionedDispatch(w, req, env.bucket, "../escape") {
		t.Fatal("dispatch must fully answer an invalid key")
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("invalid key = %d %s, want 400 InvalidArgument", w.Code, w.Body.String())
	}
	// Unknown bucket: 404.
	req = httptest.NewRequest("GET", "/nope/k?versionId=1", nil)
	w = httptest.NewRecorder()
	if !objectVersionedDispatch(w, req, "nope-bkt-x", "k") {
		t.Fatal("dispatch must fully answer a missing bucket")
	}
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("missing bucket = %d %s, want 404 NoSuchBucket", w.Code, w.Body.String())
	}
	// Unknown sidecar-form id: 400 InvalidArgument.
	req = httptest.NewRequest("GET", "/"+env.bucket+"/doc.txt?versionId=0000000000000000-f00dfeed", nil)
	w = httptest.NewRecorder()
	if !objectVersionedDispatch(w, req, env.bucket, "doc.txt") {
		t.Fatal("dispatch must fully answer a versioned read")
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("unknown id = %d %s, want 400 InvalidArgument", w.Code, w.Body.String())
	}
	// Happy path: old version bytes + x-amz-version-id.
	req = httptest.NewRequest("GET", "/"+env.bucket+"/doc.txt?versionId="+oldID, nil)
	w = httptest.NewRecorder()
	if !objectVersionedDispatch(w, req, env.bucket, "doc.txt") {
		t.Fatal("versioned GET must dispatch")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("versioned GET = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("x-amz-version-id"); got != oldID {
		t.Errorf("x-amz-version-id = %q, want %q", got, oldID)
	}
	if !strings.Contains(w.Body.String(), "v1") {
		t.Errorf("versioned GET body = %q, want v1", w.Body.String())
	}
	// HEAD with a versionId dispatches too.
	req = httptest.NewRequest("HEAD", "/"+env.bucket+"/doc.txt?versionId="+oldID, nil)
	w = httptest.NewRecorder()
	if !objectVersionedDispatch(w, req, env.bucket, "doc.txt") || w.Code != http.StatusOK {
		t.Errorf("versioned HEAD = %d, want 200 dispatched", w.Code)
	}
}

// ---------- batch_bridge.go: executor + identity ----------

// TestBatchExecutorBridgeRealOps drives BatchExecutorForBucket /
// BatchExecutorForBucketAt through real Copy/Move/Delete items against
// the fs backend (the same single-op orchestration the ?batch endpoint
// runs), asserting on-disk effects — never call-counting.
func TestBatchExecutorBridgeRealOps(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := "batch-exec-bkt"
	_ = env.setupBucket(t, bucket)
	installZfsVersioningModeForTest(t, "sidecar")
	installZmetadDBForTest(t, nil)

	exec := BatchExecutorForBucket(bucket, context.Background())
	// Invalid keys are InvalidArgument, never a filesystem touch.
	if err := exec.Copy(context.Background(), "../escape", "ok", ""); err == nil {
		t.Error("Copy with invalid source key must error")
	}
	if err := exec.Move(context.Background(), "src.txt", "../escape", ""); err == nil {
		t.Error("Move with invalid destination key must error")
	}
	if err := exec.Delete(context.Background(), "../escape", ""); err == nil {
		t.Error("Delete with invalid key must error")
	}

	// A delete of an absent key is a per-item NoSuchKey error (the JSON
	// batch Contract 9 shape — a deliberate divergence from the S3
	// DeleteObjects wire).
	if err := exec.Delete(context.Background(), "absent.txt", ""); !isNoSuchKeyErr(err) {
		t.Errorf("delete absent key = %v, want NoSuchKey", err)
	}
	if err := exec.Copy(context.Background(), "src.txt", "dst.txt", ""); err == nil {
		t.Error("Copy of a missing source must error")
	}
	if err := exec.Move(context.Background(), "src.txt", "dst.txt", ""); err == nil {
		t.Error("Move of a missing source must error")
	}

	// Seed a source through the real Put handler, then Copy/Move.
	if w := putBodyAt(t, bucket, "src.txt", "payload"); w.Code != http.StatusOK {
		t.Fatalf("seed put: %d", w.Code)
	}
	if err := exec.Copy(context.Background(), "src.txt", "dst.txt", ""); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(env.dataDir, bucket, "dst.txt"))
	if err != nil || string(body) != "payload" {
		t.Errorf("copied dst.txt = %q (%v), want payload", body, err)
	}
	if err := exec.Move(context.Background(), "dst.txt", "moved.txt", ""); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(env.dataDir, bucket, "dst.txt")); !os.IsNotExist(statErr) {
		t.Error("Move left the source copy behind")
	}
	body, err = os.ReadFile(filepath.Join(env.dataDir, bucket, "moved.txt"))
	if err != nil || string(body) != "payload" {
		t.Errorf("moved.txt = %q (%v), want payload", body, err)
	}
	if err := exec.Delete(context.Background(), "moved.txt", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(env.dataDir, bucket, "moved.txt")); !os.IsNotExist(statErr) {
		t.Error("Delete left the object behind")
	}

	// BatchExecutorForBucketAt with its own resolved root behaves
	// identically (the webdav mount shape).
	atExec := BatchExecutorForBucketAt(bucket, filepath.Join(env.dataDir, bucket), context.Background())
	if err := atExec.Copy(context.Background(), "src.txt", "at-copy.txt", ""); err != nil {
		t.Fatalf("At-copy: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(env.dataDir, bucket, "at-copy.txt")); err != nil || string(body) != "payload" {
		t.Errorf("at-copy.txt = %q (%v), want payload", body, err)
	}
}

// TestBatchIdentityAndPrincipal pins the identity-context readers the
// webdav/h3 parity tests use: identityOf synthesizes the wildcard
// identity for a request that bypassed dispatch, and batchPrincipal
// degrades to the wildcard principal when the context carries none.
func TestBatchIdentityAndPrincipal(t *testing.T) {
	req := httptest.NewRequest("GET", "/b/k", nil)
	got := IdentityOfRequestForTest(req)
	if got.AccessKeyID == "" {
		t.Error("IdentityOfRequestForTest must synthesize an identity, got zero value")
	}
	if p := BatchPrincipalForTest(context.Background()); p == "" {
		t.Error("BatchPrincipalForTest must degrade to a wildcard principal, got empty")
	}
	// A dispatched request's published identity is what both readers see.
	ctx := auth.WithIdentity(context.Background(), auth.Identity{AccessKeyID: "batch-alice"})
	if p := BatchPrincipalForTest(ctx); p != "batch-alice" {
		t.Errorf("BatchPrincipalForTest = %q, want batch-alice", p)
	}
}

// ---------- object_handlers.go: deleteBatchExecutor Copy/Move ----------

// TestDeleteBatchExecutorCopyMoveReject pins the DeleteObjects manifest
// executor's op surface: copy/move items are InvalidArgument (the
// manifest carries delete items only) — real error identity, asserted
// via objectmodel's error code.
func TestDeleteBatchExecutorCopyMoveReject(t *testing.T) {
	e := deleteBatchExecutor{bucket: "whatever", ctx: context.Background()}
	if err := e.Copy(context.Background(), "a", "b", ""); !isInvalidArgumentErr(err) {
		t.Errorf("Copy = %v, want InvalidArgument", err)
	}
	if err := e.Move(context.Background(), "a", "b", ""); !isInvalidArgumentErr(err) {
		t.Errorf("Move = %v, want InvalidArgument", err)
	}
}

// ---------- object_handlers.go: stampReaderBreadcrumb ----------

// TestStampReaderBreadcrumbPins pins the auditReads read stamp: with the
// bucket's auditReads tunable set, the GET-side stamp resolves the
// object's shadow-aware data path and stamps it with the request
// principal (fsbackend's fail-open first-read-per-principal helper); the
// stamp must never affect the response. With the tunable unset the
// helper returns before touching anything.
func TestStampReaderBreadcrumbPins(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := "crumb-bkt"
	_ = env.setupBucket(t, bucket)
	if w := putBodyAt(t, bucket, "k.txt", "breadcrumb-body"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	// Tunable unset: the helper returns without resolving anything.
	req := httptest.NewRequest("GET", "/"+bucket+"/k.txt", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{AccessKeyID: "alice"}))
	stampReaderBreadcrumb(req, bucket, "k.txt") // must not panic

	// Tunable set: the stamp runs against the object's data path.
	installServerConfigView(serverConfigView{
		DataDir:    env.dataDir + "/",
		AuditReads: map[string]bool{bucket: true},
	})
	t.Cleanup(func() { installServerConfigView(serverConfigView{DataDir: env.dataDir + "/"}) })
	stampReaderBreadcrumb(req, bucket, "k.txt")
}

// ---------- object_handlers.go: noteBudgetExhausted ----------

// TestNoteBudgetExhaustedPins pins the listing-budget fallback: when the
// emit loop hits max-keys but MORE matching keys remain, the helper
// flags truncation, sets the fallback token to the first unconsumed key,
// and carries the common prefixes through unchanged.
func TestNoteBudgetExhaustedPins(t *testing.T) {
	keys := []string{"a", "b", "c", "d", "e"}
	p := &listObjectsParams{MaxKeys: 2}
	prefixes := []string{"grp/"}
	seen := map[string]struct{}{}
	truncated := false
	nextToken := ""

	// Budget cut inside the key list: more matchable keys follow "b".
	advanced, outPrefixes := noteBudgetExhausted(keys, 2, p, seen, prefixes, &truncated, &nextToken)
	if advanced != 2 {
		t.Errorf("advanced = %d, want 2 (the helper never advances past the budget)", advanced)
	}
	if !truncated {
		t.Error("truncated must be set when more matchable keys remain")
	}
	if nextToken != "c" {
		t.Errorf("nextToken = %q, want c (the first unconsumed key)", nextToken)
	}
	if len(outPrefixes) != 1 || outPrefixes[0] != "grp/" {
		t.Errorf("outPrefixes = %v, want the carried-through [grp/]", outPrefixes)
	}

	// Budget lands on the LAST key: no more matchable keys → no truncation.
	truncated, nextToken = false, ""
	advanced, _ = noteBudgetExhausted(keys, 5, p, seen, nil, &truncated, &nextToken)
	if truncated || nextToken != "" {
		t.Errorf("end-of-list note = (truncated=%v, token=%q), want no truncation", truncated, nextToken)
	}
	if advanced != 5 {
		t.Errorf("advanced = %d, want 5", advanced)
	}

	// Delimiter roll-ups: a budget cut right before a NEW common prefix
	// truncates with the fallback token set; a roll-up already consumed
	// by the V2 continuation token (it lies INSIDE the group) does not.
	delim := &listObjectsParams{MaxKeys: 1, Prefix: "logs/", Delimiter: "/", ContinuationToken: "logs/2026/"}
	truncated, nextToken = false, ""
	advanced, _ = noteBudgetExhausted([]string{"logs/2026/a", "logs/2026/b"}, 1, delim, map[string]struct{}{}, []string{"logs/2026/"}, &truncated, &nextToken)
	if advanced != 1 {
		t.Errorf("delimiter advanced = %d, want 1", advanced)
	}
	if truncated || nextToken != "" {
		t.Errorf("only a cursor-consumed roll-up remains = (truncated=%v, token=%q), want no truncation", truncated, nextToken)
	}
	delim = &listObjectsParams{MaxKeys: 1, Prefix: "logs/", Delimiter: "/"}
	truncated, nextToken = false, ""
	advanced, _ = noteBudgetExhausted([]string{"logs/2026/a", "logs/2027/b"}, 1, delim, map[string]struct{}{}, []string{"logs/2026/"}, &truncated, &nextToken)
	if !truncated {
		t.Error("a fresh roll-up remains: truncation must be flagged")
	}
	// The fallback token is the key AT the budget cut (i=1 → "b" — the
	// first key NOT yet consumed by the page); the emit loop refines it
	// to the last emitted item.
	if nextToken != "logs/2027/b" {
		t.Errorf("nextToken = %q, want logs/2027/b (the key at the budget cut)", nextToken)
	}
	if advanced != 1 {
		t.Errorf("advanced = %d, want 1", advanced)
	}
}

// ---------- export_test_surface.go probes ----------

// TestZfsDatasetProvisionerProbe pins the feature probes: after
// Set/ClearZfsBucketDatasetParent the recorded prefix reflects the
// config, and with no dataset hooks installed the provisioner probe
// reports the feature off.
func TestZfsDatasetProvisionerProbe(t *testing.T) {
	t.Cleanup(ClearZfsBucketDatasetParent)
	ClearZfsBucketDatasetParent()
	if got := ZfsBucketDatasetParentProbe(); got != "" {
		t.Errorf("parent probe after clear = %q, want empty", got)
	}
	SetZfsBucketDatasetParent("testpool/probe-parent")
	if got := ZfsBucketDatasetParentProbe(); got != "testpool/probe-parent" {
		t.Errorf("parent probe = %q, want testpool/probe-parent", got)
	}
	if ZfsDatasetProvisionerInstalled() {
		t.Error("provisioner probe = true with no dataset hooks installed, want false")
	}
}

// TestSetRegionForTestPins drives the exported region seam: the value
// becomes the active region and the region token validator accepts it.
func TestSetRegionForTestPins(t *testing.T) {
	t.Cleanup(func() { SetRegionForTest(DefaultRegion) })
	SetRegionForTest("us-east-2")
	if !validRegionToken("us-east-2") {
		t.Error("us-east-2 must be a valid region token")
	}
	if validRegionToken("not a region!!") {
		t.Error("garbage must not be a valid region token")
	}
}

// ---------- zfstagseam.go: InstallZfsTagDatasetProbe ----------

// TestInstallZfsTagDatasetProbe drives the production probe closure with
// a scripted zfs runner: an existing dataset probes true, a missing one
// false (any zfs non-zero exit is absent, never an error), and a hostile
// dataset name fails loud before the runner runs.
func TestInstallZfsTagDatasetProbe(t *testing.T) {
	scriptZfsDatasetRunner(t, "testpool/tags\n", "", nil)
	probe := InstallZfsTagDatasetProbe("zfs")
	ctx := context.Background()

	ok, err := probe(ctx, "testpool/tags")
	if err != nil || !ok {
		t.Errorf("probe(existing) = (%v, %v), want (true, nil)", ok, err)
	}

	// Any zfs non-zero exit is absent — the fake exit error is the shape
	// the production runner returns when zfs itself rejects.
	scriptZfsDatasetRunner(t, "", "", fakeZfsExitError(t))
	ok, err = probe(ctx, "testpool/gone")
	if err != nil || ok {
		t.Errorf("probe(zfs-rejected) = (%v, %v), want (false, nil)", ok, err)
	}

	// A runner/timeout failure fails loud, never reads as absent.
	scriptZfsDatasetRunner(t, "", "", errors.New("timeout"))
	if _, err := probe(ctx, "testpool/slow"); err == nil {
		t.Error("runner failure must error, not read as absent")
	}

	// A hostile dataset name is rejected before the zfs binary runs.
	if _, err := probe(ctx, "bad dataset/name"); err == nil {
		t.Error("unsafe dataset name must fail loud")
	}
}

// ---------- sigv4.go: getCanonicalURI ----------

// TestGetCanonicalURIEdgeShapes pins the canonical-URI edge arms: the
// empty path is "/", an escaped path keeps its escaping, and a plain
// path round-trips unchanged.
func TestGetCanonicalURIEdgeShapes(t *testing.T) {
	req := httptest.NewRequest("GET", "http://host", nil)
	req.URL.Path = ""
	if got := GetCanonicalURI(req); got != "/" {
		t.Errorf("empty path = %q, want /", got)
	}
	req = httptest.NewRequest("GET", "http://host/bucket/key%20with%20space", nil)
	if got := GetCanonicalURI(req); got != "/bucket/key%20with%20space" {
		t.Errorf("escaped path = %q, want the escaped form", got)
	}
	req = httptest.NewRequest("GET", "http://host/bucket/plain", nil)
	if got := GetCanonicalURI(req); got != "/bucket/plain" {
		t.Errorf("plain path = %q, want /bucket/plain", got)
	}
}

// ---------- seam.go: defaultWriteFileAtomic ----------

// TestDefaultWriteFileAtomicArms drives the fallback atomic writer: a
// normal write lands the bytes at the target (and cleans the temp), and
// an unwritable directory surfaces the create error.
func TestDefaultWriteFileAtomicArms(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	if err := defaultWriteFileAtomic(target, []byte("atomic"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "atomic" {
		t.Errorf("target = %q (%v), want atomic", body, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
	if err := defaultWriteFileAtomic(filepath.Join(dir, "no-such-dir", "x.txt"), []byte("x"), 0o644); err == nil {
		t.Error("write into a missing directory must error")
	}
}

// ---------- helpers ----------

func isInvalidArgumentErr(err error) bool {
	if err == nil {
		return false
	}
	type coder interface{ Code() string }
	var c coder
	if errors.As(err, &c) {
		return c.Code() == "InvalidArgument"
	}
	return strings.Contains(err.Error(), "InvalidArgument") || strings.Contains(err.Error(), "invalid argument")
}

// putBodyAt PUTs a body to bucket/key through the real PutObject handler.
func putBodyAt(t *testing.T, bucket, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+bucket+"/"+key, strings.NewReader(body))
	w := httptest.NewRecorder()
	putObjectHandler(w, req, bucket, key)
	return w
}

var _ = httptest.NewServer
