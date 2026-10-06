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
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/batchops"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// bucketBatchMaxBytes bounds the manifest body read (1000 ops × generous
// per-op size) so a runaway body cannot buffer unboundedly.
const bucketBatchMaxBytes = 4 << 20

// batchCopyMaxBytesDefault is the DEFAULT ceiling for the source bytes
// ONE copy item buffers in memory: the Backend seam's Put takes a whole
// []byte, so a batch copy cannot stream. 5 GiB is S3's own single-PUT
// object limit — the same bound fsbackend's maxPutBytesDefault caps a Put
// at (fsbackend.go) — so a copy of a larger object could never have been
// written by a single PutObject either and nothing S3-reachable is
// rejected by this gate.
//
// It is declared HERE rather than imported from fsbackend because it gates
// a READ on the pre-seam path, where the backend's own cap does not
// apply: the manifest body cap (bucketBatchMaxBytes) bounds the request,
// never the bytes a manifest makes the server copy, and an unbounded read
// let an ifMatch copy item buffer an arbitrarily large source per item
// under a manifest the validator accepts.
const batchCopyMaxBytesDefault int64 = 5 << 30

// batchCopyMaxBytes is the live copy ceiling. A var rather than a const
// so a test can shrink it to a byte budget and exercise the overflow arm
// for real — the exact shape fsbackend uses for its own configurable
// maxPutBytes (fsbackend.go's FS.maxPutBytes, default
// maxPutBytesDefault). Production never writes it; only a test does, and
// only for its own duration.
var batchCopyMaxBytes = batchCopyMaxBytesDefault

// handleBucketBatch implements POST /{bucket}?batch. Mounted in
// dispatch.go's bucketLevelDispatch beside the ?delete sub-resource.
//
// Bucket validation runs FIRST and matches HandleBatchForBucket (the
// webdav/h3 bridge) EXACTLY: validBucket || !bucketExists → 404
// NoSuchBucket. Without the name gate a traversal-shaped bucket name
// reached bucketExists (and the executor's bucket path) on this surface
// only — the two ?batch mounts disagreed (bughunt M3).
func handleBucketBatch(w http.ResponseWriter, r *http.Request, bucketName string) {
	if !validBucket(bucketName) || !bucketExists(bucketName) {
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
	// bucketPath is the RESOLVED on-disk root every bucket-scoped step of
	// an item uses (the versioning store, the tag store, the version
	// capture/record). Empty means "the mounting frontend wired no
	// resolver" — the s3 ?batch mount's own shape, which is exactly the
	// unwired-seam case resolveBucketPath falls back to getBucketPath.
	//
	// Why it is carried at all: the two ?batch mounts resolve the bucket
	// through DIFFERENT resolvers (the s3 mount has no resolver argument
	// and lands on getBucketPath; the webdav/h3 mount hands over its own
	// f.bucketPath). Threading the mounting frontend's answer onto the
	// executor is what makes validation and execution non-divergent BY
	// CONSTRUCTION: the path the mount validated and authorized against is
	// the path the read and write land under. Before this field existed the
	// bucketPath argument was dead and both mounts silently executed
	// through getBucketPath — coincidentally equal in production, by
	// divergence in any wiring where the two resolvers disagree.
	bucketPath string
	ctx        context.Context
}

// resolveBucketPath returns the on-disk root an item's bucket-scoped steps
// run against: the caller-resolved path when the mounting frontend supplied
// one, else getBucketPath (the s3 mount's shape, and the documented unwired
// seam — a nil resolver has no path of its own to offer).
func (e s3BatchExecutor) resolveBucketPath() string {
	if e.bucketPath != "" {
		return e.bucketPath
	}
	return getBucketPath(e.bucket)
}

// itemCtx resolves the context an item runs under: the context the
// batchops core passes per item is the caller's (the request context), so
// a client disconnect cancels the items still to come instead of letting
// the whole manifest run on a fresh Background. The stored ctx is the
// fallback for a caller that passes none (BatchExecutorForBucket's
// construction shape). Never nil.
func (e s3BatchExecutor) itemCtx(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

// batchPrincipal resolves the authenticated principal for the item's
// context: the AccessKeyID serveHTTP published into the request context
// under auth's SHARED key (the same seam principalOfRequest reads), so a
// batch-written object carries the forensic attribution breadcrumbs
// (user.zeta.owner / user.zeta.writer.*) exactly like a PutObject/
// CopyObject write — on EVERY mount that drives this executor, webdav and
// h3 included. A request that bypassed dispatch degrades to the same
// legacy wildcard principal identityOf synthesizes — byte-identical to the
// single-op path.
func batchPrincipal(ctx context.Context) string {
	if id, ok := auth.IdentityFromContext(ctx); ok {
		return id.AccessKeyID
	}
	return auth.WildcardIdentity("unauthenticated").AccessKeyID
}

// batchCopy is the copy item: a streamed Get→Put through the Backend
// seam (the copyObjectHandler data path, minus the protocol headers it
// serves), with versioning capture on an overwrite (the leaf-05
// machinery — capture BEFORE the put, record AFTER) and the If-Match
// precondition enforced against the source's current ETag.
func (e s3BatchExecutor) Copy(ctx context.Context, from, to, ifMatch string) error {
	if err := validateObjectKey(from); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	if err := validateObjectKey(to); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	return batchCopyMove(e.itemCtx(ctx), e.bucket, e.resolveBucketPath(), from, to, ifMatch, false)
}

// batchMove is the move item: copy then DELETE the source (MOVE
// semantics — no source copy left; S3's move equivalent is CopyObject +
// DeleteObject). The source delete runs the same deleteObjectVersioned
// path the DELETE handler runs, so a versioned bucket records exactly
// what that pair records.
func (e s3BatchExecutor) Move(ctx context.Context, from, to, ifMatch string) error {
	if err := validateObjectKey(from); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	if err := validateObjectKey(to); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	ctx = e.itemCtx(ctx)
	if err := batchCopyMove(ctx, e.bucket, e.resolveBucketPath(), from, to, ifMatch, false); err != nil {
		return err
	}
	return batchDelete(ctx, e.bucket, e.resolveBucketPath(), from, ifMatch)
}

// batchDelete is the delete item: the SAME path deleteObjectCore runs
// (versioned marker on an Enabled bucket, plain delete otherwise), plus
// the If-Match precondition against the object's current ETag.
func (e s3BatchExecutor) Delete(ctx context.Context, key, ifMatch string) error {
	if err := validateObjectKey(key); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	return batchDelete(e.itemCtx(ctx), e.bucket, e.resolveBucketPath(), key, ifMatch)
}

// batchCopyMetadata reads the source object's user metadata verbatim
// (original x-amz-meta-* key casing, the raw sidecar form fsbackend's
// prefixedMetadata passes through unchanged) — the COPY-directive
// metadata the single-op CopyObject preserves. A missing or unparsable
// sidecar yields an empty map (the copy proceeds metaless), mirroring
// rawSourceSidecarMeta's own leniency.
func batchCopyMetadata(bucketPath, key string) map[string]string {
	meta := map[string]string{}
	maps.Copy(meta, rawSourceSidecarMeta(bucketPath, key))
	return meta
}

// batchCopyTags resolves the destination tag set the way CopyObject's
// COPY directive does: the SOURCE tags verbatim (the JSON batch manifest
// carries no tagging directive, so COPY is the only shape). The source
// was already proven to exist by the data Get, so a store error here is a
// real I/O failure and is surfaced — except the attribute-absent class,
// which simply means the source is untagged and yields an empty set (the
// caller's len()>0 write gate then writes nothing).
func batchCopyTags(bucketPath, key string) (map[string]string, error) {
	tags, err := tagStoreFor(bucketPath).Get(key)
	if err != nil {
		if isNoSuchKeyErr(err) {
			return map[string]string{}, nil // no sidecar: untagged source
		}
		return nil, err
	}
	return tags, nil
}

// batchCopyMove streams from → to through the Backend seam with the
// leaf-05 versioning capture (identical capture/record order to the s3
// PUT handler). isDeleteSource is unused today; the move = copy+delete
// split keeps each half reviewable against its single-op handler.
//
// ctx is the CALLER's context (the per-item context the batchops core
// hands the executor): every backend call takes it, so a client
// disconnect aborts the item mid-flight instead of running on a fresh
// Background (bughunt M3).
//
// bucketPath is the executor's RESOLVED bucket root (s3BatchExecutor.
// resolveBucketPath): the caller's answer when a mounting frontend
// resolved it, else getBucketPath. It governs every path-rooted step
// here — the versioning store, the source metadata/tag read, the
// destination tag write, the capture/record pair, and the action context
// — so the item's reads and writes land under exactly the path the mount
// validated and authorized against.
func batchCopyMove(ctx context.Context, bucket, bucketPath, from, to, ifMatch string, _ bool) error {
	// ONE source read serves both the If-Match precheck and the data
	// copy, and the reader is closed on EVERY exit below — the precheck
	// used to open a second reader and discard it into `_`, leaking one
	// descriptor per ifMatch item (bughunt H4).
	rc, srcObj, err := backendCallBucket2(bucket, func(b backend.Backend) (io.ReadCloser, objectmodel.Object, error) {
		return b.Get(ctx, bucket, from, objectmodel.GetOptions{})
	})
	if err != nil {
		return err
	}
	// The read is BOUNDED. This copy is buffered whole (the Backend seam
	// Put takes a []byte), and the single-read shape above is what closed
	// the H4 leak — so the size gate has to live here, on the read, or an
	// unbounded io.ReadAll lets a manifest of ifMatch copy items buffer
	// source-after-source into the process heap under a manifest the
	// validator accepts (bucketBatchMaxBytes caps the MANIFEST, never the
	// copied bytes). LimitReader at limit+1 means "one byte past the
	// ceiling" is detectable, so an oversized source is refused with the
	// SAME InvalidArgument the fs backend's Put size cap reports
	// (maxPutBytesDefault) instead of being buffered first.
	data, readErr := io.ReadAll(io.LimitReader(rc, batchCopyMaxBytes+1))
	rc.Close()
	if readErr != nil {
		return readErr
	}
	if int64(len(data)) > batchCopyMaxBytes {
		log.Printf("Batch copy source %s/%s exceeds the maximum object size %d", strconv.Quote(bucket), strconv.Quote(from), batchCopyMaxBytes)
		return objectmodel.ErrInvalidArgument(fmt.Sprintf("object size exceeds the maximum allowed size %d", batchCopyMaxBytes))
	}

	// If-Match on the SOURCE copy is enforced against the source
	// object's current ETag (the "this still points at what I saw"
	// guarantee the manifest author means) — evaluated AFTER the read,
	// so a stale precondition still copies nothing. Stat and Get report
	// the same sidecar ETag, so the decision is unchanged.
	if ifMatch != "" && !etagMatches(ifMatch, srcObj.ETag) {
		return objectmodel.ErrPreconditionFailed()
	}

	// Versioning capture BEFORE the plain overwrite (leaf-05 order:
	// capture → put → record; ErrNoPriorVersion = create, no record).
	// A state READ FAILURE FAILS CLOSED (bughunt M6), identically to the
	// PutObject gate in object_handlers.go and to the webdav frontend's
	// captureBeforePut: an unreadable versioning state must never become a
	// silent unversioned overwrite, and all three gates must reach the SAME
	// decision for the same bucket state. A never-versioned bucket is
	// unaffected — the store answers Off with a nil error (one missing-file
	// stat), which takes the plain path.
	state, stErr := versionStoreForBucket(bucketPath).State(bucket)
	if stErr != nil {
		return objectmodel.ErrInternalError("error reading bucket versioning state.")
	}
	var capturedOld *capturedObjectVersion
	if state == versioningEnabled {
		captured, capErr := captureCurrentObjectVersion(bucketPath, to)
		if capErr != nil && !errors.Is(capErr, errNoPriorVersion) {
			return objectmodel.ErrInternalError("error capturing object version.")
		}
		capturedOld = captured
	}

	// CopyObject parity (bughunt M1): a batch copy is the CopyObject it
	// claims to mirror. The destination carries the SOURCE's Content-Type,
	// user metadata (raw x-amz-meta-* sidecar keys, verbatim casing) and
	// tag set, and the write is ATTRIBUTED to the requesting principal so
	// fsbackend stamps the user.zeta.owner / user.zeta.writer.* breadcrumbs
	// (the forensic attribution the audit charter exists for). Metadata
	// and tags are resolved BEFORE the write so a failed read copies
	// nothing (S3 rejects a bad directive with nothing written).
	dstMeta := batchCopyMetadata(bucketPath, from)
	dstTags, tagErr := batchCopyTags(bucketPath, from)
	if tagErr != nil {
		return tagErr
	}

	obj, putErr := backendCall(bucket, func(b backend.Backend) (objectmodel.Object, error) {
		return b.Put(ctx, bucket, to, bytes.NewReader(data), int64(len(data)),
			objectmodel.PutOptions{
				ContentType: srcObj.ContentType,
				Metadata:    dstMeta,
				Principal:   batchPrincipal(ctx),
			})
	})
	if putErr != nil {
		return putErr
	}
	// Tags land AFTER the destination object exists (sidecar
	// read-modify-write, exactly like CopyObject and PutObject); an empty
	// source tag set writes nothing (byte-compat sidecar form).
	if len(dstTags) > 0 {
		if storeErr := tagStoreFor(bucketPath).Put(to, dstTags); storeErr != nil {
			return storeErr
		}
	}
	if capturedOld != nil {
		if recErr := recordCapturedObjectVersion(bucketPath, bucket, to, capturedOld); recErr != nil {
			return objectmodel.ErrInternalError("error recording object version.")
		}
	}
	// Bucket-action parity with the single-op CopyObject (whose after_upload
	// fires the same shape off dstDataPath/dstMetaPath, object_handlers.go's
	// copyObjectHandler tail): a batch write is a mutating write, so a
	// per-bucket .bucket-actions hook must see it exactly as it sees the
	// single-op write. Without this the batch surface bypassed
	// putObjectHandler/copyObjectHandler's trigger entirely and every hook
	// silently skipped every batch item. Only the successful path reaches
	// here — a failed item returns above and fires nothing.
	dstDataPath := objectDataPathFor(bucketPath, to)
	dstMetaPath := filepath.Join(bucketPath, ".metadata", to+".meta")
	go triggerActions("after_upload", ActionContext{
		FilePath:     dstDataPath,
		MetadataPath: dstMetaPath,
		BucketName:   bucket,
		BucketPath:   bucketPath,
		ObjectKey:    to,
		ContentType:  srcObj.ContentType,
		ETag:         obj.ETag,
		Size:         obj.Size,
	})
	return nil
}

// batchDelete is the shared delete-item tail: If-Match against the
// current ETag, then the deleteObjectVersioned marker/plain dispatch and
// the plain delete — the deleteObjectHandler order.
//
// bucketPath is the executor's RESOLVED bucket root (s3BatchExecutor.
// resolveBucketPath), threaded for the same reason as batchCopyMove's:
// the marker/delete pair must run under the path the mounting frontend
// validated and authorized against.
//
// Bucket-action parity needs no trigger HERE: the plain delete runs
// through deleteObjectCore, which fires after_delete itself
// (object_handlers.go), and the versioned-marker arm returns before it —
// both arms byte-identical to deleteObjectHandler's own trigger behavior.
func batchDelete(ctx context.Context, bucket, bucketPath, key, ifMatch string) error {
	// The JSON batch reports a missing key as a per-item NoSuchKey error
	// (Contract 9's response shape) — a deliberate divergence from the
	// S3 DeleteObjects wire, where a missing key still reports Deleted.
	// The Stat also carries the If-Match check (one read).
	obj, statErr := backendCall(bucket, func(b backend.Backend) (objectmodel.Object, error) {
		return b.Stat(ctx, bucket, key)
	})
	if statErr != nil {
		return statErr
	}
	if ifMatch != "" && !etagMatches(ifMatch, obj.ETag) {
		return objectmodel.ErrPreconditionFailed()
	}
	suppress, markerErr := deleteObjectVersionedMarker(bucketPath, bucket, key)
	if markerErr != nil {
		return markerErr
	}
	if suppress {
		return nil
	}
	return deleteObjectCore(bucketPath, bucket, key)
}
