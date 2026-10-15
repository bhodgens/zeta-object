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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

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

// batchItemDeletesSource reports whether a manifest item deletes a source
// key. Delete does; move does too (the source is removed). Copy does not.
// An empty From is not a source: the runner rejects that manifest and
// executes nothing, and AuthorizeOp on key "" is the listing shortcut,
// not a mutate grant.
func batchItemDeletesSource(item batchops.Operation) bool {
	return (item.Op == "delete" || item.Op == "move") && item.From != ""
}

// BatchMutatingSourceDenied returns the first delete or move source the
// identity may not delete, and true. A prefix grant that matches the
// empty key is the listing shortcut (GrantExpr.MatchesObject), not a
// mutate-any-key grant, so each source key is checked on its own. denied
// is false when every such source is allowed (or there are none).
func BatchMutatingSourceDenied(id auth.Identity, bucket string, ops []batchops.Operation) (string, bool) {
	now := time.Now().UTC()
	for _, item := range ops {
		if !batchItemDeletesSource(item) {
			continue
		}
		if !auth.AuthorizeOp(id, auth.OpDelete, bucket, item.From, now) {
			return item.From, true
		}
	}
	return "", false
}

// authorizeBatchDeleteSources is the s3 mount's per-item gate, run before
// handleBucketBatch. The coarse AuthorizeOp(OpDelete, bucket, "") check
// is not sufficient: a prefix grant matches key "". This peeks the
// manifest, restores the body, and writes 403 when a delete or move
// source is outside the delete grant — the executor does not run. A
// missing bucket, a malformed or oversize manifest, or an unreadable
// body is not denied here: the handler's own contract answers (404 / 400
// / 500) and executes nothing. Returns false when the response was
// written and the caller must not run the executor.
func authorizeBatchDeleteSources(w http.ResponseWriter, r *http.Request, bucket string) bool {
	if !validBucket(bucket) || !bucketExists(bucket) {
		return true
	}
	if r.Body == nil {
		return true
	}
	body, rerr := io.ReadAll(io.LimitReader(r.Body, bucketBatchMaxBytes+1))
	cerr := r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if rerr != nil || cerr != nil {
		err := rerr
		if err == nil {
			err = cerr
		}
		log.Printf("Error reading batch body for bucket %s: %v", strconv.Quote(bucket), err)
		writeS3Error(w, "InternalError", "Error reading request body.", http.StatusInternalServerError)
		return false
	}
	if len(body) > bucketBatchMaxBytes {
		return true
	}
	var man batchops.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return true
	}
	id := identityOf(r)
	key, denied := BatchMutatingSourceDenied(id, bucket, man.Operations)
	if !denied {
		return true
	}
	log.Printf("Authorization Denied: identity %s, bucket %s, key %s (POST ?batch delete/move source outside delete grant)",
		strconv.Quote(id.AccessKeyID), strconv.Quote(bucket), strconv.Quote(key))
	writeAuthFailure(w, authFailureError{"AccessDenied", "Access Denied", httpStatusForbidden})
	return false
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
