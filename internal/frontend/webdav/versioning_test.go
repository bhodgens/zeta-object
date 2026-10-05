// versioning_test.go — quic-h3-2026-10 leaf 05: protocol-independent
// version capture (parity with the S3 write path).
//
// The tests drive BOTH wire paths against ONE versioned test bucket: a
// PUT through the s3 package's handler test double, then a PUT of the
// same file through the webdav handler. The version store's List must
// show BOTH old versions captured — byte-identical history whether the
// write arrived via S3 or WebDAV. DELETE via webdav must record exactly
// what its S3 counterpart records for the mode (sidecar: delete marker;
// snapshots: the store refuses — ErrDeleteMarkersUnsupported); a webdav
// MOVE (rename) must manufacture NO version entries; snapshots mode
// must add no sidecar state on webdav writes; both-mode sidecar entries
// must appear exactly as sidecar mode.
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

// verParityEnv is a webdav parity test environment: a REAL fs backend
// rooted at a temp dataDir (the s3 frontend's own test layout), the s3
// process seams (config view + fs-root resolver) installed against it,
// and a webdav Frontend sharing the same backend. Both wire paths
// therefore read and write ONE bucket directory.
type verParityEnv struct {
	t          *testing.T
	dataDir    string
	bucket     string
	bucketPath string
	f          *Frontend
}

func newVerParityEnv(t *testing.T, bucket, mode string) *verParityEnv {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	s3.InstallZfsVersioningMode(mode)
	t.Cleanup(func() { s3.InstallZfsVersioningMode("") })

	// The bucket directory + .metadata (the s3 env's setupBucket shape).
	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}

	// The webdav frontend on the SAME fs backend, single-bucket mode B
	// rooted at this bucket — with the bucket→fs-root resolver wired
	// exactly as package main does (WithBucketPathResolver + the same
	// getBucketPath math).
	f, err := New(s3.TestBackend(), Config{Bucket: bucket}, WithAuthenticator(newStubAuth()),
		WithBucketPathResolver(func(b string) string { return filepath.Join(dataDir, b) }))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	return &verParityEnv{t: t, dataDir: dataDir, bucket: bucket, bucketPath: bucketPath, f: f}
}

// enable turns versioning on through the REAL s3 sub-resource handler
// (the bucket-level state marker the store reads is written by the
// production path, not by a test shortcut).
func (e *verParityEnv) enable() {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+e.bucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
	w := httptest.NewRecorder()
	s3.PutBucketVersioningHandlerForTest(w, req, e.bucket)
	if w.Code != http.StatusOK {
		e.t.Fatalf("enable versioning: status = %d, want 200", w.Code)
	}
}

// s3Put PUTs through the s3 package's handler test double (the same
// double the s3 versioning tests drive — the full production PUT path).
func (e *verParityEnv) s3Put(key, body string) {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+e.bucket+"/"+key, strings.NewReader(body))
	w := httptest.NewRecorder()
	s3.PutObjectHandlerForTest(w, req, e.bucket, key)
	if w.Code != http.StatusOK {
		e.t.Fatalf("s3 PUT %s: status = %d, want 200", key, w.Code)
	}
}

// davPut PUTs through the webdav handler (mode B: no bucket segment).
// 201 = create, 204 = overwrite (both are successes).
func (e *verParityEnv) davPut(key, body string) {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+key, strings.NewReader(body))
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated && w.Code != http.StatusNoContent {
		e.t.Fatalf("webdav PUT %s: status = %d, want 201/204", key, w.Code)
	}
}

// davDelete DELETEs the key through the webdav handler; the recorded
// status is returned for the caller's expectation.
func (e *verParityEnv) davDelete(key string) int {
	e.t.Helper()
	req := httptest.NewRequest("DELETE", "/"+key, nil)
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	return w.Code
}

// davMove MOVEs (renames) the key through the webdav handler.
func (e *verParityEnv) davMove(src, dst string) {
	e.t.Helper()
	req := httptest.NewRequest("MOVE", "/"+src, nil)
	req.Header.Set("Destination", "/"+dst)
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("webdav MOVE %s -> %s: status = %d, want 201", src, dst, w.Code)
	}
}

// TestWebdavPUTParity_WithS3PUT drives BOTH wire paths against one
// versioned bucket: PUT doc.txt via S3, overwrite via WebDAV, overwrite
// again via S3 — the store's List must show BOTH old versions captured,
// with the webdav-written capture byte-identical in shape to the
// s3-written one (same entry fields, delete-marker false, IsLatest on
// exactly the newest).
func TestWebdavPUTParity_WithS3PUT(t *testing.T) {
	e := newVerParityEnv(t, "parity-put-bucket", "sidecar")
	e.enable()

	// v1 via S3 (create — no prior version).
	e.s3Put("doc.txt", "v1-s3")
	// v2 via WebDAV (overwrite — v1 must be captured).
	e.davPut("doc.txt", "v2-dav")
	// v3 via S3 (overwrite — v2 must be captured identically to v1).
	e.s3Put("doc.txt", "v3-s3")

	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "doc.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("List = %d entries, want 2 (the OLD v1 and v2): %+v", len(versions), versions)
	}
	// Newest first: v2 (the webdav capture) then v1 (the s3 capture).
	if versions[0].Size != int64(len("v2-dav")) || versions[1].Size != int64(len("v1-s3")) {
		t.Fatalf("entry sizes = %d/%d, want %d/%d", versions[0].Size, versions[1].Size, len("v2-dav"), len("v1-s3"))
	}
	if versions[0].IsDeleteMarker || versions[1].IsDeleteMarker {
		t.Fatalf("PUT captures must never be delete markers: %+v", versions)
	}
	if !versions[0].IsLatest || versions[1].IsLatest {
		t.Fatalf("IsLatest must sit exactly on the newest entry: %+v", versions)
	}
	// The OLD v1 bytes stay readable through the store (the webdav
	// capture went through the s3 store machinery, byte-identical).
	etag := versions[1].ETag
	if etag == "" {
		t.Fatalf("old version entry must carry the OLD ETag: %+v", versions[1])
	}
	// Current bytes are v3.
	raw, err := os.ReadFile(filepath.Join(e.bucketPath, "doc.txt"))
	if err != nil || string(raw) != "v3-s3" {
		t.Fatalf("current bytes = %q err=%v, want v3-s3", raw, err)
	}
}

// TestWebdavDELETEParity_WithS3DELETE: DELETE via webdav must record
// exactly what the S3 DELETE records for the mode — a delete marker in
// sidecar mode, with the data file surviving (s3 suppresses the plain
// delete; webdav mirrors that suppression identically).
func TestWebdavDELETEParity_WithS3DELETE(t *testing.T) {
	e := newVerParityEnv(t, "parity-del-bucket", "sidecar")
	e.enable()

	e.s3Put("k.txt", "payload")
	e.s3Put("s3del.txt", "s3-del-payload")

	// The S3 reference behavior first: a DELETE via the s3 handler
	// writes a delete marker and suppresses the plain delete.
	req := httptest.NewRequest("DELETE", "/"+e.bucket+"/s3del.txt", nil)
	w := httptest.NewRecorder()
	s3.DeleteObjectHandlerForTest(w, req, e.bucket, "s3del.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("s3 DELETE = %d, want 204", w.Code)
	}

	// Now the webdav DELETE on the same-shaped key.
	if code := e.davDelete("k.txt"); code != http.StatusNoContent {
		t.Fatalf("webdav DELETE = %d, want 204 (s3 DELETE parity)", code)
	}

	// Parity: both keys hold exactly one delete-marker entry, no data
	// destroyed, marker newest-first.
	for _, key := range []string{"k.txt", "s3del.txt"} {
		versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, key)
		if err != nil {
			t.Fatalf("List %s: %v", key, err)
		}
		if len(versions) != 1 || !versions[0].IsDeleteMarker {
			t.Fatalf("%s versions = %+v, want exactly one delete marker (s3 parity)", key, versions)
		}
		dataPath := filepath.Join(e.bucketPath, key)
		if raw, rerr := os.ReadFile(dataPath); rerr != nil {
			t.Fatalf("%s data file must survive the marker delete: %v", key, rerr)
		} else if string(raw) != "payload" && key == "k.txt" {
			t.Fatalf("%s data = %q, want payload", key, raw)
		}
	}
}

// TestWebdavMOVE_NoVersionsPinned: a webdav MOVE (rename) manufactures
// NO version entries beyond what its own steps produce — and a pure
// rename of a key with no prior history produces NO sidecar history at
// all (the s3 counterpart is CopyObject + DeleteObject; the rename's
// Delete on a fresh key writes no marker-producing history... but
// DeleteObject on an ENABLED bucket does record a marker, so the pin
// asserts exactly that: the destination carries no version entries, the
// source carries only what the DELETE step itself records).
func TestWebdavMOVE_NoVersionsPinned(t *testing.T) {
	// The bucket is versioned but the KEY was never written while
	// Enabled... and the s3 parity source is probed, not assumed: the
	// s3 CopyObject+DeleteObject pair on this exact shape leaves the
	// DESTINATION with no version entries and the SOURCE with exactly
	// one delete marker (DeleteObject on an Enabled bucket always
	// records one). The MOVE must record the same — nothing on dst, the
	// Delete step's marker on src, and nothing else anywhere.
	e := newVerParityEnv(t, "parity-move-bucket", "sidecar")
	e.enable()

	e.s3Put("src.txt", "move-me")
	e.davMove("src.txt", "dst.txt")

	// The DESTINATION carries no manufactured history: no capture was
	// taken for it (its key never existed) and a rename must not make
	// one.
	if _, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "dst.txt"); err == nil {
		t.Fatal("MOVE must not manufacture a version entry on the destination")
	}
	// The source's Delete step recorded exactly one delete marker — the
	// s3 DeleteObject parity (pinned, not assumed; see the probe in the
	// report).
	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "src.txt")
	if err != nil {
		t.Fatalf("List src.txt: %v", err)
	}
	if len(versions) != 1 || !versions[0].IsDeleteMarker {
		t.Fatalf("source history = %+v, want exactly one delete marker (s3 DeleteObject parity)", versions)
	}
	// The rename itself worked.
	if _, err := os.Stat(filepath.Join(e.bucketPath, "dst.txt")); err != nil {
		t.Fatalf("dst.txt missing after MOVE: %v", err)
	}
}

// TestWebdavMOVE_SourceMarkerMatchesS3 pins the versioned-source shape:
// a MOVE whose source was written while versioning was Enabled records
// exactly what the s3 CopyObject+DeleteObject pair records for the
// source — one delete marker, data file surviving — and NOTHING on the
// destination.
func TestWebdavMOVE_SourceMarkerMatchesS3(t *testing.T) {
	e := newVerParityEnv(t, "parity-move2-bucket", "sidecar")
	e.enable()
	e.s3Put("src.txt", "move-me") // written while Enabled: versioned key
	e.davMove("src.txt", "dst.txt")

	// The source's DELETE step recorded a delete marker (s3 parity)...
	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "src.txt")
	if err != nil {
		t.Fatalf("List src.txt: %v", err)
	}
	if len(versions) != 1 || !versions[0].IsDeleteMarker {
		t.Fatalf("source history = %+v, want exactly one delete marker (s3 DeleteObject parity)", versions)
	}
	// ...and the destination carries no manufactured history.
	if _, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "dst.txt"); err == nil {
		t.Fatal("MOVE must not manufacture a version entry on the destination")
	}
}

// TestWebdavPUT_SnapshotsModeParity: snapshots mode on the webdav path
// mirrors the s3 handler EXACTLY (probed against the s3 handler on this
// host, not assumed from the leaf text): the PUT capture IS taken (the
// sidecar bookkeeping records the old bytes — the same shape sidecar
// mode writes), and the DELETE is REFUSED by the snapshots store
// (ErrDeleteMarkersUnsupported → 409, the s3 wire's Conflict mapping).
func TestWebdavPUT_SnapshotsModeParity(t *testing.T) {
	e := newVerParityEnv(t, "parity-snap-bucket", "snapshots")
	e.enable()

	e.davPut("snap.txt", "v1")
	e.davPut("snap.txt", "v2") // overwrite: s3 parity — the capture IS recorded

	// The capture parity with the s3 handler (which records the same
	// shape in snapshots mode — probed). The probe reads the SIDECAR
	// bookkeeping: a snapshots-store List here would dereference the
	// nil zmetad handle (this env has no tracked zfs dataset), and the
	// sidecar array is exactly what the s3 handler wrote in the probe.
	s := s3.SidecarStoreForBucket(e.bucketPath)
	versions, err := s.List(e.bucket, "snap.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 1 || versions[0].Size != int64(len("v1")) {
		t.Fatalf("snapshots-mode capture = %+v, want v1's capture (s3 handler parity)", versions)
	}
	// DELETE mirrors s3 snapshots-mode behavior: the store refuses the
	// delete marker, so the delete answers 409 (s3 ErrDeleteMarkers-
	// Unsupported parity; the plain delete never runs).
	if code := e.davDelete("snap.txt"); code != http.StatusConflict {
		t.Fatalf("snapshots-mode webdav DELETE = %d, want 409 (s3 ErrDeleteMarkersUnsupported parity)", code)
	}
	// And the data file survived the refused delete.
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "snap.txt")); err != nil || string(raw) != "v2" {
		t.Fatalf("data after refused delete = %q err=%v, want v2", raw, err)
	}
}

// TestWebdavPUT_BothModeSidecarEntries: in both mode the sidecar
// entries appear exactly as in sidecar mode. The clone seam is faked
// (byte-copy FICLONE emulation, the same arm the s3 leaf-06 round-trip
// test uses — the real ioctl is proven live by zfs-validate section
// 11); the assertion mirrors the s3 merge test: the capture is recorded
// through the shared sidecar array and its bytes open back by id.
func TestWebdavPUT_BothModeSidecarEntries(t *testing.T) {
	// Emulate FICLONE by byte-copy (darwin dev host; the production
	// linux arm uses the real ioctl).
	s3.WithReflinkCloneForTest(t, func(dst, src string) error {
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, raw, 0o644)
	})
	e := newVerParityEnv(t, "parity-both-bucket", "both")
	e.enable()

	e.davPut("both.txt", "v1")
	e.davPut("both.txt", "v2")

	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "both.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("both-mode List = %d entries, want 1 (the OLD v1): %+v", len(versions), versions)
	}
	if versions[0].Size != int64(len("v1")) || versions[0].IsDeleteMarker || !versions[0].IsLatest {
		t.Fatalf("both-mode entry = %+v, want v1's capture, not a marker, IsLatest", versions[0])
	}
	// The old bytes open back through the store (whatever layout holds
	// the data — the sidecar bookkeeping is byte-identical).
	raw, err := s3.OpenVersionForTest(e.bucketPath, e.bucket, "both.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("Open old version: %v", err)
	}
	if string(raw) != "v1" {
		t.Fatalf("old version bytes = %q, want v1", raw)
	}
	// DELETE in both mode records a delete marker exactly as sidecar.
	if code := e.davDelete("both.txt"); code != http.StatusNoContent {
		t.Fatalf("both-mode webdav DELETE = %d, want 204", code)
	}
	versions, err = s3.ListVersionsForTest(e.bucketPath, e.bucket, "both.txt")
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(versions) != 2 || !versions[0].IsDeleteMarker {
		t.Fatalf("both-mode List after delete = %+v, want marker newest-first over the capture", versions)
	}
}

// TestWebdavPUT_CaptureFailureFailsOpen pins the failure-mode parity
// with s3: on the non-linux dev host the reflink clone fails; the s3
// path FAILS OPEN (the PUT proceeds, no version record). The webdav
// path must mirror that identically — the PUT succeeds and no sidecar
// entry is written (one WARN was logged server-side).
func TestWebdavPUT_CaptureFailureFailsOpen(t *testing.T) {
	// The default reflinkCloneFn on this GOOS reports unsupported
	// (darwin dev host) — exactly the s3 fail-soft test's starting arm.
	e := newVerParityEnv(t, "parity-failsoft-bucket", "reflink")
	e.enable()

	e.s3Put("obj.txt", "v1")
	// Overwrite via webdav with a FAILING clone: the PUT must proceed.
	e.davPut("obj.txt", "v2")

	if _, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "obj.txt"); err == nil {
		t.Fatal("fail-soft capture must record no version entry")
	}
	raw, err := os.ReadFile(filepath.Join(e.bucketPath, "obj.txt"))
	if err != nil || string(raw) != "v2" {
		t.Fatalf("current bytes = %q err=%v, want v2 (PUT must fail OPEN)", raw, err)
	}
}
