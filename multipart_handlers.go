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
	"regexp"
	"strings"
	"sync"
	"time"
)

// multipart_handlers.go — S3 multipart upload handlers

// multipartLocks protects concurrent read-modify-write operations on multipart upload metadata
var multipartLocks sync.Map // map[string]*sync.Mutex — keyed by upload metadata file path

// getMultipartLock returns the mutex for a given upload metadata path, creating one if needed
func getMultipartLock(path string) *sync.Mutex {
	mu, _ := multipartLocks.LoadOrStore(path, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// uploadIDRegex matches exactly the upload IDs this server generates (32-char lowercase md5 hex).
var uploadIDRegex = regexp.MustCompile(`^[0-9a-f]{32}$`)

// validateUploadID rejects upload IDs that are not 32-char lowercase hex.
// Must be called BEFORE any path join so a hostile uploadID cannot traverse
// out of .uploads. Callers map the error to InvalidArgument/400.
func validateUploadID(id string) error {
	if !uploadIDRegex.MatchString(id) {
		return fmt.Errorf("invalid uploadId %q: must be 32 hex characters", id)
	}
	return nil
}

// Multipart Handlers
func initiateMultipartUploadHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)

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
		log.Printf("Bucket %s does not exist for InitiateMultipartUpload", bucketName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// Generate a unique UploadID
	uploadID := fmt.Sprintf("%d-%s", time.Now().UnixNano(), objectName)
	hash := md5.Sum([]byte(uploadID))
	uploadID = hex.EncodeToString(hash[:])

	uploadsDir := filepath.Join(bucketPath, ".metadata", ".uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		log.Printf("Error creating .uploads directory %s: %v", uploadsDir, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating upload storage.")))
		return
	}

	mpUpload := MultipartUpload{
		UploadID:       uploadID,
		Key:            objectName,
		Initiated:      time.Now().UTC(),
		ContentType:    r.Header.Get("Content-Type"),
		CustomMetadata: make(map[string]string),
		Parts:          make(map[int]PartMetadata),
	}

	// Capture x-amz-meta-* custom headers for propagation to final object
	for headerName, headerValues := range r.Header {
		if strings.HasPrefix(strings.ToLower(headerName), "x-amz-meta-") {
			mpUpload.CustomMetadata[headerName] = strings.Join(headerValues, ", ")
		}
	}

	mpUploadMetaPath := filepath.Join(uploadsDir, uploadID+".json")
	if err := writeFileAtomicJSON(mpUploadMetaPath, mpUpload, 0644); err != nil {
		log.Printf("Error writing multipart upload metadata file %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error writing upload metadata.")))
		return
	}

	result := InitiateMultipartUploadResult{
		Bucket:   bucketName,
		Key:      objectName,
		UploadID: uploadID,
	}

	x, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Printf("Error marshalling InitiateMultipartUploadResult to XML: %v", err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error formatting response.")))
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(x)
	log.Printf("Initiated multipart upload for %s/%s with UploadID: %s", bucketName, objectName, uploadID)
}

func uploadPartHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName, partNumberStr, uploadID string) {
	// Validate uploadID BEFORE any path join
	if err := validateUploadID(uploadID); err != nil {
		log.Printf("Invalid uploadId in UploadPart: %v", err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidArgument", "Invalid upload id.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	mpUploadMetaPath := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+".json")

	partNumber, err := parseInt(partNumberStr, "partNumber")
	if err != nil {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidArgument", "Invalid part number.")))
		return
	}
	if partNumber < 1 || partNumber > 10000 {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidArgument", "Part number must be between 1 and 10000.")))
		return
	}

	// Acquire lock to prevent race conditions with concurrent part uploads to the same upload
	uploadLock := getMultipartLock(mpUploadMetaPath)
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Read multipart upload metadata
	metaJSON, err := os.ReadFile(mpUploadMetaPath)
	if os.IsNotExist(err) {
		log.Printf("Multipart upload metadata %s not found for UploadID %s", mpUploadMetaPath, uploadID)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}
	if err != nil {
		log.Printf("Error reading multipart upload metadata %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading upload metadata.")))
		return
	}

	var mpUpload MultipartUpload
	if err := json.Unmarshal(metaJSON, &mpUpload); err != nil {
		log.Printf("Error unmarshalling multipart upload metadata from %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error parsing upload metadata.")))
		return
	}

	if mpUpload.Key != objectName {
		log.Printf("Object name mismatch for UploadID %s. Expected %s, got %s", uploadID, mpUpload.Key, objectName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}

	// Read part data
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body for part %d of %s/%s (UploadID %s): %v", partNumber, bucketName, objectName, uploadID, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading part data.")))
		return
	}
	defer r.Body.Close()

	// Handle aws-chunked Content-Encoding (used by AWS CLI v2)
	contentEncoding := r.Header.Get("Content-Encoding")
	if strings.Contains(contentEncoding, "aws-chunked") {
		decodedBody, err := decodeAWSChunked(body)
		if err != nil {
			log.Printf("Error decoding aws-chunked body for part %d of %s/%s: %v", partNumber, bucketName, objectName, err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(errorToXML("InvalidArgument", "Failed to decode chunked part body.")))
			return
		}
		body = decodedBody
		log.Printf("Decoded aws-chunked part body: %d bytes", len(body))
	}

	partSize := int64(len(body))

	// Calculate ETag for the part (MD5 hash)
	hash := md5.Sum(body)
	eTag := hex.EncodeToString(hash[:])

	// Store the part data
	partsDir := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+"_parts")
	if err := os.MkdirAll(partsDir, 0755); err != nil {
		log.Printf("Error creating directory for parts %s: %v", partsDir, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating part storage.")))
		return
	}
	partPath := filepath.Join(partsDir, fmt.Sprintf("part-%d", partNumber))
	// Part files are rewritten on retry — write atomically so a torn part
	// file can never be picked up by CompleteMultipartUpload.
	if err := writeFileAtomic(partPath, body, 0644); err != nil {
		log.Printf("Error writing part data to %s: %v", partPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error writing part data.")))
		return
	}

	// Update multipart upload metadata
	mpUpload.Parts[partNumber] = PartMetadata{
		PartNumber: partNumber,
		ETag:       eTag,
		Size:       partSize,
		StoredPath: partPath,
	}

	if err := writeFileAtomicJSON(mpUploadMetaPath, mpUpload, 0644); err != nil {
		log.Printf("Error writing updated multipart upload metadata file %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error saving upload metadata.")))
		return
	}

	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", eTag))
	w.WriteHeader(http.StatusOK)
	log.Printf("Successfully uploaded part %d for %s/%s (UploadID %s), ETag: %s", partNumber, bucketName, objectName, uploadID, eTag)
}

func completeMultipartUploadHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName, uploadID string) {
	// Validate uploadID BEFORE any path join
	if err := validateUploadID(uploadID); err != nil {
		log.Printf("Invalid uploadId in CompleteMultipartUpload: %v", err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidArgument", "Invalid upload id.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	mpUploadMetaPath := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+".json")
	partsDir := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+"_parts")

	// Acquire lock to prevent race with concurrent uploadPartHandler calls
	uploadLock := getMultipartLock(mpUploadMetaPath)
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Read multipart upload metadata
	metaJSON, err := os.ReadFile(mpUploadMetaPath)
	if os.IsNotExist(err) {
		log.Printf("Multipart upload metadata %s not found for UploadID %s (Complete)", mpUploadMetaPath, uploadID)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}
	if err != nil {
		log.Printf("Error reading multipart upload metadata %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading upload metadata.")))
		return
	}

	var mpUpload MultipartUpload
	if err := json.Unmarshal(metaJSON, &mpUpload); err != nil {
		log.Printf("Error unmarshalling multipart upload metadata from %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error parsing upload metadata.")))
		return
	}

	if mpUpload.Key != objectName {
		log.Printf("Object name mismatch for UploadID %s during complete. Expected %s, got %s", uploadID, mpUpload.Key, objectName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}

	// Parse the XML body for part numbers and ETags
	var completeRequest CompleteMultipartUpload
	if err := xml.NewDecoder(r.Body).Decode(&completeRequest); err != nil {
		log.Printf("Error decoding CompleteMultipartUpload XML for %s/%s (UploadID %s): %v", bucketName, objectName, uploadID, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("MalformedXML", "The XML you provided was not well-formed.")))
		return
	}
	defer r.Body.Close()

	if len(completeRequest.Parts) == 0 {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidPart", "You must specify at least one part.")))
		return
	}

	// Verify parts and prepare for assembly
	// Object data stored directly in bucket (consistent with putObjectHandler)
	finalObjectPath := filepath.Join(bucketPath, objectName)

	// Create parent directories if needed
	if err := os.MkdirAll(filepath.Dir(finalObjectPath), 0755); err != nil {
		log.Printf("Error creating parent directories for final object %s: %v", finalObjectPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating object storage.")))
		return
	}

	// Atomic assembly: parts are copied into a temp file in the same
	// directory and only renamed over the final object once every part
	// has been copied and the metadata written. Until the rename, the
	// previous object version stays fully intact, and lockObject holds
	// so no concurrent GET/PUT can observe the temp file.
	unlock := lockObject(finalObjectPath)
	defer unlock()

	// Temp assembly file in the same directory (same filesystem for rename).
	finalTempPath := finalObjectPath + ".tmp-multipart"
	finalTempFile, err := os.OpenFile(finalTempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("Error creating temporary assembly file %s: %v", finalTempPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating object file.")))
		return
	}
	// failCleanup removes the temp file on every error path after creation.
	// On success it must NOT run (rename already consumed the temp file).
	failCleanup := true
	defer func() {
		if failCleanup {
			if err := os.Remove(finalTempPath); err != nil && !os.IsNotExist(err) {
				log.Printf("Warning: Error removing temporary assembly file %s: %v", finalTempPath, err)
			}
		}
	}()

	var totalSize int64
	var partETags []string // To calculate the final ETag

	for i, partToUpload := range completeRequest.Parts {
		// S3: Parts must be ordered by PartNumber
		if i > 0 && partToUpload.PartNumber <= completeRequest.Parts[i-1].PartNumber {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(errorToXML("InvalidPartOrder", "Parts must be ordered by part number.")))
			return
		}

		storedPartMeta, ok := mpUpload.Parts[partToUpload.PartNumber]
		if !ok {
			log.Printf("Part number %d not found in multipart upload %s", partToUpload.PartNumber, uploadID)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(errorToXML("InvalidPart", fmt.Sprintf("Part number %d not found in upload.", partToUpload.PartNumber))))
			return
		}
		// Handle quoted ETags
		requestETag := strings.Trim(partToUpload.ETag, "\"")
		if storedPartMeta.ETag != requestETag {
			log.Printf("ETag mismatch for part %d of upload %s. Expected %s, got %s", partToUpload.PartNumber, uploadID, storedPartMeta.ETag, requestETag)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(errorToXML("InvalidPart", fmt.Sprintf("ETag mismatch for part number %d.", partToUpload.PartNumber))))
			return
		}

		partFile, err := os.Open(storedPartMeta.StoredPath)
		if err != nil {
			log.Printf("Error opening part data %s for assembly: %v", storedPartMeta.StoredPath, err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errorToXML("InternalError", "Could not access part data.")))
			return
		}
		written, err := io.Copy(finalTempFile, partFile)
		partFile.Close()
		if err != nil {
			log.Printf("Error copying part %d data to temporary assembly file: %v", storedPartMeta.PartNumber, err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errorToXML("InternalError", "Error during object assembly.")))
			return
		}
		totalSize += written
		partETags = append(partETags, storedPartMeta.ETag)
	}

	// Flush part data to disk before the rename
	if err := finalTempFile.Sync(); err != nil {
		log.Printf("Error syncing temporary assembly file %s: %v", finalTempPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error during object assembly.")))
		return
	}
	if err := finalTempFile.Close(); err != nil {
		log.Printf("Error closing temporary assembly file %s: %v", finalTempPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error during object assembly.")))
		return
	}

	// Calculate final ETag for the assembled object
	// S3's ETag for multipart uploads is MD5 of concatenated binary MD5s of parts, followed by "-<number of parts>"
	finalETagHash := md5.New()
	for _, partETag := range partETags {
		// Assuming partETag is hex string of MD5, decode it first
		decodedETag, _ := hex.DecodeString(partETag)
		finalETagHash.Write(decodedETag)
	}
	finalETag := fmt.Sprintf("\"%s-%d\"", hex.EncodeToString(finalETagHash.Sum(nil)), len(partETags))

	// Store metadata for the completed object (consistent with PutObject)
	objectMetadataDir := filepath.Join(bucketPath, ".metadata")
	objectMetadataPath := filepath.Join(objectMetadataDir, objectName+".meta")

	// Create parent directories for metadata
	if err := os.MkdirAll(filepath.Dir(objectMetadataPath), 0755); err != nil {
		log.Printf("Error creating metadata directories for %s: %v", objectMetadataPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error creating metadata storage.")))
		return
	}

	meta := ObjectMetadata{
		ContentType:    mpUpload.ContentType,
		ContentLength:  totalSize,
		ETag:           strings.Trim(finalETag, "\""),
		CustomMetadata: mpUpload.CustomMetadata,
		LastModified:   time.Now().UTC(),
		StoragePath:    finalObjectPath, // Points to actual object data
	}

	if err := writeFileAtomicJSON(objectMetadataPath, meta, 0644); err != nil {
		log.Printf("Error writing final object metadata file %s: %v", objectMetadataPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error writing metadata.")))
		return
	}

	// Rename the fully assembled temp file over the final object. Until this
	// point the previous object version was untouched.
	if err := os.Rename(finalTempPath, finalObjectPath); err != nil {
		log.Printf("Error renaming temporary assembly file %s over %s: %v", finalTempPath, finalObjectPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error finalizing object.")))
		return
	}
	failCleanup = false // rename consumed the temp file

	// Clean up: delete the multipart upload metadata file and the temporary parts directory
	if err := os.Remove(mpUploadMetaPath); err != nil {
		log.Printf("Warning: Error deleting multipart upload metadata file %s: %v", mpUploadMetaPath, err)
	}
	if err := os.RemoveAll(partsDir); err != nil {
		log.Printf("Warning: Error deleting temporary parts directory %s: %v", partsDir, err)
	}

	result := CompletedMultipartUploadResult{
		Location: "https://" + r.Host + r.URL.Path, // Construct full object URL
		Bucket:   bucketName,
		Key:      objectName,
		ETag:     finalETag,
	}
	x, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Printf("Error marshalling CompletedMultipartUploadResult to XML: %v", err)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(x)
	log.Printf("Successfully completed multipart upload for %s/%s, UploadID: %s, Final ETag: %s", bucketName, objectName, uploadID, finalETag)

	// Trigger after_upload actions for completed multipart upload
	go triggerActions("after_upload", ActionContext{
		FilePath:     finalObjectPath,
		MetadataPath: objectMetadataPath,
		BucketName:   bucketName,
		BucketPath:   bucketPath,
		ObjectKey:    objectName,
		ContentType:  meta.ContentType,
		ETag:         strings.Trim(finalETag, "\""),
		Size:         totalSize,
	})
}

func abortMultipartUploadHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName, uploadID string) {
	// Validate uploadID BEFORE any path join
	if err := validateUploadID(uploadID); err != nil {
		log.Printf("Invalid uploadId in AbortMultipartUpload: %v", err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorToXML("InvalidArgument", "Invalid upload id.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	mpUploadMetaPath := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+".json")
	partsDir := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+"_parts")

	// Acquire lock to prevent race with concurrent part uploads
	uploadLock := getMultipartLock(mpUploadMetaPath)
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Read the upload metadata first: key validation needs it, and the
	// stat fast-path is folded into the IsNotExist check on the read.
	metaJSON, err := os.ReadFile(mpUploadMetaPath)
	if os.IsNotExist(err) {
		log.Printf("Multipart upload metadata %s not found for UploadID %s (Abort)", mpUploadMetaPath, uploadID)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}
	if err != nil {
		log.Printf("Error reading multipart upload metadata %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error reading upload metadata.")))
		return
	}

	var mpUpload MultipartUpload
	if err := json.Unmarshal(metaJSON, &mpUpload); err != nil {
		log.Printf("Error unmarshalling multipart upload metadata from %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error parsing upload metadata.")))
		return
	}

	// Key validation (mirrors complete): an upload may only be aborted via
	// its own key, so one client cannot kill another object's upload.
	if mpUpload.Key != objectName {
		log.Printf("Object name mismatch for UploadID %s during abort. Expected %s, got %s", uploadID, mpUpload.Key, objectName)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}

	// Delete the multipart upload metadata file
	if err := os.Remove(mpUploadMetaPath); err != nil && !os.IsNotExist(err) {
		log.Printf("Error deleting multipart upload metadata file %s: %v", mpUploadMetaPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errorToXML("InternalError", "Error aborting upload.")))
		return
	}

	// Delete the temporary parts directory
	if err := os.RemoveAll(partsDir); err != nil && !os.IsNotExist(err) {
		log.Printf("Error deleting temporary parts directory %s: %v", partsDir, err)
		// Don't fail - metadata already deleted
	}

	w.WriteHeader(http.StatusNoContent)
	log.Printf("Successfully aborted multipart upload for %s/%s, UploadID: %s", bucketName, objectName, uploadID)
}
