// batch_endpoint.go — quic-h3-2026-10 leaf 07: the JSON batch extension
// on the s3 frontend (POST /{bucket}?batch). ONE implementation in
// internal/batchops is mounted here AND by the webdav frontend
// (webdav/batch.go drives this same bridge), so the wire semantics are
// never duplicated across frontends. The h3 frontend needs NO code: it
// wraps the webdav handler, so ?batch is served over QUIC unchanged.
//
// Contract 9: the manifest is a flat list of explicit operations; items
// execute SEQUENTIALLY in manifest order through the batchops core,
// which drives per-item the SAME orchestration this package's single-op
// handlers use (Executor — batchExecutor below wraps the existing
// copy/delete cores and the versioning-capture write path). A malformed
// manifest (bad JSON, unknown op, invalid key, over the 1000-op limit)
// is a 400 and NOTHING executes; item failures after execution starts
// are per-item results, not request failures (no cross-item atomicity).
//
// Auth: this handler sits inside the existing dispatch, so the s3
// frontend's authenticator + grant gate run BEFORE it (bucket-level
// authorization covers every item — one request = one bucket). Audit:
// one record per REQUEST (recordAudit in serveHTTP; the op vocabulary
// and 8-key shape are unchanged).
package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/batchops"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// bucketBatchMaxBytes bounds the manifest body read (1000 ops × generous
// per-op size) so a runaway body cannot buffer unboundedly.
const bucketBatchMaxBytes = 4 << 20

// handleBucketBatch implements POST /{bucket}?batch. Mounted in
// dispatch.go's bucketLevelDispatch beside the ?delete sub-resource.
func handleBucketBatch(w http.ResponseWriter, r *http.Request, bucketName string) {
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for ?batch", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, bucketBatchMaxBytes+1))
	if err != nil {
		log.Printf("Error reading batch body for bucket %s: %v", strconv.Quote(bucketName), err)
		writeS3Error(w, "InternalError", "Error reading request body.", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close() //nolint:errcheck // server-side close.
	if len(body) > bucketBatchMaxBytes {
		writeS3Error(w, "InvalidArgument", "Batch manifest exceeds the maximum request size.", http.StatusBadRequest)
		return
	}

	runner := &batchops.Runner{Exec: s3BatchExecutor{bucket: bucketName, ctx: r.Context()}, ValidateKey: validateObjectKey}
	resp, reqErr := runner.Process(r.Context(), body)
	if reqErr != nil {
		// Malformed manifest: NOTHING executed (pinned by the batchops
		// zero-call assertions and the s3 disk-state tests).
		log.Printf("Batch manifest rejected for bucket %s: %v", strconv.Quote(bucketName), reqErr)
		writeS3Error(w, "MalformedXML", reqErr.Error(), http.StatusBadRequest)
		return
	}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("Error marshalling batch response for bucket %s: %v", strconv.Quote(bucketName), err)
		writeS3Error(w, "InternalError", "Internal Server Error.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	log.Printf("Successfully served batch for bucket %s (%d operations)", strconv.Quote(bucketName), len(resp.Results))
}

// s3BatchExecutor adapts the batchops op interface onto THIS package's
// existing single-op orchestration: each item runs the same get/put/
// delete sequence (and the same versioning capture, same per-key
// validation) the single-request handlers run. All ops are
// same-bucket (Contract 9: one request = one bucket = one auth scope).
type s3BatchExecutor struct {
	bucket string
	ctx    context.Context
}

// batchCopy is the copy item: a streamed Get→Put through the Backend
// seam (the copyObjectHandler data path, minus the protocol headers it
// serves), with versioning capture on an overwrite (the leaf-05
// machinery — capture BEFORE the put, record AFTER) and the If-Match
// precondition enforced against the destination's current ETag.
func (e s3BatchExecutor) Copy(_ context.Context, from, to, ifMatch string) error {
	if err := validateObjectKey(from); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	if err := validateObjectKey(to); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	return batchCopyMove(e.bucket, from, to, ifMatch, false)
}

// batchMove is the move item: copy then DELETE the source (MOVE
// semantics — no source copy left; S3's move equivalent is CopyObject +
// DeleteObject). The source delete runs the same deleteObjectVersioned
// path the DELETE handler runs, so a versioned bucket records exactly
// what that pair records.
func (e s3BatchExecutor) Move(_ context.Context, from, to, ifMatch string) error {
	if err := validateObjectKey(from); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	if err := validateObjectKey(to); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	if err := batchCopyMove(e.bucket, from, to, ifMatch, false); err != nil {
		return err
	}
	return batchDelete(e.bucket, from, ifMatch)
}

// batchDelete is the delete item: the SAME path deleteObjectCore runs
// (versioned marker on an Enabled bucket, plain delete otherwise), plus
// the If-Match precondition against the object's current ETag.
func (e s3BatchExecutor) Delete(_ context.Context, key, ifMatch string) error {
	if err := validateObjectKey(key); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	return batchDelete(e.bucket, key, ifMatch)
}

// batchCopyMove streams from → to through the Backend seam with the
// leaf-05 versioning capture (identical capture/record order to the s3
// PUT handler). isDeleteSource is unused today; the move = copy+delete
// split keeps each half reviewable against its single-op handler.
func batchCopyMove(bucket, from, to, ifMatch string, _ bool) error {
	// If-Match on the SOURCE copy is enforced against the source
	// object's current ETag (the "this still points at what I saw"
	// guarantee the manifest author means).
	if ifMatch != "" {
		_, srcObj, err := backendCallBucket2(bucket, func(b backend.Backend) (io.ReadCloser, objectmodel.Object, error) {
			return b.Get(context.Background(), bucket, from, objectmodel.GetOptions{})
		})
		if err != nil {
			return err
		}
		if !etagMatches(ifMatch, srcObj.ETag) {
			return objectmodel.ErrPreconditionFailed()
		}
	}

	rc, srcObj, err := backendCallBucket2(bucket, func(b backend.Backend) (io.ReadCloser, objectmodel.Object, error) {
		return b.Get(context.Background(), bucket, from, objectmodel.GetOptions{})
	})
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(rc)
	rc.Close()
	if readErr != nil {
		return readErr
	}

	// Versioning capture BEFORE the plain overwrite (leaf-05 order:
	// capture → put → record; ErrNoPriorVersion = create, no record).
	bucketPath := getBucketPath(bucket)
	var capturedOld *capturedObjectVersion
	if state, stErr := versionStoreForBucket(bucketPath).State(bucket); stErr == nil && state == versioningEnabled {
		captured, capErr := captureCurrentObjectVersion(bucketPath, to)
		if capErr != nil && !errors.Is(capErr, errNoPriorVersion) {
			return objectmodel.ErrInternalError("error capturing object version.")
		}
		capturedOld = captured
	}

	obj, putErr := backendCall(bucket, func(b backend.Backend) (objectmodel.Object, error) {
		return b.Put(context.Background(), bucket, to, bytes.NewReader(data), int64(len(data)),
			objectmodel.PutOptions{ContentType: srcObj.ContentType})
	})
	if putErr != nil {
		return putErr
	}
	_ = obj
	if capturedOld != nil {
		if recErr := recordCapturedObjectVersion(bucketPath, bucket, to, capturedOld); recErr != nil {
			return objectmodel.ErrInternalError("error recording object version.")
		}
	}
	return nil
}

// batchDelete is the shared delete-item tail: If-Match against the
// current ETag, then the deleteObjectVersioned marker/plain dispatch and
// the plain delete — the deleteObjectHandler order.
func batchDelete(bucket, key, ifMatch string) error {
	// The JSON batch reports a missing key as a per-item NoSuchKey error
	// (Contract 9's response shape) — a deliberate divergence from the
	// S3 DeleteObjects wire, where a missing key still reports Deleted.
	// The Stat also carries the If-Match check (one read).
	obj, statErr := backendCall(bucket, func(b backend.Backend) (objectmodel.Object, error) {
		return b.Stat(context.Background(), bucket, key)
	})
	if statErr != nil {
		return statErr
	}
	if ifMatch != "" && !etagMatches(ifMatch, obj.ETag) {
		return objectmodel.ErrPreconditionFailed()
	}
	bucketPath := getBucketPath(bucket)
	suppress, markerErr := deleteObjectVersionedMarker(bucketPath, bucket, key)
	if markerErr != nil {
		return markerErr
	}
	if suppress {
		return nil
	}
	return deleteObjectCore(bucketPath, bucket, key)
}
