package s3

// zfsdatasets_handlers.go — the s3-specific residue of the zfs-bucket-datasets
// handler seam (zfsdatasets.go, FROZEN: consumed only). The dataset branches of
// bucket create/delete moved into the shared bucket manager (internal/
// bucketmanager); this file keeps only the parent-dataset prefix, which the
// startup wiring records here (zfsdatasets.go's installer owns the hook
// closures and this leaf must not move them).

// zfsBucketDatasetParent is the configured parent dataset prefix
// ("<parentDataset>") recorded once at startup by the production wiring
// (s3_wiring.go, right after InstallZfsDatasetProvisioner — leaf 02's
// installer owns the hook closure, and zfsdatasets.go is consume-only for this
// leaf, so the prefix lives here). The manager derives dataset names as
// "<parent>/<bucket>" — the same deterministic derivation the create closure
// uses, never resolved from a path (the read-the-PARENT-dataset hazard
// zfsdatasets.go documents). Empty = feature off.
var zfsBucketDatasetParent string

// SetZfsBucketDatasetParent records the parent dataset prefix for the
// delete-side exists probe / destroy calls. Called by the startup wiring
// (package main) when the feature installs, and by tests.
func SetZfsBucketDatasetParent(parentDataset string) {
	zfsBucketDatasetParent = parentDataset
}

// ClearZfsBucketDatasetParent resets the recorded prefix (uninstall / test
// cleanup — the leaf-02 Uninstall cannot reach this var).
func ClearZfsBucketDatasetParent() {
	zfsBucketDatasetParent = ""
}
