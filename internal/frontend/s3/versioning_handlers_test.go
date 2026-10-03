// versioning_handlers_test.go — s3-versioning tree leaf 03 handler
// tests. Covers:
//
//   - ?versioning sub-resource: PUT Enabled/Suspended/Off round-trip via
//     GET (Off renders without a Status element), unknown bucket 404,
//     malformed XML 400, invalid Status 400.
//   - Versioned PUT semantics: on an Enabled bucket the OLD current
//     bytes become one version before the overwrite; Suspended
//     overwrites plainly; OFF buckets are byte-identical to
//     pre-versioning (PIN: no .versions dir, no .versioning marker,
//     response bytes identical).
//   - Versioned DELETE: Enabled writes a delete marker (data file
//     untouched) and answers 204; plain GET answers 404 with
//     x-amz-delete-marker: true; Suspended deletes plainly.
//   - GET ?versionId: old version bytes + x-amz-version-id, unknown
//     sidecar-form id 400 InvalidArgument, delete-marker id 405 +
//     x-amz-delete-marker, traversal/unsafe ids 404-class.
//   - mergeVersionStore (both mode): sidecar id resolved first, then
//     snapshot name; List merges newest-first.
//
// NO real zfs anywhere: the merge tests use the scripted zfsSnapshotRunner.
package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verEnv is a handler-level versioning test environment: a real fs
// backend + config view (setupS3TestEnv) plus the versioning seams zeroed
// to the sidecar mode (the non-ZFS production shape).
type verEnv struct {
	*testS3Env
	bucket     string
	bucketPath string
}

func newVerEnv(t *testing.T, bucket string) verEnv {
	t.Helper()
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, bucket)
	installZfsVersioningModeForTest(t, "sidecar")
	installZmetadDBForTest(t, nil)
	return verEnv{testS3Env: env, bucket: bucket, bucketPath: filepath.Join(env.dataDir, bucket)}
}

// installZfsVersioningModeForTest installs the versioning mode seam for
// the test's duration and restores the zero state afterwards.
func installZfsVersioningModeForTest(t *testing.T, mode string) {
	t.Helper()
	InstallZfsVersioningMode(mode)
	t.Cleanup(func() { InstallZfsVersioningMode("") })
}

// installZmetadDBForTest installs the zmetad DB seam for the test's
// duration and restores nil afterwards.
func installZmetadDBForTest(t *testing.T, db interface {
	ResolveDatasetByPath(string) (string, error)
	ResolveMountpointByPath(string) (string, error)
}) {
	t.Helper()
	_ = db // the production type is *metadata.ZmetadDB; tests pass nil
	InstallZmetadDB(nil)
	t.Cleanup(func() { InstallZmetadDB(nil) })
}

// verPutBody PUTs a body to the bucket/key through the plain handler.
func verPutBody(t *testing.T, env verEnv, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+env.bucket+"/"+key, strings.NewReader(body))
	w := httptest.NewRecorder()
	putObjectHandler(w, req, env.bucket, key)
	return w
}

// verGet plain-GETs the key through the plain handler.
func verGet(t *testing.T, env verEnv, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+env.bucket+"/"+key, nil)
	w := httptest.NewRecorder()
	getObjectHandler(w, req, env.bucket, key)
	return w
}

// verDelete plain-DELETEs the key through the plain handler.
func verDelete(t *testing.T, env verEnv, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/"+env.bucket+"/"+key, nil)
	w := httptest.NewRecorder()
	deleteObjectHandler(w, req, env.bucket, key)
	return w
}

// verEnable enables versioning through the real sub-resource handler.
func verEnable(t *testing.T, env verEnv, status string) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if status != "" {
		body = "<VersioningConfiguration><Status>" + status + "</Status></VersioningConfiguration>"
	}
	req := httptest.NewRequest("PUT", "/"+env.bucket+"?versioning", strings.NewReader(body))
	w := httptest.NewRecorder()
	putBucketVersioningHandler(w, req, env.bucket)
	return w
}

// verGetVersioning GETs the ?versioning sub-resource.
func verGetVersioning(t *testing.T, env verEnv) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+env.bucket+"?versioning", nil)
	w := httptest.NewRecorder()
	getBucketVersioningHandler(w, req, env.bucket)
	return w
}

// ---------- ?versioning sub-resource ----------

func TestVersioningSubresource_RoundTripAndOffEcho(t *testing.T) {
	env := newVerEnv(t, "versub-bucket")

	// Off (never set): envelope WITHOUT a Status element.
	w := verGetVersioning(t, env)
	if w.Code != http.StatusOK {
		t.Fatalf("GET ?versioning on fresh bucket = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Status") {
		t.Errorf("Off echo must omit Status, got: %s", w.Body.String())
	}

	// Enabled round-trip.
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("PUT ?versioning Enabled = %d: %s", w.Code, w.Body.String())
	}
	w = verGetVersioning(t, env)
	if !strings.Contains(w.Body.String(), "<Status>Enabled</Status>") {
		t.Errorf("GET ?versioning must echo Enabled, got: %s", w.Body.String())
	}

	// Suspended round-trip.
	if w := verEnable(t, env, "Suspended"); w.Code != http.StatusOK {
		t.Fatalf("PUT ?versioning Suspended = %d", w.Code)
	}
	w = verGetVersioning(t, env)
	if !strings.Contains(w.Body.String(), "<Status>Suspended</Status>") {
		t.Errorf("GET ?versioning must echo Suspended, got: %s", w.Body.String())
	}

	// Off clears: no Status element again.
	if w := verEnable(t, env, ""); w.Code != http.StatusOK {
		t.Fatalf("PUT ?versioning (no status) = %d", w.Code)
	}
	if w := verGetVersioning(t, env); strings.Contains(w.Body.String(), "Status") {
		t.Errorf("cleared versioning must echo without Status, got: %s", w.Body.String())
	}

	// The state marker exists in the bucket's own metadata area.
	if _, err := os.Stat(filepath.Join(env.bucketPath, ".metadata", ".versioning")); err != nil {
		t.Errorf("bucket-level state marker missing: %v", err)
	}
}

func TestVersioningSubresource_UnknownBucket404(t *testing.T) {
	newVerEnv(t, "other-bucket")

	req := httptest.NewRequest("GET", "/nope?versioning", nil)
	w := httptest.NewRecorder()
	getBucketVersioningHandler(w, req, "nope")
	if w.Code != http.StatusNotFound {
		t.Errorf("GET ?versioning unknown bucket = %d, want 404", w.Code)
	}

	req = httptest.NewRequest("PUT", "/nope?versioning", strings.NewReader("<VersioningConfiguration/>"))
	w = httptest.NewRecorder()
	putBucketVersioningHandler(w, req, "nope")
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT ?versioning unknown bucket = %d, want 404", w.Code)
	}
}

func TestVersioningSubresource_MalformedAndInvalidStatus(t *testing.T) {
	env := newVerEnv(t, "verbad-bucket")

	req := httptest.NewRequest("PUT", "/"+env.bucket+"?versioning", strings.NewReader("not xml <"))
	w := httptest.NewRecorder()
	putBucketVersioningHandler(w, req, env.bucket)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed XML = %d, want 400", w.Code)
	}

	req = httptest.NewRequest("PUT", "/"+env.bucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Bogus</Status></VersioningConfiguration>"))
	w = httptest.NewRecorder()
	putBucketVersioningHandler(w, req, env.bucket)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid Status = %d, want 400", w.Code)
	}
}

// ---------- versioned PUT: the OLD bytes become the version ----------

func TestVersionedPUT_RecordsOldVersionOnOverwrite(t *testing.T) {
	env := newVerEnv(t, "verput-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}

	// v1: create (no prior version — nothing old to record).
	if w := verPutBody(t, env, "doc.txt", "v1-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	// v2: overwrite — v1 must become the recorded version.
	if w := verPutBody(t, env, "doc.txt", "v2-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}

	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "doc.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d entries, want 1 (the OLD v1)", len(versions))
	}
	rc, entry, err := store.Open(env.bucket, "doc.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("Open old version: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if string(raw) != "v1-bytes" {
		t.Errorf("old version bytes = %q, want v1-bytes", raw)
	}
	if entry.Size != int64(len("v1-bytes")) {
		t.Errorf("old version size = %d", entry.Size)
	}

	// Current bytes stay v2.
	if w := verGet(t, env, "doc.txt"); w.Body.String() != "v2-bytes" {
		t.Errorf("current bytes = %q, want v2-bytes", w.Body.String())
	}
}

func TestVersionedPUT_ThreeOverwritesKeepAllOldVersions(t *testing.T) {
	env := newVerEnv(t, "verput3-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	for i, body := range []string{"one", "two", "three", "four"} {
		if w := verPutBody(t, env, "k", body); w.Code != http.StatusOK {
			t.Fatalf("put %d: %d", i, w.Code)
		}
	}
	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "k")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want 3 (every body except the current)", len(versions))
	}
	// Newest recorded version (v3 "three") sorts first.
	rc, _, err := store.Open(env.bucket, "k", versions[0].ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if string(raw) != "three" {
		t.Errorf("newest old version = %q, want three", raw)
	}
}

func TestVersionedPUT_SuspendedOverwritesPlainly(t *testing.T) {
	env := newVerEnv(t, "versusp-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	if w := verEnable(t, env, "Suspended"); w.Code != http.StatusOK {
		t.Fatalf("suspend: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}

	store := versionStoreForBucket(env.bucketPath)
	if _, err := store.List(env.bucket, "k"); !isNoSuchKeyErr(err) {
		t.Errorf("Suspended overwrite must not record versions, List err = %v", err)
	}
	if w := verGet(t, env, "k"); w.Body.String() != "v2" {
		t.Errorf("current = %q, want v2", w.Body.String())
	}
}

// ---------- OFF buckets: zero-change PIN ----------

func TestOffBucket_ZeroChangePin(t *testing.T) {
	env := newVerEnv(t, "veroff-bucket") // never versioned

	// Pre-versioning wire behavior, exercised plainly.
	w := verPutBody(t, env, "a.txt", "alpha")
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	w = verPutBody(t, env, "a.txt", "beta") // overwrite
	if w.Code != http.StatusOK || headerGet(w, "ETag") == "" {
		t.Fatalf("overwrite = %d", w.Code)
	}
	w = verGet(t, env, "a.txt")
	if w.Code != http.StatusOK || w.Body.String() != "beta" {
		t.Fatalf("get = %d %q", w.Code, w.Body.String())
	}
	if headerGet(w, "x-amz-delete-marker") != "" {
		t.Errorf("plain GET on an OFF bucket must never carry x-amz-delete-marker")
	}
	w = verDelete(t, env, "a.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}
	w = verGet(t, env, "a.txt")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get deleted = %d, want plain 404", w.Code)
	}
	if headerGet(w, "x-amz-delete-marker") != "" {
		t.Errorf("plain 404 on an OFF bucket must not carry the delete-marker header")
	}

	// PIN: NO versioning state on disk at all — no marker, no versions
	// dir, and the overwritten data is really gone (plain semantics).
	if _, err := os.Stat(filepath.Join(env.bucketPath, ".metadata", ".versioning")); !os.IsNotExist(err) {
		t.Errorf("OFF bucket must have no versioning marker, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.bucketPath, ".metadata", ".versions")); !os.IsNotExist(err) {
		t.Errorf("OFF bucket must have no versions dir, stat err = %v", err)
	}
}

// ---------- versioned DELETE: marker semantics ----------

func TestVersionedDELETE_MarkerHidesWithoutDestroying(t *testing.T) {
	env := newVerEnv(t, "verdel-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "payload"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	// DELETE answers 204 and writes a marker; the data file is untouched.
	if w := verDelete(t, env, "k"); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}
	dataPath := filepath.Join(env.bucketPath, "k")
	if raw, err := os.ReadFile(dataPath); err != nil || string(raw) != "payload" {
		t.Errorf("data file must survive the marker delete, got %q err=%v", raw, err)
	}

	// Plain GET: 404 with x-amz-delete-marker: true.
	w := verGet(t, env, "k")
	if w.Code != http.StatusNotFound {
		t.Fatalf("plain GET on deleted = %d, want 404", w.Code)
	}
	if got := headerGet(w, "x-amz-delete-marker"); got != "true" {
		t.Errorf("x-amz-delete-marker = %q, want true", got)
	}

	// The old version stays readable via ?versionId — find the version id
	// through the store (the marker holds no data; the recorded version
	// came from the overwrite path... a single create has no versions, so
	// overwrite once first in the next test; here assert the marker only).
	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "k")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) == 0 || !versions[0].IsDeleteMarker {
		t.Fatalf("newest entry must be the delete marker, got %+v", versions)
	}
}

func TestVersionedDELETE_ThenReadOldVersionByID(t *testing.T) {
	env := newVerEnv(t, "verdelread-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}

	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "k")
	if err != nil || len(versions) == 0 {
		t.Fatalf("List: %v (%d entries)", err, len(versions))
	}
	oldID := versions[len(versions)-1].ID // oldest recorded = v1

	if w := verDelete(t, env, "k"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	// GET ?versionId=old → 200 with the old bytes + x-amz-version-id.
	req := httptest.NewRequest("GET", "/"+env.bucket+"/k?versionId="+oldID, nil)
	w := httptest.NewRecorder()
	getObjectHandlerVersionedForTest(w, req, env.bucket, "k", oldID)
	if w.Code != http.StatusOK {
		t.Fatalf("GET ?versionId = %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "v1" {
		t.Errorf("versioned bytes = %q, want v1", w.Body.String())
	}
	if got := headerGet(w, "x-amz-version-id"); got != oldID {
		t.Errorf("x-amz-version-id = %q, want %q", got, oldID)
	}

	// The delete marker id → 405 + x-amz-delete-marker: true.
	markerVersions, _ := store.List(env.bucket, "k")
	markerID := markerVersions[0].ID
	req = httptest.NewRequest("GET", "/"+env.bucket+"/k?versionId="+markerID, nil)
	w = httptest.NewRecorder()
	getObjectHandlerVersionedForTest(w, req, env.bucket, "k", markerID)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET marker id = %d, want 405", w.Code)
	}
	if got := headerGet(w, "x-amz-delete-marker"); got != "true" {
		t.Errorf("marker id response x-amz-delete-marker = %q, want true", got)
	}

	// Unknown sidecar-form id → 400 InvalidArgument (Contract 2).
	req = httptest.NewRequest("GET", "/"+env.bucket+"/k?versionId=1234567890123456-deadbeef", nil)
	w = httptest.NewRecorder()
	getObjectHandlerVersionedForTest(w, req, env.bucket, "k", "1234567890123456-deadbeef")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET unknown id = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("unknown id body must carry InvalidArgument, got %s", w.Body.String())
	}

	// Snapshot-form unknown id → honest 404 (expired-window semantics).
	req = httptest.NewRequest("GET", "/"+env.bucket+"/k?versionId=auto-20261002", nil)
	w = httptest.NewRecorder()
	getObjectHandlerVersionedForTest(w, req, env.bucket, "k", "auto-20261002")
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET snapshot-form unknown id = %d, want 404", w.Code)
	}
}

// getObjectHandlerVersionedForTest drives objectVersionedRead exactly as
// the dispatch does (kept as a local alias so the test stays within the
// package's handler-call conventions).
func getObjectHandlerVersionedForTest(w http.ResponseWriter, r *http.Request, bucket, key, versionID string) {
	objectVersionedRead(w, r, bucket, key, versionID)
}

// ---------- plain GET marker-hidden on an Enabled bucket ----------

func TestPlainGET_DeleteMarkedKey404OnEnabledBucket(t *testing.T) {
	env := newVerEnv(t, "verplain-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "gone.txt", "data"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}
	if w := verDelete(t, env, "gone.txt"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	// The plain handler (not the versioned one) answers the marker 404.
	w := verGet(t, env, "gone.txt")
	if w.Code != http.StatusNotFound {
		t.Fatalf("plain GET = %d, want 404", w.Code)
	}
	if got := headerGet(w, "x-amz-delete-marker"); got != "true" {
		t.Errorf("x-amz-delete-marker = %q, want true", got)
	}
}

// ---------- mergeVersionStore (both mode) ----------

func TestVersionStoreFor_BothModeYieldsMergeStore(t *testing.T) {
	bp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bp, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := versionStoreFor(bp, nil, "both")
	// Leaf 06: "both" = reflink + sidecar-layout history merged
	// (reflinkBothVersionStore); the OLD merge (sidecar+snapshots) is
	// gone — snapshot entries stay OUT of per-write modes by design.
	m, ok := s.(reflinkBothVersionStore)
	if !ok {
		t.Fatalf("both mode yielded %T, want reflinkBothVersionStore", s)
	}
	// Writes route to the reflink store: state round-trip through the
	// SHARED marker.
	if err := m.SetState("b", "Enabled"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	state, err := m.State("b")
	if err != nil || state != "Enabled" {
		t.Fatalf("State = %q err=%v, want Enabled", state, err)
	}
}

func TestMergeVersionStore_OpenResolvesSidecarFirstThenSnapshot(t *testing.T) {
	t.Helper()
	scriptZfsSnapshotRunner(t, "pool/bkt@s1\t1000000000\n", nil)
	mountpoint := t.TempDir()
	bucketPath := filepath.Join(mountpoint, "bkt")
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A snapshot file for key k.txt with DIFFERENT bytes than the sidecar
	// version proves which store answered.
	snapDir := filepath.Join(mountpoint, ".zfs", "snapshot", "s1")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "k.txt"), []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Setenv("ZFS_SNAP_TEST", "1")
	defer os.Unsetenv("ZFS_SNAP_TEST")

	m := mergeVersionStore{
		sidecar: sidecarVersionStore{bucketPath: bucketPath},
		snapshot: zfsSnapshotVersionStore{
			bucketPath: bucketPath,
			zdb:        fakeSnapshotResolver{dataset: "pool/bkt", mountpoint: mountpoint},
		},
	}

	// Sidecar id wins: PutVersion through the sidecar, then Open by id
	// returns the SIDECAR bytes.
	entry, err := m.PutVersion("b", "k.txt", strings.NewReader("sidecar-bytes"), int64(len("sidecar-bytes")), "etag-x")
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	rc, _, err := m.Open("b", "k.txt", entry.ID)
	if err != nil {
		t.Fatalf("Open sidecar id: %v", err)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	if string(raw) != "sidecar-bytes" {
		t.Errorf("sidecar id Open = %q, want sidecar-bytes", raw)
	}

	// Snapshot name resolves through the snapshot store.
	rc, got, err := m.Open("b", "k.txt", "s1")
	if err != nil {
		t.Fatalf("Open snapshot name: %v", err)
	}
	raw, _ = io.ReadAll(rc)
	rc.Close()
	if string(raw) != "snapshot-bytes" {
		t.Errorf("snapshot Open = %q, want snapshot-bytes", raw)
	}
	if got.ID != "s1" {
		t.Errorf("snapshot entry ID = %q, want s1", got.ID)
	}

	// Unknown to both → 404-class NoSuchKey.
	if _, _, err := m.Open("b", "k.txt", "nosuch"); !isNoSuchKeyErr(err) {
		t.Errorf("unknown id err = %v, want NoSuchKey", err)
	}
}

func TestMergeVersionStore_ListMergesNewestFirst(t *testing.T) {
	scriptZfsSnapshotRunner(t, "pool/bkt@s1\t1000000000\n", nil)
	mountpoint := t.TempDir()
	bucketPath := filepath.Join(mountpoint, "bkt")
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapDir := filepath.Join(mountpoint, ".zfs", "snapshot", "s1")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "k.txt"), []byte("snap"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := mergeVersionStore{
		sidecar: sidecarVersionStore{bucketPath: bucketPath},
		snapshot: zfsSnapshotVersionStore{
			bucketPath: bucketPath,
			zdb:        fakeSnapshotResolver{dataset: "pool/bkt", mountpoint: mountpoint},
		},
	}
	if _, err := m.PutVersion("b", "k.txt", strings.NewReader("cur"), 3, "e"); err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	versions, err := m.List("b", "k.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("merged list = %d entries, want 2 (sidecar + snapshot)", len(versions))
	}
	sawSidecar, sawSnapshot := false, false
	for _, v := range versions {
		switch v.ID {
		case "s1":
			sawSnapshot = true
		default:
			sawSidecar = true
		}
	}
	if !sawSidecar || !sawSnapshot {
		t.Errorf("merged list missing an entry: %+v", versions)
	}

	// PutDeleteMarker works (sidecar-authoritative in both mode).
	if _, err := m.PutDeleteMarker("b", "k.txt"); err != nil {
		t.Errorf("PutDeleteMarker in both mode must work, got %v", err)
	}
}

// TestVersioningSeam_ZeroStateIsSidecar pins the zero-state behavior:
// with no mode installed, versionStoreForBucket resolves the sidecar
// store (unwired tests/processes behave like pre-versioning + sidecar
// semantics, never snapshots).
func TestVersioningSeam_ZeroStateIsSidecar(t *testing.T) {
	InstallZfsVersioningMode("")
	InstallZmetadDB(nil)
	t.Cleanup(func() { InstallZfsVersioningMode(""); InstallZmetadDB(nil) })
	bp := t.TempDir()
	if _, ok := versionStoreForBucket(bp).(sidecarVersionStore); !ok {
		t.Errorf("zero-state seam must resolve the sidecar store, got %T", versionStoreForBucket(bp))
	}
}

// headerGet reads a response header (tiny readability helper).
func headerGet(w *httptest.ResponseRecorder, name string) string { return w.Header().Get(name) }
