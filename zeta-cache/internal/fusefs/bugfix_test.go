package fusefs

// bugfix_test.go - regression tests for the bughunt findings:
//
//	H5:  Write after Flush/Fsync re-commits (the committed latch is
//	     reset by the direct->staging reroute, and the old direct fd is
//	     closed, not leaked).
//	M25: a Mkdir-created directory survives the 250ms entry TTL (the
//	     local mirror dir re-stats as isDir).
//	M26: an open with O_TRUNC does not silently truncate the cached
//	     generation; the row still becomes dirty and the bytes
//	     re-hydrate.
import (
	"os"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// TestH5WriteAfterFlushCommits - write(A), Flush, write(B), Flush must
// commit B too (the first Flush's committed latch must not eat it).
func TestH5WriteAfterFlushCommits(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "remote generation")
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	fh, _, errno := fn.Open(h.ctx, uint32(os.O_RDWR))
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}

	// First write + flush: the direct->staging reroute, then commit.
	if n, errno := fn.Write(h.ctx, fh, []byte("A"), 0); errno != 0 || n != 1 {
		t.Fatalf("Write A: n=%d errno=%v", n, errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush 1: %v", errno)
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatal("dirty after first Flush")
	}

	// Second write AFTER the Flush: the handle is direct again over the
	// committed generation (committed=true). The reroute must reset the
	// latch or this Flush silently drops B. The reroute seeds staging
	// from the committed generation, so B lands at offset 1 of "A...".
	if n, errno := fn.Write(h.ctx, fh, []byte("B"), 1); errno != 0 || n != 1 {
		t.Fatalf("Write B: n=%d errno=%v", n, errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush 2: %v", errno)
	}
	got := h.readLocal("doc.txt")
	if len(got) < 2 || got[:2] != "AB" {
		t.Fatalf("post-second-Flush content = %q, want %q prefix (write after Flush dropped!)", got, "AB")
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatal("second write must re-dirty the row")
	}
}

// TestH5WriteAfterFsyncCommits - same shape through Fsync (close flush +
// upload): the latch reset applies there too. The upload leg after a
// dirty-row flush 412s against the transport's create/overwrite
// precondition contract (the sync scan owns that reconciliation), so the
// commit assertions use Flush + local bytes, and the row's dirtiness
// proves the second write re-entered the dirty state.
func TestH5WriteAfterFsyncCommits(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "remote generation")
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	fh, _, errno := fn.Open(h.ctx, uint32(os.O_RDWR))
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	// A clean file + Fsync: pure upload, must succeed (the pre-existing
	// upload path), and must NOT wedge the handle's later writes.
	if errno := fn.Fsync(h.ctx, fh, 0); errno != 0 {
		t.Fatalf("Fsync on clean file: %v", errno)
	}
	// Write A -> Fsync commits the staged bytes even though the upload
	// leg may refuse (dirty-row precondition): the COMMIT must happen.
	if n, errno := fn.Write(h.ctx, fh, []byte("A"), 0); errno != 0 || n != 1 {
		t.Fatalf("Write A: n=%d errno=%v", n, errno)
	}
	fn.Fsync(h.ctx, fh, 0) // upload leg may 412; the commit is what H5 guards
	if got := h.readLocal("doc.txt"); got[:1] != "A" {
		t.Fatalf("Fsync lost the write: content = %q", got)
	}
	// Write B AFTER the Fsync: the latch must not eat it.
	if n, errno := fn.Write(h.ctx, fh, []byte("B"), 1); errno != 0 || n != 1 {
		t.Fatalf("Write B: n=%d errno=%v", n, errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush 2: %v", errno)
	}
	got := h.readLocal("doc.txt")
	if len(got) < 2 || got[:2] != "AB" {
		t.Fatalf("write after Fsync dropped: content = %q", got)
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatal("post-Fsync write must re-dirty the row")
	}
}

// TestM25DirSurvivesEntryTTL - Mkdir, then a lookupChild AFTER the 250ms
// entry TTL: the local mirror dir re-stats and the child inode must stay
// a DIRECTORY (not be replaced by a fileNode).
func TestM25DirSurvivesEntryTTL(t *testing.T) {
	h := newHarness(t)
	root := h.root()
	child, errno := root.Mkdir(h.ctx, "sub", 0, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Mkdir: %v", errno)
	}
	if child == nil || !child.IsDir() {
		t.Fatalf("Mkdir child is not a dir inode")
	}
	// Local mirror dir exists on disk.
	fi, err := os.Stat(h.fs.cachePath("sub"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("local mirror dir missing/not a dir: %v", err)
	}
	// Let the short entry TTL lapse, then re-lookup.
	time.Sleep(short + 100*time.Millisecond)
	out := &fuse.EntryOut{}
	got, errno := root.Lookup(h.ctx, "sub", out)
	if errno != 0 {
		t.Fatalf("Lookup after TTL: %v", errno)
	}
	if got == nil || !got.IsDir() {
		t.Fatalf("dir inode replaced after entry TTL (M25): isDir=%v", got != nil && got.IsDir())
	}
	// And statKey itself reports isDir for the dir key.
	isDir, _, _, stErr := h.fs.statKey(h.ctx, "sub")
	if stErr != 0 || !isDir {
		t.Fatalf("statKey(sub) = isDir=%v errno=%v, want true/0", isDir, stErr)
	}
}

// TestM26OpenWithOTruncDoesNotSilentlyTruncate - O_TRUNC must not reach
// the cache file: the local generation survives AND the row ends up
// dirty so the sync reconciles (the Setattr path is the legal truncate).
func TestM26OpenWithOTruncDoesNotSilentlyTruncate(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "precious bytes")
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	if etag, _ := h.idx.CleanETag("doc.txt"); etag == "" {
		t.Fatal("precondition: hydrated row must be clean with an ETag")
	}
	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	// The kernel sends open(O_RDWR|O_TRUNC) for `> file` / truncating
	// opens (syscall.O_TRUNC).
	truncFlags := uint32(os.O_RDWR) | uint32(os.O_TRUNC)
	fh, _, errno := fn.Open(h.ctx, truncFlags)
	if errno != 0 {
		t.Fatalf("Open(O_TRUNC): %v", errno)
	}
	// The cached generation must NOT have been truncated behind our back.
	if got := h.readLocal("doc.txt"); got != "precious bytes" {
		t.Fatalf("O_TRUNC silently truncated the cache: %q", got)
	}
	// The row must be dirty so the state converges (never a clean ETag
	// over bytes that changed under the mount).
	hdl := fh.(*fileHandle)
	if errno := fn.Flush(h.ctx, hdl); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatal("O_TRUNC open+close must leave the row dirty")
	}
	// The legal truncate path (Setattr with size) hydrates-then-truncates
	// and stays dirty; bytes re-hydrate from the server on the next scan.
	if err := h.fs.hydrate(h.ctx, "doc.txt"); err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}
	if got := h.readLocal("doc.txt"); got != "precious bytes" {
		t.Fatalf("server bytes did not re-hydrate: %q", got)
	}
}
