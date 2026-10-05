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
func (f *Frontend) serveObject(w http.ResponseWriter, r *http.Request, res resource, obj objectmodel.Object, isHead bool) {
	etag := objectmodel.QuotedETag(obj.ETag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatchesAny(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
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
