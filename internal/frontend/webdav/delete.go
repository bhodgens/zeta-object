// delete.go — DELETE (leaf 03 Task 4): files delete directly; collections
// delete recursively (Depth-infinity delete IS expressible: List + Delete
// loop with full pagination). Bucket deletion and root deletion are 403 —
// bucket deletion is not expressible safely over WebDAV (no seam method,
// and a runaway recursive delete of a whole bucket is never a default).
package webdav

import (
	"context"
	"net/http"

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
