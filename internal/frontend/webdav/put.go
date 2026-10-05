// put.go — PUT (leaf 03 Task 2): create (201) / overwrite (204) with the
// ETag response header, ETag conditionals through PutOptions, streaming
// bodies with the declared Content-Length. Chunked bodies without a length
// are rejected with 400 rather than buffered unboundedly (leaf 03 pin).
// Conditionals are ALSO enforced here at the frontend (W2): the fs backend
// ignores PutOptions.IfMatch/IfNoneMatch in v1, so forwarding alone would
// let a stale-If-Match PUT clobber the current bytes.
package webdav

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// handlePUT implements PUT for files. PUT to a collection URL (trailing
// slash) is 405; the parent prefix being absent is NOT an error (S3
// semantics).
func (f *Frontend) handlePUT(w http.ResponseWriter, r *http.Request, res resource) {
	if res.isRoot || res.isCollection {
		w.Header().Set("Allow", allowHeader)
		writeDavError(w, http.StatusMethodNotAllowed, "")
		return
	}
	existed := false
	var curETag string
	if obj, isFile, err := statFile(r.Context(), f.be, res.bucket, res.key); err != nil {
		writeDavErrorFrom(w, err)
		return
	} else if isFile {
		existed = true
		curETag = obj.ETag
	}

	// W2: enforce the PUT conditionals HERE, against the Stat snapshot
	// this handler already fetched. The headers are still forwarded in
	// PutOptions below so a backend that gains conditional support
	// enforces them at the seam too.
	//
	// Residual race (W3, parent-graded MED — do not fix here): a
	// concurrent writer can change the object between this Stat and the
	// Put below; the backend has no compare-and-swap in v1, so the
	// stat-then-Put window cannot be closed at this layer. The common
	// non-racing path (stale If-Match on a quiescent object) is fully
	// guarded.
	if !putConditionalOK(r.Header.Get("If-Match"), r.Header.Get("If-None-Match"), existed, curETag) {
		writeDavError(w, http.StatusPreconditionFailed, "")
		return
	}

	opts := objectmodel.PutOptions{
		ContentType: r.Header.Get("Content-Type"),
		IfMatch:     r.Header.Get("If-Match"),
		IfNoneMatch: r.Header.Get("If-None-Match"),
	}

	// Size strategy (issue #6): stream with the declared length when
	// present; chunked bodies (Transfer-Encoding, ContentLength < 0) are
	// accepted and capped by the backend's maxPutBytes LimitReader —
	// rejecting chunked PUT broke real clients (curl -T -, some Windows
	// Explorer configs). No unbounded buffering: the backend reads at
	// most maxPutBytes+1 before rejecting.
	size := r.ContentLength

	// Leaf 05 (quic-h3-2026-10): on a versioning-ENABLED bucket the
	// object's CURRENT bytes become one version on overwrite — the SAME
	// capture the s3 PUT handler takes, resolved through the SAME seams
	// and the SAME mode dispatch (sidecar reads; reflink/both clone;
	// snapshots captures into the sidecar bookkeeping exactly as the s3
	// handler does — verified against the s3 handler on this host).
	// The old bytes are captured BEFORE the plain overwrite; the record
	// lands AFTER the successful backend Put below (the backend rewrites
	// the sidecar wholesale). Fail-closed on capture failure (s3 parity:
	// object_handlers.go answers 500 before any write); the reflink
	// clone's fail-soft contract rides inside the capture (cloneOK=false
	// → the Put proceeds, no version record).
	if f.bucketPathFn != nil {
		bucketPath := f.bucketPath(res.bucket)
		capturedOld, captured, capErr := captureBeforePut(bucketPath, res.bucket, res.key)
		if capErr != nil {
			log.Printf("webdav PUT %s/%s: capturing prior version: %v", strconv.Quote(res.bucket), strconv.Quote(res.key), capErr)
			writeDavError(w, http.StatusInternalServerError, "")
			return
		}
		obj, err := f.be.Put(r.Context(), res.bucket, res.key, r.Body, size, opts)
		if err != nil {
			writeDavErrorFrom(w, err)
			return
		}
		// The version RECORD lands AFTER the successful backend Put
		// (the pinned capture/record invariant; the s3 handler's record
		// step sits in the identical position). A missing capture means
		// create/unversioned — no record. A record failure fails the
		// response exactly as the s3 handler does (500 after the data
		// landed).
		if captured {
			if recErr := recordAfterPut(bucketPath, res.bucket, res.key, capturedOld); recErr != nil {
				log.Printf("webdav PUT %s/%s: recording prior version: %v", strconv.Quote(res.bucket), strconv.Quote(res.key), recErr)
				writeDavError(w, http.StatusInternalServerError, "")
				return
			}
		}
		w.Header().Set("ETag", objectmodel.QuotedETag(obj.ETag))
		status := http.StatusCreated
		if existed {
			status = http.StatusNoContent
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(status)
		return
	}

	obj, err := f.be.Put(r.Context(), res.bucket, res.key, r.Body, size, opts)
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}

	w.Header().Set("ETag", objectmodel.QuotedETag(obj.ETag))
	status := http.StatusCreated
	if existed {
		status = http.StatusNoContent
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// putConditionalOK evaluates RFC 7232 If-Match / If-None-Match for a PUT
// against the Stat snapshot (existed, curETag). RFC order: If-Match wins
// when present; If-None-Match applies only when If-Match is absent.
//
//   - If-Match present: the object must exist and its ETag must match one
//     list entry (compared with and without quotes; "*" matches any
//     existing object) — otherwise 412.
//   - If-None-Match "*": the object must NOT exist — otherwise 412.
//   - If-None-Match with an ETag list: 412 when the current ETag is in
//     the list (quote-insensitive).
func putConditionalOK(ifMatch, ifNoneMatch string, existed bool, curETag string) bool {
	if ifMatch != "" {
		if !existed {
			return false
		}
		if ifMatch == "*" {
			return true
		}
		return etagListContains(ifMatch, curETag)
	}
	if ifNoneMatch == "" {
		return true
	}
	if ifNoneMatch == "*" {
		return !existed
	}
	if !existed {
		return true
	}
	return !etagListContains(ifNoneMatch, curETag)
}

// etagListContains reports whether an ETag header list ("a", "b", W/"a")
// contains the given ETag, comparing with and without surrounding quotes
// and ignoring any weakness prefix (RFC 7232 comparison rule).
func etagListContains(header, etag string) bool {
	normalize := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "W/")
		return strings.Trim(s, `"`)
	}
	want := normalize(etag)
	for entry := range strings.SplitSeq(header, ",") {
		if normalize(entry) == want {
			return true
		}
	}
	return false
}
