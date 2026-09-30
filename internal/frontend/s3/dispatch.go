// dispatch.go — request dispatch: the former rootHandler pipeline
// (main.go), extracted into the s3 frontend with identical routing.
//
// V1 seam shape (documented per backend-interface master Contract 4):
// multipart part staging remains direct-fs orchestration above the seam.
// The handler files resolve the bucket's fs root via backendRootFor — a
// thin indirection over package main's backendFor seam installed by
// installFSRootResolver at wiring time. Leaf 03's registry wiring passes
// the resolver; the zero value keeps the legacy package-main lookup so
// tests and the default mount work unchanged.
package s3

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// Handlers call getBucketPath for the above-seam multipart staging paths;
// the hook and its fallback live in seam.go.

// backendRootFor resolves the bucket's fs root, preferring the injected
// resolver and falling back to the config-view layout math when no
// resolver is installed.
func backendRootFor(bucket string) string {
	return getBucketPath(bucket)
}

// serveHTTP is the former rootHandler: parse path → authenticate →
// route by method+query. Order of checks preserved exactly.
func (f *Frontend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Basic path parsing to differentiate between service-level and bucket-level requests
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	bucketName := ""
	objectName := ""

	if len(pathParts) >= 1 && pathParts[0] != "" {
		bucketName = pathParts[0]
	}
	if len(pathParts) >= 2 {
		objectName = strings.Join(pathParts[1:], "/")
	}

	log.Printf("Request: %s %s, Bucket: '%s', Object: '%s'", strconv.Quote(r.Method), strconv.Quote(r.URL.Path), strconv.Quote(bucketName), strconv.Quote(objectName))

	// Authenticate request: presigned query auth when X-Amz-* params present
	// and no Authorization header; header auth wins otherwise (leaf 3.2).
	// Zero-auth dev mode (auth.mode "none", pluggable-auth tree leaf 05)
	// short-circuits both wire forms through the loud DevAuthenticator.
	var identity auth.Identity
	if dev := devAuthenticatorFor(); dev != nil {
		id, err := dev.Authenticate(r)
		if err != nil {
			log.Printf("Authentication Error: dev authenticator rejected request: %v", err)
			writeAuthFailure(w, authFailureError{"AccessDenied", "Access Denied", httpStatusForbidden})
			return
		}
		identity = id
	} else if isPresignedRequest(r) {
		id, failure, ok := f.authenticatePresignedRequest(r)
		if !ok {
			// authenticatePresignedRequest does not write; render here.
			if failure != nil {
				writeAuthFailure(w, *failure)
			}
			return
		}
		identity = id
	} else if id, failure, ok := f.authenticateRequest(r); !ok {
		// authenticateRequest failed; render the identical S3 error bytes.
		if failure != nil {
			writeAuthFailure(w, *failure)
		}
		return
	} else {
		identity = id
	}

	// Authorization (pluggable-authentication tree leaf 02): enforce the
	// identity's bucket grants AFTER authentication, BEFORE the ACL stub
	// and any handler. The grant decision itself lives in internal/auth
	// (Identity.CanRead/CanWrite) via the shared frontend.AuthorizeRequest
	// helper; this site only renders the S3 wire error.
	if !authorizeS3Request(w, identity, bucketName, r) {
		return
	}

	// ACL specific handling (stubbed)
	if _, aclPresent := r.URL.Query()["acl"]; aclPresent {
		handleACL(w, r, bucketName, objectName)
		return
	}

	switch {
	case bucketName == "":
		f.serviceLevelDispatch(w, r, identity)
	case objectName == "":
		f.bucketLevelDispatch(w, r, bucketName)
	default:
		f.objectLevelDispatch(w, r, bucketName, objectName)
	}
}

// authorizeS3Request enforces identity grants for bucket/object requests.
// Returns true when the request may proceed; false when an AccessDenied S3
// error was written. Service-level requests (bucket == "") are not denied
// here — ListBuckets filters to granted buckets in serviceLevelDispatch
// (a scoped identity sees its buckets, never a 403 that would leak nothing
// anyway but break legitimate clients).
func authorizeS3Request(w http.ResponseWriter, id auth.Identity, bucket string, r *http.Request) bool {
	if bucket == "" {
		return true
	}
	write := requestWritesBucket(r)
	if err := frontend.AuthorizeRequest(id, bucket, write); err != nil {
		log.Printf("Authorization Denied: identity %s, bucket %s, write=%v", strconv.Quote(id.AccessKeyID), strconv.Quote(bucket), write)
		writeAuthFailure(w, authFailureError{"AccessDenied", "Access Denied", httpStatusForbidden})
		return false
	}
	return true
}

// requestWritesBucket classifies a bucket-level or object-level request as
// a write (PUT/POST/DELETE) vs a read (GET/HEAD/OPTIONS). One function, one
// place — the classification the grant check consumes (leaf 02: create/
// delete bucket, versioning, actions, put, copy, multipart
// initiate/upload/complete/abort, delete batch are all writes; GET/HEAD
// are reads).
func requestWritesBucket(r *http.Request) bool {
	switch r.Method {
	case "PUT", "POST", "DELETE":
		return true
	default:
		return false
	}
}

// serviceLevelDispatch routes service-level (no bucket) requests.
// ListBuckets filters to the identity's granted buckets when the identity
// is not wildcard — a scoped identity sees only what it may read.
func (f *Frontend) serviceLevelDispatch(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	switch r.Method {
	case "GET":
		listBucketsHandler(w, r, grantedBucketFilter(id))
	default:
		http.Error(w, "Method Not Allowed at service level", http.StatusMethodNotAllowed)
	}
}

// grantedBucketFilter returns nil when the identity holds the "*" wildcard
// (no filtering — today's byte-identical behavior); otherwise the set of
// buckets the identity may read.
func grantedBucketFilter(id auth.Identity) map[string]bool {
	if id.BucketGrants == nil {
		return nil
	}
	if g, ok := id.BucketGrants["*"]; ok && g.Read {
		return nil
	}
	filter := make(map[string]bool, len(id.BucketGrants))
	for bucket, g := range id.BucketGrants {
		if bucket != "*" && g.Read {
			filter[bucket] = true
		}
	}
	return filter
}

// bucketLevelDispatch routes bucket-level requests (bucket set, no object),
// including the ?location and ?list-type=2 sub-resources.
func (f *Frontend) bucketLevelDispatch(w http.ResponseWriter, r *http.Request, bucketName string) {
	// Check if location parameter is present for GetBucketLocation
	if _, ok := r.URL.Query()["location"]; ok && r.Method == "GET" {
		getBucketLocationHandler(w, r, bucketName)
		return
	}
	// Check if list-type=2 parameter is present for ListObjectsV2
	if val, ok := r.URL.Query()["list-type"]; ok && val[0] == "2" && r.Method == "GET" {
		listObjectsV2Handler(w, r, bucketName)
		return
	}
	// Leaf 3.3: ListMultipartUploads sub-resource
	if _, ok := r.URL.Query()["uploads"]; ok && r.Method == "GET" {
		listMultipartUploadsHandler(w, r, bucketName)
		return
	}
	// metadata-zfs leaf 04: capability endpoints — bucket event summary
	// (?events) and the derived version-listing extension
	// (?events&versions). MUST sit above the plain ?versions check below
	// so the combined query resolves to the events extension, never the
	// standard unversioned listing.
	if _, ok := r.URL.Query()["events"]; ok && r.Method == "GET" {
		handleBucketEvents(w, r, bucketName)
		return
	}
	// Leaf 5.1 [a]-2: ListObjectVersions sub-resource (GET /bucket?versions).
	// Unversioned wire shape: every object = one version with the null ID.
	if _, ok := r.URL.Query()["versions"]; ok && r.Method == "GET" {
		listObjectVersionsHandler(w, r, bucketName)
		return
	}
	// Leaf 3.5: DeleteObjects sub-resource (POST /bucket?delete)
	if _, ok := r.URL.Query()["delete"]; ok && r.Method == "POST" {
		deleteObjectsHandler(w, r, bucketName)
		return
	}

	switch r.Method {
	case "PUT":
		createBucketHandler(w, r, bucketName)
	case "GET": // This would be ListObjectsV1 or GetBucketACL etc.
		// For now, assume ListObjectsV2 is the primary way to list.
		// If no specific query params for listing, could be GetBucketACL or other bucket specific GETs.
		// We'll default to a simple "Not Implemented" or treat as ListObjectsV2 if query params match.
		listObjectsV2Handler(w, r, bucketName) // Or a more specific handler based on query params
	case "DELETE":
		deleteBucketHandler(w, r, bucketName)
	case "HEAD":
		headBucketHandler(w, r, bucketName)
	default:
		http.Error(w, "Method Not Allowed for bucket", http.StatusMethodNotAllowed)
	}
}

// objectLevelDispatch routes object-level requests, including the multipart
// sub-resources (?uploads, ?partNumber, ?uploadId).
func (f *Frontend) objectLevelDispatch(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	// Check for multipart upload query parameters
	if _, ok := r.URL.Query()["uploads"]; ok && r.Method == "POST" {
		initiateMultipartUploadHandler(w, r, bucketName, objectName)
		return
	}
	if partNumber, ok := r.URL.Query()["partNumber"]; ok && r.Method == "PUT" {
		if uploadID, ok := r.URL.Query()["uploadId"]; ok {
			uploadPartHandler(w, r, bucketName, objectName, partNumber[0], uploadID[0])
			return
		}
	}
	if uploadID, ok := r.URL.Query()["uploadId"]; ok && r.Method == "POST" {
		completeMultipartUploadHandler(w, r, bucketName, objectName, uploadID[0])
		return
	}
	if uploadID, ok := r.URL.Query()["uploadId"]; ok && r.Method == "DELETE" {
		abortMultipartUploadHandler(w, r, bucketName, objectName, uploadID[0])
		return
	}
	// Leaf 3.3: ListParts sub-resource
	if uploadID, ok := r.URL.Query()["uploadId"]; ok && r.Method == "GET" {
		listPartsHandler(w, r, bucketName, objectName, uploadID[0])
		return
	}

	// metadata-zfs leaf 04: capability endpoint — object event history
	// (?events). Placed with the other sub-resource checks, before the
	// method switch.
	if _, ok := r.URL.Query()["events"]; ok && r.Method == "GET" {
		handleObjectEvents(w, r, bucketName, objectName)
		return
	}

	switch r.Method {
	case "PUT":
		putObjectHandler(w, r, bucketName, objectName)
	case "GET":
		getObjectHandler(w, r, bucketName, objectName)
	case "DELETE":
		deleteObjectHandler(w, r, bucketName, objectName)
	case "HEAD":
		headObjectHandler(w, r, bucketName, objectName)
	default:
		http.Error(w, "Method Not Allowed for object", http.StatusMethodNotAllowed)
	}
}
