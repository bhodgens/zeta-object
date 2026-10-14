package sync

// r1_matrix_pins_test.go - R1 pins for the conflict matrix under the
// etag-preservation change: MarkDirty (via PutPath seeds here) keeps the
// row's etag on dirty rows, so
//
//	matrix 5: dirty row WITH a preserved etag + remote deleted ->
//	         KEPT-LOCAL conflict, never a create-only upload;
//	matrix 8: dirty row with NO etag (never uploaded) + absent remote ->
//	         create-only upload (If-None-Match:*).
//
// The matrix-5 case is the one the fusefs etag preservation feeds: an
// uploaded-then-edited file now carries a REAL known-good etag when the
// scan meets a remote delete.

import (
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// Dirty + preserved etag + remote deleted: KEPT local, no upload, no
// create. The row stays dirty and no server object appears.
func TestR1Matrix5DirtyWithETagRemoteGoneKeptNotCreated(t *testing.T) {
	h := newHarness(t)
	const key = "doc.txt"
	v1 := transport.MD5Hex([]byte("v1"))
	h.putLocal(key, "local edit")
	// Seed like the post-fix fusefs flow: the row keeps the last
	// uploaded generation's etag through the dirty flip. The server
	// never listed the key (no PutRaw) - the matrix-5 shape: remote
	// generation gone while a dirty local row with a known etag exists.
	h.seedDirty(key, v1)
	// Sanity: the row is dirty WITH an etag - the exact state the fix
	// now guarantees.
	if row := h.row(key); !row.Dirty || row.ETag != v1 {
		t.Fatalf("arrange: row = %+v, want dirty with etag %q", row, v1)
	}

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	rep := h.eng.LastReport()
	if rep.RemoteDeleteKept != 1 || rep.Uploads != 0 {
		t.Fatalf("report = %+v, want RemoteDeleteKept=1 Uploads=0 (kept-local, no create)", rep)
	}
	if body := h.fs.ServerBodyOrEmpty(key); body != "" {
		t.Fatalf("server object resurrected by the scan: %q", body)
	}
	if row := h.row(key); !row.Dirty || row.Deleted {
		t.Fatalf("post-scan row = %+v, want still dirty, not tombstoned", row)
	}
	if got := h.localBody(key); got != "local edit" {
		t.Fatalf("local bytes lost: %q", got)
	}
}

// Dirty + NO etag (never uploaded) + absent remote: matrix-8 create-only
// upload. Pins that the etag-preservation change did not turn the
// never-uploaded case into a blind overwrite.
func TestR1Matrix8DirtyNoETagAbsentRemoteCreateUpload(t *testing.T) {
	h := newHarness(t)
	const key = "fresh.txt"
	h.putLocal(key, "brand new")
	h.seedDirty(key, "") // never uploaded: no known-good etag

	if err := h.eng.SyncOnce(bg()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	rep := h.eng.LastReport()
	if rep.Uploads != 1 {
		t.Fatalf("report = %+v, want Uploads=1 (create-only upload)", rep)
	}
	if got := h.fs.ServerBodyOrEmpty(key); got != "brand new" {
		t.Fatalf("server body = %q, want the create", got)
	}
	if row := h.row(key); row.Dirty || row.ETag == "" {
		t.Fatalf("post-scan row = %+v, want clean with the new etag", row)
	}
}
