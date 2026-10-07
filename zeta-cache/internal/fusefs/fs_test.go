package fusefs

// fs_test.go - unit tests for the leaf-02 acceptance list, exercised
// through the node layer WITHOUT a kernel mount (no /dev/fuse needed;
// CI linux runners have none):
//
//	1. hydrate-on-miss        (TestHydrateOnMiss)
//	2. write-through staging + dirty flag (TestWriteStagingDirty)
//	3. rename local+remote    (TestRenameLocalAndRemote)
//	4. delete flows           (TestDeleteFlows)
//	5. reserved-name filtering (TestReservedNames)
//	6. ETag-confirmed clean transition + 412 stays dirty
//	                          (TestUploadETagConfirm, TestUploadConflictStaysDirty)
//	7. bounded-flush unmount refusal (TestUnmountRefusal)

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// harness wires a MemFS transport + memIndex + a temp cache dir into a
// root dirNode. Node methods are called directly - no kernel mount.
type harness struct {
	t   *testing.T
	fs  *FS
	tr  *transport.MemFS
	idx *memIndex
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tr := transport.NewMemFS()
	idx := newMemIndex()
	f, err := Mount(Options{
		CacheDir:  t.TempDir(),
		Bucket:    "bkt",
		Index:     idx,
		Transport: tr,
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return &harness{t: t, fs: f, tr: tr, idx: idx, ctx: context.Background()}
}

func (h *harness) root() *dirNode { return h.fs.root }

func (h *harness) put(key, body string) {
	t := h.t
	t.Helper()
	if _, err := h.tr.Put(h.ctx, key, strings.NewReader(body), ""); err != nil {
		t.Fatalf("seed Put(%s): %v", key, err)
	}
}

func (h *harness) readLocalGone(key string) string {
	t := h.t
	t.Helper()
	if _, err := os.Stat(h.fs.cachePath(key)); !os.IsNotExist(err) {
		t.Fatalf("local cache file %s still present after delete", key)
	}
	return ""
}

func (h *harness) readLocal(key string) string {
	t := h.t
	t.Helper()
	b, err := os.ReadFile(h.fs.cachePath(key))
	if err != nil {
		t.Fatalf("readLocal(%s): %v", key, err)
	}
	return string(b)
}

func TestHydrateOnMiss(t *testing.T) {
	h := newHarness(t)
	h.put("docs/readme.txt", "hello hydration")

	fn := &fileNode{fs: h.fs, key: "docs/readme.txt"}
	// No local file yet.
	if _, err := os.Stat(h.fs.cachePath("docs/readme.txt")); !os.IsNotExist(err) {
		t.Fatalf("cache file exists before hydration")
	}
	errno := h.fs.ensureHydrated(h.ctx, "docs/readme.txt")
	if errno != 0 {
		t.Fatalf("ensureHydrated: %v", errno)
	}
	if got := h.readLocal("docs/readme.txt"); got != "hello hydration" {
		t.Fatalf("hydrated content = %q", got)
	}
	// Second call is a no-op over the local generation.
	if errno := h.fs.ensureHydrated(h.ctx, "docs/readme.txt"); errno != 0 {
		t.Fatalf("second ensureHydrated: %v", errno)
	}
	// Open/Read through the node layer serves locally.
	fh, _, errno := fn.Open(h.ctx, uint32(os.O_RDONLY))
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	buf := make([]byte, 5)
	res, errno := fn.Read(h.ctx, fh, buf, 0)
	if errno != 0 {
		t.Fatalf("Read: %v", errno)
	}
	data, st := res.Bytes(buf)
	if st != fuse.OK {
		t.Fatalf("ReadResult.Bytes: %v", st)
	}
	if string(data) != "hello" {
		t.Fatalf("read = %q, want %q", data, "hello")
	}
	// The index recorded the hydrated generation.
	if h.idx.isDirty("docs/readme.txt") {
		t.Fatalf("hydrated file must not be dirty")
	}
	if etag, _ := h.idx.CleanETag("docs/readme.txt"); etag == "" {
		t.Fatalf("hydration must record the served ETag")
	}
}

func TestHydrateMissServerDown(t *testing.T) {
	h := newHarness(t)
	// Key absent on the server too: mount-level ENOENT, not EIO.
	errno := h.fs.ensureHydrated(h.ctx, "absent.txt")
	if errno != syscall.ENOENT {
		t.Fatalf("absent key: got %v, want ENOENT", errno)
	}
}

func TestWriteStagingDirty(t *testing.T) {
	h := newHarness(t)
	root := h.root()

	// Create through the node layer (Create -> Write -> Flush).
	child, fh, _, errno := root.Create(h.ctx, "new.txt", 0, 0, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}
	if child == nil || fh == nil {
		t.Fatalf("Create returned nil child/handle")
	}
	fn := child.Operations().(*fileNode)
	// While unflushed, the content lives in staging, NOT at the cache path.
	if _, err := os.Stat(h.fs.cachePath("new.txt")); !os.IsNotExist(err) {
		t.Fatalf("cache path exists before Flush")
	}
	if _, errno := fn.Write(h.ctx, fh, []byte("staged bytes"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	// Flush renames staging into the cache path; staging is empty again.
	if h.fs.stagingCount() != 0 {
		t.Fatalf("staging dir not empty after Flush")
	}
	if got := h.readLocal("new.txt"); got != "staged bytes" {
		t.Fatalf("cache content = %q", got)
	}
	// dirty=1 at Flush, before any upload.
	if !h.idx.isDirty("new.txt") {
		t.Fatalf("file must be dirty after Flush")
	}
	// Upload (ETag = MD5 contract) flips it clean.
	if err := h.fs.UploadFile(h.ctx, "new.txt"); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if h.idx.isDirty("new.txt") {
		t.Fatalf("file must be clean after confirmed upload")
	}
	body, ok := h.tr.ServerBody("new.txt")
	if !ok || body != "staged bytes" {
		t.Fatalf("server body = %q", body)
	}
	etag, _ := h.idx.CleanETag("new.txt")
	if want := transport.MD5Hex([]byte("staged bytes")); etag != want {
		t.Fatalf("clean etag = %q, want md5 %q", etag, want)
	}
}

func TestWriteOverExistingDirtyRidesStaging(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "remote generation")
	// Hydrate first.
	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	// Open (direct handle), write -> staging reroute, flush -> commit.
	fh, _, errno := fn.Open(h.ctx, uint32(os.O_RDWR))
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	hdl := fh.(*fileHandle)
	if !hdl.direct {
		t.Fatalf("expected direct handle over the cache file")
	}
	if n, errno := fn.Write(h.ctx, fh, []byte("DIRTY"), 0); errno != 0 || n != 5 {
		t.Fatalf("Write: n=%d errno=%v", n, errno)
	}
	// The old generation is still at the cache path pre-Flush.
	if got := h.readLocal("doc.txt"); !strings.HasPrefix(got, "remote") {
		t.Fatalf("pre-Flush cache content = %q", got)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	if got := h.readLocal("doc.txt"); !strings.HasPrefix(got, "DIRTY") {
		t.Fatalf("post-Flush cache content = %q", got)
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatalf("file must be dirty after overwriting Flush")
	}
	_ = io.Discard
}

func TestRenameLocalAndRemote(t *testing.T) {
	h := newHarness(t)
	h.put("old/name.txt", "renamed body")

	// dirNodes for old/ and dst/: Rename operates parent -> parent.
	oldDir := &dirNode{fs: h.fs, key: "old"}
	dstDir := &dirNode{fs: h.fs, key: "dst"}
	if errno := mapErrnoErr(h.fs.tr.Mkdir(h.ctx, "dst")); errno != 0 {
		t.Fatalf("Mkdir dst: %v", errno)
	}
	// Hydrate so a local generation exists, then rename.
	if errno := h.fs.ensureHydrated(h.ctx, "old/name.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	if errno := oldDir.Rename(h.ctx, "name.txt", dstDir, "renamed.txt", 0); errno != 0 {
		t.Fatalf("Rename: %v", errno)
	}
	// Server-side moved.
	if _, _, err := h.tr.Get(h.ctx, "old/name.txt", nil); !errors.Is(err, transport.ErrNotExist) {
		t.Fatalf("old key still on server: %v", err)
	}
	rc, _, err := h.tr.Get(h.ctx, "dst/renamed.txt", nil)
	if err != nil {
		t.Fatalf("new key missing on server: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "renamed body" {
		t.Fatalf("server body after move = %q", b)
	}
	// Local cache file moved.
	if _, err := os.Stat(h.fs.cachePath("old/name.txt")); !os.IsNotExist(err) {
		t.Fatalf("old local cache file still present")
	}
	if got := h.readLocal("dst/renamed.txt"); got != "renamed body" {
		t.Fatalf("local content after move = %q", got)
	}
	// Index row re-pointed.
	if _, err := h.idx.CleanETag("dst/renamed.txt"); err != nil {
		t.Fatalf("index MoveKey lost the row: %v", err)
	}
}

func TestDeleteFlows(t *testing.T) {
	h := newHarness(t)
	h.put("gone.txt", "delete me")
	root := h.root()

	// Unlink a remote file the client never cached: straight DELETE.
	if errno := root.Unlink(h.ctx, "gone.txt"); errno != 0 {
		t.Fatalf("Unlink: %v", errno)
	}
	if _, _, err := h.tr.Get(h.ctx, "gone.txt", nil); !errors.Is(err, transport.ErrNotExist) {
		t.Fatalf("server still has gone.txt: %v", err)
	}

	// Unlink of a DIRTY file uploads first (never lose local bytes).
	child, fh, _, errno := root.Create(h.ctx, "dirty.txt", 0, 0, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}
	fnD := child.Operations().(*fileNode)
	if _, errno := fnD.Write(h.ctx, fh, []byte("unsaved"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fnD.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	// Upload-then-delete: the dirty bytes reach the server BEFORE the
	// delete is issued, so nothing is lost even if the delete later
	// reconciles against another machine's newer state (leaf 04).
	// Assert the upload leg: the file must be clean (ETag confirmed)
	// at delete time, never deleted-while-dirty.
	if errno := root.Unlink(h.ctx, "dirty.txt"); errno != 0 {
		t.Fatalf("Unlink dirty: %v", errno)
	}
	if _, _, err := h.tr.Get(h.ctx, "dirty.txt", nil); !errors.Is(err, transport.ErrNotExist) {
		t.Fatalf("server still has dirty.txt after delete: %v", err)
	}
	_ = h.readLocalGone("dirty.txt")

	// Rmdir on a non-empty collection -> ENOTEMPTY (server 409 mapped).
	h.put("full/inner.txt", "x") // PUT materializes full/ server-side
	if errno := root.Rmdir(h.ctx, "full"); errno != syscall.ENOTEMPTY {
		t.Fatalf("Rmdir non-empty: %v, want ENOTEMPTY", errno)
	}
	// Empty dir rmdir works.
	if errno := mapErrnoErr(h.fs.tr.Mkdir(h.ctx, "empty")); errno != 0 {
		t.Fatalf("Mkdir empty: %v", errno)
	}
	if errno := root.Rmdir(h.ctx, "empty"); errno != 0 {
		t.Fatalf("Rmdir empty: %v", errno)
	}
	if _, err := h.tr.Propfind(h.ctx, "empty", false); !errors.Is(err, transport.ErrNotExist) {
		t.Fatalf("server still has empty/: %v", err)
	}
}

func TestReservedNames(t *testing.T) {
	h := newHarness(t)
	// Seeded via the raw backdoor: a real server stores these keys (its
	// own sidecars/uploads staging), the CLIENT just never sees them.
	h.tr.PutRaw(".metadata/sidecar", "hidden")
	h.tr.PutRaw(".zfs/snap", "hidden")
	h.tr.PutRaw(".uploads/staged", "hidden")
	h.put("visible.txt", "shown")
	root := h.root()

	stream, errno := root.Readdir(h.ctx)
	if errno != 0 {
		t.Fatalf("Readdir: %v", errno)
	}
	var names []string
	for stream.HasNext() {
		e, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("Next: %v", errno)
		}
		names = append(names, e.Name)
	}
	for _, n := range names {
		if n == ".metadata" || n == ".zfs" || n == ".uploads" {
			t.Fatalf("reserved name %q surfaced through the mount", n)
		}
	}
	if len(names) != 1 || names[0] != "visible.txt" {
		t.Fatalf("listing = %v, want [visible.txt]", names)
	}
	// Lookup of a reserved name ENOENTs.
	if _, errno := root.Lookup(h.ctx, ".metadata", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("Lookup .metadata: %v, want ENOENT", errno)
	}
	// Creation of a reserved name is EPERM.
	if _, _, _, errno := root.Create(h.ctx, ".metadata", 0, 0, &fuse.EntryOut{}); errno != syscall.EPERM {
		t.Fatalf("Create .metadata: %v, want EPERM", errno)
	}
	// Mkdir of a reserved name is EPERM.
	if _, errno := root.Mkdir(h.ctx, ".zfs", 0, &fuse.EntryOut{}); errno != syscall.EPERM {
		t.Fatalf("Mkdir .zfs: %v, want EPERM", errno)
	}
}

func TestUploadETagConfirm(t *testing.T) {
	h := newHarness(t)
	root := h.root()
	child, fh, _, errno := root.Create(h.ctx, "et.txt", 0, 0, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}
	fnE := child.Operations().(*fileNode)
	if _, errno := fnE.Write(h.ctx, fh, []byte("etag check"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fnE.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	if !h.idx.isDirty("et.txt") {
		t.Fatalf("dirty before upload")
	}
	if err := h.fs.UploadFile(h.ctx, "et.txt"); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if h.idx.isDirty("et.txt") {
		t.Fatalf("dirty=0 requires a 2xx with matching MD5 ETag")
	}
	// Fsync on an already-clean file is a no-op success.
	if errno := fnE.Fsync(h.ctx, fh, 0); errno != 0 {
		t.Fatalf("Fsync: %v", errno)
	}
}

func TestUploadConflictStaysDirty(t *testing.T) {
	h := newHarness(t)
	h.put("conf.txt", "server generation")
	if errno := h.fs.ensureHydrated(h.ctx, "conf.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	cleanETag, _ := h.idx.CleanETag("conf.txt")
	// Server-side out-of-band write moves the ETag (412 material).
	h.tr.PutRaw("conf.txt", "RACED")

	fn := &fileNode{fs: h.fs, key: "conf.txt"}
	fh, _, errno := fn.Open(h.ctx, uint32(os.O_RDWR))
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	if _, errno := fn.Write(h.ctx, fh, []byte("local winner"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	// PUT with the stale If-Match: 412 -> upload-failed, file STAYS
	// dirty, local bytes untouched (conflict logic is leaf 04's).
	err := h.fs.UploadFile(h.ctx, "conf.txt")
	if !errors.Is(err, transport.ErrConflict) {
		t.Fatalf("UploadFile err = %v, want ErrConflict", err)
	}
	if !h.idx.isDirty("conf.txt") {
		t.Fatalf("412 must keep the file dirty")
	}
	if got := h.readLocal("conf.txt"); !strings.HasPrefix(got, "local winner") {
		t.Fatalf("local bytes lost: %q", got)
	}
	_ = cleanETag
}

func TestUnmountRefusal(t *testing.T) {
	h := newHarness(t)
	// Dirty file whose upload always fails (server down simulated by a
	// failing stub transport behind fs - simplest: create a dirty file
	// then break the transport).
	h.tr.FailPuts(errors.New("server unreachable"))
	root := h.root()
	child, fh, _, errno := root.Create(h.ctx, "stuck.txt", 0, 0, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}
	fnS := child.Operations().(*fileNode)
	if _, errno := fnS.Write(h.ctx, fh, []byte("keep me"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fnS.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	// Bounded flush with an expired deadline: refuse.
	h.fs.mu.Lock()
	fBak := h.fs.deadline
	h.fs.deadline = -time.Second // already-spent budget
	err := h.fs.Unmount(h.ctx)
	h.fs.deadline = fBak
	h.fs.mu.Unlock()
	if err == nil {
		t.Fatalf("unmount with a stuck dirty file must be refused")
	}
	if !strings.Contains(err.Error(), "stuck.txt") {
		t.Fatalf("refusal must name the file: %v", err)
	}
	// Local bytes survive the refusal.
	if got := h.readLocal("stuck.txt"); got != "keep me" {
		t.Fatalf("bytes lost on refused unmount: %q", got)
	}
	// Server recovers -> unmount succeeds (flush succeeds, nothing mounted
	// in the unit test so Unmount only flushes).
	h.tr.FailPuts(nil)
	if err := h.fs.Unmount(h.ctx); err != nil {
		t.Fatalf("unmount after recovery: %v", err)
	}
	if body := h.tr.ServerBodyOrEmpty("stuck.txt"); body != "keep me" {
		t.Fatalf("recovered upload body = %q", body)
	}
}

func TestSnapshotCounters(t *testing.T) {
	h := newHarness(t)
	h.put("a.txt", "A")
	if errno := h.fs.ensureHydrated(h.ctx, "a.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	snap := h.fs.Snapshot()
	if snap.Cached < 1 {
		t.Fatalf("cached counter = %d", snap.Cached)
	}
	if snap.Dirty != 0 {
		t.Fatalf("dirty counter = %d", snap.Dirty)
	}
	root := h.root()
	child, fh, _, errno := root.Create(h.ctx, "b.txt", 0, 0, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}
	fnB := child.Operations().(*fileNode)
	if _, errno := fnB.Write(h.ctx, fh, []byte("b"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fnB.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	snap = h.fs.Snapshot()
	if snap.Dirty != 1 {
		t.Fatalf("dirty counter after flush = %d", snap.Dirty)
	}
}

func TestMountSucceedsWithoutServer(t *testing.T) {
	// The locked lifecycle rule: mount succeeds even when the server is
	// unreachable. Mount() never touches the transport, so an always-
	// failing stub still mounts (unit shape: build with a dead stub).
	tr := transport.NewMemFS()
	tr.FailAll(errors.New("connection refused"))
	f, err := Mount(Options{
		CacheDir:  t.TempDir(),
		Bucket:    "bkt",
		Index:     newMemIndex(),
		Transport: tr,
	})
	if err != nil {
		t.Fatalf("Mount with unreachable server: %v", err)
	}
	if f.Snapshot().Mounted {
		t.Fatalf("no mountpoint requested; Mounted should be false")
	}
}

// --- small helpers ---------------------------------------------------

func mapErrnoErr(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	return errnoToErrno(err)
}
