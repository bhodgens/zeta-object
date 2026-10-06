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
		bucketPath := f.bucketPath(res.bucket)
		if bucketPath != "" {
			suppress, markerErr := deleteMarkerOrPlain(bucketPath, res.bucket, res.key)
			if markerErr != nil {
				log.Printf("webdav DELETE %s/%s: versioned marker: %v", strconv.Quote(res.bucket), strconv.Quote(res.key), markerErr)
				if webdavVersioningConflict(markerErr) {
					writeDavError(w, http.StatusConflict, "")
				} else {
					writeDavError(w, http.StatusInternalServerError, "")
				}
				return
			}
			if suppress {
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if err := f.be.Delete(r.Context(), res.bucket, res.key); err != nil {
			writeDavErrorFrom(w, err)
			return
		}
	case kindCollection:
		if err := f.deleteCollection(r.Context(), res.bucket, res.collectionPrefix()); err != nil {
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

// deleteCollection removes every key under the prefix. The loop re-lists
// until the prefix is empty (guarding against concurrent puts during the
// delete) with a bounded retry count — termination is guaranteed: bounded
// retries then a 500 with a clear message, never a hang (leaf 03 Task 4).
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
		for _, key := range keys {
			if err := f.be.Delete(ctx, bucket, key); err != nil {
				return err
			}
		}
		// Loop: re-list and confirm the prefix is empty before returning.
	}
}
