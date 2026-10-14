// delete.go — DELETE (leaf 03 Task 4): files delete directly; collections
// delete recursively (Depth-infinity delete IS expressible: List + Delete
// loop with full pagination). Bucket deletion and root deletion are 403 —
// bucket deletion is not expressible safely over WebDAV (no seam method,
// and a runaway recursive delete of a whole bucket is never a default).
package webdav

import (
	"context"
	"log"
	"net/http"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// handleDELETE implements DELETE for files and collections.
func (f *Frontend) handleDELETE(w http.ResponseWriter, r *http.Request, res resource) {
	if res.isRoot {
		writeDavError(w, http.StatusForbidden, "")
		return
	}
	if res.key == "" {
		// Mode A bucket: 403 (documented above).
		writeDavError(w, http.StatusForbidden, "")
		return
	}
	_, kind, err := f.resolveKind(r.Context(), res)
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}
	switch kind {
	case kindFile:
		// Leaf 05 (quic-h3-2026-10): on a versioning-ENABLED bucket a
		// DELETE writes a delete marker (sidecar/reflink/both modes)
		// and the plain backend delete is SUPPRESSED — the data file
		// stays; plain GET answers 404 via the marker. Identical to the
		// s3 DELETE handler's marker step (deleteObjectVersionedMarker).
		// OFF/Suspended buckets proceed to the plain delete unchanged
		// (byte-identical path). In snapshots mode the store REFUSES
		// the marker (ErrDeleteMarkersUnsupported): the delete is
		// answered 409 Conflict — the same wire mapping the s3 error
		// table gives that error class (never a 500, never a fake
		// plain-delete success while the bucket claims versioning).
		// Gate on the RESOLVED path, not the resolver field (bughunt H1) —
		// production wires only WithLockStoreRoot, so the old
		// `f.bucketPathFn != nil` test made this branch dead: a DELETE on a
		// versioning-Enabled bucket took the plain path, destroying the
		// bytes and recording no recoverable delete marker.
		if err := f.deleteCollectionKey(r.Context(), res.bucket, res.key); err != nil {
			if webdavVersioningConflict(err) {
				writeDavError(w, http.StatusConflict, "")
			} else {
				writeDavErrorFrom(w, err)
			}
			return
		}
	case kindCollection:
		// H10: the recursive sweep must drive the SAME per-key
		// marker-aware delete the kindFile branch drives — on a
		// versioning-Enabled bucket a plain f.be.Delete destroys the
		// bytes with no marker and no captured versions, and the
		// sweep loop would happily "confirm" an emptied prefix while
		// every plain view still lists nothing. Each removed key goes
		// through deleteCollectionKey (deleteMarkerOrPlain + 409 on
		// the snapshots refusal), preserving the sweep bounds.
		if err := f.deleteCollection(r.Context(), res.bucket, res.collectionPrefix()); err != nil {
			// Snapshots mode refuses the marker: the sentinel maps to
			// 409 Conflict — the same answer a single-key DELETE gives
			// for the identical store state (davStatus has no case for
			// the versioning sentinels; without this branch it would
			// render a 500 for a conflict).
			if webdavVersioningConflict(err) {
				writeDavError(w, http.StatusConflict, "")
				return
			}
			writeDavErrorFrom(w, err)
			return
		}
	case kindMissing:
		writeDavError(w, http.StatusNotFound, "")
		return
	default:
		writeDavError(w, http.StatusInternalServerError, "")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// deleteCollectionKey is the ONE marker-aware per-key delete both the
// single-key DELETE and the recursive collection sweep drive (bughunt H10:
// the sweep used to call f.be.Delete directly, destroying versioned data
// with no marker and no captured history on a versioning-Enabled bucket).
// On a versioning-Enabled bucket it records a delete marker and suppresses
// the plain delete (the data file stays; plain views hide it — identical to
// the s3 DELETE handler's marker step); OFF/Suspended take the plain
// delete; in snapshots mode the store refuses the marker and the sentinel
// (ErrDeleteMarkersUnsupported) is returned for the caller's 409 mapping.
func (f *Frontend) deleteCollectionKey(ctx context.Context, bucket, key string) error {
	if bucketPath := f.bucketPath(bucket); bucketPath != "" {
		suppress, markerErr := deleteMarkerOrPlain(bucketPath, bucket, key)
		if markerErr != nil {
			log.Printf("webdav DELETE %s/%s: versioned marker: %v", strconv.Quote(bucket), strconv.Quote(key), markerErr)
			return markerErr
		}
		if suppress {
			return nil
		}
	}
	return f.be.Delete(ctx, bucket, key)
}

// deleteCollection removes every key under the prefix. The loop re-lists
// until the prefix is empty (guarding against concurrent puts during the
// delete) with a bounded retry count — termination is guaranteed: bounded
// retries then a 500 with a clear message, never a hang (leaf 03 Task 4).
//
// Each key is removed through deleteCollectionKey (the SAME marker-aware
// path a single-key DELETE takes — bughunt H10). On a versioning-Enabled
// bucket a suppressed key keeps its data file in the backend listing, so
// the sweep cannot wait for an empty listing: keys whose latest history
// entry is a delete marker count as REMOVED (every plain view already
// hides them — the same visibility rule PROPFIND/GET apply), and the loop
// terminates when every listed key is marker-hidden or actually gone.
// Snapshots mode refuses the marker: the sentinel propagates and the wire
// answers 409, the same conflict a single-key DELETE answers.
func (f *Frontend) deleteCollection(ctx context.Context, bucket, prefix string) error {
	const maxSweeps = 100
	for sweep := 0; ; sweep++ {
		if sweep >= maxSweeps {
			return objectmodel.ErrInternalError("recursive delete did not converge within the sweep bound")
		}
		var keys []string
		token := ""
		for page := 0; ; page++ {
			if page >= maxListPages {
				return objectmodel.ErrInternalError("listing exceeded the page bound")
			}
			p, err := f.be.List(ctx, bucket, objectmodel.ListParams{
				Prefix:            prefix,
				Delimiter:         "/",
				MaxKeys:           propfindPageKeys,
				ContinuationToken: token,
			})
			if err != nil {
				return err
			}
			for _, o := range p.Objects {
				keys = append(keys, o.Key)
			}
			// Common prefixes under a delete are recursed into (nested
			// collections), not just rolled up.
			for _, cp := range p.CommonPrefixes {
				if err := f.deleteCollection(ctx, bucket, cp); err != nil {
					return err
				}
			}
			if !p.IsTruncated || p.NextToken == "" || p.NextToken == token {
				break
			}
			token = p.NextToken
		}
		if len(keys) == 0 {
			return nil
		}
		remaining := 0
		for _, key := range keys {
			if err := f.deleteCollectionKey(ctx, bucket, key); err != nil {
				return err
			}
			// A marker-suppressed key stays listed by the backend; it
			// is removed from every plain view (markerhide.go's rule)
			// and must not keep the sweep looping. An unsuppressed
			// key (plain delete) is gone — re-check anyway: if it
			// still lists it is marker-hidden or a concurrent write.
			if !f.objectHiddenByDeleteMarker(bucket, key) {
				remaining++
			}
		}
		if remaining == 0 {
			return nil
		}
		// Loop: re-list and confirm the prefix is plain-view-empty
		// before returning.
	}
}
