// copymove.go — COPY/MOVE (leaf 03 Task 5): file-level copies with
// Overwrite/Depth semantics, streamed transfer (Get reader straight into
// Put — no full buffering), cross-bucket COPY legal in mode A (two Backend
// calls), Destination escaping the configured bucket 403 in mode B.
// Collection COPY/MOVE at any Depth is 403 — never a partial fake copy.
// MOVE deletes the source only after a successful destination Put; the
// acceptable crash window (duplicate, not loss) is documented in the
// master Notes.
package webdav

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// handleCopyMove implements COPY (isMove=false) and MOVE (isMove=true).
func (f *Frontend) handleCopyMove(w http.ResponseWriter, r *http.Request, src resource, isMove bool) {
	dstRes, ok := f.resolveDestination(w, r)
	if !ok {
		return
	}

	// Source gate: COPY needs Read on the source bucket; MOVE's
	// destructive half already required Write on the source bucket in
	// authorize (requestGrantsWrite).
	if err := frontend.AuthorizeRequest(identityOf(r), src.bucket, false); err != nil {
		writeDavError(w, http.StatusForbidden, "")
		return
	}
	// Destination gate: independent Write check on the destination bucket
	// (leaf 04: cross-bucket COPY checks src-read/dst-write independently).
	if err := frontend.AuthorizeRequest(identityOf(r), dstRes.bucket, true); err != nil {
		writeDavError(w, http.StatusForbidden, "")
		return
	}

	if src.isRoot || src.key == "" {
		// Root or bucket: collection COPY/MOVE (Depth infinity) is not
		// expressible — 403, never a partial copy.
		writeDavError(w, http.StatusForbidden, "")
		return
	}
	srcObj, kind, err := f.resolveKind(r.Context(), src)
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}
	if kind == kindMissing {
		writeDavError(w, http.StatusNotFound, "")
		return
	}
	if kind == kindCollection {
		writeDavError(w, http.StatusForbidden, "")
		return
	}

	dstExists := false
	var dstETag string
	if dstObj, isFile, err := statFile(r.Context(), f.be, dstRes.bucket, dstRes.key); err != nil {
		writeDavErrorFrom(w, err)
		return
	} else if isFile {
		dstExists = true
		dstETag = dstObj.ETag
	}
	if !destinationPreconditionsOK(w, r, dstExists, dstETag) {
		return
	}
	// Destination parent must exist ⇒ 409 (no auto-vivify).
	if parent := dstRes.parentPrefix(); parent != "" {
		parentExists, err := collectionExists(r.Context(), f.be, dstRes.bucket, parent)
		if err != nil {
			writeDavErrorFrom(w, err)
			return
		}
		if !parentExists {
			writeDavError(w, http.StatusConflict, "")
			return
		}
	}

	// Streamed copy: the Get reader feeds Put directly (leaf 03: no full
	// buffering for large objects).
	rc, _, err := f.be.Get(r.Context(), src.bucket, src.key, objectmodel.GetOptions{})
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}
	defer rc.Close() //nolint:errcheck // read-side close.

	putOpts := objectmodel.PutOptions{
		ContentType: srcObj.ContentType,
		IfNoneMatch: conditionForOverwrite(r, dstExists),
	}
	dstObj, err := f.be.Put(r.Context(), dstRes.bucket, dstRes.key, rc, srcObj.Size, putOpts)
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}
	// PUT-condition recheck: the backend ignores PutOptions conditionals
	// in v1 (fs backend documents conditionals as protocol-side), so the
	// Overwrite:F-on-race case is re-verified here from the authoritative
	// Put result.
	if !overwriteAllowed(r.Header.Get("Overwrite")) && dstExists {
		writeDavError(w, http.StatusPreconditionFailed, "")
		return
	}

	if isMove {
		if err := f.be.Delete(r.Context(), src.bucket, src.key); err != nil {
			// The destination copy succeeded; the source delete failed.
			// Report the failure — a duplicate is the acceptable crash
			// shape, silent loss is not.
			writeDavErrorFrom(w, err)
			return
		}
	}

	status := http.StatusCreated
	if dstExists {
		status = http.StatusNoContent
	}
	w.Header().Set("ETag", objectmodel.QuotedETag(dstObj.ETag))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// resolveDestination parses the Destination header (full URL or absolute
// path) through the SAME URL→resource resolver (leaf 03: no second
// parser). Missing/garbage ⇒ 400; mode-B escape ⇒ 403.
func (f *Frontend) resolveDestination(w http.ResponseWriter, r *http.Request) (resource, bool) {
	raw := r.Header.Get("Destination")
	if raw == "" {
		writeDavError(w, http.StatusBadRequest, "")
		return resource{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		writeDavError(w, http.StatusBadRequest, "")
		return resource{}, false
	}
	dst, ok := f.parseResource(u.Path)
	if !ok {
		// W1: a dot-segment destination would escape dataDir through the
		// fs backend's unchecked root+bucket join (COPY/MOVE writes and
		// MOVE's source delete all use the destination). 403.
		writeDavError(w, http.StatusForbidden, "")
		return resource{}, false
	}
	if dst.isRoot {
		writeDavError(w, http.StatusBadRequest, "")
		return resource{}, false
	}
	if f.bucket != "" && dst.bucket != f.bucket {
		// Mode B: the destination may not leave the configured bucket.
		writeDavError(w, http.StatusForbidden, "")
		return resource{}, false
	}
	if dst.isCollection {
		// File-level COPY/MOVE only; a collection destination would
		// silently merge — reject rather than emulate.
		writeDavError(w, http.StatusForbidden, "")
		return resource{}, false
	}
	return dst, true
}

// destinationPreconditionsOK enforces the pre-Put destination gates for
// COPY/MOVE and writes the error itself (returns false when it wrote one):
//   - Overwrite header: "F"/"false" forbids overwriting an existing
//     destination ⇒ 412. Absent or "T"/"true" allows (RFC default T).
//   - W2: If-Match/If-None-Match are enforced against the DESTINATION using
//     the statFile snapshot already fetched. The fs backend ignores
//     PutOptions conditionals in v1 (see the copymove.go header note), so
//     COPY/MOVE with conditionals would otherwise clobber unconditionally.
func destinationPreconditionsOK(w http.ResponseWriter, r *http.Request, dstExists bool, dstETag string) bool {
	if ov := overwriteAllowed(r.Header.Get("Overwrite")); !ov && dstExists {
		writeDavError(w, http.StatusPreconditionFailed, "")
		return false
	}
	if r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "" {
		if !putConditionalOK(r.Header.Get("If-Match"), r.Header.Get("If-None-Match"), dstExists, dstETag) {
			writeDavError(w, http.StatusPreconditionFailed, "")
			return false
		}
	}
	return true
}

// overwriteAllowed interprets the Overwrite header (case-insensitive;
// absent ⇒ true, the RFC default).
func overwriteAllowed(header string) bool {
	switch strings.ToLower(header) {
	case "", "t", "true":
		return true
	default:
		return false
	}
}

// conditionForOverwrite converts Overwrite:F into an IfNoneMatch:* Put
// option so a backend that honors conditionals enforces it below the seam.
// (The fs backend leaves it unapplied in v1; handleCopyMove re-verifies.)
func conditionForOverwrite(r *http.Request, _ bool) string {
	if !overwriteAllowed(r.Header.Get("Overwrite")) {
		return "*"
	}
	return ""
}
