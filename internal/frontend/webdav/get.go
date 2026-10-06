// get.go — GET/HEAD (leaf 02 Task 1): objects stream with ETag /
// Last-Modified / Content-Type headers and honor If-None-Match; collections
// answer 200 with an empty body and httpd/unix-directory (Finder probes
// this). Pinned decision: a GET of a prefix path without the trailing
// slash that exists only as a collection is a 404 — clients take hrefs
// from PROPFIND, which always carry the slash.
package webdav

import (
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// handleGET serves both GET and HEAD (isHead suppresses only the body).
func (f *Frontend) handleGET(w http.ResponseWriter, r *http.Request, res resource, isHead bool) {
	if res.isRoot {
		f.serveCollectionHeaders(w, isHead, "/")
		return
	}
	obj, kind, err := f.resolveKind(r.Context(), res)
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}
	switch kind {
	case kindMissing:
		writeDavError(w, http.StatusNotFound, "")
	case kindCollection:
		f.serveCollectionHeaders(w, isHead, f.davPath(res))
	case kindFile:
		f.serveObject(w, r, res, obj, isHead)
	default:
		writeDavError(w, http.StatusInternalServerError, "")
	}
}

// serveCollectionHeaders answers a collection GET/HEAD: 200, empty body,
// httpd/unix-directory.
func (f *Frontend) serveCollectionHeaders(w http.ResponseWriter, isHead bool, _ string) {
	w.Header().Set("Content-Type", "httpd/unix-directory")
	w.Header().Set("Content-Length", "0")
	if isHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// serveObject streams an object with the full header set and conditional
// evaluation at the seam (the fs backend leaves GetOptions unapplied in v1,
// so the frontend honors If-None-Match here and forwards the option
// regardless for future backends).
//
// Range handling (leaf 01): the S3 frontend owns the range grammar —
// s3.ParseMultiRange / s3.CoalesceRanges classify malformed (ignore, 200)
// vs all-unsatisfiable (416) per RFC 9110, and s3.WriteMultipartByteranges
// is the multi-span wire form. If-None-Match is evaluated BEFORE Range: a
// matching INM answers 304 regardless of any Range header.
//
// Delete-marker visibility: the marker consult runs FIRST, before the
// conditional and before any Range or body decision, so a delete-marked
// object can never leak a byte through a 206, a 304 or a full 200. See
// deleteMarkerHides.
func (f *Frontend) serveObject(w http.ResponseWriter, r *http.Request, res resource, obj objectmodel.Object, isHead bool) {
	if f.deleteMarkerHides(w, res) {
		return
	}
	etag := objectmodel.QuotedETag(obj.ETag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatchesAny(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Byte-range support is discoverable on every object response that
	// carries a body or a range decision (s3 parity: object_handlers.go
	// sets Accept-Ranges once, before the multi-range / single-range /
	// full-body split, so a 200, a 206 and an unsatisfiable-range 416 all
	// advertise it). Set AFTER the 304 branch, because s3 evaluates
	// checkObjectPreconditions before advertising ranges — a 304 carries
	// neither.
	w.Header().Set("Accept-Ranges", "bytes")
	// Parse the Range header against the object size BEFORE opening the
	// stream: the 416 and multipart outcomes never need the backend, and
	// the single-span span bounds are known up front.
	spans, ok := s3.ParseMultiRange(r.Header.Get("Range"), obj.Size)
	switch {
	case !ok:
		// Absent or syntactically malformed: ignore the header (200 full).
	case len(spans) == 0:
		// All specs interpretable but unsatisfiable (RFC 9110 14.2).
		f.serveRangeUnsatisfiable(w, obj.Size)
		return
	case len(spans) > 1:
		if coalesced, cok := s3.CoalesceRanges(spans, s3.MultiRangePartsMax); cok {
			f.serveMultiRange(w, r, res, obj, coalesced)
			return
		}
		// Over the part cap: fall through to the 200 full-body path,
		// same as the S3 frontend.
	}
	var span s3.Span
	if ok && len(spans) == 1 {
		span = spans[0]
	}

	rc, _, err := f.be.Get(r.Context(), res.bucket, res.key, objectmodel.GetOptions{
		IfNoneMatch: r.Header.Get("If-None-Match"),
	})
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}
	defer rc.Close() //nolint:errcheck // read-side close, nothing to report.

	w.Header().Set("ETag", etag)
	if !obj.LastModified.IsZero() {
		w.Header().Set("Last-Modified", obj.LastModified.UTC().Format(http.TimeFormat))
	}
	ct := obj.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)

	if ok && len(spans) == 1 {
		// Single satisfiable span: 206 + Content-Range, Content-Length is
		// the SPAN length. The backend Get seam has no offset/length
		// support in v1 (fsbackend Get ignores opts.Range), so the prefix
		// is stream-discarded — correct for v1 local-disk sizes.
		if span.Start > 0 {
			if _, err := io.CopyN(io.Discard, rc, span.Start); err != nil {
				// Nothing written yet: report a clean error.
				writeDavErrorFrom(w, err)
				return
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", span.Start, span.End-1, obj.Size))
		w.Header().Set("Content-Length", fmt.Sprint(span.End-span.Start))
		w.WriteHeader(http.StatusPartialContent)
		if isHead {
			return
		}
		if _, err := io.CopyN(w, rc, span.End-span.Start); err != nil {
			log.Printf("webdav: mid-stream copy failure for %s/%s: %v", res.bucket, res.key, err)
		}
		return
	}

	w.Header().Set("Content-Length", fmt.Sprint(obj.Size))
	w.WriteHeader(http.StatusOK)
	if isHead {
		return
	}
	// Header already sent: the response is committed, so a mid-stream copy
	// failure cannot be reported to the client — log it and let the
	// connection drop.
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("webdav: mid-stream copy failure for %s/%s: %v", res.bucket, res.key, err)
	}
}

// deleteMarkerHides is the READ-side half of the versioning contract, on
// the same store the webdav DELETE wrote its marker into: a versioning
// DELETE records a delete marker and SUPPRESSES the plain delete, so the
// data file survives on disk while the object is gone from every plain
// view. The s3 GET consults that marker (plainObjectDeleteMarker404, now
// exported as s3.PlainObjectDeleteMarker404 through versioning_bridge.go)
// and answers 404; without the same consult here, webdav GET/HEAD served
// the surviving bytes after DELETE already answered 204 — the bug this
// closes.
//
// It reports true when the response has been WRITTEN (404) and the caller
// must stop; false means "not hidden, serve normally" — including every
// error arm, which is s3 parity rather than a webdav choice:
//
//   - bucketPath "" (no resolver wired — the unit-test seam): no consult,
//     today's plain serve, byte-identical.
//   - a marker-read error: s3's gate reads `mErr == nil && markerHidden`,
//     so it proceeds on error too. An unreadable version sidecar must not
//     turn a readable object into a 404; the webdav DELETE path logs its
//     store failures, and this read arm is silent on s3 as well.
//
// The consult runs before If-None-Match, before Range and before any body
// write, so no hidden byte can escape through a 304 or a 206 either.
func (f *Frontend) deleteMarkerHides(w http.ResponseWriter, res resource) bool {
	if res.key == "" {
		return false // no object key: nothing to hide (root/collection GET)
	}
	bucketPath := f.bucketPath(res.bucket)
	if bucketPath == "" {
		return false // unwired seam (unit-test shape): plain behavior
	}
	hidden, err := s3.PlainObjectDeleteMarker404(bucketPath, res.bucket, res.key)
	if err != nil || !hidden {
		return false // s3 parity: proceed on error, serve when not hidden
	}
	// RFC 4918: 404 with the generic <D:error> body, no object bytes. The
	// s3 wire also carries x-amz-delete-marker: true here; webdav has no
	// such header form, so the status plus the absence of the body is the
	// whole contract.
	writeDavError(w, http.StatusNotFound, "")
	return true
}

// serveRangeUnsatisfiable answers an all-unsatisfiable Range header: 416
// with `Content-Range: bytes */size` and an empty body (leaf spec).
func (f *Frontend) serveRangeUnsatisfiable(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
}

// serveMultiRange answers a multi-span Range with a 206 multipart/byteranges
// response via the S3 frontend's writer (same boundary/part wire form).
// Each span re-opens the object through the Backend seam and stream-discards
// the prefix (same v1 trade-off as the single-span path).
func (f *Frontend) serveMultiRange(w http.ResponseWriter, r *http.Request, res resource, obj objectmodel.Object, spans []s3.Span) {
	ct := obj.ContentType
	fetch := func(off, end int64) (io.ReadCloser, error) {
		rc, _, err := f.be.Get(r.Context(), res.bucket, res.key, objectmodel.GetOptions{})
		if err != nil {
			return nil, err
		}
		if off > 0 {
			if _, err := io.CopyN(io.Discard, rc, off); err != nil {
				rc.Close() //nolint:errcheck // read-side close on the error path.
				return nil, fmt.Errorf("webdav: seeking to offset %d: %w", off, err)
			}
		}
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(rc, end-off), rc}, nil
	}
	if err := s3.WriteMultipartByteranges(w, res.bucket+"/"+res.key, obj.Size, ct, spans, fetch); err != nil {
		log.Printf("webdav: error serving multipart/byteranges for %s/%s: %v", res.bucket, res.key, err)
	}
}

// etagMatchesAny reports whether the If-None-Match header value (possibly a
// comma-separated list, or "*") matches etag.
func etagMatchesAny(headerValue, etag string) bool {
	if headerValue == "*" {
		return true
	}
	for _, candidate := range splitCommaList(headerValue) {
		if objectmodel.ETagsMatch(candidate, etag) {
			return true
		}
	}
	return false
}

// splitCommaList splits a comma-separated header value, trimming space.
func splitCommaList(v string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == ',' {
			part := v[start:i]
			if trimmed := trimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '	') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '	') {
		s = s[:len(s)-1]
	}
	return s
}
