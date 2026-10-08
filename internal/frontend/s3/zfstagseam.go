// zfstagseam.go — the zfs-native-tags feature gate (zfs-metadata#13
// consumer leaf). The gate parallels the zfs_bucket_datasets install
// (zfsdatasets.go + zfsdatasets_handlers.go): package main installs it
// from the loaded config; the zero state (hook nil) keeps every bucket on
// the sidecar tag store byte-identically (tests, plain dirs, hosts
// without zmetad).
//
// Selection rule as implemented (tagstore.go tagStoreFor):
//   zfs_native_tags installed AND the bucket's dataset resolves
//     (deterministic <parent>/<bucket> exists probe)  -> zmetadTagStore
//   otherwise                                          -> sidecarTagStore
//
// The dataset name is NEVER path-resolved (`zfs list <plaindir>` returns
// the PARENT dataset — the read-the-parent hazard zfsdatasets.go
// documents): the resolver derives <parent>/<bucket> from the bucket
// NAME and probes that exact name.
package s3

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// zmetadTagConfig carries the zmetad config values the tag store execs
// and reads with (the SAME defaults the events provider uses:
// zmetad_db_path / zmetad_binary, defaulted at config load).
type zmetadTagConfig struct {
	dbPath string
	binary string
}

// zmetadTagDatasetHook resolves a bucket's ZFS dataset name for the tag
// store: "" = the bucket is not dataset-backed (plain dir — sidecar
// store); a non-empty name = the bucket IS that dataset (zmetadTagStore).
// A returned ERROR means the exists probe never completed (runner /
// timeout / cancellation): selection must fail the request, never fall
// back to the sidecar store.
var zmetadTagDatasetHook func(ctx context.Context, bucketPath string) (string, error)

// zmetadTagConfigValue mirrors zmetadTagConfig under the same mutex (the
// seam struct copy would race a concurrent re-install otherwise).
var zmetadTagConfigValue zmetadTagConfig

// zmetadTagMu guards both seam vars (hookMu is the broader seam lock;
// these two are leaf-local so a test install never contends the other
// seams).
var zmetadTagMu sync.RWMutex

// InstallZmetadTagStore installs the zfs-native-tags gate. parentDataset
// is the startup-resolved zfs_bucket_datasets parent (empty parent or
// empty dbPath/binary aborts the install with an error — fail-loud, the
// zfs_startup.go style; production turns that into a startup failure, so
// a misconfigured gate never silently serves sidecar tags on ZFS).
// datasetExists probes `<zfs> list -H -o name <parent>/<bucket>` — the
// SAME probe semantics as zfsBucketDatasetExists (any zfs non-zero exit
// = not a dataset (false, nil); runner/timeout failure = error). The
// bucket name is recovered from bucketPath via the configured dataDir
// root (the same math getBucketPath used to build it), never re-derived
// from the path's dataset membership.
func InstallZmetadTagStore(parentDataset, zfsBinary, zmetadBinary, dbPath, dataDir string, datasetExists func(ctx context.Context, dataset string) (bool, error)) error {
	if parentDataset == "" {
		return errors.New("zfs_native_tags: parent dataset is required (zfs_bucket_datasets must be enabled and resolved)")
	}
	if isDatasetNameUnsafe(parentDataset) {
		return fmt.Errorf("zfs_native_tags: invalid parent dataset %q", parentDataset)
	}
	if zfsBinary == "" {
		return errors.New("zfs_native_tags: zfs binary is required")
	}
	if zmetadBinary == "" {
		return errors.New("zfs_native_tags: zmetad binary is required")
	}
	if dbPath == "" {
		return errors.New("zfs_native_tags: zmetad database path is required")
	}
	if datasetExists == nil {
		return errors.New("zfs_native_tags: dataset exists probe is required")
	}

	zmetadTagMu.Lock()
	defer zmetadTagMu.Unlock()
	zmetadTagConfigValue = zmetadTagConfig{dbPath: dbPath, binary: zmetadBinary}
	zmetadTagDatasetHook = func(ctx context.Context, bucketPath string) (string, error) {
		bucket, deriveErr := bucketNameFromPath(bucketPath, dataDir)
		if deriveErr != nil {
			return "", nil //nolint:nilerr // not a dataDir bucket (custom path / staging): sidecar is the honest answer, not a swallowed failure
		}
		if isBucketNameUnsafe(bucket) {
			return "", nil // defense in depth; handler validation already rejected
		}
		dataset := parentDataset + "/" + bucket
		exists, err := datasetExists(ctx, dataset)
		if err != nil {
			return "", err // probe never completed: fail loud
		}
		if !exists {
			return "", nil // plain-dir bucket: sidecar store
		}
		return dataset, nil
	}
	return nil
}

// UninstallZmetadTagStore clears the gate (feature off / hot-apply
// rollback / test cleanup).
func UninstallZmetadTagStore() {
	zmetadTagMu.Lock()
	defer zmetadTagMu.Unlock()
	zmetadTagDatasetHook = nil
	zmetadTagConfigValue = zmetadTagConfig{}
}

// InstallZfsTagDatasetProbe wraps zfsDatasetExists (the seeded argv
// validation + any-nonzero-exit-is-absent probe, zfsdatasets.go) into the
// func(ctx, dataset) (bool, error) shape the installer takes, bound to
// the configured zfs binary. Tests replace zfsDatasetRunner; the wiring
// passes this production closure.
func InstallZfsTagDatasetProbe(zfsBinary string) func(ctx context.Context, dataset string) (bool, error) {
	return func(ctx context.Context, dataset string) (bool, error) {
		return zfsDatasetExists(ctx, zfsBinary, dataset)
	}
}

// zmetadTagConfigSnapshot returns the installed config + hook under the
// read lock (used by tests and selection).
func zmetadTagSnapshot() (zmetadTagConfig, func(context.Context, string) (string, error)) {
	zmetadTagMu.RLock()
	defer zmetadTagMu.RUnlock()
	return zmetadTagConfigValue, zmetadTagDatasetHook
}

// bucketNameFromPath recovers the bucket name for a bucket directory
// under dataDir (the inverse of dataDir/bucket). A path outside dataDir
// (custom bucket mapping, multipart staging) is not derivable: error.
// Symlink-resolved or cleaned variants resolve because both sides are
// cleaned before the prefix compare.
func bucketNameFromPath(bucketPath, dataDir string) (string, error) {
	root := strings.TrimSuffix(dataDir, "/")
	if bucketPath == "" || root == "" {
		return "", fmt.Errorf("bucket path %q not under dataDir %q", bucketPath, dataDir)
	}
	rel, err := filepath.Rel(root, bucketPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("bucket path %q not under dataDir %q", bucketPath, dataDir)
	}
	if strings.Contains(rel, string(filepath.Separator)) {
		return "", fmt.Errorf("bucket path %q is nested under dataDir %q", bucketPath, dataDir)
	}
	return rel, nil
}
