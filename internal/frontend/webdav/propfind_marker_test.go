// propfind_marker_test.go — the LISTING half of the delete-marker visibility
// rule: a Depth-1 PROPFIND must not advertise a key a versioning-Enabled
// DELETE already reported as gone, and a Depth-0 PROPFIND of that key must
// answer the same 404 a GET answers.
//
// The write side records a delete marker and SUPPRESSES the plain delete
// (delete.go → deleteMarkerOrPlain), so the data file survives on disk while
// the object is gone from every plain view. get.go consulted the marker for
// GET/HEAD only; propfindEntries and childEntries built their rows from
// resolveKind → statFile → be.Stat, which walks the backing filesystem and
// therefore still SAW the surviving file. A WebDAV client reconciling from
// that listing re-downloads a key the server reports deleted — the exact
// inverse of the GET behavior, on the same store state.
//
// PRODUCTION WIRING (this file's whole point for pins): the frontend is
// built with WithLockStoreRoot only — exactly what frontends.go passes
// (getBucketPath) — and the env ASSERTS bucketPathFn == nil, because
// WithBucketPathResolver is a test-only seam production never sets. A pin
// built on the test-only option would keep passing even if the production
// wiring regressed to a dead branch, which is precisely what bughunt H1 was.
//
// FAIL-OPEN is the load-bearing arm, not an incidental one: a marker-read
// error (here a corrupt bucket state marker, indistinguishable from EIO at
// this seam) must NOT turn a readable object into a silently-absent row.
// Hiding a real object during a transient fault is strictly worse than the
// bug being fixed, so the consult proceeds on error — s3 parity.
package webdav

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// propfindMarkerEnv is a PROPFIND marker-visibility environment: a REAL fs
// backend on a temp dataDir with the s3 process seams installed against it,
// and a webdav frontend wired the way PRODUCTION wires it.
type propfindMarkerEnv struct {
	t          *testing.T
	dataDir    string
	bucket     string
	bucketPath string
	f          *Frontend
}

// newPropfindMarkerEnv builds the env with the production option set only:
// WithAuthenticator (frontends.go) + WithLockStoreRoot (frontends.go passes
// getBucketPath there). WithBucketPathResolver is deliberately NOT passed,
// and bucketPathFn is asserted nil — see the file comment.
func newPropfindMarkerEnv(t *testing.T, bucket string) *propfindMarkerEnv {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	s3.InstallZfsVersioningMode("sidecar")
	t.Cleanup(func() { s3.InstallZfsVersioningMode("") })

	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}

	f, err := New(s3.TestBackend(), Config{Bucket: bucket}, WithAuthenticator(newStubAuth()),
		WithLockStoreRoot(func(b string) string { return filepath.Join(dataDir, b) }))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	// The pins below are only meaningful on the production wiring: the
	// resolver that reaches the version store must come from lockRoot, and
	// the test-only bucketPathFn override must stay unset.
	if f.bucketPathFn != nil {
		t.Fatal("bucketPathFn is set: this env must exercise PRODUCTION wiring (WithLockStoreRoot only), " +
			"WithBucketPathResolver is a test-only seam production never sets")
	}
	if got := f.bucketPath(bucket); got != bucketPath {
		t.Fatalf("bucketPath(%q) = %q, want %q (lockRoot must resolve for the marker consult)", bucket, got, bucketPath)
	}
	return &propfindMarkerEnv{t: t, dataDir: dataDir, bucket: bucket, bucketPath: bucketPath, f: f}
}

// enable turns versioning on through the REAL s3 sub-resource handler (the
// state marker the store reads is written by the production path).
func (e *propfindMarkerEnv) enable() {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+e.bucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
	w := httptest.NewRecorder()
	s3.PutBucketVersioningHandlerForTest(w, req, e.bucket)
	if w.Code != http.StatusOK {
		e.t.Fatalf("enable versioning: status = %d, want 200", w.Code)
	}
}

// davWrite PUTs a key through the webdav handler (201 create / 204 overwrite).
func (e *propfindMarkerEnv) davWrite(key, body string) {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+key, strings.NewReader(body))
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated && w.Code != http.StatusNoContent {
		e.t.Fatalf("webdav PUT %s: status = %d, want 201/204", key, w.Code)
	}
}

// webdavDelete DELETEs through the handler and returns the status — the full
// production write path (marker recorded, plain delete suppressed).
func (e *propfindMarkerEnv) webdavDelete(key string) int {
	e.t.Helper()
	req := httptest.NewRequest("DELETE", "/"+key, nil)
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	return w.Code
}

// davPropfind PROPFINDs path at the given depth and returns status + body.
func (e *propfindMarkerEnv) davPropfind(path, depth string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest("PROPFIND", path, nil)
	req.Header.Set("Depth", depth)
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// hrefPresent reports whether the 207 body advertises the given href row.
func hrefPresent(body, href string) bool {
	return strings.Contains(body, "<d:href>"+href+"</d:href>")
}

// corruptStateMarker replaces the bucket-level versioning state marker with
// bytes that cannot parse. At the read-side consult this is the
// state-read-failure arm, and it is INDISTINGUISHABLE from a transient EIO:
// the store cannot say what the bucket's visibility state is.
func corruptStateMarker(t *testing.T, bucketPath string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bucketPath, ".metadata", ".versioning"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupting the versioning state marker: %v", err)
	}
}

// TestPROPFIND_Depth1OmitsDeleteMarkedKey is pin (a): the listing a client
// reconciles from must not offer a key the server reports deleted. Before the
// fix the Depth-1 body carried gone.txt while GET /gone.txt answered 404, so
// a sync client re-downloaded a deleted object.
func TestPROPFIND_Depth1OmitsDeleteMarkedKey(t *testing.T) {
	e := newPropfindMarkerEnv(t, "propfind-marker-list")
	e.enable()
	e.davWrite("gone.txt", "secret-payload")
	e.davWrite("kept.txt", "live-payload")

	if code := e.webdavDelete("gone.txt"); code != http.StatusNoContent {
		t.Fatalf("DELETE gone.txt = %d, want 204", code)
	}
	// Precondition: the data file really does survive the marker delete —
	// that survival is the only reason the listing consult is load-bearing.
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "gone.txt")); err != nil || string(raw) != "secret-payload" {
		t.Fatalf("data file = %q err=%v, want the surviving secret-payload", raw, err)
	}
	// And the GET side already hid it (the rule PROPFIND now shares).
	getRec := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(getRec, httptest.NewRequest("GET", "/gone.txt", nil))
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("GET gone.txt = %d, want 404 (fixture sanity: the read-side consult)", getRec.Code)
	}

	code, body := e.davPropfind("/", "1")
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / Depth 1 = %d, want 207; body=%.400s", code, body)
	}
	if hrefPresent(body, "/gone.txt") {
		t.Fatalf("Depth-1 listing advertises a delete-marked key:\n%s", body)
	}
	if strings.Contains(body, "secret-payload") {
		t.Fatalf("Depth-1 listing leaked the deleted object's bytes:\n%s", body)
	}
}

// TestPROPFIND_Depth0OnMarkedKeyAnswers404 is pin (b): the single-resource
// view must agree with GET — a 404, not a 207 row describing a key the
// server elsewhere reports as deleted.
func TestPROPFIND_Depth0OnMarkedKeyAnswers404(t *testing.T) {
	e := newPropfindMarkerEnv(t, "propfind-marker-depth0")
	e.enable()
	e.davWrite("gone.txt", "secret-payload")
	e.webdavDelete("gone.txt")

	code, body := e.davPropfind("/gone.txt", "0")
	if code != http.StatusNotFound {
		t.Fatalf("PROPFIND Depth 0 on a delete-marked key = %d, want 404 (GET parity); body=%.400s", code, body)
	}
	if strings.Contains(body, "secret-payload") {
		t.Fatalf("the 404 body leaked the deleted object's bytes: %s", body)
	}
}

// TestPROPFIND_NormalKeyStillListed is pin (c): the no-false-positive half.
// The consult must not become a blanket hide — an ordinary key on the same
// versioned bucket, under the same resolver, still gets its row.
func TestPROPFIND_NormalKeyStillListed(t *testing.T) {
	e := newPropfindMarkerEnv(t, "propfind-marker-positive")
	e.enable()
	e.davWrite("kept.txt", "live-payload")

	code, body := e.davPropfind("/", "1")
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / Depth 1 = %d, want 207; body=%.400s", code, body)
	}
	if !hrefPresent(body, "/kept.txt") {
		t.Fatalf("a live key disappeared from the Depth-1 listing:\n%s", body)
	}
	// Its Depth-0 row is served too.
	if code, body := e.davPropfind("/kept.txt", "0"); code != http.StatusMultiStatus || !hrefPresent(body, "/kept.txt") {
		t.Fatalf("PROPFIND Depth 0 on a live key = %d, want 207 with its row; body=%.400s", code, body)
	}
}

// TestPROPFIND_MarkerReadErrorStillListsObject is pin (d) and the Finding-4
// fail-open audit: an UNREADABLE versioning state must NOT turn a readable
// object into a silently-absent listing row.
//
// The fixture is deliberately the STRONG arm — the key really IS
// delete-marked, so a fail-closed consult would hide it. Instead the consult
// errors (unparsable state, exactly as a torn write or a transient EIO does
// at this seam) and the row must still be emitted. The failure direction is
// the whole point: silently dropping real objects from a listing during a
// metadata fault is worse for the user than the bug being fixed, and s3's own
// gate proceeds on error (`mErr == nil && markerHidden`).
func TestPROPFIND_MarkerReadErrorStillListsObject(t *testing.T) {
	e := newPropfindMarkerEnv(t, "propfind-marker-failopen")
	e.enable()
	e.davWrite("readable.txt", "live-payload")
	e.webdavDelete("gone.txt") // a genuine marker alongside it

	// Control: the marked key IS omitted while the state reads clean, so
	// the arm below cannot pass for the wrong reason.
	clean, cleanBody := e.davPropfind("/", "1")
	if clean != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / Depth 1 = %d, want 207; body=%.400s", clean, cleanBody)
	}
	if hrefPresent(cleanBody, "/gone.txt") {
		t.Fatalf("control: the clean-state listing must omit the marked key:\n%s", cleanBody)
	}

	corruptStateMarker(t, e.bucketPath)

	code, body := e.davPropfind("/", "1")
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / Depth 1 with an unreadable state = %d, want 207 (fail OPEN, never a 500 or an empty listing); body=%.400s", code, body)
	}
	if !hrefPresent(body, "/readable.txt") {
		t.Fatalf("a readable object vanished from the listing when the versioning state became unreadable — "+
			"the consult failed CLOSED, which is worse than the bug it fixes:\n%s", body)
	}
	// And the object is genuinely still readable — the listing is not
	// hiding something GET would happily serve.
	getRec := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(getRec, httptest.NewRequest("GET", "/readable.txt", nil))
	if getRec.Code != http.StatusOK || getRec.Body.String() != "live-payload" {
		t.Fatalf("GET readable.txt with an unreadable state = %d %q, want 200 live-payload "+
			"(listing and read must agree on the fail-open arm)", getRec.Code, getRec.Body.String())
	}
}

// TestPROPFIND_UnwiredResolverListsEverything pins the bucketPath == "" seam:
// a frontend built with no resolver at all (the unit-test shape) keeps the
// pre-versioning plain listing, byte-identical, with no consult run. This is
// what makes the production-wiring assertion above meaningful — the consult
// is reached only because lockRoot resolves.
func TestPROPFIND_UnwiredResolverListsEverything(t *testing.T) {
	// A REAL backend over a temp dir: s3.TestBackend() returns nil unless
	// SetupVersioningTestEnv ran first, and New refuses a nil backend — the
	// unwired seam under test is the RESOLVER, never the data plane.
	dataDir := t.TempDir()
	be, err := fsbackend.New(dataDir)
	if err != nil {
		t.Fatalf("fsbackend.New: %v", err)
	}
	// The bucket dir must exist for the mode-B root row to resolve — the
	// unwired seam under test is the RESOLVER (bucketPath == ""), not a
	// missing bucket.
	if mkErr := os.MkdirAll(filepath.Join(dataDir, "unwired-bucket", ".metadata"), 0o755); mkErr != nil {
		t.Fatalf("creating bucket dir: %v", mkErr)
	}
	f, err := New(be, Config{Bucket: "unwired-bucket"}, WithAuthenticator(newStubAuth()))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	if got := f.bucketPath("unwired-bucket"); got != "" {
		t.Fatalf("bucketPath = %q, want \"\" for an unwired frontend", got)
	}
	code, body := e2ePropfind(f, "/", "1")
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND on an unwired frontend = %d, want 207; body=%.400s", code, body)
	}
	if !strings.Contains(body, "<d:href>/</d:href>") {
		t.Fatalf("an unwired frontend must still serve the plain root row:\n%s", body)
	}
}

// e2ePropfind issues a PROPFIND against an arbitrary frontend (the seam for
// wiring shapes newPropfindMarkerEnv does not build, e.g. no resolver).
func e2ePropfind(f *Frontend, path, depth string) (int, string) {
	req := httptest.NewRequest("PROPFIND", path, nil)
	req.Header.Set("Depth", depth)
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}
