package fusefs

// w2_rename_precondition_test.go - the wave-2 LOW pin: renameKey's MOVE
// must If-Match the destination's KNOWN etag (the R1 seam UploadFile
// uses), not CleanETag. A dirty destination row still carries its last
// uploaded generation; CleanETag blanks dirty rows, so the MOVE went out
// with NO precondition and silently clobbered the dirty row's server
// generation. The memfs model answers ErrConflict when the destination's
// body md5 differs from the supplied destETag, so: with the correct
// known etag the rename 412s (EIO, nothing changes); with no precondition
// (the pre-fix behavior) it succeeds and destroys the server generation.
import (
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

func TestRenameKeyDestPreconditionUsesKnownETag(t *testing.T) {
	h := newHarness(t)
	// Two server objects; the destination is checked out and EDITED
	// locally (dirty row keeps its pre-edit etag).
	h.put("src.txt", "source body")
	h.put("dst.txt", "server generation")
	if errno := h.fs.ensureHydrated(h.ctx, "dst.txt"); errno != 0 {
		t.Fatalf("hydrate dst: %v", errno)
	}
	preEditETag, _ := h.idx.KnownETag("dst.txt")
	if preEditETag == "" {
		t.Fatal("precondition: hydrated dst row must carry its server etag")
	}

	// Local edit: dst is now dirty; CleanETag(dst) is "" but KnownETag
	// is still the pre-edit generation (R1).
	fn := &fileNode{fs: h.fs, key: "dst.txt"}
	fh, _, errno := fn.Open(h.ctx, 0)
	if errno != 0 {
		t.Fatalf("Open dst: %v", errno)
	}
	if _, errno := fn.Write(h.ctx, fh, []byte("local edit"), 0); errno != 0 {
		t.Fatalf("Write dst: %v", errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush dst: %v", errno)
	}
	if clean, _ := h.idx.CleanETag("dst.txt"); clean != "" {
		t.Fatalf("arrange: dirty dst must CleanETag-blank, got %q", clean)
	}

	// Rename src over the dirty dst: the MOVE must carry dst's KNOWN
	// etag as the If-Match. The server generation ("server generation")
	// differs from the pre-edit etag's md5? No - the pre-edit etag IS
	// "server generation"'s md5 (the row was hydrated from it). To pin
	// the precondition, race the server first: another client rewrites
	// dst, so the server's generation no longer matches the row's known
	// etag, and the MOVE MUST 412.
	h.tr.PutRaw("dst.txt", "RACED")

	errno = h.fs.renameKey(h.ctx, "src.txt", "dst.txt")
	if errno == 0 {
		t.Fatal("rename over a raced dirty destination succeeded (no If-Match precondition: CleanETag blanked the dirty row)")
	}
	// And the 412 shape: the transport answered ErrConflict -> EIO.
	if errno != mapTransportErr(transport.ErrConflict) {
		t.Fatalf("rename errno = %v, want the ErrConflict mapping (412 surfaced)", errno)
	}
	if got := h.tr.ServerBodyOrEmpty("dst.txt"); got != "RACED" {
		t.Fatalf("server dst = %q, want the raced generation intact (nothing changes on 412)", got)
	}
}
