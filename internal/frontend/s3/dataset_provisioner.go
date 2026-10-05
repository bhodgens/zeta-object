// dataset_provisioner.go — adapts the leaf-02 ZFS bucket-dataset hook vars
// (zfsdatasets.go, FROZEN: consumed only, never edited here) to the shared
// bucket manager's Provisioner interface. The manager never imports the s3
// sentinel errors; the handler maps a provisioner Destroy failure back to the
// S3 wire (409 BucketHasSnapshots) using bucketmanager.DatasetDestroyError.
package s3

import (
	"context"
	"errors"

	"github.com/bhodgens/zeta-object/internal/bucketmanager"
	"github.com/bhodgens/zeta-object/internal/fslock"
)

// datasetProvisioner adapts the package hook vars to bucketmanager.Provisioner.
// It reads the hooks at call time, so a test that (re)installs them between
// calls is observed (the manager's env is refreshed per request).
type datasetProvisioner struct{}

func (datasetProvisioner) Create(ctx context.Context, bucket string) (string, error) {
	if zfsBucketCreate == nil {
		return "", errors.New("s3: dataset create hook not installed")
	}
	return zfsBucketCreate(ctx, bucket)
}

func (datasetProvisioner) Destroy(ctx context.Context, dataset string) error {
	if zfsBucketDestroy == nil {
		return errors.New("s3: dataset destroy hook not installed")
	}
	return zfsBucketDestroy(ctx, dataset)
}

func (datasetProvisioner) Exists(ctx context.Context, dataset string) (bool, error) {
	if zfsBucketDatasetExists == nil {
		return false, errors.New("s3: dataset exists hook not installed")
	}
	return zfsBucketDatasetExists(ctx, dataset)
}

func (datasetProvisioner) Parent() string { return zfsBucketDatasetParent }

// NewDatasetProvisioner builds the manager provisioner from the installed
// dataset hooks. It returns nil (feature off) only when NO dataset hook is
// installed and no parent prefix is configured, so the manager takes the
// legacy plain-directory path. The create and delete branches each need a
// different subset of the hooks (create the create hook; delete the exists +
// destroy hooks), so presence of any hook means the feature is on. package
// main calls this at startup after installZfsDatasetProvisionerIfNeeded.
func NewDatasetProvisioner() bucketmanager.Provisioner {
	if zfsBucketCreate == nil && zfsBucketDestroy == nil && zfsBucketDatasetExists == nil && zfsBucketDatasetParent == "" {
		return nil
	}
	return datasetProvisioner{}
}

// newDatasetProvisioner is the indirection the S3 handler's env builder calls;
// tests replace it to observe delegation into the manager.
var newDatasetProvisioner = NewDatasetProvisioner

// buildBucketManagerEnv builds the manager environment from the S3 frontend's
// live seams: the one process lock table, getBucketPath, the configured custom
// buckets, and the dataset provisioner (nil when the feature is off).
func buildBucketManagerEnv() bucketmanager.Env {
	return bucketmanager.Env{
		Locks:      fslock.Default,
		BucketPath: getBucketPath,
		Custom: func(bucket string) (string, bool) {
			path, ok := currentServerConfig().Buckets[bucket]
			return path, ok
		},
		Provisioner: newDatasetProvisioner(),
	}
}

// bucketManagerEnvOverride is a test-only hook: when non-nil the handler
// installs this environment verbatim (the delegation marker).
var bucketManagerEnvOverride func() bucketmanager.Env

// installBucketManagerEnv refreshes the manager's installed environment from
// the live seams (mirroring the config-view re-mirror pattern) so a runtime
// hook/config change is observed on the next request, and installs the same
// per-path lock table the S3 frontend uses.
func installBucketManagerEnv() {
	if bucketManagerEnvOverride != nil {
		bucketmanager.Install(bucketManagerEnvOverride())
		return
	}
	bucketmanager.Install(buildBucketManagerEnv())
}
