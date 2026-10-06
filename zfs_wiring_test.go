package main

// zfs_wiring_test.go — leaf 03 (zfs-bucket-datasets) startup wiring
// tests: with serverConfig.ZfsBucketDatasets true, installS3Seams
// installs the s3 dataset provisioner (hooks non-nil) from the
// startup-resolved zfsBucketsParentDataset; with the feature off the
// hooks stay nil and nothing else changes.

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// TestInstallS3SeamsZfsBucketDatasetsOn: feature on → the provisioner
// hooks are installed from zfsBucketsParentDataset.
func TestInstallS3SeamsZfsBucketDatasetsOn(t *testing.T) {
	t.Cleanup(func() {
		s3.UninstallZfsDatasetProvisioner()
		s3.ClearZfsBucketDatasetParent()
	})

	origCfg := serverConfig
	t.Cleanup(func() { serverConfig = origCfg })
	// Save/restore the startup-resolved parent: it is a process global, and
	// leaving "pool/data" behind makes any LATER test's "no parent
	// configured" premise false (the shuffle-order failure this fix closes).
	origParent := zfsBucketsParentDataset
	t.Cleanup(func() { zfsBucketsParentDataset = origParent })
	serverConfig = ServerConfig{
		DataDir:           t.TempDir() + "/",
		ZfsBucketDatasets: true,
		ZfsBinary:         "zfs",
	}
	zfsBucketsParentDataset = "pool/data"

	s3.UninstallZfsDatasetProvisioner()
	installZfsDatasetProvisionerIfNeeded()

	if !s3.ZfsDatasetProvisionerInstalled() {
		t.Fatal("zfs_bucket_datasets on: provisioner hooks not installed by installS3Seams")
	}
	if s3.ZfsBucketDatasetParentProbe() != "pool/data" {
		t.Fatalf("dataset parent = %q, want pool/data", s3.ZfsBucketDatasetParentProbe())
	}
}

// TestInstallS3SeamsZfsBucketDatasetsOff: feature off → hooks stay nil
// (the default), and the parent probe stays empty.
func TestInstallS3SeamsZfsBucketDatasetsOff(t *testing.T) {
	t.Cleanup(func() {
		s3.UninstallZfsDatasetProvisioner()
		s3.ClearZfsBucketDatasetParent()
	})

	origCfg := serverConfig
	t.Cleanup(func() { serverConfig = origCfg })
	origParent := zfsBucketsParentDataset
	t.Cleanup(func() { zfsBucketsParentDataset = origParent })
	serverConfig = ServerConfig{
		DataDir:           t.TempDir() + "/",
		ZfsBucketDatasets: false,
		ZfsBinary:         "zfs",
	}
	zfsBucketsParentDataset = ""

	s3.UninstallZfsDatasetProvisioner()
	installZfsDatasetProvisionerIfNeeded()

	if s3.ZfsDatasetProvisionerInstalled() {
		t.Fatal("zfs_bucket_datasets off: provisioner hooks were installed")
	}
	if s3.ZfsBucketDatasetParentProbe() != "" {
		t.Fatalf("dataset parent = %q, want empty when feature off", s3.ZfsBucketDatasetParentProbe())
	}
}
