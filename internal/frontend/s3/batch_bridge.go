// batch_bridge.go — quic-h3-2026-10 leaf 07: the production bridge the
// webdav frontend drives for the JSON batch surface (POST ?batch),
// following leaf 05/06's bridge pattern (versioning_bridge.go,
// zfssurface_bridge.go): the manifest parsing, validation, ordered
// execution, per-item result mapping, and the JSON wire envelope stay in
// ONE place — the batchops core plus this s3-side bridge — so the wire
// semantics are NEVER duplicated across frontends. The webdav frontend
// (webdav/batch.go) calls HandleBatchForBucket with its own
// bucket-path resolver; the h3 frontend needs NO code (it wraps the
// webdav handler, so ?batch is served over QUIC unchanged).
//
// The Executor the core drives is the SAME s3BatchExecutor the s3
// frontend's own ?batch endpoint uses — each batch item therefore runs
// the identical single-op orchestration (Backend Get/Put/Delete, leaf-05
// versioning capture, If-Match enforcement) regardless of which frontend
// mounted the surface.
package s3

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/batchops"
)

// BatchExecutorForBucket returns the batchops.Executor wired to bucket's
// single-op orchestration (exported for the webdav twin, mirroring the
// versioning/zfssurface bridge exports). With no resolved path the
// executor resolves each item's bucket root through getBucketPath — the
// unwired-seam shape; a caller holding its own resolved root uses
// BatchExecutorForBucketAt.
func BatchExecutorForBucket(bucket string, ctx context.Context) batchops.Executor {
	return BatchExecutorForBucketAt(bucket, "", ctx)
}

// BatchExecutorForBucketAt is BatchExecutorForBucket with the mounting
// frontend's own resolved bucket root carried onto every item, so the
// executor's path-rooted steps (versioning store, tag store, capture/
// record, action context) run under the same root the caller resolved.
// An empty bucketPath behaves exactly like BatchExecutorForBucket.
func BatchExecutorForBucketAt(bucket, bucketPath string, ctx context.Context) batchops.Executor {
	return s3BatchExecutor{bucket: bucket, bucketPath: bucketPath, ctx: ctx}
}

// HandleBatchForBucket is the webdav entry for POST <bucket>?batch: the
// SAME pipeline the s3 ?batch endpoint runs (bucket validation, manifest
// processing through the batchops core with the shared key validator,
// the JSON envelope with Content-Type application/json). Wire parity
// with the s3 surface is structural — both frontends run this one code.
//
// bucketPath is the MOUNTING FRONTEND's own resolved on-disk root for
// bucketName — webdav's f.bucketPath (WithBucketPathResolver or the
// lockRoot fallback), NOT this package's getBucketPath. It is threaded
// onto the executor so the path the mount validated and authorized
// against is the path every item reads and writes under; the two
// resolvers can disagree (webdav in mode A resolves through its own
// resolver, production wires them to the same getBucketPath function but
// nothing enforces that), and a dead parameter would leave that
// coincidence load-bearing. An EMPTY bucketPath is the documented unwired
// seam (webdav refuses the surface before reaching here when its resolver
// is nil) and falls back to getBucketPath — the s3 mount's own shape.
//
// validate is the mounting frontend's key validator (the s3
// validateObjectKey — the same rules single ops enforce); when nil the
// batchops core's packaged DefaultKeyValidator (the same pinned rules)
// applies.
func HandleBatchForBucket(w http.ResponseWriter, r *http.Request, bucketName, bucketPath string, validate func(string) error) {
	// Bucket validation, same precedence as the events bridge: 404 over
	// anything else.
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

	runner := &batchops.Runner{
		Exec:        s3BatchExecutor{bucket: bucketName, bucketPath: bucketPath, ctx: r.Context()},
		ValidateKey: validate,
	}
	resp, reqErr := runner.Process(r.Context(), body)
	if reqErr != nil {
		// Malformed manifest: NOTHING executed.
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

// ValidateObjectKey is the EXPORTED key validator every mounting frontend
// passes to HandleBatchForBucket as its ValidateKey (webdav/batch.go does).
// It lives in keyvalidate.go — real production API, not a test-surface
// alias: adding a //go:build test constraint to the export_*_test_surface
// files must never be able to break the webdav batch mount.

// Compile-time guard: the executor satisfies the core's op interface.
var _ batchops.Executor = s3BatchExecutor{}

// ---------- identity context seam (cross-frontend pins) ----------

// IdentityOfRequestForTest exposes identityOf, the s3 frontend's read path
// for the shared identity context key, so a cross-package test can prove
// both frontends read ONE definition (the pin that fails if either side
// reintroduces a private key type).
func IdentityOfRequestForTest(r *http.Request) auth.Identity { return identityOf(r) }

// BatchPrincipalForTest exposes batchPrincipal, the batch executor's
// principal resolution — the reader that produced owner='unauthenticated'
// for a batch arriving over webdav/h3 while the s3 mount stamped the real
// requester.
func BatchPrincipalForTest(ctx context.Context) string { return batchPrincipal(ctx) }
