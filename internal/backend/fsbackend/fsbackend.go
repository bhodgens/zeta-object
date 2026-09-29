// Package fsbackend is the local-filesystem Backend implementation of the
// mini-s3 data-plane seam (internal/backend.Backend).
//
// ON-DISK LAYOUT IS FROZEN (byte-for-byte, master Contract 6):
//
//	<root>/<bucket>/<object-file>                # object data (or shadow: !data/<encoded-key>)
//	<root>/<bucket>/.metadata/<object>.meta      # ObjectMetadata JSON sidecar
//
// The sidecar JSON keys are exactly contentType, contentLength, eTag,
// customMetadata, lastModified, storagePath — the same wire form
// package main's handlers have always written. Existing buckets work
// with NO migration.
//
// Extraction history: this package is the leaf-02 extraction of the
// pre-seam package-main filesystem behavior (object_handlers.go /
// storage.go / object_paths.go). The atomic-write + per-object/per-dir
// lock TECHNIQUES are re-implemented in-package (atomic.go, paths.go);
// this package must NOT import package main.
//
// MULTIPART STAGING DECISION (leaf-02 Task 5, frozen per master Contract 4):
// multipart staging is S3-orchestration ABOVE the seam — package main's
// multipart_handlers.go keeps its direct-fs part staging in v1, and
// fsbackend.Put receives only whole-object writes. Option 2 ("parts stage
// fs-side; only CompleteMultipart produces a final assembled object via the
// existing direct-fs assembly") is chosen: it preserves today's part-write
// atomicity semantics exactly, whereas option 1 (Put accepting streamed
// unknown-size writes with buffer-to-temp staging) would change part-write
// behavior today for a future need. The option-1 extension is tracked for
// the backend-extension tree.
//
// CONCURRENCY: every method is safe for concurrent use. Writes serialize
// per data-file path AND per parent directory (the leaf-4.8 stress fix:
// the parent-dir lock closes the cleanupEmptyDirs check-then-act window
// against concurrent PUTs); readers hold both dir locks across
// stat→open so a concurrent Delete cannot prune directories out from
// under them.
package fsbackend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// fsbackend registers its constructor under the name "fs" at init time,
// per the leaf-pinned hook (consumed by leaf 03's registry/config wiring).
func init() {
	backend.Register("fs", func(cfg backend.BackendConfig) (backend.Backend, error) {
		return New(cfg.Root)
	})
}

// FS is a Backend rooted at a filesystem directory containing bucket
// subdirectories (the server's dataDir, or a custom bucket path when
// constructed per-bucket). Safe for concurrent use.
type FS struct {
	root string
}

// New returns an FS backend rooted at root. It does NOT create root (the
// server creates the data directory at startup; a missing root simply
// reports empty/missing buckets through the normal error paths).
func New(root string) (*FS, error) {
	if root == "" {
		return nil, errors.New("fsbackend: root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fsbackend: resolving root %q: %w", root, err)
	}
	return &FS{root: abs}, nil
}

// Root returns the backend's filesystem root (package-main's backendFor
// installer uses this to resolve custom buckets against the same instance).
func (f *FS) Root() string { return f.root }

// Capabilities reports what the fs backend can serve. The local filesystem
// backend is whole-object only in v1: no multipart-at-backend, no
// versioning, nothing immutable. Advisory metadata only — errors remain
// authoritative.
func (f *FS) Capabilities() objectmodel.CapabilitySet {
	return objectmodel.CapabilitySet{}
}

// Buckets discovers buckets: every direct child directory of root. It
// follows symlinks (os.Stat, never Lstat) and skips non-directories,
// matching the pre-seam discovery semantics in package main's
// listBucketsHandler. CreationDate is the directory's ModTime, the same
// value package main has always rendered.
func (f *FS) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(f.root)
	if err != nil {
		if os.IsNotExist(err) {
			return []objectmodel.BucketInfo{}, nil
		}
		return nil, backend.ToObjectModelError(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue // hidden dirs are control state, not buckets
		}
		fullPath := filepath.Join(f.root, entry.Name())
		info, statErr := os.Stat(fullPath) // os.Stat follows symlinks
		if statErr != nil || !info.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	out := make([]objectmodel.BucketInfo, 0, len(names))
	for _, name := range names {
		info, statErr := os.Stat(filepath.Join(f.root, name))
		mod := time.Time{}
		if statErr == nil {
			mod = info.ModTime()
		}
		out = append(out, objectmodel.BucketInfo{Name: name, CreatedAt: mod})
	}
	return out, nil
}

// putObjectLocks serializes writers per data-file path and per parent
// directory (the pre-seam lockObject pattern; leaf-4.8 parent-dir lock).
// FS carries its own sync.Map instances: package main's multipart flow
// keeps its own locks, and the two lock families must not alias.
var (
	fsObjectLocks sync.Map // map[string]*sync.Mutex keyed by path
)

// fsLockObject locks the mutex for path and returns the unlock func.
func fsLockObject(path string) func() {
	mu, _ := fsObjectLocks.LoadOrStore(path, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return func() { mu.(*sync.Mutex).Unlock() }
}
