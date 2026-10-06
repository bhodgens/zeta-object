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
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
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
//
// Audit log (auth extensions leaf 10, charter-exception layer): when an
// audit writer is installed, every request that reaches AUTHENTICATION is
// recorded exactly once — after auth and after the authorization decision
// (denials are forensically interesting: denied=true). Best-effort: the
// audit write can never break the request.
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
			recordAudit("", r.Method, bucketName, objectName, string(opForMethod(r.Method, bucketName, objectName)), httpStatusForbidden, true)
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
			recordAudit(presentedAccessKey(r), r.Method, bucketName, objectName, string(opForMethod(r.Method, bucketName, objectName)), authFailureStatus(failure), true)
			return
		}
		identity = id
	} else if id, failure, ok := f.authenticateRequest(r); !ok {
		// authenticateRequest failed; render the identical S3 error bytes.
		if failure != nil {
			writeAuthFailure(w, *failure)
		}
		recordAudit(presentedAccessKey(r), r.Method, bucketName, objectName, string(opForMethod(r.Method, bucketName, objectName)), authFailureStatus(failure), true)
		return
	} else {
		identity = id
	}

	// Authorization (pluggable-authentication tree leaf 02): enforce the
	// identity's bucket grants AFTER authentication, BEFORE the ACL stub
	// and any handler. The grant decision itself lives in internal/auth
	// (AuthorizeOp — leaf 09's single rich-grant decision) via the
	// method+key adapter in authorizeS3Request; this site only renders the
	// S3 wire error.
	if !authorizeS3Request(w, identity, bucketName, objectName, r) {
		recordAudit(identity.AccessKeyID, r.Method, bucketName, objectName, string(opForMethod(r.Method, bucketName, objectName)), httpStatusForbidden, true)
		return
	}

	// Authenticated + authorized: wrap the writer so the dispatched
	// handler's response status lands in the audit record, then route.
	aw := auditWriterFor()
	if aw == nil {
		f.routeAuthorized(w, r, identity, bucketName, objectName)
		return
	}
	rec := &statusRecorder{ResponseWriter: w, status: 0}
	f.routeAuthorized(rec, r, identity, bucketName, objectName)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	recordAudit(identity.AccessKeyID, r.Method, bucketName, objectName, string(opForMethod(r.Method, bucketName, objectName)), rec.status, false)
}

// routeAuthorized is the post-auth/post-authz dispatch switch (extracted
// so the audit wrapping in serveHTTP stays one branch): ACL stub, then the
// service/bucket/object level routing. Identical order to the pre-audit
// pipeline.
func (f *Frontend) routeAuthorized(w http.ResponseWriter, r *http.Request, identity auth.Identity, bucketName, objectName string) {
	// Publish the authenticated identity into the request context so
	// handlers needing a grant check BEYOND the URL-path bucket can reach
	// it (bughunt S1: CopyObject's source bucket). The dispatch switch does
	// not thread the identity as a parameter, so the context is the seam.
	r = r.WithContext(withAuthenticatedIdentity(r.Context(), identity))

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

// presentedAccessKey extracts the access key ID the client PRESENTED (from
// the Authorization header's Credential scope, or the presigned
// X-Amz-Credential) for audit attribution of AUTH failures. Unknown or
// unparseable keys are still recorded (as presented) — forensic value;
// the value is log-safe by the same rule the dispatch log uses.
func presentedAccessKey(r *http.Request) string {
	if cred := r.URL.Query().Get("X-Amz-Credential"); cred != "" {
		if key, _, found := strings.Cut(cred, "/"); found {
			return key
		}
		return cred
	}
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		if matches := authHeaderRegexTolerant.FindStringSubmatch(authHeader); len(matches) == 6 {
			return matches[1]
		}
	}
	return ""
}

// authFailureStatus maps an auth failure to its wire status for the audit
// record (0/failure = generic forbidden).
func authFailureStatus(failure *authFailureError) int {
	if failure != nil {
		return failure.status
	}
	return httpStatusForbidden
}

// withAuthenticatedIdentity stores id in ctx under auth's SHARED identity
// key (auth.WithIdentity) — the one definition every frontend publishes
// and reads. The private key type this package used to own
// (authenticatedIdentityContextKey) let webdav and h3 publish identities
// that this package's readers — the shared batch executor's principal
// resolution first among them — could not see, so a ?batch arriving over
// those mounts stamped owner='unauthenticated' instead of the requester.
func withAuthenticatedIdentity(ctx context.Context, id auth.Identity) context.Context {
	return auth.WithIdentity(ctx, id)
}

// identityOf returns the authenticated identity carried in the request
// context (set by serveHTTP after authentication). When no identity was
// stashed — direct handler invocation in unit tests, or any path that
// bypassed serveHTTP — the legacy wildcard identity is returned: those
// callers historically ran with unrestricted access, so synthesizing the
// wildcard principal keeps their behavior byte-identical (the same
// synthesis the legacy CredentialSource fallback performs in
// credentialSecret). The context-carrying path (real dispatch) always
// holds a scoped identity, so the CopyObject source-grant check (S1) is
// live on the wire.
func identityOf(r *http.Request) auth.Identity {
	if id, ok := auth.IdentityFromContext(r.Context()); ok {
		return id
	}
	return auth.WildcardIdentity("unauthenticated")
}

// authorizeS3Request enforces identity grants for bucket/object requests.
// Returns true when the request may proceed; false when an AccessDenied S3
// error was written. Service-level requests (bucket == "") are not denied
// here — ListBuckets filters to granted buckets in serviceLevelDispatch
// (a scoped identity sees its buckets, never a 403 that would leak nothing
// anyway but break legitimate clients).
//
// Leaf 09: the decision is auth.AuthorizeOp — the method+object key are
// passed through so prefix-scoped rich grants apply per object. The method
// → Op mapping below is THE S3 adapter table (read/write only; list/
// delete/create refine bucket-level semantics in the rich table itself,
// where e.g. a list/delete/create-only entry still satisfies the v1
// floor-mapped method check through the shared authorizeOpForMethod
// classification).
func authorizeS3Request(w http.ResponseWriter, id auth.Identity, bucket, key string, r *http.Request) bool {
	if bucket == "" {
		return true
	}
	op := opForMethod(r.Method, bucket, key)
	if !auth.AuthorizeOp(id, op, bucket, key, time.Now().UTC()) {
		log.Printf("Authorization Denied: identity %s, bucket %s, key %s, method %s",
			strconv.Quote(id.AccessKeyID), strconv.Quote(bucket), strconv.Quote(key), strconv.Quote(r.Method))
		writeAuthFailure(w, authFailureError{"AccessDenied", "Access Denied", httpStatusForbidden})
		return false
	}
	return true
}

// opForMethod maps the S3 method (+ level) onto the grant Op vocabulary —
// the one adapter table for the S3 frontend. GET/HEAD are reads (object
// data or bucket sub-resources); PUT/POST to an OBJECT path are writes;
// PUT/POST to the BUCKET root is create (CreateBucket and bucket
// sub-resource writes); DELETE of an OBJECT is delete; DELETE of a bucket
// root maps to write (delete bucket sits with the v1 write family — v1
// grants had no separate bit, and a delete-only rich entry still denies
// the root through its own op set). POST with object key = multipart
// initiate/completion = write.
func opForMethod(method, bucket, key string) auth.Op {
	switch method {
	case "GET", "HEAD":
		return auth.OpRead
	case "PUT", "POST":
		if key == "" {
			return auth.OpCreate
		}
		return auth.OpWrite
	case "DELETE":
		if key == "" {
			return auth.OpDelete
		}
		return auth.OpDelete
	default:
		// OPTIONS and anything else: least-privilege read.
		return auth.OpRead
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
	// s3-versioning leaf 03: the ?versioning sub-resource —
	// PUT SetBucketVersioning / GET GetBucketVersioning (Contract 2).
	if _, ok := r.URL.Query()["versioning"]; ok {
		switch r.Method {
		case "PUT":
			putBucketVersioningHandler(w, r, bucketName)
			return
		case "GET":
			getBucketVersioningHandler(w, r, bucketName)
			return
		}
	}
	// Leaf 3.5: DeleteObjects sub-resource (POST /bucket?delete) and the
	// quic-h3-2026-10 leaf 07 JSON batch extension (POST /bucket?batch).
	// Both are batch surfaces over the shared internal/batchops core.
	if _, ok := r.URL.Query()["delete"]; ok && r.Method == "POST" {
		deleteObjectsHandler(w, r, bucketName)
		return
	}
	if _, ok := r.URL.Query()["batch"]; ok {
		if r.Method == "POST" {
			handleBucketBatch(w, r, bucketName)
			return
		}
		// GET/PUT/DELETE ?batch: the sub-resource is POST-only — a 405,
		// never a silent fall-through into the plain method switch.
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed for bucket", http.StatusMethodNotAllowed)
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

	// Object tagging (tagging tree leaf 03): the ?tagging sub-resource,
	// GET/PUT/DELETE — GetObjectTagging / PutObjectTagging /
	// DeleteObjectTagging.
	if _, ok := r.URL.Query()["tagging"]; ok {
		switch r.Method {
		case "GET":
			getObjectTaggingHandler(w, r, bucketName, objectName)
			return
		case "PUT":
			putObjectTaggingHandler(w, r, bucketName, objectName)
			return
		case "DELETE":
			deleteObjectTaggingHandler(w, r, bucketName, objectName)
			return
		}
	}

	// s3-versioning leaf 03: the ?versionId sub-resource — a versioned
	// read (GET/HEAD, Contract 2). Sits above the plain method switch.
	// The body lives in dispatchObjectVersionId (gocyclo: keep this
	// dispatch under the complexity gate).
	if objectVersionedDispatch(w, r, bucketName, objectName) {
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
