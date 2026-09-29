package main

import (
	"bytes"
	"context"
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

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// object_handlers.go — S3 object-level operation handlers

// backendCall resolves bucket's Backend via the backendFor seam and runs
// fn. A nil Backend (construction failure) collapses to InternalError.
func backendCall(bucket string, fn func(b backend.Backend) (objectmodel.Object, error)) (objectmodel.Object, error) {
	b, err := backendFor(bucket)
	if err != nil || b == nil {
		if err == nil {
			err = objectmodel.ErrInternalError("backend unavailable")
		}
		return objectmodel.Object{}, err
	}
	return fn(b)
}

func putObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	// Leaf 3.5: x-amz-copy-source header → CopyObject (server-side copy).
	if r.Header.Get("x-amz-copy-source") != "" {
		copyObjectHandler(w, r, bucketName, objectName)
		return
	}

	// Validate object key (leaf 2.4 fix 2: traversal rejection) — first
	// line of defense handler-side; fsbackend re-validates defensively.
	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s: %v", strconv.Quote(objectName), err)
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}

	// Ensure bucket exists
	if !bucketExists(bucketName) {
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

	// Data-plane flip (leaf 02): the write path goes through the Backend.
	// The backend performs the shadow-layout choice, per-key + parent-dir
	// locking, atomic data+sidecar writes, and the RESIDUAL WINDOW
	// preservation (metadata failure leaves the data file in place).
	// Custom metadata is passed in the RAW x-amz-meta-* form (original
	// header casing): fsbackend's prefixedMetadata passes prefixed keys
	// through verbatim, reproducing the pre-seam sidecar bytes exactly.
	// (Values are the pre-joined header values; ParseMetadataHeaders'
	// surplus-join is not needed because rawMetaHeaders already joined.)
	metaNames, metaValues := rawMetaHeaders(r)
	rawMeta := make(map[string]string, len(metaNames))
	for i, name := range metaNames {
		rawMeta[name] = metaValues[i]
	}
	opts := objectmodel.PutOptions{
		ContentType: r.Header.Get("Content-Type"),
		Metadata:    rawMeta,
	}
	obj, putErr := backendCall(bucketName, func(b backend.Backend) (objectmodel.Object, error) {
		return b.Put(r.Context(), bucketName, objectName, bytes.NewReader(body), int64(len(body)), opts)
	})
	if putErr != nil {
		log.Printf("Error putting object %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), putErr)
		writeS3ErrorFrom(w, putErr)
		return
	}
	eTag := obj.ETag

	log.Printf("Successfully put object %s/%s, ETag: %s", strconv.Quote(bucketName), strconv.Quote(objectName), eTag)
	w.Header().Set("ETag", fmt.Sprintf("%q", eTag))
	w.WriteHeader(http.StatusOK)

	// Trigger after_upload actions. The action context needs the concrete
	// data/metadata paths; resolve them handler-side (pure path math — no
	// data-plane os.* calls).
	bucketPath := getBucketPath(bucketName)
	objectDataPath := objectDataPathFor(bucketPath, objectName)
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")
	go triggerActions("after_upload", ActionContext{
		FilePath:     objectDataPath,
		MetadataPath: objectMetadataPath,
		BucketName:   bucketName,
		BucketPath:   bucketPath,
		ObjectKey:    objectName,
		ContentType:  opts.ContentType,
		ETag:         eTag,
		Size:         obj.Size,
	})
}

// rawMetaHeaders enumerates the request's x-amz-meta-* header names and
// their joined values (parallel slices, multi-value headers joined with
// ", " — the pre-seam CustomMetadata loop's exact semantics).
func rawMetaHeaders(r *http.Request) (names, values []string) {
	for headerName, headerValues := range r.Header {
		if strings.HasPrefix(strings.ToLower(headerName), "x-amz-meta-") {
			names = append(names, headerName)
			values = append(values, strings.Join(headerValues, ", "))
		}
	}
	return names, values
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
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for GetObject", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s: %v", strconv.Quote(objectName), err)
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}

	// Data-plane flip (leaf 02): read metadata + open the data file through
	// the Backend seam. The backend performs the corrupt-storagePath
	// fallback and holds the leaf-4.8 reader locks across stat→open.
	srcRC, meta, getErr := backendCallBucket2(bucketName, func(b backend.Backend) (io.ReadCloser, objectmodel.Object, error) {
		return b.Get(r.Context(), bucketName, objectName, objectmodel.GetOptions{})
	})
	if getErr != nil {
		code, message, status := s3ErrorFrom(getErr)
		log.Printf("GetObject %s/%s failed: %v", strconv.Quote(bucketName), strconv.Quote(objectName), getErr)
		writeS3Error(w, code, message, status)
		return
	}
	defer srcRC.Close()
	actualSize := meta.Size

	// Set headers from metadata (leaf 2.4 fix 4: default Content-Type).
	// Custom metadata rides the neutral model in canonical key form; the
	// x-amz-meta-* prefix is re-added for the wire.
	contentType := meta.ContentType
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	// Leaf 3.1 fix 7: conditional headers are evaluated BEFORE Range.
	// The validator headers (ETag, Last-Modified) must be set first so a
	// 304 response carries them.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf("%q", meta.ETag))
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	if checkObjectPreconditions(w, r, meta.ETag, meta.LastModified) {
		return
	}

	// Leaf 3.1 fix 5: advertise byte-range support on every 200/206.
	w.Header().Set("Accept-Ranges", "bytes")
	rr := parseRangeHeader(r.Header.Get("Range"), actualSize)
	if rr.outcome != rangeFull {
		// serveObjectRange needs an *os.File for seeking; the seam returns
		// an ReadCloser. A range-read through the seam is done by draining
		// and discarding the leading bytes, then copying the window.
		if serveObjectRangeFrom(r.Context(), w, srcRC, rr, actualSize, false, fmt.Sprintf("GetObject %s/%s", bucketName, objectName)) {
			log.Printf("Served range request for object %s/%s (%s)", strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(r.Header.Get("Range"))) //nolint:gosec // G706: Range is strconv.Quote-escaped.
			go triggerActions("after_download", ActionContext{
				FilePath:     objectDataPathFor(bucketPath, objectName),
				MetadataPath: objectMetadataPath,
				BucketName:   bucketName,
				BucketPath:   bucketPath,
				ObjectKey:    objectName,
				ContentType:  meta.ContentType,
				ETag:         meta.ETag,
				Size:         meta.Size,
			})
			return
		}
		// Range handling consumed the stream — reopen for the full-body
		// fall-through (multi-range/malformed fall back to 200 full body).
		srcRC2, _, reopenErr := backendCallBucket2(bucketName, func(b backend.Backend) (io.ReadCloser, objectmodel.Object, error) {
			return b.Get(r.Context(), bucketName, objectName, objectmodel.GetOptions{})
		})
		if reopenErr != nil {
			writeS3ErrorFrom(w, reopenErr)
			return
		}
		srcRC.Close()
		srcRC = srcRC2
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", actualSize))
	for k, v := range meta.Metadata {
		w.Header().Set(objectmodel.MetadataHeaderName(k), v)
	}

	// Stream the object data (leaf 2.4 fix 6: a failed open becomes
	// NoSuchKey/404 rather than a 500 with headers half-set — enforced by
	// the backend Get above, which opens before returning).
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, srcRC); err != nil {
		log.Printf("Error streaming object %s/%s to client: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
	}
	log.Printf("Successfully served object %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))

	// Trigger after_download actions
	go triggerActions("after_download", ActionContext{
		FilePath:     objectDataPathFor(bucketPath, objectName),
		MetadataPath: objectMetadataPath,
		BucketName:   bucketName,
		BucketPath:   bucketPath,
		ObjectKey:    objectName,
		ContentType:  meta.ContentType,
		ETag:         meta.ETag,
		Size:         meta.Size,
	})
}

// serveObjectRangeFrom applies the parsed Range outcome using a stream
// (seam ReadCloser) instead of a seekable *os.File. rangeFull returns
// false (caller serves the normal 200); rangeUnsatisfiable writes 416;
// rangePartial discards rr.start bytes then copies rr.length bytes.
func serveObjectRangeFrom(ctx context.Context, w http.ResponseWriter, rc io.Reader, rr rangeRequest, actualSize int64, isHead bool, logPrefix string) bool {
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
			if _, err := io.CopyN(io.Discard, rc, rr.start); err != nil {
				log.Printf("%s: error seeking to range start %d: %v", logPrefix, rr.start, err) //nolint:gosec // G706: logPrefix is handler-constructed.
				return true
			}
			if _, err := io.CopyN(w, rc, rr.length); err != nil {
				log.Printf("%s: error streaming range to client: %v", logPrefix, err) //nolint:gosec // G706: logPrefix is handler-constructed.
			}
		}
		return true
	default:
		return false
	}
}

func deleteObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	// Leaf 2.4 fix 3: a missing bucket is a real 404 (missing KEY in an
	// existing bucket still stays 204 per S3 semantics).
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for DeleteObject", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	if err := deleteObjectCore(getBucketPath(bucketName), bucketName, objectName); err != nil {
		log.Printf("Error deleting object %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeS3ErrorFrom(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent) // S3 spec: 204 No Content
}

// deleteObjectCore is the shared deletion logic for DeleteObject (single,
// leaf 2.4) and DeleteObjects (batch, leaf 3.5). The bucket's existence must
// be checked by the caller. A missing key deletes nothing and succeeds (S3
// semantics). Returns an error only on real I/O failure of the data file.
// Data-plane flip (leaf 02): the delete goes through the Backend seam; this
// wrapper keeps the after_delete action-context plumbing handler-side.
func deleteObjectCore(bucketPath, bucketName, objectName string) error {
	if err := validateObjectKey(objectName); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	delErr := backendCallBucketErr(bucketName, func(b backend.Backend) error {
		return b.Delete(context.Background(), bucketName, objectName)
	})
	if delErr != nil {
		return delErr
	}

	// Action context: resolve the concrete paths the action may reference
	// (pure path math, no data-plane os.* calls; the sidecar is gone, so
	// the canonical location is the best-available approximation — the
	// pre-seam code reported the sidecar-resolved path, but after a
	// successful delete that path no longer exists either way).
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", objectName+".meta")
	actualDataPath := objectDataPathFor(bucketPath, objectName)
	log.Printf("Successfully deleted object %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))
	go triggerActions("after_delete", ActionContext{
		FilePath:     actualDataPath,
		MetadataPath: objectMetadataPath,
		BucketName:   bucketName,
		BucketPath:   bucketPath,
		ObjectKey:    objectName,
	})
	return nil
}

// backendCallBucket is backendCall for error-only operations (Delete).
func backendCallBucket(bucket string, fn func(b backend.Backend) error) error {
	b, err := backendFor(bucket)
	if err != nil || b == nil {
		if err == nil {
			err = objectmodel.ErrInternalError("backend unavailable")
		}
		return err
	}
	return fn(b)
}

// backendCallBucketErr is an alias for backendCallBucket (error-only ops).
func backendCallBucketErr(bucket string, fn func(b backend.Backend) error) error {
	return backendCallBucket(bucket, fn)
}

func headObjectHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	// Check if bucket exists
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for HeadObject", strconv.Quote(bucketName))
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err := validateObjectKey(objectName); err != nil {
		log.Printf("Invalid object key %s for HeadObject: %v", strconv.Quote(objectName), err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Data-plane flip (leaf 02): Stat through the Backend seam. The backend
	// performs the corrupt-storagePath fallback, serves the ACTUAL file
	// size, and holds the leaf-4.8 reader locks across stat.
	meta, statErr := backendCallBucketStat(bucketName, func(b backend.Backend) (objectmodel.Object, error) {
		return b.Stat(r.Context(), bucketName, objectName)
	})
	if statErr != nil {
		if _, _, status := s3ErrorFrom(statErr); status == http.StatusNotFound {
			log.Printf("HeadObject %s/%s not found: %v", strconv.Quote(bucketName), strconv.Quote(objectName), statErr)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	actualSize := meta.Size

	// Set headers from metadata (leaf 2.4 fix 4: default Content-Type)
	contentType := meta.ContentType
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	// Leaf 3.1 fix 7: conditional headers are evaluated BEFORE Range.
	// Validator headers (ETag, Last-Modified) are set first so a 304
	// carries them.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf("%q", meta.ETag))
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
	for k, v := range meta.Metadata {
		w.Header().Set(objectmodel.MetadataHeaderName(k), v)
	}

	w.WriteHeader(http.StatusOK)
	log.Printf("Successfully served HEAD for object %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))
}

// backendCallBucketStat is backendCall for (Object, error) operations (Stat).
func backendCallBucketStat(bucket string, fn func(b backend.Backend) (objectmodel.Object, error)) (objectmodel.Object, error) {
	b, err := backendFor(bucket)
	if err != nil || b == nil {
		if err == nil {
			err = objectmodel.ErrInternalError("backend unavailable")
		}
		return objectmodel.Object{}, err
	}
	return fn(b)
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
	// Check if bucket exists
	if !bucketExists(bucketName) {
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

	// Data-plane flip (leaf 02): key enumeration, filtering, roll-up and
	// pagination go through the Backend seam. The S3-specific V1 marker is
	// folded into the backend cursor as an at-or-below StartAfter (identical
	// exclusion semantics — leaf 5.1 [a]-3); encoding-type=url and the
	// NextMarker wire field stay HANDLER-side.
	backendParams := objectmodel.ListParams{
		Prefix:            params.prefix,
		Delimiter:         params.delimiter,
		ContinuationToken: params.continuationToken,
		StartAfter:        params.startAfter,
		MaxKeys:           params.maxKeys,
	}
	if params.marker != "" && params.continuationToken == "" && params.startAfter == "" {
		// V1 marker: exclusive at-or-below — the backend's StartAfter has
		// exactly those semantics.
		backendParams.StartAfter = params.marker
	}
	var page objectmodel.ListPage
	listErr := backendCallBucketErr(bucketName, func(b backend.Backend) error {
		var lErr error
		page, lErr = b.List(r.Context(), bucketName, backendParams)
		return lErr
	})
	if listErr != nil {
		log.Printf("Error listing bucket %s: %v", strconv.Quote(bucketName), listErr)
		writeS3ErrorFrom(w, listErr)
		return
	}

	truncated := page.IsTruncated
	nextToken := page.NextToken
	// Leaf 2.4 fix 14: IsTruncated=true must carry a non-empty token; if the
	// token is empty, no next page exists → report IsTruncated=false.
	if truncated && nextToken == "" {
		truncated = false
	}

	// S3 Contents entries carry quoted ETags and RFC3339 millisecond
	// timestamps; the backend's neutral model carries the bare ETag and
	// time.Time. Convert here (presentation, not data access).
	var objects []Object
	var lastItem string
	for _, o := range page.Objects {
		keyOut := o.Key
		if params.encodeKeys {
			keyOut = s3URLEncode(keyOut)
		}
		objects = append(objects, Object{
			Key:          keyOut,
			LastModified: o.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         fmt.Sprintf("%q", o.ETag),
			Size:         o.Size,
			StorageClass: "STANDARD",
		})
		lastItem = o.Key
	}

	var commonPrefixEntries []CommonPrefix
	for _, cp := range page.CommonPrefixes {
		cpOut := cp
		if params.encodeKeys {
			cpOut = s3URLEncode(cp)
		}
		commonPrefixEntries = append(commonPrefixEntries, CommonPrefix{Prefix: cpOut})
		// Merged-order NextMarker: a roll-up sorts among the keys; recompute
		// the page's last emitted item in MERGED order.
		if lastItem == "" || cp > lastItem {
			lastItem = cp
		}
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

	if !bucketExists(srcBucket) {
		log.Printf("Source bucket %s does not exist for CopyObject", strconv.Quote(srcBucket))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	// Data-plane flip (leaf 02): read the source through the Backend seam.
	srcRC, srcObj, srcErr := backendCallBucket2(srcBucket, func(b backend.Backend) (io.ReadCloser, objectmodel.Object, error) {
		return b.Get(r.Context(), srcBucket, srcKey, objectmodel.GetOptions{})
	})
	if srcErr != nil {
		code, message, status := s3ErrorFrom(srcErr)
		log.Printf("CopyObject source read %s/%s failed: %v", strconv.Quote(srcBucket), strconv.Quote(srcKey), srcErr)
		writeS3Error(w, code, message, status)
		return
	}
	data, readErr := io.ReadAll(srcRC)
	srcRC.Close()
	if readErr != nil {
		log.Printf("Error reading source object data %s/%s: %v", strconv.Quote(srcBucket), strconv.Quote(srcKey), readErr)
		writeS3ErrorFrom(w, readErr)
		return
	}

	// S3 computes a fresh ETag for the new object.
	hash := md5.Sum(data) //nolint:gosec // G401: S3 ETags are defined as MD5; protocol requirement, not crypto.
	eTag := hex.EncodeToString(hash[:])

	// Destination metadata per the directive: COPY (default) preserves the
	// source Content-Type and x-amz-meta-* (ORIGINAL on-disk casing — the
	// pre-seam buildCopyMetadata maps.Copy'ed the sidecar map verbatim);
	// REPLACE takes both from the request headers (original header casing).
	// The seam canonicalizes keys, so the raw sidecar keys are re-read from
	// the source metadata file for the COPY branch (one read-only stat/
	// read of the sidecar — no data-plane write touches os.* here).
	dstMeta := map[string]string{}
	if directive == "REPLACE" {
		for headerName, headerValues := range r.Header {
			if strings.HasPrefix(strings.ToLower(headerName), "x-amz-meta-") {
				// Raw prefixed form: fsbackend passes prefixed keys
				// through verbatim (original casing preserved).
				dstMeta[headerName] = strings.Join(headerValues, ", ")
			}
		}
	} else {
		// COPY: preserve the source sidecar custom keys verbatim
		// (original casing — the pre-seam maps.Copy semantics).
		maps.Copy(dstMeta, rawSourceSidecarMeta(getBucketPath(srcBucket), srcKey))
	}

	dstBucketPath := getBucketPath(bucketName)
	dstDataPath := objectDataPathFor(dstBucketPath, objectName)
	dstContentType := srcObj.ContentType
	if directive == "REPLACE" {
		dstContentType = r.Header.Get("Content-Type")
	}

	_, putErr := backendCall(bucketName, func(b backend.Backend) (objectmodel.Object, error) {
		return b.Put(r.Context(), bucketName, objectName, bytes.NewReader(data), int64(len(data)),
			objectmodel.PutOptions{ContentType: dstContentType, Metadata: dstMeta})
	})
	if putErr != nil {
		log.Printf("Error writing copy destination %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), putErr)
		writeS3ErrorFrom(w, putErr)
		return
	}

	// Action context: resolve the concrete paths the action may reference.
	dstMetaPath := filepath.Join(dstBucketPath, ".metadata", objectName+".meta")
	log.Printf("Successfully copied %s/%s to %s/%s, ETag: %s",
		strconv.Quote(srcBucket), strconv.Quote(srcKey), strconv.Quote(bucketName), strconv.Quote(objectName), eTag)

	// S3 quirk: a 200 response with an XML body.
	writeXML(w, http.StatusOK, CopyObjectResult{
		ETag:         fmt.Sprintf("%q", eTag),
		LastModified: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})

	// Trigger after_upload actions for the new object.
	go triggerActions("after_upload", ActionContext{
		FilePath:     dstDataPath,
		MetadataPath: dstMetaPath,
		BucketName:   bucketName,
		BucketPath:   dstBucketPath,
		ObjectKey:    objectName,
		ContentType:  dstContentType,
		ETag:         eTag,
		Size:         int64(len(data)),
	})
}

// backendCallBucket2 is backendCall for (ReadCloser, Object, error)
// operations (Get).
func backendCallBucket2(bucket string, fn func(b backend.Backend) (io.ReadCloser, objectmodel.Object, error)) (io.ReadCloser, objectmodel.Object, error) {
	b, err := backendFor(bucket)
	if err != nil || b == nil {
		if err == nil {
			err = objectmodel.ErrInternalError("backend unavailable")
		}
		return nil, objectmodel.Object{}, err
	}
	return fn(b)
}

// rawSourceSidecarMeta reads the source object's sidecar CustomMetadata
// verbatim (original key casing preserved) for CopyObject's COPY branch.
// Read-only: the seam does not surface raw key casing, and the frozen
// on-disk format for a copied object is byte-identical to the source's.
// Missing/unparsable sidecar yields an empty map (copy proceeds metaless —
// the pre-seam code would have failed the copy; but the seam Get above
// already proved the object exists, so this is unreachable in practice).
func rawSourceSidecarMeta(bucketPath, key string) map[string]string {
	raw, err := os.ReadFile(filepath.Join(bucketPath, ".metadata", key+".meta")) //nolint:gosec // G703: key validated by validateObjectKey.
	if err != nil {
		return map[string]string{}
	}
	var meta ObjectMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return map[string]string{}
	}
	if meta.CustomMetadata == nil {
		return map[string]string{}
	}
	return meta.CustomMetadata
}

// deleteObjectsHandler implements DeleteObjects (POST /bucket?delete): batch
// deletion of up to 1000 keys with a DeleteResult XML response. In Quiet
// mode only errors are reported.
func deleteObjectsHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	if !bucketExists(bucketName) {
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
		if err := deleteObjectCore(getBucketPath(bucketName), bucketName, obj.Key); err != nil {
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
