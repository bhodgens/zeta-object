// colltoken.go — derived collection change tokens (getetag on collections).
//
// A WebDAV collection is a namespace shape with no object behind it, so it
// has no stored ETag: resolveKind returns a zero objectmodel.Object and the
// collection's getetag rendered as "" (quoted empty) before this file
// existed. Sync clients need a CHANGE TOKEN on directories: one Depth-0
// PROPFIND of a directory answers "did anything in here change?" without a
// full Depth-1 walk.
//
// Derivation (shape "immediate children"): FNV-1a over the sorted
// name+size+mtime tuples of the collection's IMMEDIATE children. At Depth 1
// the tuples come from the SAME child rows the 207 already lists
// (tokenFromChildren — no extra backend round-trip); at Depth 0 a dedicated
// delimiter List walk collects them (collectionToken). NOTHING is persisted
// — the token is derived fresh per request, exactly like oc:fileid (charter:
// derived, never stored). The token changes when a direct child is created,
// overwritten, deleted, or renamed; a change strictly below a child
// directory does NOT move this token — the client walks down changed
// directories, which is the bounded-walk shape the zeta-cache scan engine is
// designed against.
//
// Failure policy: any List error yields the EMPTY token (the same honest
// degradation entryFor already applies to oc:size) — the token is an
// optimization hint, never a correctness input, so a transient error must
// not fail the PROPFIND. The client re-verifies with ETags before acting.
package webdav

import (
	"context"
	"fmt"
	"sort"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// childSig is one child's contribution to its parent collection's token.
type childSig struct {
	name string
	size int64 // -1 for collection rows (no stored size)
	mod  int64 // UnixNano; 0 for collection rows
}

// collectionTokenSig folds the sorted child signatures into the token hex.
func collectionTokenSig(sigs []childSig) string {
	sort.Slice(sigs, func(i, j int) bool { return sigs[i].name < sigs[j].name })
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	fold := func(s string) {
		for _, c := range []byte(s) {
			h ^= uint64(c)
			h *= prime64
		}
	}
	foldSep := func() { h ^= uint64('|'); h *= prime64 }
	for _, s := range sigs {
		fold(s.name)
		foldSep()
		fold(fmt.Sprintf("%d", s.size))
		foldSep()
		if s.mod != 0 {
			// UnixNano keeps mtime in the token even when size is unchanged
			// (an in-place edit that preserves length must still move it).
			fold(fmt.Sprintf("%d", s.mod))
		}
		foldSep()
	}
	return fmt.Sprintf("%016x", h)
}

// collectionToken derives the token for a collection prefix with a dedicated
// delimiter List walk (the Depth-0 path; Depth 1 reuses the listed children
// via tokenFromChildren instead). bucket "" (mode-A root) yields "" — the
// synthetic root spans buckets and has no single delimiter listing.
//
// hidden, when non-nil, is the delete-marker consult applied to every child
// key the walk returns. It MUST be supplied whenever the caller has a resolver
// wired: a versioning-Enabled DELETE leaves the data file on disk, so the raw
// List still returns the tombstoned key while the PROPFIND row set drops it
// (childEntries applies the same consult). Without the filter the token
// described a listing the server no longer serves, so it did NOT move across a
// deletion and a syncing client never learned the key was gone — measured
// before this fix (bughunt 2026-10-06 M2):
//
//	/2024/ token before DELETE = dir-611feda296f6e125
//	/2024/ token after  marker  = dir-611feda296f6e125   (unchanged)
//
// Common-prefix children are NOT filtered: a collection is a namespace shape
// with no object behind it, so no delete marker can hide one.
func collectionToken(ctx context.Context, be backend.Backend, bucket, prefix string, hidden func(key string) bool) string {
	if bucket == "" {
		return ""
	}
	var sigs []childSig
	token := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return "" // same bound-failure policy as collectionSize
		}
		p, err := be.List(ctx, bucket, objectmodel.ListParams{
			Prefix:            prefix,
			Delimiter:         "/",
			MaxKeys:           propfindPageKeys,
			ContinuationToken: token,
		})
		if err != nil {
			return "" // fail soft: hint, not correctness
		}
		for _, obj := range p.Objects {
			if hidden != nil && hidden(obj.Key) {
				continue // same visibility rule childEntries applies
			}
			sigs = append(sigs, childSig{name: obj.Key, size: obj.Size, mod: obj.LastModified.UnixNano()})
		}
		for _, cp := range p.CommonPrefixes {
			sigs = append(sigs, childSig{name: cp, size: -1})
		}
		if !p.IsTruncated || p.NextToken == "" || p.NextToken == token {
			break
		}
		token = p.NextToken
	}
	return collectionTokenSig(sigs)
}

// tokenFromChildren derives a collection's token from the child rows the
// Depth-1 PROPFIND already listed — no extra backend round-trip.
func tokenFromChildren(children []propfindEntry) string {
	sigs := make([]childSig, 0, len(children))
	for _, c := range children {
		if !c.found {
			continue
		}
		if c.isColl {
			sigs = append(sigs, childSig{name: c.href, size: -1})
			continue
		}
		sigs = append(sigs, childSig{
			name: c.obj.Key,
			size: c.obj.Size,
			mod:  c.obj.LastModified.UnixNano(),
		})
	}
	if len(sigs) == 0 {
		return ""
	}
	return collectionTokenSig(sigs)
}

// tokenForEntry resolves the token for a collection entry with a dedicated
// List walk (Depth 0 / single-row paths), leaving files and not-found rows
// untouched.
//
// The walk applies the SAME delete-marker visibility rule childEntries applies
// to the Depth-1 rows, so the Depth-0 token and the Depth-1 listing describe one
// state. Passing the consult through here is the whole M2 fix: the token must
// move when a child is tombstoned, because a caching client reads "unchanged
// token" as "nothing changed here".
func (f *Frontend) tokenForEntry(ctx context.Context, e *propfindEntry, prefix string) {
	if !e.isColl || !e.found || e.bucket == "" {
		return
	}
	e.collToken = collectionToken(ctx, f.be, e.bucket, prefix, func(key string) bool {
		return f.objectHiddenByDeleteMarker(e.bucket, key)
	})
}
