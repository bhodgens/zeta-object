package fusefs

// w2_m5_m6_test.go - the W2-M5 and W2-M6 regressions:
//
//	M5: a Write on a direct handle re-routes to staging; a subsequent
//	    Read through the SAME handle must serve the staged bytes (the
//	    staging generation must be opened O_RDWR, not O_WRONLY).
//	M6: SweepStaging removes every orphaned staging file (a crash
//	    mid-hydrate leaves unbounded staging bytes invisible to quota)
//	    and reports the count/bytes reclaimed; main wires it at start.
import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// TestW2M5ReadAfterWriteOnSameHandle - write via a direct handle (the
// staging reroute) then read via the SAME handle: the written bytes must
// come back (EIO before the fix: the staging fd was O_WRONLY).
func TestW2M5ReadAfterWriteOnSameHandle(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "0123456789")
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	fh, _, errno := fn.Open(h.ctx, uint32(os.O_RDWR))
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	// Write on the direct handle: the direct->staging reroute.
	if n, errno := fn.Write(h.ctx, fh, []byte("ABCD"), 2); errno != 0 || n != 4 {
		t.Fatalf("Write: n=%d errno=%v", n, errno)
	}
	// THE REGRESSION: read through the SAME handle before any Flush.
	dest := make([]byte, 6)
	res, errno := fn.Read(h.ctx, fh, dest, 0)
	if errno != 0 {
		t.Fatalf("Read after Write on the same handle: %v (staging fd not readable)", errno)
	}
	got, st := res.Bytes(dest)
	if st != fuse.OK {
		t.Fatalf("ReadResult.Bytes: %v", st)
	}
	if string(got) != "01ABCD" {
		t.Fatalf("read-after-write = %q, want %q", string(got), "01ABCD")
	}
	// And a read at the write offset sees the bytes too.
	dest2 := make([]byte, 4)
	res2, errno := fn.Read(h.ctx, fh, dest2, 2)
	if errno != 0 {
		t.Fatalf("Read at offset: %v", errno)
	}
	got2, st2 := res2.Bytes(dest2)
	if st2 != fuse.OK {
		t.Fatalf("ReadResult.Bytes: %v", st2)
	}
	if string(got2) != "ABCD" {
		t.Fatalf("read at write offset = %q, want ABCD", string(got2))
	}
}

// TestW2M6SweepStagingRemovesOrphans - seeded staging files are removed,
// the count/bytes are reported, the missing-dir case is a no-op, and
// committed cache files are untouched.
func TestW2M6SweepStagingRemovesOrphans(t *testing.T) {
	h := newHarness(t)
	staging := filepath.Join(h.fs.cacheDir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "crash-a"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "crash-b"), []byte("1234567890"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A cache file that must survive (only staging/ is swept).
	if err := os.MkdirAll(filepath.Join(h.fs.cacheDir, "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.fs.cachePath("keep.txt"), []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, bytes, err := SweepStaging(h.fs.cacheDir)
	if err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	if removed != 2 || bytes != 15 {
		t.Fatalf("SweepStaging = (%d files, %d bytes), want (2, 15)", removed, bytes)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging still holds %d entries after sweep", len(entries))
	}
	if got, err := os.ReadFile(h.fs.cachePath("keep.txt")); err != nil || string(got) != "real" {
		t.Fatalf("cache file clobbered by sweep: %q (%v)", string(got), err)
	}

	// A missing staging dir is a clean no-op (nothing reclaimed).
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	removed, bytes, err = SweepStaging(h.fs.cacheDir)
	if err != nil || removed != 0 || bytes != 0 {
		t.Fatalf("SweepStaging without staging dir = (%d, %d, %v), want (0, 0, nil)", removed, bytes, err)
	}
}
