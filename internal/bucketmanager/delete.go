package bucketmanager

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// delete is the Delete implementation: the bucket-name rule, then the
// custom-bucket 403 guard, then the exist / emptiness / in-flight-upload
// guards (all BEFORE any provisioner call — that ordering is the data-loss
// guard), then the dataset or plain-directory branch. The logic is moved
// verbatim from internal/frontend/s3/bucket_handlers.go deleteBucketHandler
// and zfsdatasets_handlers.go deleteBucketDatasetPath, plus the name rule
// (bughunt H3: without it, a ".." name resolved outside the data root and
// RemoveAll deleted it).
func (e Env) delete(ctx context.Context, name string, opts DeleteOptions) error {
	// Custom is part of the not-installed contract: Env.validateName and the
	// custom-bucket 403 guard below both call e.Custom(name), so a nil Custom
	// would be a nil dereference, not a refusal.
	if e.BucketPath == nil || e.Locks == nil || e.Custom == nil {
		return errNotInstalled()
	}

	// Bucket-name rule FIRST: refuse before resolving a path, stat-ing,
	// reading, or asking a provisioner to destroy anything. Without it a
	// traversal name (or "" / ".", which resolve to the data root itself)
	// reached os.RemoveAll below.
	if err := e.validateName(name); err != nil {
		return err
	}

	// Prevent deletion of custom-configured buckets via API.
	if _, isCustom := e.Custom(name); isCustom {
		log.Printf("Cannot delete custom-configured bucket %s via API", strconv.Quote(name))
		return objectmodel.NewError("AccessDenied", "Cannot delete custom-configured bucket via API.", 403)
	}

	bucketPath := e.BucketPath(name)
	metadataPath := filepath.Join(bucketPath, ".metadata")

	// Serialize against create of the same bucket (same lock).
	unlock := e.Locks.Lock(bucketPath)
	defer unlock()

	// Check if bucket exists.
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) { //nolint:gosec // G703
		log.Printf("Attempted to delete non-existent bucket: %s", strconv.Quote(name))
		return objectmodel.NewError(objectmodel.CodeNoSuchBucket, "The specified bucket does not exist.", 404)
	}

	if err := e.checkEmpty(name, bucketPath); err != nil {
		return err
	}
	if err := e.checkInFlightUploads(name, metadataPath); err != nil {
		return err
	}

	if e.Provisioner != nil && e.Provisioner.Parent() != "" {
		handled, derr := e.deleteDatasetIfBacked(ctx, name, opts)
		if derr != nil {
			return derr
		}
		if handled {
			return nil
		}
	}

	return e.deletePlain(bucketPath, metadataPath)
}

// checkEmpty runs the emptiness scan, excluding .metadata, .bucket-actions and
// .zfs. `.zfs` is the ZFS control directory at every dataset mountpoint
// (OpenZFS 2.3+ returns it from readdir): it is never user data and must not
// block DeleteBucket (zfs-bucket-datasets live validation, 2026-10-03).
func (e Env) checkEmpty(name, bucketPath string) error {
	files, err := os.ReadDir(bucketPath)
	if err != nil {
		log.Printf("Error reading bucket directory %s during delete: %v", bucketPath, err) //nolint:gosec // G706
		return objectmodel.NewError(objectmodel.CodeInternalError, "Error reading bucket.", 500)
	}
	for _, file := range files {
		if file.Name() == ".metadata" || file.Name() == ".bucket-actions" || file.Name() == ".zfs" {
			continue
		}
		log.Printf("Attempted to delete non-empty bucket: %s (offending entry: %q)", strconv.Quote(name), file.Name())
		return objectmodel.NewError("BucketNotEmpty", "The bucket you tried to delete is not empty.", 409)
	}
	return nil
}

// checkInFlightUploads counts in-flight multipart uploads (.uploads/*.json)
// as non-empty (leaf 2.4 fix 10).
func (e Env) checkInFlightUploads(name, metadataPath string) error {
	uploadsDir := filepath.Join(metadataPath, ".uploads")
	uploadEntries, err := os.ReadDir(uploadsDir)
	if err == nil {
		for _, entry := range uploadEntries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				log.Printf("Bucket %s has in-progress multipart uploads: %s", strconv.Quote(name), entry.Name()) //nolint:gosec // G706
				return objectmodel.NewError("BucketNotEmpty", "Bucket has in-progress multipart uploads.", 409)
			}
		}
	}
	return nil
}

// deleteDatasetIfBacked probes whether the bucket dir is a dataset mountpoint
// and, if so, applies the destructive-scope policy: with AllowDatasetDestroy
// false it refuses (ErrDatasetBucketNotDeletable) and NEVER calls Destroy; with
// true it calls Destroy (never RemoveAll on a live mountpoint). Returns
// handled=false for a plain directory. A probe error is a 500: guessing "plain
// dir" on a probe error is a data-loss hazard.
func (e Env) deleteDatasetIfBacked(ctx context.Context, name string, opts DeleteOptions) (bool, error) {
	dataset := e.Provisioner.Parent() + "/" + name
	isDataset, existsErr := e.Provisioner.Exists(ctx, dataset)
	if existsErr != nil {
		log.Printf("Error probing dataset existence for bucket %s: %v", strconv.Quote(name), existsErr)
		return false, objectmodel.NewError(objectmodel.CodeInternalError, "Internal Server Error", 500)
	}
	if !isDataset {
		return false, nil
	}
	if !opts.AllowDatasetDestroy {
		return true, ErrDatasetBucketNotDeletable
	}
	if err := e.Provisioner.Destroy(ctx, dataset); err != nil {
		return true, &DatasetDestroyError{Dataset: dataset, Err: err}
	}
	log.Printf("Successfully deleted bucket: %s (dataset %s)", strconv.Quote(name), dataset)
	return true, nil
}

// deletePlain removes the .metadata directory then the bucket directory
// (legacy plain-directory path).
func (e Env) deletePlain(bucketPath, metadataPath string) error {
	if err := os.RemoveAll(metadataPath); err != nil { //nolint:gosec // G703
		log.Printf("Error deleting metadata directory %s: %v", metadataPath, err) //nolint:gosec // G706
		return objectmodel.NewError(objectmodel.CodeInternalError, "Error deleting bucket.", 500)
	}
	if err := os.RemoveAll(bucketPath); err != nil { //nolint:gosec // G703
		log.Printf("Error deleting bucket directory %s: %v", bucketPath, err) //nolint:gosec // G706
		return objectmodel.NewError(objectmodel.CodeInternalError, "Error deleting bucket.", 500)
	}
	return nil
}
