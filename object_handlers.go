package main

import (
	"crypto/md5" //nolint:gosec // G501: MD5 is the S3 ETag algorithm — protocol requirement, not crypto.
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"maps"
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
	// Leaf 3.5: x-amz-copy-source header → CopyObject (server-side copy).
	if r.Header.Get("x-amz-copy-source") != "" {
		copyObjectHandler(w, r, bucketName, objectName)
		return
	}

	bucketPath := getBucketPath(bucketName)
	// Object data is stored directly in the bucket directory, OR in the shadow
	// data dir when the flat path is unusable (another key occupies a path
	// component — leaf 5.1 [a]-1 key/directory collision fix). The chosen path
	// is recorded in metadata StoragePath either way.
	objectDataPath := objectDataPathFor(bucketPath, objectName)
	// Metadata is stored in .metadata subdirectory
	objectMetadataDir := filepath.Join(bucketPath, ".metadata")
	objectMetadataPath := filepath.Join(objectMetadataDir, objectName+".meta")

	// Validate object key (leaf 2.4 fix 2: traversal rejection)
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s: %v", strconv.Quote(objectName), err)
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}

	// Ensure bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for PutObject", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body for %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeS3Error(w, "InternalError", "Error reading request body.", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Handle aws-chunked Content-Encoding (used by AWS CLI v2). Signed
	// streaming bodies were already decoded + signature-verified in
	// authenticateRequest (leaf 3.4) — skip the second decode pass then.
	contentEncoding := r.Header.Get("Content-Encoding")
	if strings.Contains(contentEncoding, "aws-chunked") && !isDecodedStreaming(r.Context()) {
		decodedBody, err := decodeAWSChunked(body)
		if err != nil {
			log.Printf("Error decoding aws-chunked body for %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
			writeS3Error(w, "InvalidArgument", "Failed to decode chunked body.", http.StatusBadRequest)
			return
		}
		// Leaf 2.2 cross-leaf wiring: verify declared decoded length
		if err := VerifyDecodedLength(r.Header.Get("x-amz-decoded-content-length"), len(decodedBody)); err != nil {
			log.Printf("Decoded content length mismatch for %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
			writeS3Error(w, "InvalidArgument", "Decoded content length mismatch.", http.StatusBadRequest)
			return
		}
		body = decodedBody
		log.Printf("Decoded aws-chunked body: %d bytes", len(body))
	}

	// Calculate ETag (MD5 hash of the content). MD5 is the S3 ETag algorithm —
	// required for S3 protocol compatibility, not a security primitive (G401).
	hash := md5.Sum(body) //nolint:gosec // G401: S3 ETags are defined as MD5; protocol requirement, not crypto.
	eTag := hex.EncodeToString(hash[:])

	// Leaf 2.4 fix 1: serialize writers per object and write data + metadata
	// atomically via the storage.go helpers (no torn reads/partial files).
	objectDataParentDir := filepath.Dir(objectDataPath)
	unlock := lockObject(objectDataPath)
	defer unlock()
	// Leaf-4.8 stress fix: also hold the parent-DIR lock so a concurrent
	// deleteObjectCore's cleanupEmptyDirs prune cannot remove the directory
	// between this PUT's MkdirAll and writeFileAtomic (that race surfaced as
	// spurious PUT 500s in the stress suite).
	unlockDataDir := lockObject(objectDataParentDir)
	defer unlockDataDir()

	// Create parent directories for the object data if they don't exist.
	// objectDataPath embeds objectName, validated by validateObjectKey (no
	// ".." segments) — cannot escape the bucket; G703 false positive.
	if err := os.MkdirAll(objectDataParentDir, 0755); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		log.Printf("Error creating parent directories for object data %s: %v", strconv.Quote(objectDataPath), err)
		writeS3Error(w, "InternalError", "Error creating object storage.", http.StatusInternalServerError)
		return
	}

	// Write the object data atomically
	if err := writeFileAtomic(objectDataPath, body, 0644); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		log.Printf("Error writing object data to %s: %v", strconv.Quote(objectDataPath), err)
		writeS3Error(w, "InternalError", "Error writing object data.", http.StatusInternalServerError)
		return
	}

	// Create parent directories for the metadata file if they don't exist
	metadataParentDir := filepath.Dir(objectMetadataPath)
	if err := os.MkdirAll(metadataParentDir, 0755); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		log.Printf("Error creating metadata storage for %s: %v", strconv.Quote(objectMetadataPath), err)
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
		log.Printf("Error writing metadata file %s: %v", strconv.Quote(objectMetadataPath), err)
		// RESIDUAL WINDOW (leaf 2.4 fix 1): same as above — do NOT remove the
		// data file; it may be the pre-overwrite good object. Return 500 with
		// the new data left unindexed; a retry rewrites both files.
		writeS3Error(w, "InternalError", "Error writing metadata.", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully put object %s/%s, ETag: %s", strconv.Quote(bucketName), strconv.Quote(objectName), eTag)
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

// leaf 3.1: Range request parsing and conditional (If-*) request evaluation.
// Conditionals are evaluated BEFORE Range processing (RFC 7232 §6 ordering);
// Range only applies to a 200/206 outcome.

// rangeOutcome classifies the result of parsing a Range header against an
// object size.
type rangeOutcome int

const (
	rangeFull          rangeOutcome = iota // serve 200 with the full body
	rangePartial                           // serve 206 with the byte slice
	rangeUnsatisfiable                     // serve 416 InvalidRange
)

// rangeRequest is the parsed Range outcome: what to serve and, for
// rangePartial, which byte slice (start..start+length-1).
type rangeRequest struct {
	outcome rangeOutcome
	start   int64
	length  int64
}

// parseRangeHeader parses a Range header value for an object of the given
// size. Per the leaf 3.1 contract: only `bytes=<start>-<end>`,
// `bytes=<start>-`, and `bytes=-<suffix>` single ranges are honored. Anything
// else — wrong unit, unparsable numbers, inverted ranges (`bytes=5-2`),
// empty spec (`bytes=-`) — is MALFORMED and ignored (200 full body), matching
// S3's lenient behavior. A multi-range spec (`bytes=0-1,3-4`) also falls back
// to a full-body 200: simplest legal fallback; we do not emit
// multipart/byteranges. A syntactically valid range that cannot intersect the
// object (start >= size, suffix length 0) is UNSATISFIABLE → 416.
func parseRangeHeader(spec string, size int64) rangeRequest {
	const unit = "bytes="
	if !strings.HasPrefix(spec, unit) {
		return rangeRequest{outcome: rangeFull}
	}
	specPart := strings.TrimSpace(spec[len(unit):])
	// Multi-range: comma present → fall back to a full-body 200 (documented
	// choice; S3 itself returns 200 for spec forms it won't honor).
	if strings.Contains(specPart, ",") {
		return rangeRequest{outcome: rangeFull}
	}
	startStr, endStr, found := strings.Cut(specPart, "-")
	if !found {
		return rangeRequest{outcome: rangeFull}
	}
	startStr = strings.TrimSpace(startStr)
	endStr = strings.TrimSpace(endStr)

	switch {
	case startStr == "" && endStr == "":
		// "bytes=-" — no numbers at all: malformed.
		return rangeRequest{outcome: rangeFull}
	case startStr == "":
		// Suffix form: last <endStr> bytes.
		suffixLen, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || suffixLen < 0 {
			return rangeRequest{outcome: rangeFull}
		}
		if suffixLen == 0 || size == 0 {
			return rangeRequest{outcome: rangeUnsatisfiable}
		}
		if suffixLen > size {
			suffixLen = size // "-N" beyond EOF → whole object
		}
		return rangeRequest{outcome: rangePartial, start: size - suffixLen, length: suffixLen}
	default:
		start, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 {
			return rangeRequest{outcome: rangeFull}
		}
		if start >= size {
			return rangeRequest{outcome: rangeUnsatisfiable}
		}
		end := size - 1
		if endStr != "" {
			parsedEnd, err := strconv.ParseInt(endStr, 10, 64)
			if err != nil || parsedEnd < start {
				// "5-2" (inverted) or garbage end: malformed.
				return rangeRequest{outcome: rangeFull}
			}
			if parsedEnd < end {
				end = parsedEnd
			}
		}
		return rangeRequest{outcome: rangePartial, start: start, length: end - start + 1}
	}
}

// etagMatches reports whether an If-(None-)Match header value matches the
// stored ETag (raw, unquoted hex). Comparison is per RFC 7232: "*" matches
// any current representation, list members are comma-separated, the weak
// validator prefix "W/" is ignored, and quoting is not significant.
func etagMatches(headerValue, etag string) bool {
	if headerValue == "*" {
		return true
	}
	for member := range strings.SplitSeq(headerValue, ",") {
		candidate := strings.TrimSpace(member)
		candidate = strings.TrimPrefix(candidate, "W/")
		candidate = strings.Trim(candidate, `"`)
		if candidate == etag {
			return true
		}
	}
	return false
}

// evaluatePreconditions evaluates the RFC 7232 conditional request headers in
// the mandated order against the object's ETag (raw, unquoted) and
// Last-Modified time. If a precondition fails or short-circuits, it returns
// the response status with done=true; otherwise (0, false) — the caller
// proceeds with the normal GET/HEAD (and then Range) handling.
//
// Order (RFC 7232 §6):
//  1. If-Match: no match → 412. Present and matching → If-Unmodified-Since
//     is NOT evaluated.
//  2. If-Unmodified-Since (only when If-Match absent): modified since → 412.
//  3. If-None-Match: match → 304 for GET/HEAD. Present but not matching →
//     If-Modified-Since is NOT evaluated.
//  4. If-Modified-Since (only when If-None-Match absent): not modified → 304.
//
// Dates compare at second granularity (http.TimeFormat has no sub-second
// precision).
func evaluatePreconditions(r *http.Request, etag string, lastModified time.Time) (status int, done bool) {
	modTime := lastModified.Truncate(time.Second)

	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		if !etagMatches(ifMatch, etag) {
			return http.StatusPreconditionFailed, true
		}
	} else if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		if t, err := http.ParseTime(ius); err == nil && modTime.After(t) {
			return http.StatusPreconditionFailed, true
		}
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatches(inm, etag) {
			return http.StatusNotModified, true
		}
	} else if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !modTime.After(t) {
			return http.StatusNotModified, true
		}
	}

	return 0, false
}

// checkObjectPreconditions applies evaluatePreconditions to a GET/HEAD object
// request and writes the terminating response (304 or 412) when a condition
// short-circuits. Returns true when the caller must stop. A 304 carries ETag
// + Last-Modified and no body; Content-Length is removed (it described the
// full entity, which a 304 must not re-advertise).
func checkObjectPreconditions(w http.ResponseWriter, r *http.Request, etag string, lastModified time.Time) bool {
	status, done := evaluatePreconditions(r, etag, lastModified)
	if !done {
		return false
	}
	if status == http.StatusNotModified {
		w.Header().Del("Content-Length")
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	writeS3Error(w, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold", status)
	return true
}

// serveObjectRange applies the parsed Range outcome to the response for an
// object of actualSize bytes whose data file is already open. For rangeFull
// it returns false so the caller serves the normal full-body 200. For
// rangeUnsatisfiable it writes 416 InvalidRange with `Content-Range: bytes
// */<total>`. For rangePartial it writes 206 with Content-Range and the
// sliced Content-Length, seeking into the file; isHead suppresses the body.
// Returns true when the response is fully written.
func serveObjectRange(w http.ResponseWriter, file *os.File, rr rangeRequest, actualSize int64, isHead bool, logPrefix string) bool {
	switch rr.outcome {
	case rangeUnsatisfiable:
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", actualSize))
		writeS3Error(w, "InvalidRange", "The requested range is not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return true
	case rangePartial:
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rr.start, rr.start+rr.length-1, actualSize))
		w.Header().Set("Content-Length", strconv.FormatInt(rr.length, 10))
		w.WriteHeader(http.StatusPartialContent)
		if !isHead {
			if _, err := file.Seek(rr.start, io.SeekStart); err != nil {
				log.Printf("%s: error seeking to range start %d: %v", logPrefix, rr.start, err) //nolint:gosec // G706: logPrefix is handler-constructed, not client input.
				return true
			}
			if _, err := io.CopyN(w, file, rr.length); err != nil {
				log.Printf("%s: error streaming range to client: %v", logPrefix, err) //nolint:gosec // G706: logPrefix is handler-constructed, not client input.
			}
		}
		return true
	default:
		return false
	}
}

func getObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for GetObject", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Read metadata. objectMetadataPath embeds objectName, validated by
	// validateObjectKey — cannot escape the bucket; G703 false positive.
	metaJSON, err := os.ReadFile(objectMetadataPath) //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
	if os.IsNotExist(err) {
		log.Printf("Object metadata %s not found for %s/%s", strconv.Quote(objectMetadataPath), strconv.Quote(bucketName), strconv.Quote(objectName))
		writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error reading metadata file %s: %v", strconv.Quote(objectMetadataPath), err)
		writeS3Error(w, "InternalError", "Error reading object metadata.", http.StatusInternalServerError)
		return
	}

	var meta ObjectMetadata
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		log.Printf("Error unmarshalling metadata from %s: %v", strconv.Quote(objectMetadataPath), err)
		writeS3Error(w, "InternalError", "Error parsing object metadata.", http.StatusInternalServerError)
		return
	}

	// Leaf 2.4 fix 6: fall back to the canonical path on corrupt StoragePath
	objectDataPath := resolveObjectDataPath(bucketPath, objectName, &meta)

	// Leaf-4.8 stress fix: hold the data+meta dir locks across stat→open→copy
	// so a concurrent deleteObjectCore cannot prune the directories out from
	// under this read (that race produced spurious GET 500s).
	unlockDataDir := lockObject(filepath.Dir(objectDataPath))
	defer unlockDataDir()
	unlockMetaDir := lockObject(filepath.Dir(objectMetadataPath))
	defer unlockMetaDir()

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
	// Leaf 3.1 fix 7: conditional headers are evaluated BEFORE Range.
	// The validator headers (ETag, Last-Modified) must be set first so a
	// 304 response carries them.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	if checkObjectPreconditions(w, r, meta.ETag, meta.LastModified) {
		return
	}

	// Leaf 3.1 fix 5: advertise byte-range support on every 200/206.
	w.Header().Set("Accept-Ranges", "bytes")
	rr := parseRangeHeader(r.Header.Get("Range"), actualSize)
	if rr.outcome != rangeFull {
		// Open AFTER headers are computed but write the status only once we
		// know the file opens; a failed open becomes NoSuchKey/404 rather
		// than a 206/416 with headers half-set.
		file, err := os.Open(objectDataPath)
		if err != nil {
			log.Printf("Error opening object data file %s: %v", objectDataPath, err)
			writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
			return
		}
		defer file.Close()
		if serveObjectRange(w, file, rr, actualSize, false, fmt.Sprintf("GetObject %s/%s", bucketName, objectName)) {
			log.Printf("Served range request for object %s/%s (%s)", strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(r.Header.Get("Range"))) //nolint:gosec // G706: Range is strconv.Quote-escaped.
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
			return
		}
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", actualSize))
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
		log.Printf("Error streaming object %s/%s to client: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
	}
	log.Printf("Successfully served object %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))

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

	// Leaf 2.4 fix 3: a missing bucket is a real 404 (missing KEY in an
	// existing bucket still stays 204 per S3 semantics).
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for DeleteObject", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	if err := deleteObjectCore(bucketPath, bucketName, objectName); err != nil {
		log.Printf("Error deleting object %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeS3Error(w, "InternalError", "Error deleting object data.", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent) // S3 spec: 204 No Content
}

// deleteObjectCore is the shared deletion logic for DeleteObject (single,
// leaf 2.4) and DeleteObjects (batch, leaf 3.5). The bucket's existence must
// be checked by the caller. A missing key deletes nothing and succeeds (S3
// semantics). Returns an error only on real I/O failure of the data file.
func deleteObjectCore(bucketPath, bucketName, objectName string) error {
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")
	objectDataPath := filepath.Join(bucketPath, objectName)

	// Leaf-4.8 stress fix: serialize against concurrent writers on this key
	// AND against empty-dir pruning. cleanupEmptyDirs has a check-then-act
	// window (ReadDir empty -> Remove) that can delete a directory a
	// concurrent PUT's MkdirAll just created, turning that PUT's atomic
	// write into a 500. Holding the parent-dir lock across the delete +
	// prune closes the window against PUTs into the same directory.
	unlockDataDir := lockObject(filepath.Dir(objectDataPath))
	defer unlockDataDir()
	unlockMetaDir := lockObject(filepath.Dir(objectMetadataPath))
	defer unlockMetaDir()

	// Try to read metadata to get actual storage path
	var actualDataPath string
	metaJSON, err := os.ReadFile(objectMetadataPath) //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
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
	if err := os.Remove(actualDataPath); err != nil { //nolint:gosec // G703: actualDataPath derived from validated objectName; no traversal possible.
		if !os.IsNotExist(err) {
			log.Printf("Error deleting object data file %s: %v", strconv.Quote(actualDataPath), err)
			return fmt.Errorf("deleting object data %s: %w", actualDataPath, err)
		}
	} else {
		dataDeleted = true
	}

	// Delete the metadata file
	metaDeleted := false
	if err := os.Remove(objectMetadataPath); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		if !os.IsNotExist(err) {
			log.Printf("Error deleting metadata file %s: %v", strconv.Quote(objectMetadataPath), err)
			// Don't fail - data is already deleted
		}
	} else {
		metaDeleted = true
	}

	// Clean up empty parent directories (best effort)
	cleanupEmptyDirs(filepath.Dir(actualDataPath), bucketPath)
	cleanupEmptyDirs(filepath.Dir(objectMetadataPath), filepath.Join(bucketPath, ".metadata"))

	if dataDeleted || metaDeleted {
		log.Printf("Successfully deleted object %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))

		// Trigger after_delete actions
		go triggerActions("after_delete", ActionContext{
			FilePath:     actualDataPath,
			MetadataPath: objectMetadataPath,
			BucketName:   bucketName,
			BucketPath:   bucketPath,
			ObjectKey:    objectName,
		})
	} else {
		log.Printf("Object %s/%s did not exist for deletion", strconv.Quote(bucketName), strconv.Quote(objectName))
	}

	return nil
}

func headObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	bucketPath := getBucketPath(bucketName)
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for HeadObject", strconv.Quote(bucketName))
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Read metadata (HeadObject path). objectMetadataPath embeds objectName,
	// validated by validateObjectKey — G703 false positive.
	metaJSON, err := os.ReadFile(objectMetadataPath) //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
	if os.IsNotExist(err) {
		log.Printf("Object metadata %s not found for %s/%s for HeadObject", strconv.Quote(objectMetadataPath), strconv.Quote(bucketName), strconv.Quote(objectName))
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error reading metadata file %s for HeadObject: %v", strconv.Quote(objectMetadataPath), err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var meta ObjectMetadata
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		log.Printf("Error unmarshalling metadata from %s for HeadObject: %v", strconv.Quote(objectMetadataPath), err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Leaf 2.4 fix 6: fall back to the canonical path on corrupt StoragePath
	objectDataPath := resolveObjectDataPath(bucketPath, objectName, &meta)

	// Leaf-4.8 stress fix: hold the data+meta dir locks across stat→open→copy
	// so a concurrent deleteObjectCore cannot prune the directories out from
	// under this read (that race produced spurious GET 500s).
	unlockDataDir := lockObject(filepath.Dir(objectDataPath))
	defer unlockDataDir()
	unlockMetaDir := lockObject(filepath.Dir(objectMetadataPath))
	defer unlockMetaDir()

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
	// Leaf 3.1 fix 7: conditional headers are evaluated BEFORE Range.
	// Validator headers (ETag, Last-Modified) are set first so a 304
	// carries them.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	if checkObjectPreconditions(w, r, meta.ETag, meta.LastModified) {
		return
	}

	// Leaf 3.1 fix 5: advertise byte-range support on 200/206 HEAD.
	w.Header().Set("Accept-Ranges", "bytes")
	rr := parseRangeHeader(r.Header.Get("Range"), actualSize)
	if rr.outcome != rangeFull {
		// HEAD never streams the data file — headers only (leaf 3.1 fix 6).
		if serveObjectRange(w, nil, rr, actualSize, true, fmt.Sprintf("HeadObject %s/%s", bucketName, objectName)) {
			log.Printf("Served range HEAD for object %s/%s (%s)", strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(r.Header.Get("Range"))) //nolint:gosec // G706: Range is strconv.Quote-escaped.
			return
		}
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", actualSize))
	for k, v := range meta.CustomMetadata {
		w.Header().Set(k, v)
	}

	w.WriteHeader(http.StatusOK)
	log.Printf("Successfully served HEAD for object %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))
}

// s3URLEncode percent-encodes a key for encoding-type=url responses: S3
// encodes every byte except unreserved chars and "/", so a path stays a
// path. (url.QueryEscape then restore "/" — leaf 2.4 fix 15.)
func s3URLEncode(key string) string {
	// S3 encoding-type=url encodes per RFC 3986 unreserved set: space is %20,
	// never '+' (bug found by the leaf-3.6 e2e suite); '/' stays literal.
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(key), "+", "%20"), "%2F", "/")
}

// listObjectsV2Handler implementation
func listObjectsV2Handler(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucketPath := getBucketPath(bucketName)
	metadataDir := filepath.Join(bucketPath, ".metadata")

	// Check if bucket exists
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for ListObjectsV2", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Parse query parameters
	params := parseListObjectsParams(r)

	// Leaf 2.4 fix 13: max-keys=0 → empty result, no prefixes, not truncated
	if params.maxKeys == 0 {
		result := ListBucketResult{
			IsTruncated: false,
			Name:        bucketName,
			Prefix:      params.prefix,
			Delimiter:   params.delimiter,
			MaxKeys:     0,
			KeyCount:    0,
		}
		if params.encodeKeys {
			result.EncodingType = "url"
		}
		writeXML(w, http.StatusOK, result)
		return
	}

	allObjectKeys, walkErr := collectObjectKeys(metadataDir)
	if walkErr != nil {
		log.Printf("Error walking metadata directory %s: %v", metadataDir, walkErr)
		writeS3Error(w, "InternalError", "Error listing objects.", http.StatusInternalServerError)
		return
	}
	sort.Strings(allObjectKeys)

	// Leaf 2.4 fix 14: IsTruncated=true must carry a non-empty token; if the
	// token is empty, no next page exists → report IsTruncated=false.
	truncated, nextToken, objects, commonPrefixes, lastItem := listObjectsFromKeys(allObjectKeys, params, bucketName, metadataDir)
	if truncated && nextToken == "" {
		truncated = false
	}

	var commonPrefixEntries []CommonPrefix
	for _, cp := range commonPrefixes {
		cpOut := cp
		if params.encodeKeys {
			cpOut = s3URLEncode(cp)
		}
		commonPrefixEntries = append(commonPrefixEntries, CommonPrefix{Prefix: cpOut})
	}
	sort.Slice(commonPrefixEntries, func(i, j int) bool {
		return commonPrefixEntries[i].Prefix < commonPrefixEntries[j].Prefix
	})

	result := ListBucketResult{
		IsTruncated:           truncated,
		Contents:              objects,
		Name:                  bucketName,
		Prefix:                params.prefix,
		Delimiter:             params.delimiter,
		MaxKeys:               params.maxKeys,
		CommonPrefixes:        commonPrefixEntries,
		KeyCount:              len(objects) + len(commonPrefixEntries),
		ContinuationToken:     params.continuationToken,
		NextContinuationToken: nextToken,
		StartAfter:            params.startAfter,
		Marker:                params.marker,
	}
	// Leaf 5.1 [a]-3/[a]-4: AWS V1 rule — NextMarker is returned only when
	// the response is truncated AND a delimiter was requested; its value is
	// the page's last emitted item in MERGED key order (a key that sorts
	// before a roll-up wins the slot).
	if truncated && params.delimiter != "" && lastItem != "" {
		result.NextMarker = lastItem
	}
	if params.encodeKeys {
		result.EncodingType = "url"
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Successfully served ListObjectsV2 for bucket %s", strconv.Quote(bucketName))
}

// listObjectsParams holds the parsed ListObjectsV2 query parameters.
type listObjectsParams struct {
	prefix            string
	delimiter         string
	continuationToken string
	startAfter        string
	marker            string // ListObjects V1 marker (leaf 5.1 [a]-3)
	encodeKeys        bool
	maxKeys           int
}

// parseListObjectsParams extracts and clamps the ListObjectsV2 query
// parameters. Invalid or out-of-range max-keys falls back to defaults with a
// log line (S3 caps maxKeys at 1000).
func parseListObjectsParams(r *http.Request) listObjectsParams {
	p := listObjectsParams{
		prefix:            r.URL.Query().Get("prefix"),
		delimiter:         r.URL.Query().Get("delimiter"),
		continuationToken: r.URL.Query().Get("continuation-token"),
		startAfter:        r.URL.Query().Get("start-after"),
		marker:            r.URL.Query().Get("marker"),
		encodeKeys:        r.URL.Query().Get("encoding-type") == "url",
		maxKeys:           1000,
	}
	maxKeysStr := r.URL.Query().Get("max-keys")
	if maxKeysStr == "" {
		return p
	}
	n, err := strconv.Atoi(maxKeysStr)
	switch {
	case err != nil:
		log.Printf("Invalid max-keys value: '%s'. Using default %d.", strconv.Quote(maxKeysStr), 1000)
	case n < 0:
		log.Printf("max-keys must be non-negative. Received %d. Using default %d.", n, 1000)
	case n > 1000:
		p.maxKeys = 1000 // S3 caps at 1000
	default:
		p.maxKeys = n
	}
	return p
}

// collectObjectKeys walks the metadata directory and returns every object
// key (metadata path relative to metadataDir, minus the .meta suffix).
// A missing metadataDir is not an error — it is an empty bucket.
func collectObjectKeys(metadataDir string) ([]string, error) {
	var allObjectKeys []string
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
			// filepath.Rel only fails when relPath can't be made relative
			// (mismatched absoluteness); path came FROM metadataDir via
			// WalkDir, so this is unreachable in practice — skip the entry.
			return nil //nolint:nilerr // deliberately skip malformed paths instead of aborting the whole listing.
		}
		objectKey := strings.TrimSuffix(relPath, ".meta")
		allObjectKeys = append(allObjectKeys, objectKey)
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return allObjectKeys, nil
	}
	return allObjectKeys, err
}

// listObjectsFromKeys walks the sorted key list applying the cursor, prefix,
// delimiter roll-up and maxKeys truncation (leaf 2.4 fixes 12/13). Returns
// the truncation flag, next continuation token, object entries, and the
// first-seen common prefixes (deduplicated, in first-seen order).
func listObjectsFromKeys(allObjectKeys []string, p listObjectsParams, bucketName, metadataDir string) (truncated bool, nextToken string, objects []Object, commonPrefixes []string, lastItemOut string) {
	processedCount := 0
	seenPrefixes := make(map[string]struct{})

	// Batch meta reads (design Option A): the filter+truncate walk and the
	// batched meta fetch live in appendEntries. listObjectsFromKeys keeps
	// only cursor setup and returns. lastItem carries the page's final
	// emitted item (key or prefix) for the V1 NextMarker field.
	var lastItem string
	truncated, nextToken, objects, commonPrefixes = appendEntries(&p, allObjectKeys, bucketName, metadataDir, truncated, nextToken, objects, commonPrefixes, &processedCount, seenPrefixes, &lastItem)
	return truncated, nextToken, objects, commonPrefixes, lastItem
}

func appendEntries(p *listObjectsParams, allObjectKeys []string, bucketName, metadataDir string, truncated bool, nextToken string, objects []Object, commonPrefixes []string, processedCount *int, seenPrefixes map[string]struct{}, lastEmitted *string) (bool, string, []Object, []string) {
	i := 0
	for i < len(allObjectKeys) {
		need := p.maxKeys - *processedCount
		if need <= 0 {
			break
		}

		window := gatherListWindow(allObjectKeys, i, p, processedCount, seenPrefixes, commonPrefixes, &truncated, &nextToken)
		i = window.advanced
		commonPrefixes = window.commonPrefixes
		if len(window.items) == 0 {
			// Nothing page-eligible left in the key space.
			break
		}

		// Batch-read the window's key metas concurrently (Option A).
		paths := make([]string, len(window.entries))
		for j, e := range window.entries {
			paths[j] = filepath.Join(metadataDir, e.objectKey+".meta")
		}
		metas := readMetasBatch(paths, batchWorkerCount())

		// Emit items in MERGED KEY ORDER (leaf 5.1 [a]-4): a key that sorts
		// before a roll-up consumes the page budget first, matching AWS.
		// Keys whose meta is unreadable/unparsable are skipped with the same
		// per-key log lines as the serial loop (they never counted).
		// lastEmittedItem records the final item of the page (key or prefix,
		// raw) for the V1 NextMarker response field.
		metaIdx := 0
		lastEmittedItem := ""
		for _, item := range window.items {
			if *processedCount >= p.maxKeys {
				// Budget exhausted mid-window. The next page resumes after
				// the LAST EMITTED item (leaf 5.1 [a]-4): the token is the
				// raw key/prefix last emitted, so a roll-up group the
				// previous page emitted is consumed by the next page.
				truncated = true
				if lastEmittedItem != "" {
					nextToken = lastEmittedItem
				}
				break
			}
			if item.isPrefix {
				// Raw prefix; the handler applies encoding-type=url once
				// when building the response.
				commonPrefixes = append(commonPrefixes, item.prefix)
				lastEmittedItem = item.prefix
				*processedCount++
				continue
			}
			e := window.entries[item.entryIdx]
			res := metas[metaIdx]
			metaIdx++
			if res.readErr != nil {
				log.Printf("Error reading metadata for %s/%s: %v. Skipping.", strconv.Quote(bucketName), strconv.Quote(e.objectKey), res.readErr)
				continue
			}
			if res.parseErr != nil {
				log.Printf("Error unmarshalling metadata for %s/%s: %v. Skipping.", strconv.Quote(bucketName), strconv.Quote(e.objectKey), res.parseErr)
				continue
			}
			objectKeyOut := e.objectKey
			if p.encodeKeys {
				objectKeyOut = s3URLEncode(objectKeyOut)
			}
			objects = append(objects, Object{
				Key:          objectKeyOut,
				LastModified: res.meta.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
				ETag:         fmt.Sprintf("\"%s\"", res.meta.ETag),
				Size:         res.meta.ContentLength,
				StorageClass: "STANDARD",
			})
			lastEmittedItem = e.objectKey
			*processedCount++
		}
		// Leaf 5.1 [a]-4: the V1 NextMarker is the page's last emitted
		// item (key or prefix) in merged order.
		*lastEmitted = lastEmittedItem
		if *processedCount >= p.maxKeys {
			break
		}
	}

	return truncated, nextToken, objects, commonPrefixes
}

// listWindow is one batch-gather window: page-eligible items (keys and
// delimiter roll-ups) in merged KEY ORDER, plus gather-loop bookkeeping.
type listWindow struct {
	entries []listEntry
	// items is the merged-order page: prefix items reference the prefix,
	// key items reference entries[entryIdx]. Preserves AWS ordering so a
	// key gathered before a roll-up fills the page first (leaf 5.1 [a]-4).
	items []listItem
	// advanced is the index into allObjectKeys just past the last key this
	// window examined.
	advanced int
	// commonPrefixes carries the updated roll-up list (append passthrough).
	commonPrefixes []string
}

type listItem struct {
	isPrefix bool
	prefix   string
	entryIdx int // index into window.entries when isPrefix == false
}

// gatherListWindow collects the next window of page-eligible items
// (cursor/prefix/delimiter pre-filter — no meta I/O) in merged key order.
// Delimiter roll-ups and keys count toward maxKeys in the order they appear
// in the sorted key space (AWS merged-order semantics; leaf 5.1 [a]-4).
func gatherListWindow(allObjectKeys []string, start int, p *listObjectsParams, processedCount *int, seenPrefixes map[string]struct{}, commonPrefixes []string, truncated *bool, nextToken *string) (window listWindow) {
	need := p.maxKeys - *processedCount
	if need <= 0 {
		window.advanced = start
		window.commonPrefixes = commonPrefixes
		return window
	}

	i := start
	for i < len(allObjectKeys) {
		if len(window.items) >= need {
			// Page budget consumed in merged order.
			window.advanced, window.commonPrefixes = noteBudgetExhausted(allObjectKeys, i, p, seenPrefixes, commonPrefixes, truncated, nextToken)
			return window
		}
		objectKey := allObjectKeys[i]
		i++
		if keyExcludedByCursor(objectKey, p) {
			continue
		}
		if !keyMatchesPrefixFilter(objectKey, p) {
			continue
		}
		if p.delimiter != "" {
			if gatherDelimiterKey(&window, objectKey, p, seenPrefixes) {
				continue
			}
		}
		window.entries = append(window.entries, listEntry{objectKey: objectKey})
		window.items = append(window.items, listItem{entryIdx: len(window.entries) - 1})
	}
	window.advanced = i
	window.commonPrefixes = commonPrefixes
	return window
}

// gatherDelimiterKey folds one delimiter-delimited key into the window: it
// registers the key's roll-up prefix as a page item, or skips it as already
// seen/cursor-consumed/outside the request prefix (consumed=true), or
// reports consumed=false meaning the key is a plain page entry.
func gatherDelimiterKey(window *listWindow, objectKey string, p *listObjectsParams, seenPrefixes map[string]struct{}) (consumed bool) {
	keyPartAfterRequestPrefix := objectKey
	if strings.HasPrefix(objectKey, p.prefix) {
		keyPartAfterRequestPrefix = objectKey[len(p.prefix):]
	} else if p.prefix != "" {
		return true // outside the request prefix: excluded
	}
	idx := strings.Index(keyPartAfterRequestPrefix, p.delimiter)
	if idx == -1 {
		return false // no delimiter after the prefix: plain key
	}
	commonPrefixValue := p.prefix + keyPartAfterRequestPrefix[:idx+len(p.delimiter)]
	if _, exists := seenPrefixes[commonPrefixValue]; exists {
		return true // duplicate roll-up: free (dedupe before counting)
	}
	// Leaf 5.1 [a]-4: a cursor at or beyond the roll-up consumed the whole
	// group (V1 marker semantics; V2 token INSIDE the group likewise).
	if groupConsumedByCursor(commonPrefixValue, p) {
		return true
	}
	// First-seen roll-up is one page item (leaf-2.4 fix 12). The emit loop
	// appends to commonPrefixes (it owns encoding); gather only dedupes and
	// orders.
	seenPrefixes[commonPrefixValue] = struct{}{}
	window.items = append(window.items, listItem{isPrefix: true, prefix: commonPrefixValue})
	return true
}

// groupConsumedByCursor reports whether a delimiter roll-up group was fully
// consumed by the request cursor (leaf 5.1 [a]-4): AWS V1 — the roll-up
// itself at or below the marker/start-after cursor; V2 — the opaque
// continuation token lies INSIDE the prefix group, meaning the page that
// issued the token already emitted the group.
func groupConsumedByCursor(commonPrefixValue string, p *listObjectsParams) bool {
	if p.continuationToken != "" {
		return strings.HasPrefix(p.continuationToken, commonPrefixValue)
	}
	return keyExcludedByCursor(commonPrefixValue, p)
}

// noteBudgetExhausted finalizes the window once the merged-order page budget
// is consumed: truncated only if some LATER key still yields a NEW page item
// (leaf 5.1 [a]-4: marker='boo/' with only already-emitted groups left must
// NOT be truncated). The token is the first unconsumed key as a fallback;
// the emit loop refines it to the last emitted item.
func noteBudgetExhausted(allObjectKeys []string, i int, p *listObjectsParams, seenPrefixes map[string]struct{}, commonPrefixes []string, truncated *bool, nextToken *string) (advanced int, outPrefixes []string) {
	hasMore := false
	for _, later := range allObjectKeys[i:] {
		if keyExcludedByCursor(later, p) || !keyMatchesPrefixFilter(later, p) {
			continue
		}
		if p.delimiter != "" {
			// The key folds into a roll-up; it yields a NEW item only if
			// that roll-up is unseen and unconsumed.
			after := later
			if strings.HasPrefix(later, p.prefix) {
				after = later[len(p.prefix):]
			}
			if idx := strings.Index(after, p.delimiter); idx != -1 {
				pv := p.prefix + after[:idx+len(p.delimiter)]
				if _, seen := seenPrefixes[pv]; seen {
					continue
				}
				if groupConsumedByCursor(pv, p) {
					continue
				}
				hasMore = true
				break
			}
		}
		hasMore = true // plain key → new item
		break
	}
	if hasMore {
		*truncated = true
		// Fallback token = first unconsumed key; the emit loop refines it
		// to the last emitted item (leaf 5.1 [a]-4).
		if i < len(allObjectKeys) {
			*nextToken = allObjectKeys[i]
		}
	}
	return i, commonPrefixes
}

// keyExcludedByCursor reports whether objectKey is excluded by the
// continuation/start-after/marker cursor. Semantics differ per parameter
// (AWS behavior, leaf-3.6 e2e finding; V1 marker added by leaf 5.1 [a]-3):
//   - continuation-token: the token IS the first key of the next page, so
//     the boundary key must be LISTED — exclude strictly below it (<).
//   - start-after: exclusive marker — exclude everything at or below it
//     (<=), including the marker key itself.
//   - marker (ListObjects V1): exclusive like start-after — exclude keys
//     at or below it (<=). AWS V1: "Specifies the key to start with";
//     the marker itself is never listed.
func keyExcludedByCursor(objectKey string, p *listObjectsParams) bool {
	if p.continuationToken != "" {
		return objectKey < p.continuationToken
	}
	if p.startAfter != "" {
		return objectKey <= p.startAfter
	}
	if p.marker != "" {
		return objectKey <= p.marker
	}
	return false
}

// keyMatchesPrefixFilter reports whether objectKey passes the prefix filter.
func keyMatchesPrefixFilter(objectKey string, p *listObjectsParams) bool {
	return p.prefix == "" || strings.HasPrefix(objectKey, p.prefix)
}

// parseInt converts a string to an integer, rejecting partial parses like "5a"
func parseInt(valueStr string, paramName string) (int, error) {
	val, err := strconv.Atoi(valueStr)
	if err != nil {
		log.Printf("Invalid %s value: %s", paramName, strconv.Quote(valueStr))
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
	for seg := range strings.SplitSeq(key, "/") {
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
		if err := os.Remove(dir); err != nil { //nolint:gosec // G703: dir derived from validated object metadata path; no traversal possible.
			break
		}
		dir = filepath.Dir(dir)
	}
}

// Leaf 3.5 — CopyObject + DeleteObjects (batch)

// maxBatchDeleteKeys is the S3 cap on the number of keys per DeleteObjects
// request; more than this is MalformedXML.
const maxBatchDeleteKeys = 1000

// parseCopySource parses an x-amz-copy-source header value of the form
// "bucket/key" or "/bucket/key", optionally with a "?versionId=..." suffix.
// Returns the source bucket, source key, and whether a versionId suffix was
// present (rejected with 501 by the caller — no versioning here).
func parseCopySource(copySource string) (srcBucket, srcKey string, hasVersionID bool) {
	if q := strings.IndexRune(copySource, '?'); q != -1 {
		if strings.Contains(copySource[q:], "versionId=") {
			hasVersionID = true
		}
		copySource = copySource[:q]
	}
	copySource = strings.TrimPrefix(copySource, "/")
	srcBucket, srcKey, _ = strings.Cut(copySource, "/")
	return srcBucket, srcKey, hasVersionID
}

// buildCopyMetadata constructs the destination ObjectMetadata for a copy.
// COPY (default) preserves the source Content-Type and x-amz-meta-*;
// REPLACE takes Content-Type and x-amz-meta-* from the request headers.
func buildCopyMetadata(srcMeta *ObjectMetadata, r *http.Request, data []byte, eTag, dstDataPath string) ObjectMetadata {
	meta := ObjectMetadata{
		ContentLength:  int64(len(data)),
		ETag:           eTag,
		CustomMetadata: make(map[string]string),
		LastModified:   time.Now().UTC(),
		StoragePath:    dstDataPath,
	}
	directive := r.Header.Get("x-amz-metadata-directive")
	if directive == "" {
		directive = "COPY"
	}
	switch directive {
	case "REPLACE":
		meta.ContentType = r.Header.Get("Content-Type")
		for headerName, headerValues := range r.Header {
			if strings.HasPrefix(strings.ToLower(headerName), "x-amz-meta-") {
				meta.CustomMetadata[headerName] = strings.Join(headerValues, ", ")
			}
		}
	default: // COPY
		meta.ContentType = srcMeta.ContentType
		maps.Copy(meta.CustomMetadata, srcMeta.CustomMetadata)
	}
	return meta
}

// copyObjectHandler implements CopyObject: a PUT with x-amz-copy-source.
// The object data is copied server-side (source data + meta read, then
// written to the destination via the storage.go atomic helpers), and the
// response is the S3 quirk 200-with-XML-body CopyObjectResult.
func copyObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s for CopyObject: %v", strconv.Quote(objectName), err)
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}

	copySource := r.Header.Get("x-amz-copy-source")
	decoded, err := url.PathUnescape(copySource)
	if err != nil {
		log.Printf("Invalid x-amz-copy-source header %s: %v", strconv.Quote(copySource), err)
		writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source header.", http.StatusBadRequest)
		return
	}
	srcBucket, srcKey, hasVersionID := parseCopySource(decoded)
	if hasVersionID {
		log.Printf("CopyObject with versionId not supported (source %s)", strconv.Quote(decoded))
		writeS3Error(w, "NotImplemented", "Copy from a specific version is not implemented.", http.StatusNotImplemented)
		return
	}
	if srcBucket == "" || srcKey == "" {
		log.Printf("Invalid x-amz-copy-source header %s: expected bucket/key", strconv.Quote(decoded))
		writeS3Error(w, "InvalidArgument", "x-amz-copy-source must be of the form bucket/key.", http.StatusBadRequest)
		return
	}
	if err := validateObjectKey(srcKey); err != nil {
		log.Printf("Invalid source key %s for CopyObject: %v", strconv.Quote(srcKey), err)
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}

	// S3 rejects a copy where source and destination are the same object
	// (without a directive changing the copy semantics).
	if srcBucket == bucketName && srcKey == objectName {
		log.Printf("CopyObject source and destination are identical: %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))
		writeS3Error(w, "InvalidRequest", "This copy request is illegal because it is trying to copy an object to itself.", http.StatusBadRequest)
		return
	}

	// Metadata directive: COPY (default) preserves the source metadata;
	// REPLACE uses the request headers. Anything else is InvalidArgument.
	directive := r.Header.Get("x-amz-metadata-directive")
	if directive == "" {
		directive = "COPY"
	}
	switch directive {
	case "COPY", "REPLACE":
	default:
		log.Printf("Invalid x-amz-metadata-directive %s for CopyObject", strconv.Quote(directive))
		writeS3Error(w, "InvalidArgument", "Unknown metadata directive.", http.StatusBadRequest)
		return
	}

	srcBucketPath := getBucketPath(srcBucket)
	if _, err := os.Stat(srcBucketPath); os.IsNotExist(err) {
		log.Printf("Source bucket %s does not exist for CopyObject", strconv.Quote(srcBucket))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	srcMetaPath := filepath.Join(srcBucketPath, ".metadata", srcKey+".meta")
	srcMetaJSON, err := os.ReadFile(srcMetaPath) //nolint:gosec // G703: srcKey validated by validateObjectKey; no traversal possible.
	if os.IsNotExist(err) {
		log.Printf("Source object %s/%s does not exist for CopyObject", strconv.Quote(srcBucket), strconv.Quote(srcKey))
		writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Error reading source metadata %s: %v", strconv.Quote(srcMetaPath), err)
		writeS3Error(w, "InternalError", "Error reading source object metadata.", http.StatusInternalServerError)
		return
	}
	var srcMeta ObjectMetadata
	if err := json.Unmarshal(srcMetaJSON, &srcMeta); err != nil {
		log.Printf("Error parsing source metadata %s: %v", strconv.Quote(srcMetaPath), err)
		writeS3Error(w, "InternalError", "Error parsing source object metadata.", http.StatusInternalServerError)
		return
	}
	srcDataPath := resolveObjectDataPath(srcBucketPath, srcKey, &srcMeta)

	// Read the source data. Lock both source and destination so a concurrent
	// writer cannot tear the copy.
	srcUnlock := lockObject(srcDataPath)
	defer srcUnlock()

	data, err := os.ReadFile(srcDataPath) //nolint:gosec // G703: srcDataPath derived from validated srcKey; no traversal possible.
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Source object data %s does not exist for CopyObject", strconv.Quote(srcDataPath))
			writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
			return
		}
		log.Printf("Error reading source object data %s: %v", strconv.Quote(srcDataPath), err)
		writeS3Error(w, "InternalError", "Error reading source object data.", http.StatusInternalServerError)
		return
	}

	// S3 computes a fresh ETag for the new object.
	hash := md5.Sum(data) //nolint:gosec // G401: S3 ETags are defined as MD5; protocol requirement, not crypto.
	eTag := hex.EncodeToString(hash[:])

	dstBucketPath := getBucketPath(bucketName)
	// Shadow-aware data path (leaf 5.1 [a]-1): same collision rules as PUT.
	dstDataPath := objectDataPathFor(dstBucketPath, objectName)
	dstMetaPath := filepath.Join(dstBucketPath, ".metadata", objectName+".meta")

	dstUnlock := lockObject(dstDataPath)
	defer dstUnlock()

	if err := os.MkdirAll(filepath.Dir(dstDataPath), 0755); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		log.Printf("Error creating destination directories for %s: %v", strconv.Quote(dstDataPath), err)
		writeS3Error(w, "InternalError", "Error creating object storage.", http.StatusInternalServerError)
		return
	}
	if err := writeFileAtomic(dstDataPath, data, 0644); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		log.Printf("Error writing destination object data %s: %v", strconv.Quote(dstDataPath), err)
		writeS3Error(w, "InternalError", "Error writing object data.", http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dstMetaPath), 0755); err != nil { //nolint:gosec // G703: objectName validated by validateObjectKey; no traversal possible.
		log.Printf("Error creating destination metadata storage for %s: %v", strconv.Quote(dstMetaPath), err)
		writeS3Error(w, "InternalError", "Error creating metadata storage.", http.StatusInternalServerError)
		return
	}

	// Build the destination metadata per the directive.
	meta := buildCopyMetadata(&srcMeta, r, data, eTag, dstDataPath)

	if err := writeFileAtomicJSON(dstMetaPath, meta, 0644); err != nil {
		log.Printf("Error writing destination metadata %s: %v", strconv.Quote(dstMetaPath), err)
		writeS3Error(w, "InternalError", "Error writing metadata.", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully copied %s/%s to %s/%s, ETag: %s",
		strconv.Quote(srcBucket), strconv.Quote(srcKey), strconv.Quote(bucketName), strconv.Quote(objectName), eTag)

	// S3 quirk: a 200 response with an XML body.
	writeXML(w, http.StatusOK, CopyObjectResult{
		ETag:         fmt.Sprintf("%q", eTag),
		LastModified: meta.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
	})

	// Trigger after_upload actions for the new object.
	go triggerActions("after_upload", ActionContext{
		FilePath:     dstDataPath,
		MetadataPath: dstMetaPath,
		BucketName:   bucketName,
		BucketPath:   dstBucketPath,
		ObjectKey:    objectName,
		ContentType:  meta.ContentType,
		ETag:         eTag,
		Size:         meta.ContentLength,
	})
}

// deleteObjectsHandler implements DeleteObjects (POST /bucket?delete): batch
// deletion of up to 1000 keys with a DeleteResult XML response. In Quiet
// mode only errors are reported.
func deleteObjectsHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucketPath := getBucketPath(bucketName)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		log.Printf("Bucket %s does not exist for DeleteObjects", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading DeleteObjects body for bucket %s: %v", strconv.Quote(bucketName), err)
		writeS3Error(w, "InternalError", "Error reading request body.", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var req DeleteRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		log.Printf("MalformedXML in DeleteObjects request for bucket %s: %v", strconv.Quote(bucketName), err)
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest)
		return
	}
	// S3 rule: empty key list or more than 1000 keys is MalformedXML.
	if len(req.Objects) == 0 || len(req.Objects) > maxBatchDeleteKeys {
		log.Printf("DeleteObjects for bucket %s: invalid object count %d", strconv.Quote(bucketName), len(req.Objects))
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest)
		return
	}

	result := DeleteResult{}
	for _, obj := range req.Objects {
		if err := validateObjectKey(obj.Key); err != nil {
			log.Printf("Invalid key %s in DeleteObjects for bucket %s: %v", strconv.Quote(obj.Key), strconv.Quote(bucketName), err)
			result.Error = append(result.Error, DeleteErrorEntry{
				Key:     obj.Key,
				Code:    "InvalidArgument",
				Message: err.Error(),
			})
			continue
		}
		if err := deleteObjectCore(bucketPath, bucketName, obj.Key); err != nil {
			log.Printf("Error deleting %s/%s in DeleteObjects: %v", strconv.Quote(bucketName), strconv.Quote(obj.Key), err)
			result.Error = append(result.Error, DeleteErrorEntry{
				Key:     obj.Key,
				Code:    "InternalError",
				Message: "Error deleting object data.",
			})
			continue
		}
		// S3 semantics: a missing key still reports Deleted.
		result.Deleted = append(result.Deleted, DeletedEntry(obj))
	}

	if req.Quiet {
		// Quiet mode: successful deletions are suppressed, errors only.
		result.Deleted = nil
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Successfully served DeleteObjects for bucket %s (%d keys, quiet=%t)",
		strconv.Quote(bucketName), len(req.Objects), req.Quiet)
}
