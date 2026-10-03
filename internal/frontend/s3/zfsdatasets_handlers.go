package s3

// zfsdatasets_handlers.go — leaf 03 (zfs-bucket-datasets) named helpers
// for the dataset branches of the bucket create/delete handlers. The
// handlers branch on the leaf-02 hook vars (nil = feature off, legacy
// plain-dir path) and delegate here; extracting these keeps
// createBucketHandler/deleteBucketHandler under the gocognit gate (cf.
// the serveMultiRangeIfNeeded precedent).
//
// CONTRACT (master.md Contract 3, binding):
//   - create: the stat guards + lockObject have ALREADY run by the time
//     the handler calls createBucketDatasetPath; a hook error is a 500
//     InternalError with NO mkdir fallback (never a partial dir); a
//     .metadata mkdir failure rolls the dataset back via the destroy
//     hook (a refused rollback because snapshots raced the create still
//     answers 500 and leaves the empty dataset — never `-r`).
//   - delete: the emptiness checks have ALREADY run by the time the
//     handler calls deleteBucketDatasetPath — destroy never happens on
//     a non-empty bucket (zfs destroy succeeds on datasets holding
//     objects; that ordering is the data-loss guard). Only
//     errors.Is(err, ErrDatasetHasSnapshots) maps to 409
//     BucketHasSnapshots; every other hook error is a 500 fail-loud and
//     the directory is never removed. NEVER RemoveAll after a destroy —
//     the mountpoint is gone (or a cron-held dataset is left intact).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
)

// zfsBucketDatasetParent is the configured parent dataset prefix
// ("<parentDataset>") recorded once at startup by the production wiring
// (s3_wiring.go, right after InstallZfsDatasetProvisioner — leaf 02's
// installer owns the hook closure, and zfsdatasets.go is consume-only
// for this leaf, so the prefix lives here). Delete-side names derive as
// "<parent>/<bucket>" — the same deterministic derivation the create
// closure uses, never resolved from a path (the read-the-PARENT-dataset
// hazard zfsdatasets.go documents). Empty = feature off.
var zfsBucketDatasetParent string

// SetZfsBucketDatasetParent records the parent dataset prefix for the
// delete-side exists probe / destroy calls. Called by the startup
// wiring (package main) when the feature installs, and by tests.
func SetZfsBucketDatasetParent(parentDataset string) {
	zfsBucketDatasetParent = parentDataset
}

// ClearZfsBucketDatasetParent resets the recorded prefix (uninstall /
// test cleanup — the leaf-02 Uninstall cannot reach this var).
func ClearZfsBucketDatasetParent() {
	zfsBucketDatasetParent = ""
}

// createBucketDatasetPath provisions the bucket as its own ZFS dataset
// (the handler has already validated the name, excluded custom buckets
// and existing paths, and taken the bucket lock).
func createBucketDatasetPath(w http.ResponseWriter, ctx context.Context, bucketName, bucketPath, metadataPath string) {
	dataset, createErr := zfsBucketCreate(ctx, bucketName)
	if createErr != nil {
		// Fail loud, NO mkdir fallback: a half-provisioned dataset must
		// never silently degrade into a plain directory (the whole point
		// of the feature is dataset-level history).
		log.Printf("Error creating ZFS dataset for bucket %s: %v", strconv.Quote(bucketName), createErr)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating bucket.")))
		return
	}

	// Create .metadata directory within the (now-mounted) dataset.
	//nolint:gosec // G703: metadataPath under bucketPath built from validateBucketName-checked name
	if err := os.Mkdir(metadataPath, 0755); err != nil {
		log.Printf("Error creating metadata directory %s for bucket %s: %v — rolling back dataset %s", metadataPath, bucketName, err, dataset)
		if rollbackErr := zfsBucketDestroy(ctx, dataset); rollbackErr != nil {
			// Rare race: a host snapshot cron created a snapshot between
			// create and rollback. Still a 500, the empty dataset is LEFT
			// in place (host policy: never -r, never auto-remove
			// snapshots), and the operator is told loudly.
			log.Printf("CRITICAL: rollback destroy of dataset %s failed after metadata mkdir failure for bucket %s: %v — the empty dataset is left in place; remove it manually (zfs destroy %s), snapshots are never auto-destroyed", dataset, strconv.Quote(bucketName), rollbackErr, dataset)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating bucket metadata storage.")))
		return
	}

	log.Printf("Successfully created bucket: %s (dataset %s)", strconv.Quote(bucketName), dataset)
	w.WriteHeader(http.StatusOK)
}

// deleteBucketDatasetPath destroys the bucket's ZFS dataset (the
// handler has already validated the name, excluded custom buckets,
// confirmed the path exists, and run the emptiness checks — including
// in-flight multipart uploads — before any hook fires).
func deleteBucketDatasetPath(w http.ResponseWriter, ctx context.Context, bucketName, dataset string) {
	if err := zfsBucketDestroy(ctx, dataset); err != nil {
		if errors.Is(err, ErrDatasetHasSnapshots) {
			// Host snapshot policy holds the data: surface the refusal
			// (count + per-snapshot destroy hint ride in the wrapped
			// error message) — snapshots are NEVER auto-removed.
			log.Printf("Refusing to delete bucket %s: dataset %s has snapshots: %v", strconv.Quote(bucketName), dataset, err)
			writeS3Error(w, "BucketHasSnapshots",
				fmt.Sprintf("%v (delete snapshots first with `zfs destroy %s@<snapshot>`)", err, dataset),
				http.StatusConflict)
			return
		}
		// Any other destroy failure: fail loud, remove nothing.
		log.Printf("Error destroying ZFS dataset %s for bucket %s: %v", dataset, strconv.Quote(bucketName), err)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully deleted bucket: %s (dataset %s)", strconv.Quote(bucketName), dataset)
	w.WriteHeader(http.StatusNoContent)
}
