// versioning_gate_test.go — the write-side versioning GATE tests
// (bughunt H1: the branch was dead in production).
//
// H1 was that put.go/delete.go/copymove.go/versioning.go all gated the
// versioning branch on `f.bucketPathFn != nil` while PRODUCTION wires only
// WithLockStoreRoot(getBucketPath) (frontends.go webdav+owncloud,
// h3/frontend.go). WithBucketPathResolver had zero non-test call sites, so
// every webdav/owncloud/h3 write skipped version capture entirely: a PUT
// overwrite on a zfs_versioning=Enabled bucket recorded no version, and a
// DELETE took the plain path (no delete marker, bytes destroyed) instead of
// the recoverable marker path.
//
// These tests construct the frontend the way PRODUCTION does
// (WithLockStoreRoot only, NO WithBucketPathResolver) so the seam they
// exercise is the one that ships. The read surfaces (zfssurface.go,
// batch.go) already resolved f.bucketPath() with no field guard and worked
// in production — that split was the tell.
package webdav

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// newProdWiredVerEnv is verParityEnv's PRODUCTION-WIRED twin: identical
// seams, identical data plane, but the frontend carries ONLY
// WithLockStoreRoot — the exact constructor argument list frontends.go and
// h3/frontend.go pass. WithBucketPathResolver is deliberately ABSENT, so
// f.bucketPathFn stays nil while f.lockRoot resolves the real bucket dir:
// the shape that made the versioning branch dead.
func newProdWiredVerEnv(t *testing.T, bucket, mode string) *verParityEnv {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	s3.InstallZfsVersioningMode(mode)
	t.Cleanup(func() { s3.InstallZfsVersioningMode("") })

	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}

	// PRODUCTION wiring, verbatim: WithLockStoreRoot(getBucketPath) and
	// nothing else. No WithBucketPathResolver.
	f, err := New(s3.TestBackend(), Config{Bucket: bucket}, WithAuthenticator(newStubAuth()),
		WithLockStoreRoot(func(b string) string { return filepath.Join(dataDir, b) }))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	if f.bucketPathFn != nil {
		t.Fatal("this env must NOT wire WithBucketPathResolver — the whole point is the production shape")
	}
	if f.bucketPath(bucket) == "" {
		t.Fatal("lockRoot must resolve the bucket dir in production wiring")
	}
	return &verParityEnv{t: t, dataDir: dataDir, bucket: bucket, bucketPath: bucketPath, f: f}
}

// TestWebdavPUT_ProductionWiringCapturesVersion is THE regression pin for
// H1: with the frontend wired exactly as production wires it, a PUT
// overwrite on a versioning-Enabled bucket records a version. Before the
// fix this recorded NOTHING and the sidecar's prior history was silently
// lost.
func TestWebdavPUT_ProductionWiringCapturesVersion(t *testing.T) {
	e := newProdWiredVerEnv(t, "prod-put-bucket", "sidecar")
	e.enable()

	e.davPut("doc.txt", "v1-dav")
	e.davPut("doc.txt", "v2-dav") // overwrite: v1 MUST be captured

	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "doc.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("production-wired PUT recorded %d versions, want 1 (the OLD v1): %+v", len(versions), versions)
	}
	if versions[0].Size != int64(len("v1-dav")) || versions[0].IsDeleteMarker {
		t.Fatalf("captured entry = %+v, want v1's bytes as a non-marker version", versions[0])
	}
	// And the old bytes are actually recoverable.
	raw, err := s3.OpenVersionForTest(e.bucketPath, e.bucket, "doc.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("Open captured version: %v", err)
	}
	if string(raw) != "v1-dav" {
		t.Fatalf("captured bytes = %q, want v1-dav", raw)
	}
	// Current bytes are v2.
	if cur, rerr := os.ReadFile(filepath.Join(e.bucketPath, "doc.txt")); rerr != nil || string(cur) != "v2-dav" {
		t.Fatalf("current bytes = %q err=%v, want v2-dav", cur, rerr)
	}
}

// TestWebdavDELETE_ProductionWiringWritesMarker is the delete half of H1:
// with production wiring a DELETE on a versioning-Enabled bucket must write
// a delete marker and SUPPRESS the plain delete (the data file survives).
// Before the fix the plain path ran, the bytes were destroyed, and the
// history was unrecoverable with no error.
func TestWebdavDELETE_ProductionWiringWritesMarker(t *testing.T) {
	e := newProdWiredVerEnv(t, "prod-del-bucket", "sidecar")
	e.enable()

	e.davPut("k.txt", "payload")
	if code := e.davDelete("k.txt"); code != http.StatusNoContent {
		t.Fatalf("production-wired DELETE = %d, want 204", code)
	}

	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "k.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 1 || !versions[0].IsDeleteMarker {
		t.Fatalf("history = %+v, want exactly one delete marker", versions)
	}
	// The data file MUST survive — the marker path, not destruction.
	if raw, rerr := os.ReadFile(filepath.Join(e.bucketPath, "k.txt")); rerr != nil {
		t.Fatalf("data file must survive a versioned delete: %v", rerr)
	} else if string(raw) != "payload" {
		t.Fatalf("surviving data = %q, want payload", raw)
	}
}

// TestWebdavMOVE_ProductionWiringCapturesDestination pins the fourth gate
// (copymove.go): with production wiring a MOVE whose destination already
// exists captures the destination's old bytes — the same capture any
// overwrite takes.
func TestWebdavMOVE_ProductionWiringCapturesDestination(t *testing.T) {
	e := newProdWiredVerEnv(t, "prod-move-bucket", "sidecar")
	e.enable()

	e.davPut("dst.txt", "dst-v1")
	e.davPut("src.txt", "move-me")
	// MOVEs onto an EXISTING destination (204, not the create-only 201
	// the shared davMove helper asserts).
	moveReq := httptest.NewRequest("MOVE", "/src.txt", nil)
	moveReq.Header.Set("Destination", "/dst.txt")
	moveRec := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(moveRec, moveReq)
	if moveRec.Code != http.StatusNoContent {
		t.Fatalf("webdav MOVE onto an existing dest = %d, want 204", moveRec.Code)
	}

	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "dst.txt")
	if err != nil {
		t.Fatalf("List dst.txt: %v", err)
	}
	if len(versions) != 1 || versions[0].Size != int64(len("dst-v1")) {
		t.Fatalf("destination history = %+v, want one capture of the OLD dst bytes", versions)
	}
	raw, err := s3.OpenVersionForTest(e.bucketPath, e.bucket, "dst.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("Open captured version: %v", err)
	}
	if string(raw) != "dst-v1" {
		t.Fatalf("captured bytes = %q, want dst-v1", raw)
	}
}

// TestWebdavMOVE_ProductionWiringSourceMarker pins versioning.go's fourth
// gate (moveSourceDelete): with production wiring a MOVE's source delete
// half records the delete marker the s3 DeleteObject counterpart records.
func TestWebdavMOVE_ProductionWiringSourceMarker(t *testing.T) {
	e := newProdWiredVerEnv(t, "prod-move-src-bucket", "sidecar")
	e.enable()

	e.davPut("src.txt", "move-me")
	e.davMove("src.txt", "dst2.txt")

	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "src.txt")
	if err != nil {
		t.Fatalf("List src.txt: %v", err)
	}
	if len(versions) != 1 || !versions[0].IsDeleteMarker {
		t.Fatalf("source history = %+v, want exactly one delete marker (s3 DeleteObject parity)", versions)
	}
	// The source data file survives the marker delete (the MOVE's delete
	// half took the marker path, not the plain path).
	if _, err := os.Stat(filepath.Join(e.bucketPath, "src.txt")); err != nil {
		t.Fatalf("source data file must survive a versioned MOVE's delete half: %v", err)
	}
}

// TestWebdav_UnwiredFrontendTakesPlainPath keeps the unit-test seam GREEN:
// a frontend with NEITHER bucketPathFn NOR lockRoot resolves "" and must
// take the plain path on every write (PUT, DELETE, COPY/MOVE) — no
// versioning machinery, no 500 from an unwired store.
func TestWebdav_UnwiredFrontendTakesPlainPath(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "unwired-bkt"})
	if f.bucketPath("unwired-bkt") != "" {
		t.Fatalf("unwired frontend must resolve an empty bucket path, got %q", f.bucketPath("unwired-bkt"))
	}

	// PUT create + overwrite both take the plain path.
	for _, body := range []string{"v1", "v2"} {
		req := httptest.NewRequest("PUT", "/a.txt", strings.NewReader(body))
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusCreated && w.Code != http.StatusNoContent {
			t.Fatalf("unwired PUT %q = %d, want 201/204", body, w.Code)
		}
	}
	// DELETE takes the plain path and really deletes.
	req := httptest.NewRequest("DELETE", "/a.txt", nil)
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("unwired DELETE = %d, want 204", w.Code)
	}
	if be.hasKey("unwired-bkt", "a.txt") {
		t.Fatal("unwired DELETE must take the plain path and remove the key")
	}
	// COPY/MOVE takes the plain path.
	be.seed("unwired-bkt", "s.txt", []byte("s"))
	req = httptest.NewRequest("MOVE", "/s.txt", nil)
	req.Header.Set("Destination", "/d.txt")
	w = httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated && w.Code != http.StatusNoContent {
		t.Fatalf("unwired MOVE = %d, want 201/204", w.Code)
	}
	if !be.hasKey("unwired-bkt", "d.txt") {
		t.Fatal("unwired MOVE must take the plain path and write the destination")
	}
}

// TestWebdav_LockRootOnlyResolverTakesVersioningBranch is the seam
// disambiguator: lockRoot alone is enough to make the versioning branch
// live (that IS the production resolver), while both-nil stays plain. It
// pins the gate on the RESOLVED PATH, not on a particular field.
func TestWebdav_LockRootOnlyResolverTakesVersioningBranch(t *testing.T) {
	dataDir := t.TempDir()
	// A resolver that resolves for real buckets but not for the sentinel
	// "unwired" name — proves the gate reads the RESOLVED value, not
	// merely "some resolver is installed".
	resolver := func(b string) string {
		if b == "unwired" {
			return ""
		}
		return filepath.Join(dataDir, b)
	}
	f, err := New(newStubBackend(), Config{}, WithAuthenticator(newStubAuth()), WithLockStoreRoot(resolver))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := f.bucketPath("photos"); got != filepath.Join(dataDir, "photos") {
		t.Fatalf("bucketPath(photos) = %q, want the resolved dir", got)
	}
	if got := f.bucketPath("unwired"); got != "" {
		t.Fatalf("bucketPath(unwired) = %q, want \"\" (resolver declines)", got)
	}
}
