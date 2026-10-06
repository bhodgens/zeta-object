package s3

import (
	"crypto/md5" //nolint:gosec // G501: MD5 is the S3 ETag/upload-ID algorithm — protocol requirement, not crypto.
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
)

// multipart_handlers.go — S3 multipart upload handlers

// multipartUploadExpiry is how long an in-progress multipart upload session
// may sit untouched before the lazy expiry sweep aborts and deletes it
// (leaf 3.3; mirrors real S3's 7-day abort rule).
const multipartUploadExpiry = 7 * 24 * time.Hour

// minPartSize is the S3 minimum size for every part except the final one;
// smaller non-final parts are rejected with EntityTooSmall at complete.
const minPartSize = 5 * 1024 * 1024

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
//
// Returns the validated id unchanged; in the success case the id is
// guaranteed to match ^[0-9a-f]{32}$ (no path separators, no "..", no
// traversal potential), which neutralizes downstream G703 path-traversal
// taint on any path built from it.
func validateUploadID(id string) (string, error) {
	if !uploadIDRegex.MatchString(id) {
		return "", fmt.Errorf("invalid uploadId %q: must be 32 hex characters", id)
	}
	return id, nil
}

// principalOfRequest returns the authenticated principal's AccessKeyID for
// breadcrumb stamping and audit attribution. The identity is published
// into the request context by serveHTTP after authentication (the same
// seam CopyObject's source-bucket grant check consumes); requests that
// bypassed dispatch (direct handler invocation in tests) degrade to the
// legacy wildcard principal exactly like identityOf.
func principalOfRequest(r *http.Request) string {
	return identityOf(r).AccessKeyID
}

// Multipart Handlers
func initiateMultipartUploadHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)

	// Validate object key
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s: %v", strconv.Quote(objectName), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidArgument", err.Error())))
		return
	}

	// Ensure bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) { //nolint:gosec // G703: bucketPath derived from validateBucketName-checked name
		log.Printf("Bucket %s does not exist for InitiateMultipartUpload", strconv.Quote(bucketName))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchBucket", "The specified bucket does not exist.")))
		return
	}

	// Generate a unique UploadID. MD5 is used ONLY as a non-cryptographic
	// identifier generator (S3 ETags are MD5 too) — gosec G401/G501 are
	// by-design false positives here, same as the ETag computation.
	uploadID := fmt.Sprintf("%d-%s", time.Now().UnixNano(), objectName)
	hash := md5.Sum([]byte(uploadID)) //nolint:gosec // G401: identifier generation, not crypto — see comment above.
	uploadID = hex.EncodeToString(hash[:])

	uploadsDir := filepath.Join(bucketPath, ".metadata", ".uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil { //nolint:gosec // G703: bucketPath derived from validateBucketName-checked name
		log.Printf("Error creating .uploads directory %s: %v", uploadsDir, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating upload storage.")))
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
		log.Printf("Error writing multipart upload metadata file %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error writing upload metadata.")))
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
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error formatting response.")))
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(x)
	log.Printf("Initiated multipart upload for %s/%s with UploadID: %s", strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(uploadID))
}

func uploadPartHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName, partNumberStr, uploadID string) {
	// Validate uploadID BEFORE any path join
	validatedID, err := validateUploadID(uploadID)
	if err != nil {
		log.Printf("Invalid uploadId in UploadPart: %v", err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Invalid upload id.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	uploadID = validatedID
	mpUploadMetaPath := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+".json")
	partNumber, err := parseInt(partNumberStr, "partNumber")
	if err != nil {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Invalid part number.")))
		return
	}
	if partNumber < 1 || partNumber > 10000 {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Part number must be between 1 and 10000.")))
		return
	}

	// Acquire lock to prevent race conditions with concurrent part uploads to the same upload
	uploadLock := getMultipartLock(mpUploadMetaPath)
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Read multipart upload metadata. mpUploadMetaPath is built from
	// uploadID (validated 32-hex by validateUploadID above) joined under
	// bucketPath — traversal is impossible, so G703 is a false positive.
	metaJSON, err := os.ReadFile(mpUploadMetaPath) //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
	if os.IsNotExist(err) {
		log.Printf("Multipart upload metadata %s not found for UploadID %s", strconv.Quote(mpUploadMetaPath), strconv.Quote(uploadID))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}
	if err != nil {
		log.Printf("Error reading multipart upload metadata %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error reading upload metadata.")))
		return
	}

	var mpUpload MultipartUpload
	if err := json.Unmarshal(metaJSON, &mpUpload); err != nil {
		log.Printf("Error unmarshalling multipart upload metadata from %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error parsing upload metadata.")))
		return
	}

	if mpUpload.Key != objectName {
		log.Printf("Object name mismatch for UploadID %s. Expected %s, got %s", strconv.Quote(uploadID), strconv.Quote(mpUpload.Key), strconv.Quote(objectName))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}

	// Read part data
	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		//nolint:gosec // G706 false positive: every interpolated value is
		// sanitized via strconv.Quote (gosec's own listed sanitizer); the
		// taint analyzer still flags the call site — see leaf 1.2 report.
		log.Printf("Error reading request body for part %d of %s/%s (UploadID %s): %v", partNumber, strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(uploadID), readErr)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error reading part data.")))
		return
	}
	defer r.Body.Close()

	// Handle aws-chunked Content-Encoding (used by AWS CLI v2). Signed
	// streaming bodies were already decoded + signature-verified in
	// authenticateRequest (leaf 3.4) — skip the second decode pass then.
	contentEncoding := r.Header.Get("Content-Encoding")
	if strings.Contains(contentEncoding, "aws-chunked") && !isDecodedStreaming(r.Context()) {
		decodedBody, decodeErr := decodeAWSChunked(body)
		if decodeErr != nil {
			//nolint:gosec // G706 false positive: sanitized via strconv.Quote.
			log.Printf("Error decoding aws-chunked body for part %d of %s/%s: %v", partNumber, strconv.Quote(bucketName), strconv.Quote(objectName), decodeErr)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Failed to decode chunked part body.")))
			return
		}
		body = decodedBody
		// Wire VerifyDecodedLength (leaf 2.2 helper) — truncated/lying
		// aws-chunked part uploads are rejected instead of stored.
		if lenErr := VerifyDecodedLength(r.Header.Get("x-amz-decoded-content-length"), len(body)); lenErr != nil {
			//nolint:gosec // G706 false positive: sanitized via strconv.Quote.
			log.Printf("Decoded length mismatch for part %d of %s/%s: %v", partNumber, strconv.Quote(bucketName), strconv.Quote(objectName), lenErr)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Decoded content length mismatch.")))
			return
		}
		log.Printf("Decoded aws-chunked part body: %d bytes", len(body))
	}

	partSize := int64(len(body))

	// Calculate ETag for the part. MD5 is the S3 ETag algorithm — required
	// for S3 protocol compatibility, not a security primitive (gosec G401).
	hash := md5.Sum(body) //nolint:gosec // G401: S3 ETags are defined as MD5; protocol requirement, not crypto.
	eTag := hex.EncodeToString(hash[:])

	// Store the part data. partsDir embeds uploadID (validated 32-hex) —
	// no traversal possible; G703 false positive.
	partsDir := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+"_parts")
	if err := os.MkdirAll(partsDir, 0755); err != nil { //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
		log.Printf("Error creating directory for parts %s: %v", strconv.Quote(partsDir), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating part storage.")))
		return
	}
	partPath := filepath.Join(partsDir, fmt.Sprintf("part-%d", partNumber))
	// Part files are rewritten on retry — write atomically so a torn part
	// file can never be picked up by CompleteMultipartUpload.
	if err := writeFileAtomic(partPath, body, 0644); err != nil {
		log.Printf("Error writing part data to %s: %v", strconv.Quote(partPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error writing part data.")))
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
		log.Printf("Error writing updated multipart upload metadata file %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error saving upload metadata.")))
		return
	}

	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", eTag))
	w.WriteHeader(http.StatusOK)
	//nolint:gosec // G706 false positive: sanitized via strconv.Quote.
	log.Printf("Successfully uploaded part %d for %s/%s (UploadID %s), ETag: %s", partNumber, strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(uploadID), strconv.Quote(eTag))
}

// completeMultipartUploadHandler finalizes a multipart upload: verifies the
// requested part list, assembles the parts into the final object atomically,
// and records metadata. objectName is re-validated before any path join.
// Decomposed (leaf 1.2): validation+load → assembleParts → finalizeComplete
// keep each function under the gocyclo ceiling; behavior is identical.
func completeMultipartUploadHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName, uploadID string) {
	// Validate uploadID BEFORE any path join
	validatedID, err := validateUploadID(uploadID)
	if err != nil {
		log.Printf("Invalid uploadId in CompleteMultipartUpload: %v", err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Invalid upload id.")))
		return
	}

	// Validate object key BEFORE any path join (mirrors uploadPartHandler);
	// guarantees no ".."/".metadata" segments and no escape from bucketPath.
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s: %v", strconv.Quote(objectName), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Invalid object key.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	uploadID = validatedID
	mpUploadMetaPath := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+".json")
	partsDir := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+"_parts")

	// Acquire lock to prevent race with concurrent uploadPartHandler calls
	uploadLock := getMultipartLock(mpUploadMetaPath)
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Read multipart upload metadata. mpUploadMetaPath embeds uploadID
	// (validated 32-hex) — no traversal possible; G703 false positive.
	metaJSON, err := os.ReadFile(mpUploadMetaPath) //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
	if os.IsNotExist(err) {
		log.Printf("Multipart upload metadata %s not found for UploadID %s (Complete)", strconv.Quote(mpUploadMetaPath), strconv.Quote(uploadID))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}
	if err != nil {
		log.Printf("Error reading multipart upload metadata %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error reading upload metadata.")))
		return
	}

	var mpUpload MultipartUpload
	if err := json.Unmarshal(metaJSON, &mpUpload); err != nil {
		log.Printf("Error unmarshalling multipart upload metadata from %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error parsing upload metadata.")))
		return
	}

	if mpUpload.Key != objectName {
		log.Printf("Object name mismatch for UploadID %s during complete. Expected %s, got %s", strconv.Quote(uploadID), strconv.Quote(mpUpload.Key), strconv.Quote(objectName))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}

	// Parse the XML body for part numbers and ETags
	var completeRequest CompleteMultipartUpload
	if err := xml.NewDecoder(r.Body).Decode(&completeRequest); err != nil {
		log.Printf("Error decoding CompleteMultipartUpload XML for %s/%s (UploadID %s): %v", strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(uploadID), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("MalformedXML", "The XML you provided was not well-formed.")))
		return
	}
	defer r.Body.Close()

	if len(completeRequest.Parts) == 0 {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidPart", "You must specify at least one part.")))
		return
	}

	finalObjectPath, objectMetadataPath, meta, finalETag, totalSize, ok := assembleCompletedObject(w, r, bucketPath, objectName, uploadID, mpUpload, completeRequest)

	// failCleanup removes the temp assembly file on every error path after
	// creation (including errors raised inside assembleCompletedObject and
	// finalizeComplete, before the rename). On success it must NOT run —
	// finalizeComplete clears the flag via the pointer once the rename has
	// consumed the temp file. When assembly failed before creating the temp
	// file, finalObjectPath is empty and the Remove is a no-op.
	finalTempPath := finalObjectPath + ".tmp-multipart"
	failCleanup := true
	defer func() {
		if failCleanup {
			if err := os.Remove(finalTempPath); err != nil && !os.IsNotExist(err) { //nolint:gosec // G703: finalTempPath derived from validated objectName; no traversal possible.
				log.Printf("Warning: Error removing temporary assembly file %s: %v", strconv.Quote(finalTempPath), err)
			}
		}
	}()

	if !ok {
		return // error response already written
	}

	finalizeComplete(w, r, bucketName, objectName, uploadID, bucketPath, mpUploadMetaPath, partsDir, finalObjectPath, objectMetadataPath, finalTempPath, meta, finalETag, totalSize, &failCleanup)
}

// assembleCompletedObject verifies each requested part, copies part data
// into a temp assembly file next to the final object, and computes the
// final S3 multipart ETag (MD5 of concatenated binary part MD5s + "-N").
// It writes an error response and returns ok=false on any failure. The
// path fields are populated as soon as they are known (even on failure)
// so the caller's failCleanup defer can remove any created temp file.
func assembleCompletedObject(w http.ResponseWriter, r *http.Request, bucketPath, objectName, uploadID string, mpUpload MultipartUpload, completeRequest CompleteMultipartUpload) (finalObjectPath, objectMetadataPath string, meta ObjectMetadata, finalETag string, totalSize int64, ok bool) {
	// Verify parts and prepare for assembly
	// Object data stored directly in the bucket (consistent with putObjectHandler)
	// objectName was validated by validateObjectKey above (no ".." segments) —
	// the path cannot escape bucketPath; G703 false positive.
	// Shadow-aware (leaf 5.1 [a]-1): a colliding key stores under the shadow
	// data dir exactly like putObjectHandler.
	finalObjectPath = objectDataPathFor(bucketPath, objectName)

	// Create parent directories if needed
	if err := os.MkdirAll(filepath.Dir(finalObjectPath), 0755); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		log.Printf("Error creating parent directories for final object %s: %v", finalObjectPath, err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating object storage.")))
		return finalObjectPath, "", ObjectMetadata{}, "", 0, false
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
	finalTempFile, err := os.OpenFile(finalTempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644) //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
	if err != nil {
		log.Printf("Error creating temporary assembly file %s: %v", strconv.Quote(finalTempPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating object file.")))
		return finalObjectPath, "", ObjectMetadata{}, "", 0, false
	}
	// failCleanup is owned by the CALLER (completeMultipartUploadHandler):
	// its deferred removal must stay pending until after the rename in
	// finalizeComplete, so no cleanup defer is installed here.
	totalSize, partETags, ok := copyPartsToAssembly(w, uploadID, mpUpload, completeRequest, finalTempFile)
	if !ok {
		return finalObjectPath, "", ObjectMetadata{}, "", 0, false
	}

	// Flush part data to disk before the rename
	if err := finalTempFile.Sync(); err != nil {
		log.Printf("Error syncing temporary assembly file %s: %v", strconv.Quote(finalTempPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error during object assembly.")))
		return finalObjectPath, "", ObjectMetadata{}, "", 0, false
	}
	if err := finalTempFile.Close(); err != nil {
		log.Printf("Error closing temporary assembly file %s: %v", strconv.Quote(finalTempPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error during object assembly.")))
		return finalObjectPath, "", ObjectMetadata{}, "", 0, false
	}

	// Calculate final ETag for the assembled object
	// S3's ETag for multipart uploads is MD5 of concatenated binary MD5s of parts, followed by "-<number of parts>"
	// MD5 here is the S3 ETag algorithm — protocol compatibility, not crypto (gosec G401).
	finalETag = computeMultipartETag(partETags)

	// Store metadata for the completed object (consistent with PutObject)
	objectMetadataDir := filepath.Join(bucketPath, ".metadata")
	objectMetadataPath = filepath.Join(objectMetadataDir, objectName+".meta")

	// Create parent directories for metadata
	if err := os.MkdirAll(filepath.Dir(objectMetadataPath), 0755); err != nil { //nolint:gosec // G703: derived from validated objectName; no traversal possible.
		log.Printf("Error creating metadata directories for %s: %v", strconv.Quote(objectMetadataPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error creating metadata storage.")))
		return finalObjectPath, objectMetadataPath, ObjectMetadata{}, "", 0, false
	}

	meta = ObjectMetadata{
		ContentType:    mpUpload.ContentType,
		ContentLength:  totalSize,
		ETag:           strings.Trim(finalETag, "\""),
		CustomMetadata: mpUpload.CustomMetadata,
		LastModified:   time.Now().UTC(),
		StoragePath:    finalObjectPath, // Points to actual object data
	}
	return finalObjectPath, objectMetadataPath, meta, finalETag, totalSize, true
}

// copyPartsToAssembly verifies part order/ETags against mpUpload and copies
// each part file into finalTempFile. Writes an error response and returns
// ok=false on the first bad part.
func copyPartsToAssembly(w http.ResponseWriter, uploadID string, mpUpload MultipartUpload, completeRequest CompleteMultipartUpload, finalTempFile *os.File) (totalSize int64, partETags []string, ok bool) {
	for i, partToUpload := range completeRequest.Parts {
		// S3: Parts must be ordered by PartNumber
		if i > 0 && partToUpload.PartNumber <= completeRequest.Parts[i-1].PartNumber {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errorToXML("InvalidPartOrder", "Parts must be ordered by part number.")))
			return 0, nil, false
		}

		// S3 minimum part size: every part except the LAST one in the
		// complete request must be >= minPartSize (leaf 3.3). Checked before
		// any I/O so the rejection is cheap and the session stays intact.
		isFinalPart := i == len(completeRequest.Parts)-1
		if !isFinalPart {
			if stored, found := mpUpload.Parts[partToUpload.PartNumber]; found && stored.Size < minPartSize {
				log.Printf("Part %d of upload %s is %d bytes, below the %d-byte non-final minimum", partToUpload.PartNumber, strconv.Quote(uploadID), stored.Size, minPartSize)
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(errorToXML("EntityTooSmall", fmt.Sprintf("Your proposed upload is smaller than the minimum allowed size. Each part must be at least %d bytes in size, except the last part.", minPartSize))))
				return 0, nil, false
			}
		}

		storedPartMeta, found := mpUpload.Parts[partToUpload.PartNumber]
		if !found {
			log.Printf("Part number %d not found in multipart upload %s", partToUpload.PartNumber, strconv.Quote(uploadID))
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errorToXML("InvalidPart", fmt.Sprintf("Part number %d not found in upload.", partToUpload.PartNumber))))
			return 0, nil, false
		}
		// Handle quoted ETags
		requestETag := strings.Trim(partToUpload.ETag, "\"")
		if storedPartMeta.ETag != requestETag {
			log.Printf("ETag mismatch for part %d of upload %s. Expected %s, got %s", partToUpload.PartNumber, strconv.Quote(uploadID), storedPartMeta.ETag, requestETag)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errorToXML("InvalidPart", fmt.Sprintf("ETag mismatch for part number %d.", partToUpload.PartNumber))))
			return 0, nil, false
		}

		partFile, err := os.Open(storedPartMeta.StoredPath)
		if err != nil {
			log.Printf("Error opening part data %s for assembly: %v", strconv.Quote(storedPartMeta.StoredPath), err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(errorToXML("InternalError", "Could not access part data.")))
			return 0, nil, false
		}
		written, err := io.Copy(finalTempFile, partFile)
		partFile.Close()
		if err != nil {
			log.Printf("Error copying part %d data to temporary assembly file: %v", storedPartMeta.PartNumber, err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(errorToXML("InternalError", "Error during object assembly.")))
			return 0, nil, false
		}
		totalSize += written
		partETags = append(partETags, storedPartMeta.ETag)
	}
	return totalSize, partETags, true
}

// computeMultipartETag builds the S3 multipart ETag: MD5 of the concatenated
// binary MD5s of the parts, hex-encoded, suffixed with "-<number of parts>".
func computeMultipartETag(partETags []string) string {
	// MD5 here is the S3 ETag algorithm — protocol compatibility, not crypto (gosec G401).
	finalETagHash := md5.New() //nolint:gosec // G401: S3 ETags are defined as MD5; protocol requirement, not crypto.
	for _, partETag := range partETags {
		// Assuming partETag is hex string of MD5, decode it first
		decodedETag, _ := hex.DecodeString(partETag)
		finalETagHash.Write(decodedETag)
	}
	return fmt.Sprintf("\"%s-%d\"", hex.EncodeToString(finalETagHash.Sum(nil)), len(partETags))
}

// finalizeComplete writes the object metadata, renames the temp assembly
// file over the final object, cleans up the upload session, and writes the
// success XML. Every failure path writes its own error response.
func finalizeComplete(w http.ResponseWriter, r *http.Request, bucketName, objectName, uploadID, bucketPath, mpUploadMetaPath, partsDir, finalObjectPath, objectMetadataPath, finalTempPath string, meta ObjectMetadata, finalETag string, totalSize int64, failCleanup *bool) {

	if err := writeFileAtomicJSON(objectMetadataPath, meta, 0644); err != nil {
		log.Printf("Error writing final object metadata file %s: %v", strconv.Quote(objectMetadataPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error writing metadata.")))
		return
	}

	// Rename the fully assembled temp file over the final object. Until this
	// point the previous object version was untouched.
	if err := os.Rename(finalTempPath, finalObjectPath); err != nil { //nolint:gosec // G703: derived from validated objectName; no traversal possible.
		log.Printf("Error renaming temporary assembly file %s over %s: %v", strconv.Quote(finalTempPath), strconv.Quote(finalObjectPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error finalizing object.")))
		return
	}
	*failCleanup = false // rename consumed the temp file

	// Principal breadcrumbs (auth extensions leaf 10): the multipart
	// assembly is pinned ABOVE the seam (master Contract 4), so the
	// completed object is stamped here. The rename replaces the inode, so
	// the PRE-rename object's breadcrumbs are snapshotted and carried
	// over: owner set-once (the previous owner, else the completing
	// principal), prior writer names, then the multipart breadcrumb.
	// Best-effort: stamp failures never fail the completed upload.
	principal := principalOfRequest(r)
	if principal != "" {
		priorOwner, priorWriters := fsbackend.CollectBreadcrumbNames(finalObjectPath)
		fsbackend.StampOwnerCarried(finalObjectPath, principal, priorOwner)
		fsbackend.CarryWriterBreadcrumbs(finalObjectPath, priorWriters)
		fsbackend.StampWriter(finalObjectPath, principal)
	}

	// Clean up: delete the multipart upload metadata file and the temporary parts directory
	if err := os.Remove(mpUploadMetaPath); err != nil { //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
		log.Printf("Warning: Error deleting multipart upload metadata file %s: %v", strconv.Quote(mpUploadMetaPath), err)
	}
	if err := os.RemoveAll(partsDir); err != nil { //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
		log.Printf("Warning: Error deleting temporary parts directory %s: %v", strconv.Quote(partsDir), err)
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
	_, _ = w.Write(x)
	log.Printf("Successfully completed multipart upload for %s/%s, UploadID: %s, Final ETag: %s", strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(uploadID), strconv.Quote(finalETag))

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
	validatedID, err := validateUploadID(uploadID)
	if err != nil {
		log.Printf("Invalid uploadId in AbortMultipartUpload: %v", err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorToXML("InvalidArgument", "Invalid upload id.")))
		return
	}

	bucketPath := getBucketPath(bucketName)
	uploadID = validatedID
	mpUploadMetaPath := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+".json")
	partsDir := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+"_parts")

	// Acquire lock to prevent race with concurrent part uploads
	uploadLock := getMultipartLock(mpUploadMetaPath)
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Read the upload metadata first: key validation needs it, and the
	// stat fast-path is folded into the IsNotExist check on the read.
	// mpUploadMetaPath embeds uploadID (validated 32-hex) — G703 false positive.
	metaJSON, err := os.ReadFile(mpUploadMetaPath) //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
	if os.IsNotExist(err) {
		log.Printf("Multipart upload metadata %s not found for UploadID %s (Abort)", strconv.Quote(mpUploadMetaPath), strconv.Quote(uploadID))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}
	if err != nil {
		log.Printf("Error reading multipart upload metadata %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error reading upload metadata.")))
		return
	}

	var mpUpload MultipartUpload
	if err := json.Unmarshal(metaJSON, &mpUpload); err != nil {
		log.Printf("Error unmarshalling multipart upload metadata from %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error parsing upload metadata.")))
		return
	}

	// Key validation (mirrors complete): an upload may only be aborted via
	// its own key, so one client cannot kill another object's upload.
	if mpUpload.Key != objectName {
		log.Printf("Object name mismatch for UploadID %s during abort. Expected %s, got %s", strconv.Quote(uploadID), strconv.Quote(mpUpload.Key), strconv.Quote(objectName))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorToXML("NoSuchUpload", "The specified multipart upload does not exist.")))
		return
	}

	// Delete the multipart upload metadata file
	if err := os.Remove(mpUploadMetaPath); err != nil && !os.IsNotExist(err) { //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
		log.Printf("Error deleting multipart upload metadata file %s: %v", strconv.Quote(mpUploadMetaPath), err)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errorToXML("InternalError", "Error aborting upload.")))
		return
	}

	// Delete the temporary parts directory
	if err := os.RemoveAll(partsDir); err != nil && !os.IsNotExist(err) { //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
		log.Printf("Error deleting temporary parts directory %s: %v", strconv.Quote(partsDir), err)
		// Don't fail - metadata already deleted
	}

	w.WriteHeader(http.StatusNoContent)
	log.Printf("Successfully aborted multipart upload for %s/%s, UploadID: %s", strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(uploadID))
}

// s3Timestamp formats a time in the S3 XML timestamp layout.
func s3Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// listMultipartUploadsHandler implements ListMultipartUploads
// (GET /bucket?uploads): XML ListMultipartUploadsResult of all in-progress
// upload sessions in the bucket, sorted lexicographically by Key then
// UploadId, honoring prefix / key-marker / max-uploads.
func listMultipartUploadsHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Bucket-name gate FIRST (bughunt 2026-10-05 C1): the os.Stat below
	// runs on filepath.Join(dataDir, name), which CLEANS "..", so without
	// this a traversal name statted and enumerated the data root's PARENT.
	// This also retires the G703 nolint claim: the name IS validated here.
	if !validBucket(bucketName) {
		log.Printf("Invalid bucket name %s for ListMultipartUploads", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}
	bucketPath := getBucketPath(bucketName)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) { //nolint:gosec // G703: name validated by validBucket immediately above
		log.Printf("Bucket %s does not exist for ListMultipartUploads", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	query := r.URL.Query()
	prefix := query.Get("prefix")
	keyMarker := query.Get("key-marker")
	maxUploads := 1000
	if v := query.Get("max-uploads"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxUploads = n
		}
	}

	uploadsDir := filepath.Join(bucketPath, ".metadata", ".uploads")
	entries, err := os.ReadDir(uploadsDir)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Error reading uploads dir %s: %v", strconv.Quote(uploadsDir), err)
		writeS3Error(w, "InternalError", "Error listing uploads.", http.StatusInternalServerError)
		return
	}

	type uploadRef struct {
		key, id string
		meta    MultipartUpload
	}
	var uploads []uploadRef
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		metaPath := filepath.Join(uploadsDir, entry.Name())
		data, err := os.ReadFile(metaPath) //nolint:gosec // G703: entry names come from ReadDir, not request input.
		if err != nil {
			log.Printf("Skipping unreadable upload meta %s: %v", strconv.Quote(metaPath), err)
			continue
		}
		var mp MultipartUpload
		if err := json.Unmarshal(data, &mp); err != nil {
			log.Printf("Skipping corrupt upload meta %s: %v", strconv.Quote(metaPath), err)
			continue
		}
		if prefix != "" && !strings.HasPrefix(mp.Key, prefix) {
			continue
		}
		if keyMarker != "" {
			// S3 semantics: listing begins AFTER the marker. Without an
			// upload-id-marker, sessions with key == key-marker are excluded;
			// with one, only sessions with UploadId strictly greater count.
			if mp.Key < keyMarker {
				continue
			}
			if mp.Key == keyMarker {
				uploadIDMarker := query.Get("upload-id-marker")
				if uploadIDMarker == "" || mp.UploadID <= uploadIDMarker {
					continue
				}
			}
		}
		uploads = append(uploads, uploadRef{key: mp.Key, id: mp.UploadID, meta: mp})
	}

	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].key != uploads[j].key {
			return uploads[i].key < uploads[j].key
		}
		return uploads[i].id < uploads[j].id
	})

	result := ListMultipartUploadsResult{
		Bucket:      bucketName,
		KeyMarker:   keyMarker,
		Prefix:      prefix,
		MaxUploads:  maxUploads,
		IsTruncated: len(uploads) > maxUploads,
	}
	listed := uploads
	if result.IsTruncated {
		listed = uploads[:maxUploads]
		last := uploads[maxUploads-1]
		result.NextKeyMarker = last.key
		result.NextUploadIDMarker = last.id
	}
	for _, u := range listed {
		result.Upload = append(result.Upload, MultipartUploadEntry{
			Key:       u.key,
			UploadID:  u.id,
			Initiated: s3Timestamp(u.meta.Initiated),
		})
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Listed %d multipart uploads in %s (truncated=%v)", len(result.Upload), strconv.Quote(bucketName), result.IsTruncated)
}

// listPartsHandler implements ListParts (GET /object?uploadId=...): XML
// ListPartsResult of the parts recorded for the upload session, sorted by
// part number, honoring part-number-marker / max-parts. The uploadId and
// key must match the session exactly (NoSuchUpload otherwise).
func listPartsHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName, uploadID string) {
	// Validate uploadID BEFORE any path join (same gate as uploadPart).
	validatedID, err := validateUploadID(uploadID)
	if err != nil {
		log.Printf("Invalid uploadId in ListParts: %v", err)
		writeS3Error(w, "InvalidArgument", "Invalid upload id.", http.StatusBadRequest)
		return
	}
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s in ListParts: %v", strconv.Quote(objectName), err)
		writeS3Error(w, "InvalidArgument", "Invalid object key.", http.StatusBadRequest)
		return
	}

	bucketPath := getBucketPath(bucketName)
	uploadID = validatedID
	mpUploadMetaPath := filepath.Join(bucketPath, ".metadata", ".uploads", uploadID+".json")

	uploadLock := getMultipartLock(mpUploadMetaPath)
	uploadLock.Lock()
	defer uploadLock.Unlock()

	// Read the session meta; mpUploadMetaPath embeds uploadID (validated
	// 32-hex) — no traversal possible; G703 false positive.
	metaJSON, err := os.ReadFile(mpUploadMetaPath) //nolint:gosec // G703: uploadID validated 32-hex; no traversal possible.
	if os.IsNotExist(err) {
		log.Printf("Multipart upload metadata %s not found for UploadID %s (ListParts)", strconv.Quote(mpUploadMetaPath), strconv.Quote(uploadID))
		writeS3Error(w, "NoSuchUpload", "The specified multipart upload does not exist.", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error reading multipart upload metadata %s: %v", strconv.Quote(mpUploadMetaPath), err)
		writeS3Error(w, "InternalError", "Error reading upload metadata.", http.StatusInternalServerError)
		return
	}
	var mpUpload MultipartUpload
	if err := json.Unmarshal(metaJSON, &mpUpload); err != nil {
		log.Printf("Error unmarshalling multipart upload metadata from %s: %v", strconv.Quote(mpUploadMetaPath), err)
		writeS3Error(w, "InternalError", "Error parsing upload metadata.", http.StatusInternalServerError)
		return
	}
	// Key must match the session, else the upload "does not exist" for this
	// object (mirrors uploadPart/complete/abort).
	if mpUpload.Key != objectName {
		log.Printf("Object name mismatch for UploadID %s during ListParts. Expected %s, got %s", strconv.Quote(uploadID), strconv.Quote(mpUpload.Key), strconv.Quote(objectName))
		writeS3Error(w, "NoSuchUpload", "The specified multipart upload does not exist.", http.StatusNotFound)
		return
	}

	query := r.URL.Query()
	maxParts := 1000
	if v := query.Get("max-parts"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxParts = n
		}
	}
	partNumberMarker := 0
	if v := query.Get("part-number-marker"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			partNumberMarker = n
		}
	}

	// Sort part numbers ascending, then filter to those after the marker.
	partNumbers := make([]int, 0, len(mpUpload.Parts))
	for num := range mpUpload.Parts {
		if num > partNumberMarker {
			partNumbers = append(partNumbers, num)
		}
	}
	sort.Ints(partNumbers)

	result := ListPartsResult{
		Bucket:           bucketName,
		Key:              objectName,
		UploadID:         uploadID,
		Initiated:        s3Timestamp(mpUpload.Initiated),
		PartNumberMarker: partNumberMarker,
		MaxParts:         maxParts,
		IsTruncated:      len(partNumbers) > maxParts,
	}
	listed := partNumbers
	if result.IsTruncated {
		listed = partNumbers[:maxParts]
		result.NextPartNumberMarker = listed[len(listed)-1]
	}
	for _, num := range listed {
		pm := mpUpload.Parts[num]
		result.Part = append(result.Part, PartEntry{
			PartNumber:   pm.PartNumber,
			ETag:         fmt.Sprintf("%q", pm.ETag),
			Size:         pm.Size,
			LastModified: s3Timestamp(mpUpload.Initiated),
		})
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Listed %d parts of upload %s for %s/%s", len(result.Part), strconv.Quote(uploadID), strconv.Quote(bucketName), strconv.Quote(objectName))
}

// sweepExpiredUploads lazily aborts multipart upload sessions whose Initiated
// timestamp is older than multipartUploadExpiry: the session JSON and its
// _parts dir are removed. It scans the bucket's .metadata/.uploads dir and
// returns the number of sessions removed. Called hourly from main() and
// exported for tests.
func sweepExpiredUploads(bucketPath string) int {
	uploadsDir := filepath.Join(bucketPath, ".metadata", ".uploads")
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Expiry sweep: cannot read uploads dir %s: %v", strconv.Quote(uploadsDir), err)
		}
		return 0
	}

	cutoff := time.Now().UTC().Add(-multipartUploadExpiry)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		metaPath := filepath.Join(uploadsDir, entry.Name())
		data, err := os.ReadFile(metaPath) //nolint:gosec // G703: entry names come from ReadDir, not request input.
		if err != nil {
			log.Printf("Expiry sweep: skipping unreadable upload meta %s: %v", strconv.Quote(metaPath), err)
			continue
		}
		var mp MultipartUpload
		if err := json.Unmarshal(data, &mp); err != nil {
			log.Printf("Expiry sweep: skipping corrupt upload meta %s: %v", strconv.Quote(metaPath), err)
			continue
		}
		if mp.Initiated.After(cutoff) {
			continue
		}

		uploadID := strings.TrimSuffix(entry.Name(), ".json")
		// Serialize against concurrent part uploads/completes on this session.
		uploadLock := getMultipartLock(metaPath)
		uploadLock.Lock()
		// Re-check under the lock: the session may have been completed or
		// aborted while we waited.
		if data, err := os.ReadFile(metaPath); err == nil {
			var fresh MultipartUpload
			if json.Unmarshal(data, &fresh) == nil && fresh.Initiated.After(cutoff) {
				uploadLock.Unlock()
				continue
			}
		}
		if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) { //nolint:gosec // G703: uploadID from session filename; no traversal.
			log.Printf("Expiry sweep: error removing %s: %v", strconv.Quote(metaPath), err)
			uploadLock.Unlock()
			continue
		}
		partsDir := filepath.Join(uploadsDir, uploadID+"_parts")
		if err := os.RemoveAll(partsDir); err != nil { //nolint:gosec // G703: uploadID validated below; no traversal.
			log.Printf("Expiry sweep: error removing parts dir %s: %v", strconv.Quote(partsDir), err)
		}
		uploadLock.Unlock()
		removed++
		log.Printf("Expiry sweep: removed expired multipart upload %s (key %s, initiated %s)", strconv.Quote(uploadID), strconv.Quote(mp.Key), mp.Initiated.Format(time.RFC3339))
	}
	return removed
}

// sweepInterval is how often startMultipartExpirySweeper runs a sweep pass.
// A var (not const) so tests can shorten it if ever needed; production keeps
// the hourly default.
var sweepInterval = time.Hour

// startMultipartExpirySweeper runs sweepAllBucketsOnce every sweepInterval,
// forever. Started once from main(). The goroutine body is intentionally a
// thin ticker wrapper — the per-pass logic is in sweepAllBucketsOnce, which
// tests drive directly; with the real 1h interval nothing fires during a test.
func startMultipartExpirySweeper() {
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for range ticker.C {
			sweepAllBucketsOnce()
		}
	}()
}

// sweepAllBucketsOnce performs a single expiry-sweep pass over every
// discovered bucket: the dataDir buckets plus any configured custom buckets.
// It returns the total number of sessions removed across all buckets.
// (Extracted verbatim from the former sweepAllBuckets body.)
func sweepAllBucketsOnce() int {
	total := 0
	bucketRoots := filepath.Join(currentServerConfig().DataDir, "*")
	paths, _ := filepath.Glob(bucketRoots)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			continue
		}
		total += sweepExpiredUploads(p)
	}
	for _, p := range currentServerConfig().Buckets {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			total += sweepExpiredUploads(p)
		}
	}
	return total
}

// SweepAllBucketsOnce is the exported sweep entry package main's hourly
// ticker drives (wiring installs it as the sweep pass).
func SweepAllBucketsOnce() int { return sweepAllBucketsOnce() }
