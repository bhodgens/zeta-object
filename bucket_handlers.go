package main

import (
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// bucket_handlers.go — S3 bucket-level operation handlers

// getBucketPath returns the filesystem path for a bucket.
// It checks custom bucket mappings first, then falls back to dataDir.
func getBucketPath(bucketName string) string {
	if customPath, ok := serverConfig.Buckets[bucketName]; ok {
		return customPath
	}
	return filepath.Join(serverConfig.DataDir, bucketName)
}

// bucketExists checks if a bucket exists (follows symlinks)
func bucketExists(bucketName string) bool {
	bucketPath := getBucketPath(bucketName)
	info, err := os.Stat(bucketPath) // os.Stat follows symlinks
	if err != nil {
		return false
	}
	return info.IsDir()
}

// Placeholder handlers - to be implemented in handlers.go or similar
func listBucketsHandler(w http.ResponseWriter, r *http.Request) {
	bucketSet := make(map[string]Bucket) // Use map to deduplicate

	// First, add all custom-configured buckets
	for bucketName, bucketPath := range serverConfig.Buckets {
		info, err := os.Stat(bucketPath) // os.Stat follows symlinks
		if err != nil {
			log.Printf("Warning: Custom bucket '%s' at '%s' not accessible: %v", bucketName, bucketPath, err)
			continue
		}
		if !info.IsDir() {
			continue
		}
		creationDate := info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
		bucketSet[bucketName] = Bucket{Name: bucketName, CreationDate: creationDate}
	}

	// Then, scan the data directory for buckets (including symlinks)
	dirs, err := os.ReadDir(serverConfig.DataDir)
	if err != nil {
		log.Printf("Error reading data directory %s: %v", serverConfig.DataDir, err)
		// Don't fail if we have custom buckets
		if len(bucketSet) == 0 {
			writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
			return
		}
	} else {
		for _, dir := range dirs {
			if strings.HasPrefix(dir.Name(), ".") {
				continue // Exclude hidden dirs
			}

			// Use os.Stat to follow symlinks and check if it's a directory
			fullPath := filepath.Join(serverConfig.DataDir, dir.Name())
			info, err := os.Stat(fullPath)
			if err != nil {
				log.Printf("Warning: Could not stat %s: %v", fullPath, err)
				continue
			}
			if !info.IsDir() {
				continue
			}

			// Skip if already defined as a custom bucket (custom takes precedence)
			if _, exists := serverConfig.Buckets[dir.Name()]; exists {
				continue
			}

			creationDate := info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
			bucketSet[dir.Name()] = Bucket{Name: dir.Name(), CreationDate: creationDate}
		}
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
		Owner:   Owner{ID: "minis3-user-id", DisplayName: "minis3-user"}, // Placeholder owner
		Buckets: Buckets{Bucket: s3Buckets},
	}

	x, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Printf("Error marshalling ListAllMyBucketsResult to XML: %v", err)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(x)
}

func createBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Validate bucket name
	if err := validateBucketName(bucketName); err != nil {
		log.Printf("Invalid bucket name %s: %v", bucketName, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidBucketName", err.Error())))
		return
	}

	// Check if this is a custom-configured bucket (can't create via API)
	if _, isCustom := serverConfig.Buckets[bucketName]; isCustom {
		log.Printf("Bucket %s is a custom-configured bucket, already exists.", bucketName)
		w.WriteHeader(http.StatusOK) // Idempotent
		return
	}

	bucketPath := getBucketPath(bucketName)
	metadataPath := filepath.Join(bucketPath, ".metadata")

	// Check if bucket already exists
	if _, err := os.Stat(bucketPath); !os.IsNotExist(err) {
		log.Printf("Bucket %s already exists.", bucketName)
		w.WriteHeader(http.StatusOK) // S3 PUT Bucket is idempotent
		return
	}

	// Create bucket directory
	if err := os.MkdirAll(bucketPath, 0755); err != nil {
		log.Printf("Error creating bucket directory %s: %v", bucketPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating bucket.")))
		return
	}

	// Create .metadata directory within the bucket
	if err := os.Mkdir(metadataPath, 0755); err != nil {
		log.Printf("Error creating metadata directory %s for bucket %s: %v", metadataPath, bucketName, err)
		os.RemoveAll(bucketPath)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating bucket metadata storage.")))
		return
	}

	log.Printf("Successfully created bucket: %s", bucketName)
	w.WriteHeader(http.StatusOK)
}

func deleteBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Prevent deletion of custom-configured buckets via API
	if _, isCustom := serverConfig.Buckets[bucketName]; isCustom {
		log.Printf("Cannot delete custom-configured bucket %s via API", bucketName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(errorToXML("AccessDenied", "Cannot delete custom-configured bucket via API.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	metadataPath := filepath.Join(bucketPath, ".metadata")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Attempted to delete non-existent bucket: %s", bucketName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// Check if bucket is empty (excluding .metadata directory)
	files, err := os.ReadDir(bucketPath)
	if err != nil {
		log.Printf("Error reading bucket directory %s during delete: %v", bucketPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading bucket.")))
		return
	}

	for _, file := range files {
		if file.Name() != ".metadata" && file.Name() != ".bucket-actions" {
			log.Printf("Attempted to delete non-empty bucket: %s", bucketName)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(errorToXML("BucketNotEmpty", "The bucket you tried to delete is not empty.")))
			return
		}
	}

	// Delete .metadata directory first
	if err := os.RemoveAll(metadataPath); err != nil {
		log.Printf("Error deleting metadata directory %s for bucket %s: %v", metadataPath, bucketName, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error deleting bucket.")))
		return
	}

	// Delete bucket directory
	if err := os.RemoveAll(bucketPath); err != nil {
		log.Printf("Error deleting bucket directory %s: %v", bucketPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error deleting bucket.")))
		return
	}

	log.Printf("Successfully deleted bucket: %s", bucketName)
	w.WriteHeader(http.StatusNoContent)
}

func getBucketLocationHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucketPath := getBucketPath(bucketName)
	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for GetBucketLocation", bucketName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// S3 returns an empty LocationConstraint for US Standard (us-east-1)
	location := LocationConstraint{Location: ""}
	x, err := xml.MarshalIndent(location, "", "  ")
	if err != nil {
		log.Printf("Error marshalling LocationConstraint to XML for bucket %s: %v", bucketName, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error formatting response.")))
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(x)
	log.Printf("Successfully served GetBucketLocation for %s", bucketName)
}

func headBucketHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Check if bucket exists (bucketExists also rejects non-dir paths, unlike
	// a bare os.IsNotExist check — audit fix, leaf 2.4 expands semantics)
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for HeadBucket", bucketName)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	log.Printf("Successfully served HeadBucket for %s", bucketName)
}

// validateBucketName validates S3 bucket naming rules
func validateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name must be between 3 and 63 characters")
	}
	// Must start with lowercase letter or number
	if !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= '0' && name[0] <= '9')) {
		return fmt.Errorf("bucket name must start with a lowercase letter or number")
	}
	// Must end with lowercase letter or number
	last := name[len(name)-1]
	if !((last >= 'a' && last <= 'z') || (last >= '0' && last <= '9')) {
		return fmt.Errorf("bucket name must end with a lowercase letter or number")
	}
	// Check valid characters and no consecutive periods
	prevChar := byte(0)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
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
