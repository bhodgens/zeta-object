// versioning_statefail_test.go — bughunt M6: a versioning STATE-READ
// failure was swallowed as "not versioned", turning a transient EIO or a
// corrupt/half-written .versioning marker into a SILENT UNVERSIONED
// OVERWRITE — the one fail-open hole in an otherwise fail-closed design
// (capture failures already fail closed: 500 before any write).
//
// The hole had to be fixed on BOTH sides identically, or the gates would
// produce DIFFERENT decisions for identical state and the cross-frontend
// parity the tree rests on would break:
//   - webdav: versioning.go captureBeforePut (the state read)
//   - s3:     object_handlers.go:81 putObjectHandler (stErr == nil && ...)
//
// The fail-CLOSED choice is the right one for a versioning gate: when the
// store cannot say whether the bucket is versioned, "Enabled" is the only
// answer that preserves the promise the bucket already made to its clients
// (a delete marker instead of destruction, a captured version instead of a
// silent loss). A wrong "fail-closed" costs a 500 on a broken bucket; a
// wrong "fail-open" destroys data irrecoverably.
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

// corruptVersioningMarker replaces the bucket's state marker with bytes
// that cannot parse as the state sidecar. A parse error is the realistic
// shape here (a torn write from a crash, a truncated file); it is
// indistinguishable from EIO at the gate, which is why both must fail the
// same way.
func corruptVersioningMarker(t *testing.T, bucketPath string) {
	t.Helper()
	statePath := filepath.Join(bucketPath, ".metadata", ".versioning")
	if err := os.WriteFile(statePath, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupting the versioning state marker: %v", err)
	}
}

// TestWebdavPUT_StateReadFailureFailsClosed pins the webdav half of M6:
// with an unreadable versioning state the PUT must be REJECTED (500) and
// the object's current bytes must be INTACT. Before the fix the PUT
// succeeded and the prior version was silently discarded.
func TestWebdavPUT_StateReadFailureFailsClosed(t *testing.T) {
	e := newVerParityEnv(t, "statefail-put-bucket", "sidecar")
	e.enable()

	e.davPut("doc.txt", "v1")
	corruptVersioningMarker(t, e.bucketPath)

	req := httptest.NewRequest("PUT", "/doc.txt", strings.NewReader("v2"))
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with an unreadable versioning state = %d, want 500 (fail CLOSED, s3 parity)", w.Code)
	}
	// The old bytes must survive: no write happened at all.
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "doc.txt")); err != nil {
		t.Fatalf("current object must survive a refused PUT: %v", err)
	} else if string(raw) != "v1" {
		t.Fatalf("current bytes = %q, want v1 (the write must not land)", raw)
	}
}

// TestWebdavDELETE_StateReadFailureFailsClosed pins the delete half: with
// an unreadable versioning state the DELETE must be REJECTED (500) and the
// DATA FILE MUST SURVIVE. Before the fix the plain delete ran, the bytes
// were destroyed, and no marker was recorded — unrecoverable.
func TestWebdavDELETE_StateReadFailureFailsClosed(t *testing.T) {
	e := newVerParityEnv(t, "statefail-del-bucket", "sidecar")
	e.enable()

	e.davPut("k.txt", "payload")
	corruptVersioningMarker(t, e.bucketPath)

	if code := e.davDelete("k.txt"); code != http.StatusInternalServerError {
		t.Fatalf("DELETE with an unreadable versioning state = %d, want 500 (fail CLOSED)", code)
	}
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "k.txt")); err != nil {
		t.Fatalf("data must survive a refused DELETE: %v", err)
	} else if string(raw) != "payload" {
		t.Fatalf("data = %q, want payload (the delete must not land)", raw)
	}
}

// TestWebdavS3_StateReadFailureIdenticalDecision is the PARITY pin that
// keeps the two sides identical: for ONE unreadable state marker, the s3
// PUT handler and the webdav PUT handler must produce the SAME status and
// the same on-disk outcome. Whichever fail-closed form is chosen, both
// gates must choose it.
func TestWebdavS3_StateReadFailureIdenticalDecision(t *testing.T) {
	e := newVerParityEnv(t, "statefail-parity-bucket", "sidecar")
	e.enable()

	// Two keys, same corrupt state: one written through each wire path.
	e.s3Put("via-s3.txt", "s3-v1")
	e.davPut("via-dav.txt", "dav-v1")
	corruptVersioningMarker(t, e.bucketPath)

	s3Req := httptest.NewRequest("PUT", "/"+e.bucket+"/via-s3.txt", strings.NewReader("s3-v2"))
	s3Rec := httptest.NewRecorder()
	s3.PutObjectHandlerForTest(s3Rec, s3Req, e.bucket, "via-s3.txt")

	davReq := httptest.NewRequest("PUT", "/via-dav.txt", strings.NewReader("dav-v2"))
	davRec := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(davRec, davReq)

	if s3Rec.Code != davRec.Code {
		t.Fatalf("state-read failure must decide identically on both gates: s3 = %d, webdav = %d", s3Rec.Code, davRec.Code)
	}
	if s3Rec.Code != http.StatusInternalServerError {
		t.Fatalf("both gates must fail CLOSED: got s3 = %d, webdav = %d, want 500", s3Rec.Code, davRec.Code)
	}
	// Neither write landed.
	for key, want := range map[string]string{"via-s3.txt": "s3-v1", "via-dav.txt": "dav-v1"} {
		if raw, err := os.ReadFile(filepath.Join(e.bucketPath, key)); err != nil {
			t.Fatalf("%s must survive a refused PUT: %v", key, err)
		} else if string(raw) != want {
			t.Fatalf("%s = %q, want %q (neither gate may write through an unreadable state)", key, raw, want)
		}
	}
}

// TestWebdavPUT_MissingStateMarkerStillOff pins the OTHER arm, so the
// fail-closed change cannot turn "never versioned" into a 500 storm: a
// bucket with NO state marker is Off (the store answers Off with a nil
// error — one missing-file stat), and the PUT takes the plain path.
func TestWebdavPUT_MissingStateMarkerStillOff(t *testing.T) {
	e := newVerParityEnv(t, "statefail-off-bucket", "sidecar")

	e.davPut("doc.txt", "v1")
	e.davPut("doc.txt", "v2") // plain overwrite: no marker file, state Off

	if _, err := os.Stat(filepath.Join(e.bucketPath, ".metadata", ".versioning")); !os.IsNotExist(err) {
		t.Fatalf("never-versioned bucket must not gain a state marker (stat err = %v)", err)
	}
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "doc.txt")); err != nil || string(raw) != "v2" {
		t.Fatalf("current bytes = %q err=%v, want v2 (an Off bucket takes the plain path)", raw, err)
	}
	if _, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, "doc.txt"); err == nil {
		t.Fatal("an Off bucket must record no versions")
	}
}

// TestWebdavDELETE_MissingStateMarkerStillOff is the delete twin of the
// above: an Off bucket's DELETE really deletes (the plain path), which is
// the byte-identical pre-versioning behavior the fail-closed change must
// not disturb.
func TestWebdavDELETE_MissingStateMarkerStillOff(t *testing.T) {
	e := newVerParityEnv(t, "statefail-off-del-bucket", "sidecar")

	e.davPut("k.txt", "payload")
	if code := e.davDelete("k.txt"); code != http.StatusNoContent {
		t.Fatalf("Off-bucket DELETE = %d, want 204", code)
	}
	if _, err := os.Stat(filepath.Join(e.bucketPath, "k.txt")); !os.IsNotExist(err) {
		t.Fatalf("Off-bucket DELETE must remove the data file (stat err = %v)", err)
	}
}
