package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
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

	// Validate object key
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s: %v", objectName, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidArgument", err.Error())))
		return
	}

	// Ensure bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for PutObject", bucketName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body for %s/%s: %v", bucketName, objectName, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading request body.")))
		return
	}
	defer r.Body.Close()

	// Handle aws-chunked Content-Encoding (used by AWS CLI v2)
	contentEncoding := r.Header.Get("Content-Encoding")
	if strings.Contains(contentEncoding, "aws-chunked") {
		decodedBody, err := decodeAWSChunked(body)
		if err != nil {
			log.Printf("Error decoding aws-chunked body for %s/%s: %v", bucketName, objectName, err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(errorToXML("InvalidArgument", "Failed to decode chunked body.")))
			return
		}
		body = decodedBody
		log.Printf("Decoded aws-chunked body: %d bytes", len(body))
	}

	// Calculate ETag (MD5 hash of the content)
	hash := md5.Sum(body)
	eTag := hex.EncodeToString(hash[:])

	// Create parent directories for the object data if they don't exist
	objectDataParentDir := filepath.Dir(objectDataPath)
	if err := os.MkdirAll(objectDataParentDir, 0755); err != nil {
		log.Printf("Error creating parent directories for object data %s: %v", objectDataPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating object storage.")))
		return
	}

	// Write the object data
	if err := os.WriteFile(objectDataPath, body, 0644); err != nil {
		log.Printf("Error writing object data to %s: %v", objectDataPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error writing object data.")))
		return
	}

	// Create parent directories for the metadata file if they don't exist
	metadataParentDir := filepath.Dir(objectMetadataPath)
	if err := os.MkdirAll(metadataParentDir, 0755); err != nil {
		log.Printf("Error creating parent directories for metadata %s: %v", objectMetadataPath, err)
		// Clean up the object data file since metadata creation failed
		os.Remove(objectDataPath)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating metadata storage.")))
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

	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		log.Printf("Error marshalling metadata for %s/%s: %v", bucketName, objectName, err)
		os.Remove(objectDataPath) // Clean up object data
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating metadata.")))
		return
	}

	if err := os.WriteFile(objectMetadataPath, metaJSON, 0644); err != nil {
		log.Printf("Error writing metadata file %s: %v", objectMetadataPath, err)
		os.Remove(objectDataPath) // Clean up object data
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error writing metadata.")))
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

func getObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for GetObject", bucketName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// Read metadata
	metaJSON, err := os.ReadFile(objectMetadataPath)
	if os.IsNotExist(err) {
		log.Printf("Object metadata %s not found for %s/%s", objectMetadataPath, bucketName, objectName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchKey", "The specified key does not exist.")))
		return
	}
	if err != nil {
		log.Printf("Error reading metadata file %s: %v", objectMetadataPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading object metadata.")))
		return
	}

	var meta ObjectMetadata
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		log.Printf("Error unmarshalling metadata from %s: %v", objectMetadataPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error parsing object metadata.")))
		return
	}

	// Check if actual object data file exists
	objectDataPath := meta.StoragePath
	if _, err := os.Stat(objectDataPath); os.IsNotExist(err) {
		log.Printf("Object data file %s not found for %s/%s", objectDataPath, bucketName, objectName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchKey", "The specified key does not exist.")))
		return
	}

	// Set headers from metadata
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.ContentLength))
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	for k, v := range meta.CustomMetadata {
		w.Header().Set(k, v)
	}

	// Stream the object data
	file, err := os.Open(objectDataPath)
	if err != nil {
		log.Printf("Error opening object data file %s: %v", objectDataPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading object data.")))
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

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for DeleteObject", bucketName)
		// S3 returns 204 No Content for delete even if object doesn't exist
		w.WriteHeader(http.StatusNoContent)
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
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errorToXML("InternalError", "Error deleting object data.")))
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

	// Check if actual object data file exists
	if _, err := os.Stat(meta.StoragePath); os.IsNotExist(err) {
		log.Printf("Object data file %s not found for %s/%s during HeadObject", meta.StoragePath, bucketName, objectName)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Set headers from metadata
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.ContentLength))
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	for k, v := range meta.CustomMetadata {
		w.Header().Set(k, v)
	}

	w.WriteHeader(http.StatusOK)
	log.Printf("Successfully served HEAD for object %s/%s", bucketName, objectName)
}

// listObjectsV2Handler implementation
func listObjectsV2Handler(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucketPath := getBucketPath(bucketName)
	metadataDir := filepath.Join(bucketPath, ".metadata")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for ListObjectsV2", bucketName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// Parse query parameters
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	continuationToken := r.URL.Query().Get("continuation-token")
	startAfter := r.URL.Query().Get("start-after")
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
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error listing objects.")))
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

	for _, objectKey := range allObjectKeys {
		if startKey != "" && objectKey <= startKey {
			continue
		}

		if prefix != "" && !strings.HasPrefix(objectKey, prefix) {
			continue
		}

		if processedCount >= maxKeys && maxKeys > 0 {
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
					commonPrefixesMap[commonPrefixValue] = struct{}{}
				}
				continue
			}
		}

		if maxKeys == 0 {
			isTruncated = len(allObjectKeys) > 0
			if isTruncated && len(allObjectKeys) > 0 && (startKey == "" || allObjectKeys[0] > startKey) {
				for _, k := range allObjectKeys {
					if startKey != "" && k <= startKey {
						continue
					}
					if prefix != "" && !strings.HasPrefix(k, prefix) {
						continue
					}
					nextContinuationToken = k
					break
				}
			}
			break
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

		objects = append(objects, Object{
			Key:          objectKey,
			LastModified: meta.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         fmt.Sprintf("\"%s\"", meta.ETag),
			Size:         meta.ContentLength,
			StorageClass: "STANDARD",
		})
		processedCount++
	}

	var commonPrefixEntries []CommonPrefix
	for cp := range commonPrefixesMap {
		commonPrefixEntries = append(commonPrefixEntries, CommonPrefix{Prefix: cp})
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

	x, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Printf("Error marshalling ListBucketResult to XML for bucket %s: %v", bucketName, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error formatting object list response.")))
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(x)
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

// validateObjectKey validates S3 object key constraints
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
