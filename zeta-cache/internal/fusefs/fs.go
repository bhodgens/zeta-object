// Package fusefs is the FUSE filesystem layer (client-cache-2026-10
// leaf 02): a mount serving the bucket namespace from the local cache
// dir + the narrow index seam + the transport interface.
//
// Library: github.com/hanwen/go-fuse/v2 (fs package, inode API).
// Rationale (leaf requires ONE pick, pure Go, no cgo):
//   - go-fuse is pure Go; macFUSE/libfuse is only the RUNTIME kernel
//     bridge the plan names as the dependency either way. cgofuse is
//     ruled out by the repo-wide cgo ban; jacobsa/fuse is Linux-only
//     in practice (no macOS mount path) while v1 is Linux + macOS.
//   - go-fuse is what the plan text names for the Linux platform
//     (master.md decision 2: "FUSE-only on Linux (go-fuse)").
//   - Unit tests exercise the node layer WITHOUT a kernel mount via
//     fs.ServerCallbacks stubs, so `go test -race` is green on CI
//     runners with no /dev/fuse.
//
// Operation mapping (full table in the leaf-02 report):
//
//	Lookup/Getattr  cache file present -> local stat; miss -> transport
//	                Propfind row (dir or remote file attrs); miss both ->
//	                ENOENT. Hydration happens on Open, not Lookup.
//	Setattr         size (truncate) + mtime on the local file; dirty=1
//	                via the same Flush path on release.
//	Readdir         transport Propfind(non-recursive) merged with
//	                local-only dirty rows; reserved names filtered.
//	Open/Read       miss -> full-file hydrate (staging temp + rename),
//	                then pread from the cache file. Server down -> EIO.
//	Write           writes land in a per-handle staging file.
//	Flush           staging -> rename into files/<key>, index dirty=1,
//	                then the prompt-upload hook (leaf 07 calls in; this
//	                leaf calls the hook synchronously when set).
//	Fsync           same as Flush (upload entry point), result to caller.
//	Create          fresh staging file + child inode.
//	Mkdir           transport MKCOL (local mirror dir created too).
//	Unlink/Rmdir    transport DELETE (404 -> ENOENT, non-empty -> ENOT-
//	                EMPTY); local cache file removed; dirty bytes are
//	                uploaded first when dirty (never lose local bytes).
//	Rename          transport MOVE (If-Match on destination; 412 ->
//	                EIO, file stays put) + index update + local rename.
//	Release         close the staging handle; nothing else (Flush did
//	                the commit).
package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Index is the NARROW index seam leaf 02 uses (leaf 03 replaces the
// implementation behind it - see the report's "for leaf 03" list).
type Index interface {
	// CleanETag returns the last confirmed-clean server ETag for key
	// ("" when unknown). Used as PUT If-Match.
	CleanETag(key string) (string, error)
	// MarkDirty records key as locally dirty (dirty=1) with the local
	// size/mtime. Called at Flush.
	MarkDirty(key string, size, mtime int64) error
	// MarkClean records a confirmed upload (dirty=0) with the server
	// ETag. Called only after a verified PUT.
	MarkClean(key string, etag string, size int64) error
	// RecordHydrated records a full-file hydration with the served ETag.
	RecordHydrated(key, etag string, size, mtime int64) error
	// DirtyKeys lists dirty file keys (the bounded-flush and IPC-status
	// source).
	DirtyKeys() ([]string, error)
	// DirtyCount / HydratedCount feed the IPC status counters.
	DirtyCount() (int, error)
	HydratedCount() (int, error)
	// ListPrefixed returns index rows whose key sits under dirKey
	// (direct children only matter to the FUSE layer). Local-only dirty
	// files must appear in listings even when the server omits them.
	ListPrefixed(dirKey string) ([]IndexRow, error)
	// Forget drops the index row for key (after a confirmed delete).
	Forget(key string) error
	// MoveKey re-points an index row after a confirmed rename.
	MoveKey(oldKey, newKey string) error
}

// IndexRow is one listing row from the index seam.
type IndexRow struct {
	Key   string
	IsDir bool
	Size  int64
	Mtime int64
}

// Uploader is the prompt-upload entry point (leaf 07's scheduler calls
// in on it). Leaf 02 calls it synchronously after Flush when set; nil
// means "stay dirty until the scheduler exists".
type Uploader interface {
	UploadNow(ctx context.Context, key string)
}

// Options configures the mount.
type Options struct {
	CacheDir string
	Bucket   string
	// Mountpoint is where the kernel bridge attaches. Empty in unit
	// tests (no mount; the node layer is exercised directly).
	Mountpoint string
	// UploadDeadline bounds the SIGTERM flush path (leaf: bounded flush,
	// then refuse unmount). Zero = 5s.
	UploadDeadline time.Duration
	// Uploader is the prompt-upload hook (leaf 07); optional.
	Uploader Uploader
	// Index is the narrow index seam (required).
	Index Index
	// Transport is the webdav seam (required).
	Transport transport.Transport
	// Logger is optional; defaults to a discard logger.
	Logger *log.Logger
}

// FS is the mounted filesystem root handle.
type FS struct {
	root *dirNode

	cacheDir string
	bucket   string
	tr       transport.Transport
	idx      Index
	up       Uploader
	log      *log.Logger
	deadline time.Duration

	mu      sync.Mutex         // guards hydrate + commit paths
	rmu     sync.Mutex         // guards rename/unlink/rmdir vs flush
	targets map[string]*target // mountpoint -> server handle
}

// target is one active mount (the daemon mounts one bucket; the map
// keeps Unmount honest if a future leaf mounts more).
type target struct {
	mountpoint string
	server     *fuse.Server
}

// Mount builds the filesystem and, when opts.Mountpoint is set, mounts
// it. Mount SUCCEEDS even when the server is unreachable (locked
// lifecycle rule): nothing here touches the transport.
func Mount(opts Options) (*FS, error) {
	if opts.Index == nil || opts.Transport == nil {
		return nil, fmt.Errorf("fusefs: index and transport are required")
	}
	if opts.CacheDir == "" {
		return nil, fmt.Errorf("fusefs: CacheDir is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = discardLog()
	}
	deadline := opts.UploadDeadline
	if deadline == 0 {
		deadline = 5 * time.Second
	}
	f := &FS{
		cacheDir: opts.CacheDir,
		bucket:   opts.Bucket,
		tr:       opts.Transport,
		idx:      opts.Index,
		up:       opts.Uploader,
		log:      logger,
		deadline: deadline,
		targets:  map[string]*target{},
	}
	if err := f.reserveCacheDirs(); err != nil {
		return nil, err
	}
	// The root is passed UNINITIALIZED: fs.NewNodeFS stamps the bridge
	// onto the embedded Inode itself (initInode in go-fuse's
	// bridge.go). Building the bridge ALWAYS (not only when mounting)
	// keeps the node layer unit-testable without a kernel mount.
	root := &dirNode{fs: f, key: ""}
	f.root = root
	// Pure-Go daemon; the kernel bridge (macFUSE on darwin, fuse3 on
	// linux) is the runtime dependency.
	mOpts := fuse.MountOptions{
		Name:          "zeta-cache",
		FsName:        "zeta-cache",
		DisableXAttrs: true, // v1: no xattr surface (leaf 06 may revisit)
		MaxWrite:      1 << 20,
	}
	fOpts := &fs.Options{
		MountOptions: mOpts,
		// Short attr/entry TTLs keep multi-machine coherence snappy;
		// the sync scan (leaf 04) is the real consistency tool.
		EntryTimeout: &short,
		AttrTimeout:  &short,
	}
	rawFS := fs.NewNodeFS(root, fOpts)

	if opts.Mountpoint != "" {
		srv, err := fuse.NewServer(rawFS, opts.Mountpoint, &mOpts)
		if err != nil {
			return nil, fmt.Errorf("fusefs: mounting %s: %w", opts.Mountpoint, err)
		}
		go srv.Serve()
		if err := srv.WaitMount(); err != nil {
			return nil, fmt.Errorf("fusefs: waiting for mount %s: %w", opts.Mountpoint, err)
		}
		f.targets[opts.Mountpoint] = &target{mountpoint: opts.Mountpoint, server: srv}
	}
	return f, nil
}

// NewPersistentInode is a package-level helper (go-fuse exposes inode
// creation through *Inode methods; the root has none before it exists,
// so the root bootstraps through its own embedded Inode).
var _ = (*fs.Inode)(nil)

var short = 250 * time.Millisecond

// Unmount detaches the mount. Bounded-flush-then-refuse: with dirty
// files the flush budget is opts.UploadDeadline; anything still dirty
// after it refuses the unmount (process stays alive, error state via
// the leaf-01 IPC status - main.go owns reporting).
func (f *FS) Unmount(ctx context.Context) error {
	// Bounded flush FIRST - the refusal must trigger even when the
	// kernel-side mount is already gone (crashed FUSE bridge) or in
	// unit tests where no kernel mount exists.
	budget := deadlineAfter(f.deadline)
	if f.deadline < 0 {
		budget = expired() // negative = already spent ("no budget")
	}
	if stuck := f.flushDirty(ctx, budget); len(stuck) > 0 {
		return fmt.Errorf("fusefs: unmount refused: %d file(s) still dirty (upload failed or deadline %s hit): %s",
			len(stuck), f.deadline, strings.Join(stuck, ", "))
	}
	for mp, t := range f.targets {
		if err := t.server.Unmount(); err != nil {
			return fmt.Errorf("fusefs: unmount %s: %w", mp, err)
		}
		f.mu.Lock()
		delete(f.targets, mp)
		f.mu.Unlock()
	}
	return nil
}

// Mounted reports whether any mount is live.
func (f *FS) Mounted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.targets) > 0
}

// ---- stat plumbing -------------------------------------------------

func modeDir(flag bool) uint32 {
	if flag {
		return fuse.S_IFDIR | 0o755
	}
	return fuse.S_IFREG | 0o644
}

func fillAttr(out *fuse.Attr, key string, isDir bool, size, mtime int64) {
	out.Mode = modeDir(isDir)
	out.Size = uint64(max(size, 0)) // #nosec G115 -- size is a stat result, clamped non-negative
	out.Mtime = uint64(mtime)
	out.Atime = out.Mtime
	out.Ctime = out.Mtime
	out.Ctimensec = 0
	out.Nlink = 1
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	out.Blksize = 4096
	_ = key
}

// statKey resolves one key to display attributes WITHOUT hydrating:
// local generation wins (it may be dirtier/newer than the server);
// otherwise the transport listing answers.
func (f *FS) statKey(ctx context.Context, key string) (isDir bool, size, mtime int64, errno syscall.Errno) {
	if size, mt, ok, err := f.localStat(key); err == nil && ok {
		return false, size, mt, 0
	} else if err != nil {
		return false, 0, 0, syscall.EIO
	}
	entries, err := f.tr.Propfind(ctx, key, false)
	if errors.Is(err, transport.ErrNotExist) {
		// Local-only dirty row?
		if rows, ierr := f.idx.ListPrefixed(parentKey(key)); ierr == nil {
			for _, r := range rows {
				if r.Key == key && !r.IsDir {
					return false, r.Size, r.Mtime, 0
				}
			}
		}
		return false, 0, 0, syscall.ENOENT
	}
	if err != nil {
		return false, 0, 0, syscall.EIO
	}
	for _, e := range entries {
		if e.Key == key {
			mt := e.ModTime.Unix()
			return e.IsDir, e.Size, mt, 0
		}
	}
	return false, 0, 0, syscall.ENOENT
}

func parentKey(key string) string {
	_, parent, found := strings.CutLast(key, "/")
	if !found {
		return ""
	}
	return parent
}

// inoHash is the FNV-1a basis for stable inode numbers.
const inoHash uint64 = 14695981039346656037

// inoFor derives a STABLE inode number from the key (FNV-1a of the key
// path). Stable Ino per StableAttr means go-fuse reuses the same inode
// when the kernel re-looks-up a forgotten node, keeping stat()
// consistent across listing/Lookup churn. Reserved Ino values are
// avoided by skipping exactly 0 and ^uint64(0).
func (f *FS) inoFor(dir, name string, isDir bool) uint64 {
	h := inoHash
	s := dir + "/" + name
	if isDir {
		s = "d/" + s
	}
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	if h == 0 || h == ^uint64(0) {
		h = 1
	}
	return h
}

// childKey joins a directory key and a child name.
func childKey(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// reserved mirrors the server's reserved-name gate (top-level segment
// only, same as the transport stub's reservedKey).
func reserved(name string) bool {
	switch name {
	case ".metadata", ".zfs", ".uploads":
		return true
	}
	return false
}

// ---- inodes --------------------------------------------------------

// fileNode is one regular file in the namespace.
type fileNode struct {
	fs.Inode
	fs  *FS
	key string
}

// dirNode is one collection (the root is the bucket itself).
type dirNode struct {
	fs.Inode
	fs  *FS
	key string
}

var (
	_ fs.InodeEmbedder = (*fileNode)(nil)
	_ fs.InodeEmbedder = (*dirNode)(nil)
)

// newChild builds a child inode for parent (NewInode keeps it only
// while the kernel holds a reference; go-fuse reclaims forgotten nodes
// and our Lookup re-creates them from the store, so nothing leaks).
func (d *dirNode) newChild(ctx context.Context, name string, isDir bool) *fs.Inode {
	if isDir {
		ch := &dirNode{fs: d.fs, key: childKey(d.key, name)}
		return d.NewInode(ctx, ch, fs.StableAttr{Mode: fuse.S_IFDIR, Ino: d.fs.inoFor(d.key, name, true)})
	}
	ch := &fileNode{fs: d.fs, key: childKey(d.key, name)}
	return d.NewInode(ctx, ch, fs.StableAttr{Mode: fuse.S_IFREG, Ino: d.fs.inoFor(d.key, name, false)})
}

// lookupRows resolves the child's attrs through statKey and returns a
// child inode (Lookup per the go-fuse inode API).
func (d *dirNode) lookupChild(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.ENOENT
	}
	key := childKey(d.key, name)
	isDir, size, mtime, errno := d.fs.statKey(ctx, key)
	if errno != 0 {
		return nil, errno
	}
	child := d.GetChild(name)
	if child == nil || (isDir && !child.IsDir()) || (!isDir && child.IsDir()) {
		child = d.newChild(ctx, name, isDir)
	}
	out.Mode = modeDir(isDir)
	fillAttr(&out.Attr, key, isDir, size, mtime)
	out.SetEntryTimeout(short)
	out.SetAttrTimeout(short)
	return child, 0
}

// ---- dirNode: directory operations ---------------------------------

// Lookup implements fs.NodeLookuper.
func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return d.lookupChild(ctx, name, out)
}

// Getattr implements fs.NodeGetattrer.
func (d *dirNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillAttr(&out.Attr, d.key, true, 0, time.Now().Unix())
	return 0
}

// Setattr on a directory: mode/chown accepted in-memory only.
func (d *dirNode) Setattr(_ context.Context, _ fs.FileHandle, _ *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	fillAttr(&out.Attr, d.key, true, 0, time.Now().Unix())
	return 0
}

// Readdir implements fs.NodeReaddirer (both READDIR and READDIRPLUS
// route here in go-fuse; ReadDirPlus needs no separate method).
func (d *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := d.fs.listDirEntries(ctx, d.key)
	if err != nil {
		if errors.Is(err, transport.ErrNotExist) {
			return nil, syscall.ENOENT
		}
		return nil, syscall.EIO
	}
	out := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		mode := uint32(fuse.S_IFREG)
		if e.isDir {
			mode = fuse.S_IFDIR
		}
		out = append(out, fuse.DirEntry{Name: e.name, Mode: mode})
	}
	return &dirStream{entries: out}, 0
}

type dirStream struct {
	entries []fuse.DirEntry
	i       int
}

func (s *dirStream) HasNext() bool { return s.i < len(s.entries) }

func (s *dirStream) Next() (fuse.DirEntry, syscall.Errno) {
	e := s.entries[s.i]
	s.i++
	return e, 0
}

func (s *dirStream) Close() {}

// Statfs implements fs.NodeStatfser (macOS mounts require it).
func (d *dirNode) Statfs(_ context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.fs.cacheDir, &st); err != nil {
		return syscall.EIO
	}
	out.FromStatfsT(&st)
	return 0
}

// Mkdir implements fs.NodeMkdirer: transport MKCOL + local mirror dir.
func (d *dirNode) Mkdir(ctx context.Context, name string, _ uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.EPERM
	}
	key := childKey(d.key, name)
	if err := d.fs.tr.Mkdir(ctx, key); err != nil {
		return nil, mapTransportErr(err)
	}
	if err := os.MkdirAll(filepath.Join(d.fs.cacheDir, "files", filepath.FromSlash(key)), 0o700); err != nil {
		d.fs.log.Printf("fusefs: mkdir local mirror %s: %v", key, err)
	}
	if err := d.fs.idx.MarkClean(key, "", 0); err != nil { // dir row, clean by definition
		d.fs.log.Printf("fusefs: index MarkClean(dir %s): %v", key, err)
	}
	child := d.newChild(ctx, name, true)
	fillAttr(&out.Attr, key, true, 0, time.Now().Unix())
	return child, 0
}

// Unlink implements fs.NodeUnlinker.
func (d *dirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if reserved(name) {
		return syscall.EPERM
	}
	return d.fs.removeChild(ctx, childKey(d.key, name))
}

// Rmdir implements fs.NodeRmdirer: transport DELETE; the server answers
// 404 on missing, 409-ish when non-empty -> ENOTEMPTY.
func (d *dirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if reserved(name) {
		return syscall.EPERM
	}
	return d.fs.removeChild(ctx, childKey(d.key, name))
}

// Rename implements fs.NodeRenamer: transport MOVE (atomic server-side;
// MOVE enforces If-Match on the destination) + index update + local
// rename.
func (d *dirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, _ uint32) syscall.Errno {
	if reserved(name) || reserved(newName) {
		return syscall.EPERM
	}
	nd, ok := newParent.(*dirNode)
	if !ok {
		return syscall.EPERM
	}
	oldKey := childKey(d.key, name)
	newKey := childKey(nd.key, newName)
	return d.fs.renameKey(ctx, oldKey, newKey)
}

// ---- fileNode: file operations --------------------------------------

// Getattr implements fs.NodeGetattrer.
func (fn *fileNode) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	isDir, size, mtime, errno := fn.fs.statKey(ctx, fn.key)
	if errno != 0 {
		return errno
	}
	if isDir {
		return syscall.EIO // type changed underneath us; refetch via lookup
	}
	fillAttr(&out.Attr, fn.key, false, size, mtime)
	return 0
}

// Setattr implements fs.NodeSetattrer: truncate (size) and mtime updates
// land on the local generation and mark it dirty at release/Flush.
func (fn *fileNode) Setattr(ctx context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	f := fn.fs
	path := f.cachePath(fn.key)
	st, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return syscall.EIO
		}
		// Truncate-on-miss hydrates first (never clobber a remote
		// generation with a local truncation of unknown content).
		if _, ok := in.GetSize(); ok {
			if hErr := f.hydrate(ctx, fn.key); hErr != nil {
				return errnoToErrno(hErr)
			}
		} else {
			return syscall.ENOENT
		}
		st, err = os.Stat(path)
		if err != nil {
			return syscall.EIO
		}
	}
	size := st.Size()
	mtime := st.ModTime().Unix()
	if sz, ok := in.GetSize(); ok {
		if err := os.Truncate(path, int64(sz)); err != nil {
			return syscall.EIO
		}
		size = int64(sz)
	}
	if mt, ok := in.GetMTime(); ok {
		t := mt
		if err := os.Chtimes(path, t, t); err != nil {
			return syscall.EIO
		}
		mtime = t.Unix()
	}
	if err := f.idx.MarkDirty(fn.key, size, mtime); err != nil {
		f.log.Printf("fusefs: index MarkDirty(%s) [setattr]: %v", fn.key, err)
	}
	fillAttr(&out.Attr, fn.key, false, size, mtime)
	return 0
}

// Open implements fs.NodeOpener: hydrate on miss, then hand back a
// handle over the LOCAL cache file (reads never touch the server).
func (fn *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if err := fn.fs.ensureHydrated(ctx, fn.key); err != 0 {
		return nil, 0, err
	}
	path := fn.fs.cachePath(fn.key)
	fh, err := os.OpenFile(path, int(flags), 0o600) // #nosec G304 -- constructed cache path
	if err != nil {
		return nil, 0, syscall.EIO
	}
	return &fileHandle{fs: fn.fs, key: fn.key, stagingPath: path, local: fh, direct: true}, 0, 0
}

// Read implements fs.NodeReader.
func (fn *fileNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h, ok := fh.(*fileHandle)
	if !ok || h == nil {
		// Un-opened read: hydrate + one-shot pread.
		if errno := fn.fs.ensureHydrated(ctx, fn.key); errno != 0 {
			return nil, errno
		}
		return readLocal(fn.fs.cachePath(fn.key), dest, off)
	}
	return h.Read(dest, off)
}

// Write implements fs.NodeWriter.
func (fn *fileNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	h, ok := fh.(*fileHandle)
	if !ok || h == nil {
		return 0, syscall.EIO
	}
	return h.Write(ctx, data, off)
}

// Flush implements fs.NodeFlusher (close(2)): commit staging ->
// files/<key>, index dirty=1, then the prompt-upload hook.
func (fn *fileNode) Flush(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	h, ok := fh.(*fileHandle)
	if !ok || h == nil {
		return 0
	}
	return h.Flush(ctx)
}

// Fsync implements fs.NodeFsyncer: the upload entry point, errors
// surface to the caller.
func (fn *fileNode) Fsync(ctx context.Context, fh fs.FileHandle, _ uint32) syscall.Errno {
	h, ok := fh.(*fileHandle)
	if !ok || h == nil {
		return 0
	}
	return h.Fsync(ctx)
}

// Release implements fs.NodeReleaser: close the descriptor; the commit
// happened in Flush.
func (fn *fileNode) Release(_ context.Context, fh fs.FileHandle) syscall.Errno {
	h, ok := fh.(*fileHandle)
	if !ok || h == nil {
		return 0
	}
	return h.Release()
}

// ---- Create ---------------------------------------------------------

// Create implements fs.NodeCreater on the directory: fresh staging file
// + child inode (the leaf operation list's Create).
func (d *dirNode) Create(ctx context.Context, name string, flags uint32, _ uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if reserved(name) {
		return nil, nil, 0, syscall.EPERM
	}
	key := childKey(d.key, name)
	f := d.fs
	f.mu.Lock()
	staging, err := f.stageDirty(key)
	f.mu.Unlock()
	if err != nil {
		return nil, nil, 0, errnoToErrno(err)
	}
	fh, err := os.OpenFile(staging, os.O_RDWR, 0o600) // #nosec G304 -- staging path we just created
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	child := d.GetChild(name)
	if child == nil || child.IsDir() {
		child = d.newChild(ctx, name, false)
	}
	fillAttr(&out.Attr, key, false, 0, time.Now().Unix())
	return child, &fileHandle{fs: f, key: key, stagingPath: staging, local: fh}, 0, 0
}

// ---- fileHandle ------------------------------------------------------

// fileHandle is the open-file state: writes land in a staging copy,
// Flush renames it into files/<key> and flips dirty=1.
type fileHandle struct {
	fs          *FS
	key         string
	stagingPath string
	local       *os.File // descriptor over stagingPath (staging) or the cache file (direct)
	direct      bool     // opened read-only over the local generation
	committed   bool
}

func (h *fileHandle) Read(dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if h.direct {
		return fuse.ReadResultFd(h.local.Fd(), off, len(dest)), 0
	}
	n, err := h.local.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *fileHandle) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if h.direct {
		// A write on a direct handle re-routes to staging.
		staging, err := h.fs.stageDirty(h.key)
		if err != nil {
			return 0, errnoToErrno(err)
		}
		out, err := os.OpenFile(staging, os.O_WRONLY, 0o600)
		if err != nil {
			return 0, syscall.EIO
		}
		if in, err := os.Open(h.stagingPath); err == nil { // #nosec G304 -- constructed cache path
			if _, err := io.Copy(out, in); err != nil {
				in.Close()
				out.Close()
				return 0, syscall.EIO
			}
			in.Close()
		}
		h.stagingPath = staging
		h.local = out
		h.direct = false
	}
	n, err := h.local.WriteAt(data, off)
	if err != nil {
		return 0, syscall.EIO
	}
	return uint32(n), 0
}

// Flush commits the staging generation: rename into files/<key>,
// dirty=1 in the index, then the prompt-upload hook. NEVER loses local
// bytes: the rename is atomic and errors keep the staging file.
func (h *fileHandle) Flush(ctx context.Context) syscall.Errno {
	if h.direct || h.committed {
		return 0
	}
	f := h.fs
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if err := h.local.Sync(); err != nil {
		return syscall.EIO
	}
	size := fileSizeOrZero(h.stagingPath)
	mtime := time.Now().Unix()
	f.mu.Lock()
	err := f.commitStaging(h.key, h.stagingPath, size, mtime)
	f.mu.Unlock()
	if err != nil {
		return errnoToErrno(err)
	}
	h.committed = true
	// Reopen the handle over the committed cache file so late reads see
	// the new generation.
	if in, err := os.Open(f.cachePath(h.key)); err == nil { // #nosec G304 -- constructed cache path
		h.local.Close()
		h.local = in
		h.stagingPath = f.cachePath(h.key)
		h.direct = true
	}
	if f.up != nil {
		f.up.UploadNow(ctx, h.key) // prompt upload (leaf 07 scheduler)
	}
	return 0
}

// Fsync pushes the upload synchronously: success ONLY when the server
// confirmed 2xx with a matching MD5 ETag (then dirty=0). 412 or any
// other failure: EIO to the caller, file stays dirty.
func (h *fileHandle) Fsync(ctx context.Context) syscall.Errno {
	if err := h.Flush(ctx); err != 0 {
		return err
	}
	if h.direct && !h.committed {
		return 0
	}
	if err := h.fs.UploadFile(ctx, h.key); err != nil {
		h.fs.log.Printf("fusefs: fsync upload %s failed (staying dirty): %v", h.key, err)
		return syscall.EIO
	}
	return 0
}

// Release closes the descriptor. A never-flushed handle (dup'ed fd
// closed without close) leaves staging in place - leaf 03's reconcile
// sweeps orphaned staging files at daemon start.
func (h *fileHandle) Release() syscall.Errno {
	if err := h.local.Close(); err != nil {
		return syscall.EIO
	}
	return 0
}

func fileSizeOrZero(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func readLocal(path string, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	in, err := os.Open(path) // #nosec G304 -- constructed cache path
	if err != nil {
		return nil, syscall.EIO
	}
	defer in.Close()
	n, err := in.ReadAt(dest, off)
	if err != nil && n == 0 {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

// ensureHydrated fetches the remote generation when nothing is local.
// Dirty local generations are NEVER overwritten by hydration.
func (f *FS) ensureHydrated(ctx context.Context, key string) syscall.Errno {
	if f.hydrated(key) {
		return 0
	}
	return errnoToErrno(f.hydrate(ctx, key))
}

// removeChild implements Unlink/Rmdir bodies: dirty bytes upload first
// (never lose local bytes), then transport DELETE, then local cleanup.
func (f *FS) removeChild(ctx context.Context, key string) syscall.Errno {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	// Upload-then-delete when dirty: a delete racing the upload is
	// reconciled by leaf 04's scan; losing the bytes is not an option.
	if dirty, err := f.idx.DirtyKeys(); err == nil {
		for _, k := range dirty {
			if k == key {
				if uerr := f.UploadFile(ctx, key); uerr != nil {
					f.log.Printf("fusefs: delete of dirty %s: upload failed, refusing (stays dirty): %v", key, uerr)
					return syscall.EIO
				}
			}
		}
	}
	if err := f.tr.Delete(ctx, key, ""); err != nil {
		return mapTransportErr(err)
	}
	if err := os.RemoveAll(f.cachePath(key)); err != nil {
		f.log.Printf("fusefs: remove local %s: %v", key, err)
	}
	if err := f.idx.Forget(key); err != nil {
		f.log.Printf("fusefs: index Forget(%s): %v", key, err)
	}
	return 0
}

// renameKey implements Rename bodies: transport MOVE (If-Match on the
// destination; 412 = EIO, nothing changes) + local rename + index
// update. Staged/dirty bytes ride along.
func (f *FS) renameKey(ctx context.Context, oldKey, newKey string) syscall.Errno {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	// Local dirty generation? Push it first so the MOVE moves the NEW
	// content, not the stale server generation.
	if dirty, err := f.idx.DirtyKeys(); err == nil {
		for _, k := range dirty {
			if k == oldKey {
				if uerr := f.UploadFile(ctx, oldKey); uerr != nil {
					f.log.Printf("fusefs: rename of dirty %s: upload failed, refusing: %v", oldKey, uerr)
					return syscall.EIO
				}
			}
		}
	}
	// If-Match on the destination: empty (no known clean dest) means no
	// precondition; a known dest ETag makes the server 412 on a lost
	// race (which maps to EIO here - conflict logic is leaf 04's).
	destETag := ""
	if etag, err := f.idx.CleanETag(newKey); err == nil {
		destETag = etag
	}
	if err := f.tr.Move(ctx, oldKey, newKey, destETag); err != nil {
		return mapTransportErr(err)
	}
	// Local rename (ignore missing local generations).
	oldPath := f.cachePath(oldKey)
	newPath := f.cachePath(newKey)
	if _, err := os.Stat(oldPath); err == nil {
		if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
			return syscall.EIO
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			f.log.Printf("fusefs: local rename %s -> %s: %v", oldKey, newKey, err)
		}
	} else if dirErr := os.MkdirAll(filepath.Dir(newPath), 0o700); dirErr != nil {
		f.log.Printf("fusefs: rename mkdir %s: %v", filepath.Dir(newPath), dirErr)
	}
	if err := f.idx.MoveKey(oldKey, newKey); err != nil {
		f.log.Printf("fusefs: index MoveKey(%s -> %s): %v", oldKey, newKey, err)
	}
	return 0
}

// errnoToErrno converts the store layer's error returns (which carry
// syscall errno values for ENOENT/EIO) into mount errnos.
func errnoToErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if en, ok := errors.AsType[syscall.Errno](err); ok {
		return en
	}
	return syscall.EIO
}

// mapTransportErr converts transport errors to errno for the mount.
func mapTransportErr(err error) syscall.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, transport.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, transport.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, transport.ErrConflict): // 412: upload-failed, stays dirty; leaf 04 owns conflicts
		return syscall.EIO
	case errors.Is(err, transport.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, transport.ErrNoParent):
		return syscall.ENOENT
	default:
		return syscall.EIO
	}
}

// StatusSnapshot is what main.go feeds the leaf-01 IPC status: the
// dirty/cached counters come from the index, not guesses.
type StatusSnapshot struct {
	Dirty      int
	Cached     int
	Staging    int
	Mounted    bool
	UnmountErr string
}

// Snapshot returns the current counters (IPC status source).
func (f *FS) Snapshot() StatusSnapshot {
	dirty, _ := f.idx.DirtyCount()
	cached, _ := f.idx.HydratedCount()
	f.mu.Lock()
	mounted := len(f.targets) > 0
	f.mu.Unlock()
	return StatusSnapshot{Dirty: dirty, Cached: cached, Staging: f.stagingCount(), Mounted: mounted}
}
