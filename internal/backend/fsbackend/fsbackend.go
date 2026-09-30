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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// fsbackend registers its constructor under the name "fs" at init time,
// per the leaf-pinned hook (consumed by leaf 03's registry/config wiring).
// The "single_bucket_bucket" option switches the instance into custom-bucket
// mode (root IS the bucket directory); its absence keeps the dataDir layout
// exactly as before.
func init() {
	backend.Register("fs", func(cfg backend.BackendConfig) (backend.Backend, error) {
		var f *FS
		var err error
		if b := cfg.Options[optSingleBucketBucket]; b != "" {
			f, err = NewAt(cfg.Root, b)
		} else {
			f, err = New(cfg.Root)
		}
		if err != nil {
			return nil, err
		}
		// BUGHUNT B11: optional per-object Put size cap override. Values
		// <= 0 or unparseable keep the 5 GiB default (fail-open to the
		// sane S3-aligned limit rather than refusing startup).
		if v := cfg.Options[optMaxPutBytes]; v != "" {
			if n, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil && n > 0 {
				f.maxPutBytes = n
			}
		}
		return f, nil
	})
}

// FS is a Backend rooted at a filesystem directory containing bucket
// subdirectories (the server's dataDir), or — single-bucket mode — at the
// custom bucket's own directory (its root IS the bucket). Safe for
// concurrent use.
type FS struct {
	root string
	// singleBucket pins the FS to one bucket: when non-empty, the root IS
	// that bucket's directory and object paths resolve to root/key (never
	// root/<bucket>/key). Set only via NewAt (explicitly-configured custom
	// buckets); the dataDir-rooted default keeps the nested layout.
	singleBucket string
	// maxPutBytes caps a single Put's buffered body (bughunt B11). Zero
	// means the default (maxPutBytesDefault). Configurable via the
	// optMaxPutBytes backend option; values <= 0 fall back to the default.
	maxPutBytes int64
}

// Compile-time assertion: *FS must satisfy the backend seam interface.
// (Mirrored in fsbackend_test.go; this copy fails the build at the source
// file when the interface drifts.)
var _ backend.Backend = (*FS)(nil)

// maxPutBytesDefault is the default per-object Put size cap: 5 GiB, S3's
// single-PUT object limit.
const maxPutBytesDefault int64 = 5 << 30

// optMaxPutBytes is the BackendConfig.Options key overriding maxPutBytes
// (decimal or 512-style binary GiB values via a plain int64 parse).
const optMaxPutBytes = "max_put_bytes"

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
	return &FS{root: abs, maxPutBytes: maxPutBytesDefault}, nil
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
