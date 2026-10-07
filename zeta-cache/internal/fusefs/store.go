package fusefs

// store.go owns the local cache-dir data plane behind the mount:
//
//	<cacheDir>/files/<key>        hydrated object bytes (mirror of the
//	                              server namespace; full-file hydration)
//	<cacheDir>/staging/<uuid>     in-flight write staging, renamed into
//	                              files/ on Flush (leaf-02 dirty model)
//
// The dirty flag is the index's job (narrowStore persists through the
// narrow index seam); this file only moves bytes and tells the index
// what happened. The staging-then-rename dance never leaves partial
// bytes at the cache path, so a crash mid-write keeps the previous
// generation clean.

import (
	"context"
	"crypto/md5" // #nosec G401 -- the wire ETag IS the body MD5 (server contract); not a security use
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
	"github.com/google/uuid"
)

// cachePath is the local cache file for key (files/<key>).
func (f *FS) cachePath(key string) string {
	return filepath.Join(f.cacheDir, "files", filepath.FromSlash(key))
}

// stagingPath creates (and returns) a fresh staging temp file path.
func (f *FS) stagingPath() string {
	return filepath.Join(f.cacheDir, "staging", uuid.NewString())
}

// reserveCacheDirs creates files/ and staging/ once.
func (f *FS) reserveCacheDirs() error {
	for _, d := range []string{"files", "staging"} {
		if err := os.MkdirAll(filepath.Join(f.cacheDir, d), 0o700); err != nil {
			return fmt.Errorf("fusefs: creating cache %s dir: %w", d, err)
		}
	}
	return nil
}

// localStat returns the local file size/mtime if the cache file exists.
func (f *FS) localStat(key string) (size int64, mtime int64, ok bool, err error) {
	fi, err := os.Stat(f.cachePath(key))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("fusefs: stat %s: %w", key, err)
	}
	return fi.Size(), fi.ModTime().Unix(), true, nil
}

// hydrated reports whether the bytes are local (clean or dirty).
func (f *FS) hydrated(key string) bool {
	ok, _ := f.localStatOK(key)
	return ok
}

func (f *FS) localStatOK(key string) (bool, error) {
	_, _, ok, err := f.localStat(key)
	return ok, err
}

// hydrate full-file fetches key from the transport into a staging temp
// and renames it into files/<key>. Called on a read miss. Server
// unreachable = EIO, the mount stays up (locked lifecycle rule).
func (f *FS) hydrate(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hydrateLocked(ctx, key)
}

func (f *FS) hydrateLocked(ctx context.Context, key string) error {
	rc, info, err := f.tr.Get(ctx, key, nil) // full-file hydration; Range is the documented v1 stretch
	if err != nil {
		if errors.Is(err, transport.ErrNotExist) {
			return syscall.ENOENT
		}
		return syscall.EIO
	}
	defer rc.Close()
	tmp := f.stagingPath()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return syscall.EIO
	}
	if _, err := io.Copy(out, rc); err != nil { //nolint:gosec // G110: hydration is bounded by the server's Content-Length and the quota leaf; v1 accepts
		out.Close()
		os.Remove(tmp)
		return syscall.EIO
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return syscall.EIO
	}
	// Trust the transport's metadata for mtime/size when present.
	if info != nil && !info.ModTime.IsZero() {
		_ = os.Chtimes(tmp, info.ModTime, info.ModTime)
	}
	dst := f.cachePath(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		os.Remove(tmp)
		return syscall.EIO
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return syscall.EIO
	}
	// Record the hydrated generation in the index (narrow seam). The
	// server ETag travels with the row so leaf 04's scan can diff.
	etag := ""
	size := int64(0)
	if info != nil {
		etag = info.ETag
		size = info.Size
	}
	mt := int64(0)
	if info != nil && !info.ModTime.IsZero() {
		mt = info.ModTime.Unix()
	}
	if err := f.idx.RecordHydrated(key, etag, size, mt); err != nil {
		// Bytes are already local; a bookkeeping failure must not fail
		// the read. The next scan (leaf 04) repairs the row.
		f.log.Printf("fusefs: index RecordHydrated(%s): %v", key, err)
	}
	return nil
}

// stageDirty copies the current local generation (if any) into staging
// and returns the staging path. Writers append/patch there.
func (f *FS) stageDirty(key string) (string, error) {
	dst := f.stagingPath()
	src := f.cachePath(key)
	if _, err := os.Stat(src); err == nil {
		if err := copyFile(src, dst, 0o600); err != nil {
			return "", syscall.EIO
		}
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", syscall.EIO
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", syscall.EIO
	}
	return dst, out.Close()
}

func mtimeToTime(unix int64) time.Time {
	return time.Unix(unix, 0)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) // #nosec G304 -- src is a constructed cache path
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil { //nolint:gosec // local file copy
		out.Close()
		return err
	}
	return out.Close()
}

// commitStaging renames a staged file into files/<key> and flips the
// index row to dirty=1. Called on Flush (the leaf-02 dirty model:
// staging/<uuid> -> cache path at close, dirty flag at the same point,
// upload on the prompt-upload path).
func (f *FS) commitStaging(key, staging string, size, mtime int64) error {
	dst := f.cachePath(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return syscall.EIO
	}
	if err := os.Rename(staging, dst); err != nil {
		return syscall.EIO
	}
	if mtime > 0 {
		t := mtimeToTime(mtime)
		_ = os.Chtimes(dst, t, t)
	}
	// dirty=1 at Flush - from here the file exists ONLY locally and the
	// index (not the disk) is the crash-recovery record.
	if err := f.idx.MarkDirty(key, size, mtime); err != nil {
		f.log.Printf("fusefs: index MarkDirty(%s): %v", key, err)
	}
	return nil
}

// md5File hashes a local file - the client-side MD5 the leaf requires
// to verify the PUT ETag (the server's ETag IS the body MD5).
func md5File(path string) (string, int64, error) {
	in, err := os.Open(path) // #nosec G304 -- constructed cache path
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	h := md5.New() // #nosec G401 -- ETag contract, see file header
	n, err := io.Copy(h, in)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// UploadFile pushes one dirty cache file: PUT with If-Match of the last
// known clean ETag, verify the response ETag equals our MD5, then
// dirty=0. This is the entry point leaf 07's prompt-upload scheduler
// calls in on (leaf-02 doc: "this leaf provides the upload entry point
// as an interface").
func (f *FS) UploadFile(ctx context.Context, key string) error {
	local := f.cachePath(key)
	etag, size, err := md5File(local)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("fusefs: %s: %w", key, os.ErrNotExist)
		}
		return fmt.Errorf("fusefs: hashing %s: %w", key, err)
	}
	ifMatch, _ := f.idx.CleanETag(key)
	serverETag, err := f.tr.Put(ctx, key, mustSeek(local), ifMatch)
	if err != nil {
		// Server 412 (transport.ErrConflict) = lost race: upload-failed,
		// file STAYS dirty - conflict logic is leaf 04's. Any other
		// server error: same outcome. Never lose local bytes.
		return err
	}
	if serverETag != etag {
		// The stored bytes are not what we sent: stay dirty, re-diff on
		// the next scan (leaf 04).
		return fmt.Errorf("fusefs: %s: server etag %q != sent md5 %q", key, serverETag, etag)
	}
	if err := f.idx.MarkClean(key, serverETag, size); err != nil {
		f.log.Printf("fusefs: index MarkClean(%s): %v", key, err)
	}
	return nil
}

// mustSeek wraps a path in a ReadSeeker (Put requires Content-Length;
// the transport seeks to count the bytes).
func mustSeek(path string) io.ReadSeeker {
	f, err := os.Open(path) // #nosec G304 -- constructed cache path
	if err != nil {
		return errReader{err}
	}
	return f
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error)       { return 0, e.err }
func (e errReader) Seek(int64, int) (int64, error) { return 0, e.err }

// flushDirty uploads every dirty file, bounded by deadline (the SIGTERM
// path). Returns the keys still dirty after the deadline - unmount
// refuses when the list is non-empty.
func (f *FS) flushDirty(ctx context.Context, deadline timeAt) []string {
	keys, err := f.idx.DirtyKeys()
	if err != nil {
		f.log.Printf("fusefs: index DirtyKeys: %v", err)
		return nil
	}
	sort.Strings(keys)
	var stuck []string
	for i, k := range keys {
		if deadline.reached() {
			// Budget spent: everything not yet uploaded stays dirty and
			// is reported as stuck (unmount refuses on a non-empty list).
			stuck = append(stuck, keys[i:]...)
			break
		}
		if err := f.UploadFile(ctx, k); err != nil {
			f.log.Printf("fusefs: bounded flush %s: %v (staying dirty)", k, err)
			stuck = append(stuck, k)
		}
	}
	return stuck
}

// stagingCount is a diagnostic: files left in staging/ (a crashed write
// is recovered by leaf 03's daemon-start reconcile).
func (f *FS) stagingCount() int {
	entries, err := os.ReadDir(filepath.Join(f.cacheDir, "staging"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

// listDirEntries serves a directory listing for the mount: the union of
// remote siblings (transport Propfind) and local-only dirty files,
// reserved names filtered. Sorted by name; deterministic (go-fuse
// requires deterministic DirStreams).
type dirEntry struct {
	name  string
	isDir bool
	size  int64
	mtime int64
}

func (f *FS) listDirEntries(ctx context.Context, dirKey string) ([]dirEntry, error) {
	entries, err := f.tr.Propfind(ctx, dirKey, false)
	if err != nil && !errors.Is(err, transport.ErrNotExist) {
		return nil, err
	}
	seen := map[string]bool{}
	var out []dirEntry
	add := func(name string, isDir bool, size, mtime int64) {
		if seen[name] || name == "" || name == "/" {
			return
		}
		seen[name] = true
		out = append(out, dirEntry{name: name, isDir: isDir, size: size, mtime: mtime})
	}
	buildDirEntries(entries, dirKey, f, add)
	return out, nil
}

// buildDirEntries merges the transport listing with local-only rows.
func buildDirEntries(entries []transport.Entry, dirKey string, f *FS, add func(string, bool, int64, int64)) {
	prefix := ""
	if dirKey != "" {
		prefix = dirKey + "/"
	}
	for _, e := range entries {
		if e.Key == dirKey {
			continue
		}
		rel := strings.TrimPrefix(e.Key, prefix)
		if rel == "" {
			continue
		}
		// Only direct children surface; PROPFIND non-recursive already
		// guarantees that, but TrimSuffix keeps a trailing-slash dir key
		// from leaking.
		rel = strings.TrimSuffix(rel, "/")
		if strings.Contains(rel, "/") {
			continue
		}
		add(rel, e.IsDir, e.Size, e.ModTime.Unix())
	}
	// Local-only dirty files (written while offline) must surface even
	// when the server listing omits them.
	if rows, err := f.idx.ListPrefixed(dirKey); err == nil {
		for _, r := range rows {
			rel := strings.TrimPrefix(r.Key, prefix)
			rel = strings.TrimSuffix(rel, "/")
			if rel == "" || strings.Contains(rel, "/") {
				continue
			}
			add(rel, r.IsDir, r.Size, r.Mtime)
		}
	}
}

// renameGuard serializes Rename/Unlink/Rmdir against concurrent Flushes.
var _ sync.Locker = (*renameGuard)(nil)

type renameGuard struct{ mu sync.Mutex }

func (g *renameGuard) Lock()   { g.mu.Lock() }
func (g *renameGuard) Unlock() { g.mu.Unlock() }
