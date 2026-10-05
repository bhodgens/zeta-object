// export_versioning_test_surface.go — the cross-package test surface
// quic-h3-2026-10 leaf 05's webdav parity tests need (the ONLY consumer
// of these aliases is internal/frontend/webdav's versioning_test.go).
// The versioning machinery itself (capture/record/stores/seams) stays
// unexported; webdav drives it through the REAL handler and seam
// surfaces, never around them. Extends export_test_surface.go's
// test-only convention — no production code path depends on these.
package s3

import (
	"io"
	"net/http"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
)

// SetupVersioningTestEnv installs the s3 package's process seams
// (config view + fs-root resolver + backend lookup on one real fs) for
// a cross-package parity test rooted at dataDir.
func SetupVersioningTestEnv(t *testing.T, dataDir string) {
	t.Helper()
	f, err := fsbackend.New(dataDir)
	if err != nil {
		t.Fatalf("fsbackend.New: %v", err)
	}
	installServerConfigView(serverConfigView{DataDir: dataDir + "/"})
	installFSRootResolver(func(bucket string) string {
		return dataDir + "/" + bucket
	})
	installBackendLookup(func(bucket string) (backend.Backend, error) {
		return f, nil
	})
	t.Cleanup(func() {
		installBackendLookup(nil)
		installFSRootResolver(nil)
	})
}

// TestBackend returns the Backend the last SetupVersioningTestEnv
// installed (the webdav frontend shares the SAME data plane).
func TestBackend() backend.Backend {
	if fn := installedBackendLookup(); fn != nil {
		if b, err := fn(""); err == nil && b != nil {
			return b
		}
	}
	return nil
}

// PutBucketVersioningHandlerForTest drives the REAL ?versioning PUT
// handler from a cross-package test.
func PutBucketVersioningHandlerForTest(w http.ResponseWriter, r *http.Request, bucketName string) {
	putBucketVersioningHandler(w, r, bucketName)
}

// PutObjectHandlerForTest drives the REAL PutObject handler from a
// cross-package test (the same double the in-package versioning tests
// use).
func PutObjectHandlerForTest(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	putObjectHandler(w, r, bucketName, objectName)
}

// DeleteObjectHandlerForTest drives the REAL DeleteObject handler from
// a cross-package test.
func DeleteObjectHandlerForTest(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	deleteObjectHandler(w, r, bucketName, objectName)
}

// SidecarStoreForBucket returns the plain sidecar store for bucketPath
// (the bookkeeping reader the webdav parity tests assert against —
// snapshots/both assertions probe the sidecar array directly, never a
// dataset-resolving store).
func SidecarStoreForBucket(bucketPath string) *VersionStore {
	s := sidecarVersionStore{bucketPath: bucketPath}
	return &VersionStore{s}
}

// WithReflinkCloneForTest swaps the FICLONE clone seam for fn for the
// test's duration (the webdav both/reflink parity tests emulate the
// ioctl by byte-copy, exactly as the in-package leaf-06 tests do).
func WithReflinkCloneForTest(t *testing.T, fn func(dstPath, srcPath string) error) {
	t.Helper()
	old := reflinkCloneFn
	reflinkCloneFn = fn
	t.Cleanup(func() { reflinkCloneFn = old })
}

// VersionStore wraps a versionStore for cross-package tests (List/Open
// only — the read surface).
type VersionStore struct{ s versionStore }

// List proxies to the wrapped store.
func (v *VersionStore) List(bucket, key string) ([]VersionEntry, error) {
	return v.s.List(bucket, key)
}

// Open proxies to the wrapped store and reads the bytes.
func (v *VersionStore) Open(bucket, key, versionID string) ([]byte, error) {
	rc, _, err := v.s.Open(bucket, key, versionID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ListVersionsForTest lists a key's version history through the
// request-resolved store (versionStoreForBucket — the same resolution
// the ?versions listing uses). ErrNoSuchKey propagates as-is.
func ListVersionsForTest(bucketPath, bucketName, objectName string) ([]VersionEntry, error) {
	return versionStoreForBucket(bucketPath).List(bucketName, objectName)
}

// OpenVersionForTest reads one version's bytes back through the store.
func OpenVersionForTest(bucketPath, bucketName, objectName, versionID string) ([]byte, error) {
	rc, _, err := versionStoreForBucket(bucketPath).Open(bucketName, objectName, versionID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
