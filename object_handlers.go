package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// object_handlers.go — S3 object-level operation handlers

func putObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)
	// Object data is stored directly in the bucket directory
	objectDataPath := filepath.Join(bucketPath, objectName)
	// Metadata is stored in .metadata subdirectory
	objectMetadataDir := filepath.Join(bucketPath, ".metadata")
	objectMetadataPath := filepath.Join(objectMetadataDir, objectName+".meta")

	// Validate object key (leaf 2.4 fix 2: traversal rejection)
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s: %v", objectName, err)
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}

	// Ensure bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for PutObject", bucketName)
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body for %s/%s: %v", bucketName, objectName, err)
		writeS3Error(w, "InternalError", "Error reading request body.", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Handle aws-chunked Content-Encoding (used by AWS CLI v2)
	contentEncoding := r.Header.Get("Content-Encoding")
	if strings.Contains(contentEncoding, "aws-chunked") {
		decodedBody, err := decodeAWSChunked(body)
		if err != nil {
			log.Printf("Error decoding aws-chunked body for %s/%s: %v", bucketName, objectName, err)
			writeS3Error(w, "InvalidArgument", "Failed to decode chunked body.", http.StatusBadRequest)
			return
		}
		// Leaf 2.2 cross-leaf wiring: verify declared decoded length
		if err := VerifyDecodedLength(r.Header.Get("x-amz-decoded-content-length"), len(decodedBody)); err != nil {
			log.Printf("Decoded content length mismatch for %s/%s: %v", bucketName, objectName, err)
			writeS3Error(w, "InvalidArgument", "Decoded content length mismatch.", http.StatusBadRequest)
			return
		}
		body = decodedBody
		log.Printf("Decoded aws-chunked body: %d bytes", len(body))
	}

	// Calculate ETag (MD5 hash of the content)
	hash := md5.Sum(body)
	eTag := hex.EncodeToString(hash[:])

	// Leaf 2.4 fix 1: serialize writers per object and write data + metadata
	// atomically via the storage.go helpers (no torn reads/partial files).
	unlock := lockObject(objectDataPath)
	defer unlock()

	// Create parent directories for the object data if they don't exist
	objectDataParentDir := filepath.Dir(objectDataPath)
	if err := os.MkdirAll(objectDataParentDir, 0755); err != nil {
		log.Printf("Error creating parent directories for object data %s: %v", objectDataPath, err)
		writeS3Error(w, "InternalError", "Error creating object storage.", http.StatusInternalServerError)
		return
	}

	// Write the object data atomically
	if err := writeFileAtomic(objectDataPath, body, 0644); err != nil {
		log.Printf("Error writing object data to %s: %v", objectDataPath, err)
		writeS3Error(w, "InternalError", "Error writing object data.", http.StatusInternalServerError)
		return
	}

	// Create parent directories for the metadata file if they don't exist
	metadataParentDir := filepath.Dir(objectMetadataPath)
	if err := os.MkdirAll(metadataParentDir, 0755); err != nil {
		log.Printf("Error creating metadata storage for %s: %v", objectMetadataPath, err)
		// RESIDUAL WINDOW (leaf 2.4 fix 1): the data file has been written
		// but metadata creation failed. We can't tell whether the data file
		// existed before this request — os.Remove here would delete the old
		// good object on a PUT-overwrite. Full two-phase commit is out of
		// scope; serve 500 and leave the new data in place unindexed.
		writeS3Error(w, "InternalError", "Error creating metadata storage.", http.StatusInternalServerError)
		return
	}

	// Store metadata - use actual body length, not Content-Length header
	meta := ObjectMetadata{
		ContentType:    r.Header.Get("Content-Type"),
		ContentLength:  int64(len(body)), // Use actual body length
		ETag:           eTag,
		CustomMetadata: make(map[string]string),
		LastModified:   time.Now().UTC(),
		StoragePath:    objectDataPath, // Points to actual object data
	}

	for headerName, headerValues := range r.Header {
		if strings.HasPrefix(strings.ToLower(headerName), "x-amz-meta-") {
			meta.CustomMetadata[headerName] = strings.Join(headerValues, ", ")
		}
	}

	if err := writeFileAtomicJSON(objectMetadataPath, meta, 0644); err != nil {
		log.Printf("Error writing metadata file %s: %v", objectMetadataPath, err)
		// RESIDUAL WINDOW (leaf 2.4 fix 1): same as above — do NOT remove the
		// data file; it may be the pre-overwrite good object. Return 500 with
		// the new data left unindexed; a retry rewrites both files.
		writeS3Error(w, "InternalError", "Error writing metadata.", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully put object %s/%s, ETag: %s", bucketName, objectName, eTag)
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", eTag))
	w.WriteHeader(http.StatusOK)

	// Trigger after_upload actions
	go triggerActions("after_upload", ActionContext{
		FilePath:     objectDataPath,
		MetadataPath: objectMetadataPath,
		BucketName:   bucketName,
		BucketPath:   bucketPath,
		ObjectKey:    objectName,
		ContentType:  meta.ContentType,
		ETag:         eTag,
		Size:         meta.ContentLength,
	})
}

// resolveObjectDataPath returns the data file path for an object, honoring
// meta.StoragePath with a fallback to the canonical location when the stored
// path is corrupt/empty (leaf 2.4 fix 6).
func resolveObjectDataPath(bucketPath, objectName string, meta *ObjectMetadata) string {
	if meta.StoragePath == "" {
		// Corrupt metadata (empty StoragePath): fall back to the canonical
		// bucket/key location.
		return filepath.Join(bucketPath, objectName)
	}
	return meta.StoragePath
}

func getObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for GetObject", bucketName)
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Read metadata
	metaJSON, err := os.ReadFile(objectMetadataPath)
	if os.IsNotExist(err) {
		log.Printf("Object metadata %s not found for %s/%s", objectMetadataPath, bucketName, objectName)
		writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error reading metadata file %s: %v", objectMetadataPath, err)
		writeS3Error(w, "InternalError", "Error reading object metadata.", http.StatusInternalServerError)
		return
	}

	var meta ObjectMetadata
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		log.Printf("Error unmarshalling metadata from %s: %v", objectMetadataPath, err)
		writeS3Error(w, "InternalError", "Error parsing object metadata.", http.StatusInternalServerError)
		return
	}

	// Leaf 2.4 fix 6: fall back to the canonical path on corrupt StoragePath
	objectDataPath := resolveObjectDataPath(bucketPath, objectName, &meta)

	// Check if actual object data file exists
	if _, err := os.Stat(objectDataPath); os.IsNotExist(err) {
		log.Printf("Object data file %s not found for %s/%s", objectDataPath, bucketName, objectName)
		writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
		return
	}

	// Leaf 2.4 fix 5: stat the data file and serve the ACTUAL size. A meta
	// lie no longer aborts the response — log a warning and serve truth.
	fileInfo, err := os.Stat(objectDataPath)
	if err != nil {
		log.Printf("Error statting object data file %s: %v", objectDataPath, err)
		writeS3Error(w, "InternalError", "Error reading object data.", http.StatusInternalServerError)
		return
	}
	actualSize := fileInfo.Size()
	if actualSize != meta.ContentLength {
		log.Printf("WARNING: metadata ContentLength (%d) differs from actual file size (%d) for %s/%s; serving actual size",
			meta.ContentLength, actualSize, bucketName, objectName)
	}

	// Set headers from metadata (leaf 2.4 fix 4: default Content-Type)
	contentType := meta.ContentType
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", actualSize))
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	for k, v := range meta.CustomMetadata {
		w.Header().Set(k, v)
	}

	// Stream the object data. Open AFTER headers are computed but write the
	// status only once we know the file opens; a failed open becomes
	// NoSuchKey/404 rather than a 500 with headers half-set (leaf 2.4 fix 6).
	file, err := os.Open(objectDataPath)
	if err != nil {
		log.Printf("Error opening object data file %s: %v", objectDataPath, err)
		writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
		return
	}
	defer file.Close()

	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, file); err != nil {
		log.Printf("Error streaming object %s/%s to client: %v", bucketName, objectName, err)
	}
	log.Printf("Successfully served object %s/%s", bucketName, objectName)

	// Trigger after_download actions
	go triggerActions("after_download", ActionContext{
		FilePath:     objectDataPath,
		MetadataPath: objectMetadataPath,
		BucketName:   bucketName,
		BucketPath:   bucketPath,
		ObjectKey:    objectName,
		ContentType:  meta.ContentType,
		ETag:         meta.ETag,
		Size:         meta.ContentLength,
	})
}

func deleteObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")
	objectDataPath := filepath.Join(bucketPath, objectName)

	// Leaf 2.4 fix 3: a missing bucket is a real 404 (missing KEY in an
	// existing bucket still stays 204 per S3 semantics).
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for DeleteObject", bucketName)
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Try to read metadata to get actual storage path
	var actualDataPath string
	metaJSON, err := os.ReadFile(objectMetadataPath)
	if err == nil {
		var meta ObjectMetadata
		if jsonErr := json.Unmarshal(metaJSON, &meta); jsonErr == nil && meta.StoragePath != "" {
			actualDataPath = meta.StoragePath
		} else {
			actualDataPath = objectDataPath
		}
	} else {
		actualDataPath = objectDataPath
	}

	// Delete the object data file
	dataDeleted := false
	if err := os.Remove(actualDataPath); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error deleting object data file %s: %v", actualDataPath, err)
			writeS3Error(w, "InternalError", "Error deleting object data.", http.StatusInternalServerError)
			return
		}
	} else {
		dataDeleted = true
	}

	// Delete the metadata file
	metaDeleted := false
	if err := os.Remove(objectMetadataPath); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error deleting metadata file %s: %v", objectMetadataPath, err)
			// Don't fail - data is already deleted
		}
	} else {
		metaDeleted = true
	}

	// Clean up empty parent directories (best effort)
	cleanupEmptyDirs(filepath.Dir(actualDataPath), bucketPath)
	cleanupEmptyDirs(filepath.Dir(objectMetadataPath), filepath.Join(bucketPath, ".metadata"))

	if dataDeleted || metaDeleted {
		log.Printf("Successfully deleted object %s/%s", bucketName, objectName)

		// Trigger after_delete actions
		go triggerActions("after_delete", ActionContext{
			FilePath:     actualDataPath,
			MetadataPath: objectMetadataPath,
			BucketName:   bucketName,
			BucketPath:   bucketPath,
			ObjectKey:    objectName,
		})
	} else {
		log.Printf("Object %s/%s did not exist for deletion", bucketName, objectName)
	}

	w.WriteHeader(http.StatusNoContent) // S3 spec: 204 No Content
}

func headObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for HeadObject", bucketName)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Read metadata
	metaJSON, err := os.ReadFile(objectMetadataPath)
	if os.IsNotExist(err) {
		log.Printf("Object metadata %s not found for %s/%s for HeadObject", objectMetadataPath, bucketName, objectName)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error reading metadata file %s for HeadObject: %v", objectMetadataPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var meta ObjectMetadata
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		log.Printf("Error unmarshalling metadata from %s for HeadObject: %v", objectMetadataPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Leaf 2.4 fix 6: fall back to the canonical path on corrupt StoragePath
	objectDataPath := resolveObjectDataPath(bucketPath, objectName, &meta)

	// Check if actual object data file exists
	if _, err := os.Stat(objectDataPath); os.IsNotExist(err) {
		log.Printf("Object data file %s not found for %s/%s during HeadObject", objectDataPath, bucketName, objectName)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Leaf 2.4 fix 5: serve the ACTUAL file size; warn when meta lies.
	fileInfo, err := os.Stat(objectDataPath)
	if err != nil {
		log.Printf("Error statting object data file %s during HeadObject: %v", objectDataPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	actualSize := fileInfo.Size()
	if actualSize != meta.ContentLength {
		log.Printf("WARNING: metadata ContentLength (%d) differs from actual file size (%d) for %s/%s during HeadObject; serving actual size",
			meta.ContentLength, actualSize, bucketName, objectName)
	}

	// Set headers from metadata (leaf 2.4 fix 4: default Content-Type)
	contentType := meta.ContentType
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", actualSize))
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	for k, v := range meta.CustomMetadata {
		w.Header().Set(k, v)
	}

	w.WriteHeader(http.StatusOK)
	log.Printf("Successfully served HEAD for object %s/%s", bucketName, objectName)
}

// s3URLEncode percent-encodes a key for encoding-type=url responses: S3
// encodes every byte except unreserved chars and "/", so a path stays a
// path. (url.QueryEscape then restore "/" — leaf 2.4 fix 15.)
func s3URLEncode(key string) string {
	return strings.ReplaceAll(url.QueryEscape(key), "%2F", "/")
}

// listObjectsV2Handler implementation
func listObjectsV2Handler(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucketPath := getBucketPath(bucketName)
	metadataDir := filepath.Join(bucketPath, ".metadata")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for ListObjectsV2", bucketName)
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Parse query parameters
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	continuationToken := r.URL.Query().Get("continuation-token")
	startAfter := r.URL.Query().Get("start-after")
	encodingType := r.URL.Query().Get("encoding-type")
	encodeKeys := encodingType == "url"
	maxKeysStr := r.URL.Query().Get("max-keys")
	maxKeys := 1000 // Default S3 maxKeys
	if maxKeysStr != "" {
		if n, err := strconv.Atoi(maxKeysStr); err != nil {
			log.Printf("Invalid max-keys value: '%s'. Using default %d.", maxKeysStr, 1000)
		} else {
			if n < 0 {
				log.Printf("max-keys must be non-negative. Received %d. Using default %d.", n, 1000)
			} else if n > 1000 {
				maxKeys = 1000 // S3 caps at 1000
			} else {
				maxKeys = n
			}
		}
	}

	// Leaf 2.4 fix 13: max-keys=0 → empty result, no prefixes, not truncated
	if maxKeys == 0 {
		result := ListBucketResult{
			IsTruncated: false,
			Name:        bucketName,
			Prefix:      prefix,
			Delimiter:   delimiter,
			MaxKeys:     0,
			KeyCount:    0,
		}
		if encodeKeys {
			result.EncodingType = "url"
		}
		writeXML(w, http.StatusOK, result)
		return
	}

	var objects []Object
	var commonPrefixesMap = make(map[string]struct{})
	var allObjectKeys []string

	// Use filepath.WalkDir to handle nested paths
	err := filepath.WalkDir(metadataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip directories and non-.meta files
		if d.IsDir() {
			// Skip .uploads directory
			if d.Name() == ".uploads" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}

		// Calculate object key from path relative to metadata dir
		relPath, err := filepath.Rel(metadataDir, path)
		if err != nil {
			return nil
		}
		objectKey := strings.TrimSuffix(relPath, ".meta")
		allObjectKeys = append(allObjectKeys, objectKey)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Error walking metadata directory %s: %v", metadataDir, err)
		writeS3Error(w, "InternalError", "Error listing objects.", http.StatusInternalServerError)
		return
	}
	sort.Strings(allObjectKeys)

	startKey := ""
	if continuationToken != "" {
		startKey = continuationToken
	} else if startAfter != "" {
		startKey = startAfter
	}

	processedCount := 0
	isTruncated := false
	var nextContinuationToken string

	// keyAtOrBeforeStart reports whether objectKey is excluded by the
	// continuation/start-after cursor.
	keyAtOrBeforeStart := func(objectKey string) bool {
		return startKey != "" && objectKey <= startKey
	}
	// keyMatchesPrefix reports whether objectKey passes the prefix filter.
	keyMatchesPrefix := func(objectKey string) bool {
		return prefix == "" || strings.HasPrefix(objectKey, prefix)
	}

	for _, objectKey := range allObjectKeys {
		if keyAtOrBeforeStart(objectKey) {
			continue
		}
		if !keyMatchesPrefix(objectKey) {
			continue
		}

		// Leaf 2.4 fix 12: truncation check runs BEFORE adding either an
		// object or a new common prefix.
		if processedCount >= maxKeys {
			isTruncated = true
			nextContinuationToken = objectKey
			break
		}

		if delimiter != "" {
			keyPartAfterRequestPrefix := objectKey
			if strings.HasPrefix(objectKey, prefix) {
				keyPartAfterRequestPrefix = objectKey[len(prefix):]
			} else if prefix != "" {
				continue
			}

			if idx := strings.Index(keyPartAfterRequestPrefix, delimiter); idx != -1 {
				commonPrefixValue := prefix + keyPartAfterRequestPrefix[:idx+len(delimiter)]
				if _, exists := commonPrefixesMap[commonPrefixValue]; !exists {
					// First-seen roll-up counts toward maxKeys (fix 12);
					// duplicates are free (dedupe before counting).
					commonPrefixesMap[commonPrefixValue] = struct{}{}
					processedCount++
				}
				continue
			}
		}

		metaJSON, err := os.ReadFile(filepath.Join(metadataDir, objectKey+".meta"))
		if err != nil {
			log.Printf("Error reading metadata for %s/%s: %v. Skipping.", bucketName, objectKey, err)
			continue
		}
		var meta ObjectMetadata
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			log.Printf("Error unmarshalling metadata for %s/%s: %v. Skipping.", bucketName, objectKey, err)
			continue
		}

		objectKeyOut := objectKey
		if encodeKeys {
			objectKeyOut = s3URLEncode(objectKey)
		}
		objects = append(objects, Object{
			Key:          objectKeyOut,
			LastModified: meta.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         fmt.Sprintf("\"%s\"", meta.ETag),
			Size:         meta.ContentLength,
			StorageClass: "STANDARD",
		})
		processedCount++
	}

	// Leaf 2.4 fix 14: IsTruncated=true must carry a non-empty token; if the
	// token is empty, no next page exists → report IsTruncated=false.
	if isTruncated && nextContinuationToken == "" {
		isTruncated = false
	}

	var commonPrefixEntries []CommonPrefix
	for cp := range commonPrefixesMap {
		cpOut := cp
		if encodeKeys {
			cpOut = s3URLEncode(cp)
		}
		commonPrefixEntries = append(commonPrefixEntries, CommonPrefix{Prefix: cpOut})
	}
	sort.Slice(commonPrefixEntries, func(i, j int) bool {
		return commonPrefixEntries[i].Prefix < commonPrefixEntries[j].Prefix
	})

	result := ListBucketResult{
		IsTruncated:           isTruncated,
		Contents:              objects,
		Name:                  bucketName,
		Prefix:                prefix,
		Delimiter:             delimiter,
		MaxKeys:               maxKeys,
		CommonPrefixes:        commonPrefixEntries,
		KeyCount:              len(objects) + len(commonPrefixEntries),
		ContinuationToken:     continuationToken,
		NextContinuationToken: nextContinuationToken,
		StartAfter:            startAfter,
	}
	if encodeKeys {
		result.EncodingType = "url"
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Successfully served ListObjectsV2 for bucket %s", bucketName)
}

// parseInt converts a string to an integer, rejecting partial parses like "5a"
func parseInt(valueStr string, paramName string) (int, error) {
	val, err := strconv.Atoi(valueStr)
	if err != nil {
		log.Printf("Invalid %s value: %s", paramName, valueStr)
		return 0, err
	}
	return val, nil
}

// validateObjectKey validates S3 object key constraints, including
// path-traversal rejection (leaf 2.4 fix 2).
//
// DIVERGENCE from real S3 (documented per plan): real S3 permits ".." as a
// key segment (keys are flat strings). Here safety wins: any path segment
// equal to ".." is rejected, as is any ".metadata" segment (it would let a
// key reach the metadata subtree or collide with the metadata directory
// itself). Segments like "a..b", "..hidden", "x.metadata" are fine — only
// whole segments match.
func validateObjectKey(key string) error {
	if len(key) == 0 {
		return fmt.Errorf("object key cannot be empty")
	}
	if len(key) > 1024 {
		return fmt.Errorf("object key cannot exceed 1024 characters")
	}
	// Check for null bytes
	if strings.ContainsRune(key, 0) {
		return fmt.Errorf("object key cannot contain null bytes")
	}
	// Traversal defense: cleaned path must not escape the bucket, and no
	// segment may be ".." or ".metadata".
	cleaned := filepath.Clean("/" + key)
	if cleaned == "/.." || strings.HasPrefix(cleaned, "/../") {
		return fmt.Errorf("object key cannot escape the bucket directory")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return fmt.Errorf("object key cannot contain %q path segments", "..")
		}
		if seg == ".metadata" {
			return fmt.Errorf("object key cannot contain %q path segments", ".metadata")
		}
	}
	return nil
}

// cleanupEmptyDirs removes empty directories up to stopAt directory
func cleanupEmptyDirs(dir, stopAt string) {
	for dir != stopAt && dir != "." && dir != "/" {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		if err := os.Remove(dir); err != nil {
			break
		}
		dir = filepath.Dir(dir)
	}
}
