// mkcol.go — MKCOL (leaf 03 Task 3): collections are VIRTUAL prefixes —
// no marker object is ever written (master Notes rule). RFC 4918 §9.3:
// 201 on success, 405 when the target exists, 409 when the parent is
// missing (no auto-vivify — Finder/davfs2 issue MKCOL first), 415 on a
// request body, 403 on the mode-B root.
//
// Bucket creation in mode A: the Backend seam has NO create-bucket method
// (internal/backend/fsbackend/object.go pins "the seam has no
// CreateBucket"; leaf 03's STOP rule forbids extending the seam), so a
// top-level MKCOL in mode A is rejected 403 with the reason documented
// here.
package webdav

import (
	"net/http"
)

// handleMKCOL implements MKCOL for both modes.
func (f *Frontend) handleMKCOL(w http.ResponseWriter, r *http.Request, res resource) {
	// RFC 4918 §9.3.5: a request body is an unsupported media type.
	if r.ContentLength > 0 {
		writeDavError(w, http.StatusUnsupportedMediaType, "")
		return
	}
	if res.isRoot {
		if f.bucket != "" {
			// Mode B: bucket selection is fixed by config.
			writeDavError(w, http.StatusForbidden, "")
			return
		}
		writeDavError(w, http.StatusMethodNotAllowed, "")
		return
	}
	if res.key == "" {
		// Mode A top-level collection = bucket. The seam has no
		// create-bucket method (see file comment) — a rejection, never a
		// silent no-op or a partial emulation.
		writeDavError(w, http.StatusForbidden, "")
		return
	}
	// Exists (as file OR as collection with content) ⇒ 405.
	if _, isFile, err := statFile(r.Context(), f.be, res.bucket, res.key); err != nil {
		writeDavErrorFrom(w, err)
		return
	} else if isFile {
		writeDavError(w, http.StatusMethodNotAllowed, "")
		return
	}
	exists, err := collectionExists(r.Context(), f.be, res.bucket, res.collectionPrefix())
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}
	if exists {
		writeDavError(w, http.StatusMethodNotAllowed, "")
		return
	}
	// Parent must exist (RFC 4918 §9.3.1: 409, never auto-vivify).
	if parent := res.parentPrefix(); parent != "" {
		parentExists, err := collectionExists(r.Context(), f.be, res.bucket, parent)
		if err != nil {
			writeDavErrorFrom(w, err)
			return
		}
		if !parentExists {
			writeDavError(w, http.StatusConflict, "")
			return
		}
	}
	// Success writes NOTHING (virtual collections): the collection becomes
	// visible once a child lands under the prefix.
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}
