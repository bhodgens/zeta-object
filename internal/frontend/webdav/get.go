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
func (f *Frontend) serveObject(w http.ResponseWriter, r *http.Request, res resource, obj objectmodel.Object, isHead bool) {
	etag := objectmodel.QuotedETag(obj.ETag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatchesAny(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
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
