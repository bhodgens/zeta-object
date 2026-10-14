// delete_versioning_test.go — bughunt H10: the recursive collection DELETE
// used to call f.be.Delete directly per key, so on a versioning-Enabled
// bucket a DELETE /dir/ destroyed every data file with NO delete marker and
// NO captured versions — recoverability the single-key DELETE on the same
// bucket keeps. The fix routes each removed key through the SAME
// marker-aware path the kindFile branch drives (deleteCollectionKey →
// deleteMarkerOrPlain), and a snapshots-mode collection DELETE answers 409
// (the refusal a single-key DELETE gives for the identical store state)
// instead of executing the destruction.
//
// The env is newVerParityEnv (the package's production-wired parity env),
// so the store assertions below read the real sidecar the same way
// TestWebdavDELETEParity_WithS3DELETE does.
package webdav

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// davDeleteStatus DELETEs a collection path and returns the status (the
// shared davDelete helper asserts nothing; this arm pins conflict/204
// statuses directly).
func (e *verParityEnv) davDeleteStatus(path string) int {
	e.t.Helper()
	req := httptest.NewRequest("DELETE", "/"+path, nil)
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	return w.Code
}

// TestWebdavDELETE_CollectionEnabledLeavesMarkersAndData: on a
// versioning-Enabled bucket a recursive DELETE /dir/ records a delete
// marker for EVERY removed key, the data files SURVIVE (the plain delete is
// suppressed per key, exactly what a single-key DELETE leaves), and every
// plain view answers as deleted (the marker-hiding rule) — no data
// destroyed, history recoverable.
func TestWebdavDELETE_CollectionEnabledLeavesMarkersAndData(t *testing.T) {
	e := newVerParityEnv(t, "coll-del-enabled-bkt", "sidecar")
	e.enable()

	e.s3Put("dir/a.txt", "body-a")
	e.s3Put("dir/sub/b.txt", "body-b")

	if code := e.davDeleteStatus("dir/"); code != http.StatusNoContent {
		t.Fatalf("recursive DELETE /dir/ = %d, want 204", code)
	}

	for _, key := range []string{"dir/a.txt", "dir/sub/b.txt"} {
		// One delete-marker entry each — the sweep recorded what a
		// single-key DELETE records, never a bare destruction.
		versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, key)
		if err != nil {
			t.Fatalf("List %s: %v", key, err)
		}
		if len(versions) != 1 || !versions[0].IsDeleteMarker {
			t.Fatalf("%s history = %+v, want exactly one delete marker (the recursive sweep must record what a single-key DELETE records)", key, versions)
		}
		// The data file survived the suppressed plain delete.
		raw, rerr := os.ReadFile(filepath.Join(e.bucketPath, key))
		if rerr != nil {
			t.Fatalf("%s data file must survive the marker delete: %v", key, rerr)
		}
		want := map[string]string{"dir/a.txt": "body-a", "dir/sub/b.txt": "body-b"}[key]
		if string(raw) != want {
			t.Fatalf("%s data = %q, want %q", key, raw, want)
		}
		// Plain views hide it (the same visibility rule GET/PROPFIND
		// apply) — the collection is gone from the client's view.
		hidden, err := s3.PlainObjectDeleteMarker404(e.bucketPath, e.bucket, key)
		if err != nil {
			t.Fatalf("marker consult %s: %v", key, err)
		}
		if !hidden {
			t.Fatalf("%s still visible in plain views after the recursive delete", key)
		}
	}
}

// TestWebdavDELETE_CollectionSnapshotsRefused409: in snapshots mode the
// store refuses delete markers, so the recursive DELETE must answer 409 —
// the SAME conflict a single-key DELETE answers — and must not destroy the
// tree (a conflict is a refusal, never a half-applied wipe).
func TestWebdavDELETE_CollectionSnapshotsRefused409(t *testing.T) {
	e := newVerParityEnv(t, "coll-del-snap-bkt", "snapshots")
	e.enable()

	e.davPut("dir/a.txt", "snap-a")

	// The single-key DELETE contract for reference on this same bucket:
	// the store refuses, the wire answers 409.
	if code := e.davDelete("dir/a.txt"); code != http.StatusConflict {
		t.Fatalf("fixture: single-key snapshots DELETE = %d, want 409", code)
	}
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "dir/a.txt")); err != nil || string(raw) != "snap-a" {
		t.Fatalf("fixture: single-key refused delete left %q err=%v", raw, err)
	}

	if code := e.davDeleteStatus("dir/"); code != http.StatusConflict {
		t.Fatalf("snapshots-mode recursive DELETE /dir/ = %d, want 409 (the same refusal a single-key DELETE gets)", code)
	}
	// The tree survived the refusal.
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "dir/a.txt")); err != nil || string(raw) != "snap-a" {
		t.Fatalf("refused recursive delete must not destroy data: %q err=%v", raw, err)
	}
}

// TestWebdavDELETE_CollectionUnversionedStillDestroys guards against fixing
// H10 by refusing recursive deletes outright: on a bucket that is NOT
// versioned (no mode installed), DELETE /dir/ still removes every key and
// the plain data files (the byte-identical pre-versioning path).
func TestWebdavDELETE_CollectionUnversionedStillDestroys(t *testing.T) {
	e := newVerParityEnv(t, "coll-del-off-bkt", "sidecar")
	// No e.enable(): versioning stays Off.

	e.s3Put("dir/a.txt", "plain-a")
	e.s3Put("dir/sub/b.txt", "plain-b")

	if code := e.davDeleteStatus("dir/"); code != http.StatusNoContent {
		t.Fatalf("recursive DELETE on an unversioned bucket = %d, want 204", code)
	}
	for _, key := range []string{"dir/a.txt", "dir/sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(e.bucketPath, key)); !os.IsNotExist(err) {
			t.Fatalf("%s must be GONE after an unversioned recursive delete (stat err=%v)", key, err)
		}
	}
}
