// put.go — PUT (leaf 03 Task 2): create (201) / overwrite (204) with the
// ETag response header, ETag conditionals through PutOptions, streaming
// bodies with the declared Content-Length. Chunked bodies without a length
// are rejected with 400 rather than buffered unboundedly (leaf 03 pin).
package webdav

import (
	"net/http"

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
	if _, isFile, err := statFile(r.Context(), f.be, res.bucket, res.key); err != nil {
		writeDavErrorFrom(w, err)
		return
	} else if isFile {
		existed = true
	}

	opts := objectmodel.PutOptions{
		ContentType: r.Header.Get("Content-Type"),
		IfMatch:     r.Header.Get("If-Match"),
		IfNoneMatch: r.Header.Get("If-None-Match"),
	}

	// Size strategy (leaf 03 Task 2): stream with the declared length;
	// a body without a declared length (chunked) is rejected 400 rather
	// than buffered unboundedly.
	size := r.ContentLength
	if size < 0 {
		writeDavError(w, http.StatusBadRequest, "")
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
