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
	if isPresignedRequest(r) {
		if !f.authenticatePresigned(w, r) {
			// authenticatePresigned writes the error response on failure.
			return
		}
	} else if _, failure, ok := f.authenticateRequest(r); !ok {
		// authenticateRequest failed; render the identical S3 error bytes.
		if failure != nil {
			writeAuthFailure(w, *failure)
		}
		return
	}

	// ACL specific handling (stubbed)
	if _, aclPresent := r.URL.Query()["acl"]; aclPresent {
		handleACL(w, r, bucketName, objectName)
		return
	}

	switch {
	case bucketName == "":
		f.serviceLevelDispatch(w, r)
	case objectName == "":
		f.bucketLevelDispatch(w, r, bucketName)
	default:
		f.objectLevelDispatch(w, r, bucketName, objectName)
	}
}

// serviceLevelDispatch routes service-level (no bucket) requests.
func (f *Frontend) serviceLevelDispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		listBucketsHandler(w, r)
	default:
		http.Error(w, "Method Not Allowed at service level", http.StatusMethodNotAllowed)
	}
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
