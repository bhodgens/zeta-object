package s3

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/bucketmanager"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// bucket_handlers.go — S3 bucket-level operation handlers

// getBucketPath returns the filesystem path for a bucket: the wiring-
// installed fs-root resolver wins, else the config-view layout math
// (custom mapping first, then dataDir) — the pre-move precedence.
func getBucketPath(bucketName string) string {
	if resolver := fsRootResolver(); resolver != nil {
		return resolver(bucketName)
	}
	cfg := currentServerConfig()
	if customPath, ok := cfg.Buckets[bucketName]; ok {
		return customPath
	}
	return filepath.Join(cfg.DataDir, bucketName)
}

// validBucket reports whether a bucket-level request may proceed for this
// name: it must pass validateBucketName, OR be a config-declared custom
// bucket (custom names/paths are config-controlled and exempt from the S3
// naming rules). Rejects traversal names like "../escape" and "." before
// they can reach getBucketPath (leaf 2.4 fix 7).
func validBucket(name string) bool {
	if _, isCustom := currentServerConfig().Buckets[name]; isCustom {
		return true
	}
	return validateBucketName(name) == nil
}

// BucketPathFromInstalledView resolves a bucket's on-disk root from the
// INSTALLED configuration view - the same generation validBucket consults and
// the same one the hot-apply path rewrites (bughunt 2026-10-06 H3). The layout
// math is identical to getBucketPath's fallback: a config-declared custom path
// wins, else dataDir/bucket.
//
// It exists as an exported seam because the wiring layer's fs-root resolver
// used to close over the STARTUP snapshot, which made the resolver and the
// gate disagree after any hot config patch: the gate approved a name because
// the hot view declared it custom while the resolver resolved it through the
// startup map. Both must read one generation.
func BucketPathFromInstalledView(bucketName string) string {
	cfg := currentServerConfig()
	if customPath, ok := cfg.Buckets[bucketName]; ok {
		return customPath
	}
	return filepath.Join(cfg.DataDir, bucketName)
}

// InstalledDataDir reports the data root the INSTALLED configuration view
// resolves buckets under. The management layer's honesty gate compares it
// against the config store's reported live value: a divergence means a
// restart-required key reached the live data plane (bughunt 2026-10-06 H1).
func InstalledDataDir() string { return currentServerConfig().DataDir }

// InstalledCustomBucketPaths returns the custom-bucket map the installed view
// holds. The gate that authorizes a custom bucket name and the resolver that
// builds its path must consult the same generation (bughunt 2026-10-06 H3).
func InstalledCustomBucketPaths() map[string]string {
	cfg := currentServerConfig()
	out := make(map[string]string, len(cfg.Buckets))
	maps.Copy(out, cfg.Buckets)
	return out
}

// bucketExists checks if a bucket exists (follows symlinks)
func bucketExists(bucketName string) bool {
	bucketPath := getBucketPath(bucketName)
	info, err := os.Stat(bucketPath) //nolint:gosec // G703: bucketPath built from validateBucketName-checked name
	if err != nil {
		return false
	}
	return info.IsDir()
}

// Placeholder handlers - to be implemented in handlers.go or similar
func listBucketsHandler(w http.ResponseWriter, r *http.Request, granted map[string]bool) {
	// Data-plane flip (leaf 02): bucket discovery goes through the Backend
	// seam (one FS rooted at dataDir). Custom buckets are discovered by
	// their per-bucket FS (the default backendFor resolves them), keeping
	// the "custom takes precedence" dedup and the ModTime creation date.
	// granted == nil means no filtering (wildcard identity — byte-identical
	// to the pre-grants behavior); otherwise only granted buckets appear.
	bucketSet := make(map[string]Bucket)
	buckets, discoveryErr := backendDiscovery()
	if discoveryErr != nil {
		log.Printf("Error listing buckets: %v", discoveryErr)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}
	for _, b := range buckets {
		if granted != nil && !granted[b.Name] {
			continue
		}
		bucketSet[b.Name] = Bucket{Name: b.Name, CreationDate: b.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")}
	}

	// Convert map to sorted slice
	var s3Buckets []Bucket
	for _, bucket := range bucketSet {
		s3Buckets = append(s3Buckets, bucket)
	}
	sort.Slice(s3Buckets, func(i, j int) bool {
		return s3Buckets[i].Name < s3Buckets[j].Name
	})

	result := ListAllMyBucketsResult{
		Owner:   Owner{ID: "zetaobject-user-id", DisplayName: "zetaobject-user"}, // Placeholder owner
		Buckets: Buckets{Bucket: s3Buckets},
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Successfully listed buckets")
}

// backendDiscovery merges Buckets() from every backend serving configured
// buckets: the dataDir FS plus one FS per custom bucket path. Hidden dirs
// and non-dirs are skipped inside the seam; custom-over-auto dedup happens
// in the map above (custom wins by overwriting the same name).
//
// D3 (bughunt-gateway-2026-09-29): a dataDir discovery failure with no
// custom buckets configured must surface as 500 InternalError (the
// pre-seam behavior), not a silent 200-empty listing. With custom buckets
// configured, the dataDir failure is tolerable — the custom bucket(s) may
// still be discoverable — so it stays a logged warning.
func backendDiscovery() ([]objectmodel.BucketInfo, error) {
	seen := map[string]objectmodel.BucketInfo{}
	dataDirFailed := false
	add := func(b backend.Backend) error {
		buckets, err := b.Buckets(context.Background())
		if err != nil {
			return err
		}
		for _, bi := range buckets {
			seen[bi.Name] = bi
		}
		return nil
	}
	if f, err := backendFor(""); err == nil && f != nil {
		if err := add(f); err != nil {
			log.Printf("Warning: backend discovery failed: %v", err)
			dataDirFailed = true
		}
	}
	customBuckets := currentServerConfig().Buckets
	for name := range customBuckets {
		if f, err := backendFor(name); err == nil && f != nil {
			// Custom-bucket discovery failure stays a logged warning;
			// only the dataDir failure is fatal below.
			if err := add(f); err != nil {
				log.Printf("Warning: custom bucket %q listed but not added: %v", name, err)
			}
		} else if err != nil {
			log.Printf("Warning: custom bucket %q backend unavailable: %v", name, err)
		}
	}
	if dataDirFailed && len(customBuckets) == 0 {
		return nil, fmt.Errorf("bucket discovery failed: data directory is unreadable")
	}
	out := make([]objectmodel.BucketInfo, 0, len(seen))
	for _, bi := range seen {
		out = append(out, bi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// createBucketHandler delegates bucket creation to the shared bucket manager
// (internal/bucketmanager). The bucket-name validation stays handler-side (it
// is S3 wire: 400 InvalidBucketName); the custom-bucket guard, the existing-path
// guards, the dataset/plain branch and their ordering all live in the manager.
func createBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Validate bucket name
	if err := validateBucketName(bucketName); err != nil {
		log.Printf("Invalid bucket name %s: %v", strconv.Quote(bucketName), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidBucketName", err.Error())))
		return
	}

	// Refresh the manager environment from the live seams, then delegate. The
	// manager serializes on the SAME per-path lock table (internal/fslock).
	installBucketManagerEnv()
	if err := bucketmanager.Create(r.Context(), bucketName); err != nil {
		writeBucketManagerError(w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// writeBucketManagerError maps a bucket-manager error onto the S3 wire. The
// manager returns *objectmodel.Error triples for every wire-mapped case; a
// DatasetDestroyError wraps a provisioner Destroy failure whose snapshot
// refusal (ErrDatasetHasSnapshots) is the only non-500 result.
func writeBucketManagerError(w http.ResponseWriter, err error) {
	if destroyErr, ok := errors.AsType[*bucketmanager.DatasetDestroyError](err); ok {
		if errors.Is(err, ErrDatasetHasSnapshots) {
			// Host snapshot policy holds the data: surface the refusal
			// (count + per-snapshot destroy hint ride in the wrapped error
			// message) — snapshots are NEVER auto-removed.
			log.Printf("Refusing to delete dataset %s: has snapshots: %v", destroyErr.Dataset, err)
			writeS3Error(w, "BucketHasSnapshots",
				fmt.Sprintf("%v (delete snapshots first with `zfs destroy %s@<snapshot>`)", err, destroyErr.Dataset),
				http.StatusConflict)
			return
		}
		// Any other destroy failure: fail loud, remove nothing.
		log.Printf("Error destroying ZFS dataset %s: %v", destroyErr.Dataset, err)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if errors.Is(err, bucketmanager.ErrDatasetBucketNotDeletable) {
		// Unreachable on the S3 path (it passes AllowDatasetDestroy true); a
		// management-only refusal.
		writeS3Error(w, "Conflict", err.Error(), http.StatusConflict)
		return
	}
	if omErr, ok := errors.AsType[*objectmodel.Error](err); ok {
		writeS3Error(w, omErr.Code, omErr.Message, omErr.HTTPStatus)
		return
	}
	writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
}

// deleteBucketHandler delegates bucket deletion to the shared bucket manager.
// Bucket-name validation (validBucket, custom-exempt) stays handler-side; the
// custom 403 guard, the exist/emptiness/in-flight-upload checks and the
// dataset/plain branch (with AllowDatasetDestroy true, preserving today's zfs
// destroy + 409 BucketHasSnapshots) live in the manager.
func deleteBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Leaf 2.4 fix 7: reject invalid/traversal bucket names
	if !validBucket(bucketName) {
		log.Printf("Invalid bucket name %s for DeleteBucket", strconv.Quote(bucketName))
		writeS3Error(w, "InvalidArgument", "Invalid bucket name.", http.StatusBadRequest)
		return
	}

	installBucketManagerEnv()
	if err := bucketmanager.Delete(r.Context(), bucketName, bucketmanager.DeleteOptions{AllowDatasetDestroy: true}); err != nil {
		writeBucketManagerError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func getBucketLocationHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Leaf 2.4 fix 7: reject invalid/traversal bucket names
	if !validBucket(bucketName) {
		log.Printf("Invalid bucket name %s for GetBucketLocation", strconv.Quote(bucketName))
		writeS3Error(w, "InvalidArgument", "Invalid bucket name.", http.StatusBadRequest)
		return
	}

	// Leaf 2.4 fix 9: existence check via bucketExists (rejects non-dir
	// paths, not just IsNotExist)
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for GetBucketLocation", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// region-config-2026-10 leaf 02: report the configured region as the
	// location constraint. AWS returns an empty LocationConstraint for
	// US Standard (us-east-1); keep that convention for the default so
	// existing clients see byte-identical behavior, and report any
	// explicitly configured region.
	location := ""
	if cfg := regionOf(); cfg != defaultRegion {
		location = cfg
	}
	writeXML(w, http.StatusOK, LocationConstraint{Location: location})
	log.Printf("Successfully served GetBucketLocation for %s", strconv.Quote(bucketName))
}

func headBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Leaf 2.4 fix 7: reject invalid/traversal bucket names
	if !validBucket(bucketName) {
		log.Printf("Invalid bucket name %s for HeadBucket", strconv.Quote(bucketName))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Check if bucket exists (bucketExists also rejects non-dir paths, unlike
	// a bare os.IsNotExist check — leaf 2.4 fix 9)
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for HeadBucket", strconv.Quote(bucketName))
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	log.Printf("Successfully served HeadBucket for %s", strconv.Quote(bucketName))
}

// validateBucketName validates S3 bucket naming rules
func validateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name must be between 3 and 63 characters")
	}
	// Must start with lowercase letter or number
	if (name[0] < 'a' || name[0] > 'z') && (name[0] < '0' || name[0] > '9') {
		return fmt.Errorf("bucket name must start with a lowercase letter or number")
	}
	// Must end with lowercase letter or number
	last := name[len(name)-1]
	if (last < 'a' || last > 'z') && (last < '0' || last > '9') {
		return fmt.Errorf("bucket name must end with a lowercase letter or number")
	}
	// Check valid characters and no consecutive periods
	prevChar := byte(0)
	for i := range len(name) {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return fmt.Errorf("bucket name can only contain lowercase letters, numbers, hyphens, and periods")
		}
		if c == '.' && prevChar == '.' {
			return fmt.Errorf("bucket name cannot have consecutive periods")
		}
		prevChar = c
	}
	// Cannot be formatted as IP address
	if regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`).MatchString(name) {
		return fmt.Errorf("bucket name cannot be formatted as an IP address")
	}
	return nil
}
