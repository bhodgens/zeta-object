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
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// handleCopyMove implements COPY (isMove=false) and MOVE (isMove=true).
//
// only because gocognit fired first and masked gocyclo in its output). The conflict branch was
// extracted into writeMoveSourceDeleteError, restoring the pre-fix values. A further split of this
// handler is a dedicated refactor, not part of this fix.
//
//nolint:gocognit,gocyclo // 45/33 at HEAD BEFORE the M2 fix (both pre-existing; the lint gate passed
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
	// H9 (RFC 4918 §9.9.4): a MOVE whose destination IS the source must
	// be refused with 403 BEFORE any backend call. Without the guard the
	// flow below Puts the source bytes back onto the same key and then
	// deletes the source — on a versioning-Enabled bucket the delete
	// records a marker, so the object ends up gone from every plain view
	// while the wire answered 204: a self-MOVE destroys the object.
	if dstRes.bucket == src.bucket && dstRes.key == src.key {
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
	// Leaf 05 (quic-h3-2026-10) + bughunt W2-H2 parity: BOTH COPY and
	// MOVE now run the destination Put through the SAME versioning capture
	// path as a plain PUT (an overwrite of a versioned object records the
	// old bytes). The SOURCE delete half of a MOVE manufactures NO versions
	// beyond what its own DeleteObject counterpart records (a delete marker
	// where the mode has them), and the deliberate no-op on an unversioned
	// source is pinned by TestWebdavMOVE_NoVersionsPinned.
	//
	// WHY COPY CAPTURES NOW (W2-H2): copyObjectHandler had NO
	// capture/record call, so a plain COPY originally recorded NOTHING here
	// to stay in s3-CopyObject parity (bughunt L3). W2-H2 gave s3
	// CopyObject the same capture→Put→record gate every other write path
	// (PutObject, CMPU completion, batch copy) already had — on a
	// versioning-Enabled bucket the wholesale sidecar rewrite was silently
	// destroying the destination's history. With the s3 handler recording,
	// a webdav COPY that skipped the capture became the divergence: the
	// two copies of the same file disagreed about whether history
	// survives. The parity pins (copymove_versioning_test.go) assert
	// IDENTICAL post-copy state on both wire paths, which only a capture
	// on BOTH sides satisfies. The L3 destruction hazard the no-record
	// decision guarded against (reflinkRetention 0 pruning prior version
	// data) is equally present in s3 CopyObject and is the versioning
	// gate's deliberate semantics, not a COPY-specific defect.
	//
	// A capture failure FAILS the whole request (fail-closed, PUT parity):
	// the destination bytes must not land while the versioning record of
	// what they overwrite cannot be taken.
	// Gate on the RESOLVED path, not the resolver field (bughunt H1):
	// production wires only WithLockStoreRoot, so the old
	// `f.bucketPathFn != nil` test made this branch dead and a MOVE
	// overwrite recorded no version in production.
	if dstBucketPath := f.bucketPath(dstRes.bucket); dstBucketPath != "" {
		capturedOld, captured, capErr := captureBeforePut(dstBucketPath, dstRes.bucket, dstRes.key)
		if capErr != nil {
			log.Printf("webdav COPY/MOVE %s/%s: capturing prior version: %v", strconv.Quote(dstRes.bucket), strconv.Quote(dstRes.key), capErr)
			writeDavError(w, http.StatusInternalServerError, "")
			return
		}
		dstObj, err := f.be.Put(r.Context(), dstRes.bucket, dstRes.key, rc, srcObj.Size, putOpts)
		if err != nil {
			writeDavErrorFrom(w, err)
			return
		}
		if captured {
			if recErr := recordAfterPut(dstBucketPath, dstRes.bucket, dstRes.key, capturedOld); recErr != nil {
				log.Printf("webdav COPY/MOVE %s/%s: recording prior version: %v", strconv.Quote(dstRes.bucket), strconv.Quote(dstRes.key), recErr)
				writeDavError(w, http.StatusInternalServerError, "")
				return
			}
		}
		// PUT-condition recheck + the rest of the shared tail run below.
		if !overwriteAllowed(r.Header.Get("Overwrite")) && dstExists {
			writeDavError(w, http.StatusPreconditionFailed, "")
			return
		}
		if isMove {
			// M6: read+write is not a delete grant. moveSourceDelete
			// calls the backend delete; refuse before that call so a
			// denied MOVE leaves the source in place (403). COPY does
			// not take this branch. MKCOL is not a MOVE.
			if !authorizeMoveSourceDelete(w, r, src) {
				return
			}
			// Versioned-marker parity on MOVE's source delete: the
			// s3 counterpart of a rename is CopyObject + DeleteObject;
			// its Delete records what the dedicated DELETE handler
			// records (a marker on an Enabled sidecar/reflink/both
			// bucket, a refused marker in snapshots mode → 409). The
			// rename MANUFACTURES no version entry beyond what its own
			// steps produce; a pure rename of an unversioned key
			// produces NO sidecar history at all (pinned by
			// TestWebdavMOVE_NoVersionsPinned).
			if err := f.moveSourceDelete(r, src); err != nil {
				f.writeMoveSourceDeleteError(w, err)
				return
			}
		}
		status := http.StatusCreated
		if dstExists {
			status = http.StatusNoContent
		}
		w.Header().Set("Content-Length", "0")
		w.Header().Set("ETag", objectmodel.QuotedETag(dstObj.ETag))
		w.WriteHeader(status)
		return
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
		if !authorizeMoveSourceDelete(w, r, src) {
			return
		}
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

// authorizeMoveSourceDelete is the OpDelete gate on MOVE's source-delete
// half. read+write is not a delete grant: moveSourceDelete (and the
// unwired be.Delete tail) calls the backend delete, which a write grant
// must not authorize. Denied is 403 and the caller leaves the source in
// place. COPY does not call this. MKCOL is not a MOVE.
func authorizeMoveSourceDelete(w http.ResponseWriter, r *http.Request, src resource) bool {
	if auth.AuthorizeOp(identityOf(r), auth.OpDelete, src.bucket, src.key, time.Now().UTC()) {
		return true
	}
	writeDavError(w, http.StatusForbidden, "")
	return false
}

// writeMoveSourceDeleteError renders a moveSourceDelete failure with
// DELETE's status contract: a versioning-conflict sentinel (snapshots mode
// refusing the marker, read-only mode) is a 409 CONFLICT - the s3
// CopyObject+DeleteObject counterpart answers 409 for the same store state,
// and davStatus has no case for these sentinels so writeDavErrorFrom would
// fall to its 500 default. Everything else maps through davStatus as usual.
func (f *Frontend) writeMoveSourceDeleteError(w http.ResponseWriter, err error) {
	if webdavVersioningConflict(err) {
		writeDavError(w, http.StatusConflict, "")
		return
	}
	writeDavErrorFrom(w, err)
}
