package s3

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/backend"
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
func listBucketsHandler(w http.ResponseWriter, r *http.Request) {
	// Data-plane flip (leaf 02): bucket discovery goes through the Backend
	// seam (one FS rooted at dataDir). Custom buckets are discovered by
	// their per-bucket FS (the default backendFor resolves them), keeping
	// the "custom takes precedence" dedup and the ModTime creation date.
	bucketSet := make(map[string]Bucket)
	buckets, discoveryErr := backendDiscovery()
	if discoveryErr != nil {
		log.Printf("Error listing buckets: %v", discoveryErr)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}
	for _, b := range buckets {
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

func createBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Validate bucket name
	if err := validateBucketName(bucketName); err != nil {
		log.Printf("Invalid bucket name %s: %v", strconv.Quote(bucketName), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidBucketName", err.Error())))
		return
	}

	// Check if this is a custom-configured bucket (can't create via API).
	// Custom buckets are config-controlled: if the configured path exists on
	// disk, PUT is idempotent success; if missing, report 409 with a message
	// about the custom path (still no create — leaf 2.4 fix 8).
	if _, isCustom := currentServerConfig().Buckets[bucketName]; isCustom {
		customPath := currentServerConfig().Buckets[bucketName]
		if info, err := os.Stat(customPath); err != nil || !info.IsDir() {
			log.Printf("Bucket %s is a custom-configured bucket whose path %s is missing on disk.", strconv.Quote(bucketName), customPath)
			writeS3Error(w, "BucketAlreadyExists",
				"The requested bucket name is a custom-configured bucket whose path is missing on disk; it cannot be created via the API.",
				http.StatusConflict)
			return
		}
		log.Printf("Bucket %s is a custom-configured bucket, already exists.", strconv.Quote(bucketName))
		w.WriteHeader(http.StatusOK) // Idempotent
		return
	}

	bucketPath := getBucketPath(bucketName)
	metadataPath := filepath.Join(bucketPath, ".metadata")

	// Leaf-4.8 stress fix: serialize create against deleteBucket on the same
	// bucket name — a concurrent delete could remove the directory between
	// this create's Mkdir(bucket) and Mkdir(.metadata), turning the create
	// into a spurious 500.
	unlockBucket := lockObject(bucketPath)
	defer unlockBucket()

	// Check if bucket already exists (leaf 2.4 fix 8 error semantics).
	// Stat error that is neither nil nor IsNotExist → 500.
	info, err := os.Stat(bucketPath) //nolint:gosec // G703: bucketPath built from validateBucketName-checked name
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Error statting bucket path %s: %v", bucketPath, err) //nolint:gosec // G703: bucketPath derived from validateBucketName-checked name
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if err == nil {
		if !info.IsDir() {
			// A FILE exists at the bucket path → conflict, not ours
			log.Printf("Path %s exists as a file; cannot create bucket %s.", bucketPath, bucketName) //nolint:gosec // G703: bucketPath derived from validateBucketName-checked name
			writeS3Error(w, "BucketAlreadyExists",
				"The requested bucket name is not available.", http.StatusConflict)
			return
		}
		// Existing directory = a bucket we already own. Single-user server:
		// report BucketAlreadyOwnedByYou (409) instead of S3's silent 200.
		log.Printf("Bucket %s already exists.", strconv.Quote(bucketName))
		writeS3Error(w, "BucketAlreadyOwnedByYou",
			"Your previous request to create the named bucket succeeded and you already own it.",
			http.StatusConflict)
		return
	}

	// Create bucket directory
	if err := os.MkdirAll(bucketPath, 0755); err != nil { //nolint:gosec // G703: bucketPath built from validateBucketName-checked name
		log.Printf("Error creating bucket directory %s: %v", bucketPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating bucket.")))
		return
	}

	// Create .metadata directory within the bucket
	if err := os.Mkdir(metadataPath, 0755); err != nil { //nolint:gosec // G703: metadataPath under validated bucketPath
		log.Printf("Error creating metadata directory %s for bucket %s: %v", metadataPath, bucketName, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		os.RemoveAll(bucketPath)                                                                            //nolint:gosec // G703: bucketPath built from validateBucketName-checked name
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating bucket metadata storage.")))
		return
	}

	log.Printf("Successfully created bucket: %s", strconv.Quote(bucketName))
	w.WriteHeader(http.StatusOK)
}

func deleteBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Leaf 2.4 fix 7: reject invalid/traversal bucket names
	if !validBucket(bucketName) {
		log.Printf("Invalid bucket name %s for DeleteBucket", strconv.Quote(bucketName))
		writeS3Error(w, "InvalidArgument", "Invalid bucket name.", http.StatusBadRequest)
		return
	}

	// Prevent deletion of custom-configured buckets via API
	if _, isCustom := currentServerConfig().Buckets[bucketName]; isCustom {
		log.Printf("Cannot delete custom-configured bucket %s via API", strconv.Quote(bucketName))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(errorToXML("AccessDenied", "Cannot delete custom-configured bucket via API.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	metadataPath := filepath.Join(bucketPath, ".metadata")

	// Leaf-4.8 stress fix: serialize against createBucket (same lock).
	unlockBucket := lockObject(bucketPath)
	defer unlockBucket()

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) { //nolint:gosec // G703: bucketPath built from validateBucketName-checked name
		log.Printf("Attempted to delete non-existent bucket: %s", strconv.Quote(bucketName))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// Check if bucket is empty (excluding .metadata directory)
	files, err := os.ReadDir(bucketPath)
	if err != nil {
		log.Printf("Error reading bucket directory %s during delete: %v", bucketPath, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error reading bucket.")))
		return
	}

	for _, file := range files {
		if file.Name() != ".metadata" && file.Name() != ".bucket-actions" {
			log.Printf("Attempted to delete non-empty bucket: %s", strconv.Quote(bucketName))
			writeS3Error(w, "BucketNotEmpty", "The bucket you tried to delete is not empty.", http.StatusConflict)
			return
		}
	}

	// Leaf 2.4 fix 10: in-flight multipart uploads count as non-empty
	uploadsDir := filepath.Join(metadataPath, ".uploads")
	if uploadEntries, err := os.ReadDir(uploadsDir); err == nil {
		for _, e := range uploadEntries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				log.Printf("Bucket %s has in-progress multipart uploads: %s", strconv.Quote(bucketName), e.Name()) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
				writeS3Error(w, "BucketNotEmpty", "Bucket has in-progress multipart uploads.", http.StatusConflict)
				return
			}
		}
	}

	// Delete .metadata directory first
	if err := os.RemoveAll(metadataPath); err != nil { //nolint:gosec // G703: bucketPath derived from validateBucketName-checked name
		log.Printf("Error deleting metadata directory %s for bucket %s: %v", metadataPath, bucketName, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error deleting bucket.")))
		return
	}

	// Delete bucket directory
	if err := os.RemoveAll(bucketPath); err != nil { //nolint:gosec // G703: bucketPath derived from validateBucketName-checked name
		log.Printf("Error deleting bucket directory %s: %v", bucketPath, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error deleting bucket.")))
		return
	}

	log.Printf("Successfully deleted bucket: %s", strconv.Quote(bucketName))
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

	// S3 returns an empty LocationConstraint for US Standard (us-east-1)
	location := LocationConstraint{Location: ""}
	writeXML(w, http.StatusOK, location)
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
