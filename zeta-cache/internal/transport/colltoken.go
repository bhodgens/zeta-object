package transport

// colltoken.go - the memfs stub's derivation of the per-collection
// getetag ("dir-<hex>" immediate-children token). It mirrors the
// gateway's internal/frontend/webdav/colltoken.go: FNV-1a over the
// sorted name|size|mtime tuples of the collection's IMMEDIATE children
// (collection children contribute size -1 and no mtime). The stub must
// speak the same token dialect as the server so the sync engine's
// collection-token skip is exercised for real in unit tests; wire-level
// parity with the gateway is pinned by server e2e 19g, not here.

import (
	"fmt"
	"sort"
	"strings"
)

// dirSig is one child's contribution to its parent collection's token
// (mirrors webdav.childSig).
type dirSig struct {
	name string
	size int64 // -1 for collection children
	mod  int64 // UnixNano; 0 for collection children
}

// dirTokenHex folds sorted child signatures into the dir-<hex> token.
// Derived fresh per call, never stored (charter: derived tokens).
func dirTokenHex(sigs []dirSig) string {
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
			fold(fmt.Sprintf("%d", s.mod))
		}
		foldSep()
	}
	return fmt.Sprintf("dir-%016x", h)
}

// immediateChildren collects the direct-child signatures of key
// ("" = bucket root) from the stub's node map. Caller holds m.mu.
func (m *MemFS) immediateChildren(key string) []dirSig {
	prefix := key
	if prefix != "" {
		prefix += "/"
	}
	var sigs []dirSig
	for k, n := range m.nodes {
		if n.tomb || k == key || !strings.HasPrefix(k, prefix) {
			continue
		}
		rel := k[len(prefix):]
		if rel == "" || strings.Contains(rel, "/") {
			continue // direct children only
		}
		if n.dir {
			sigs = append(sigs, dirSig{name: rel, size: -1})
			continue
		}
		sigs = append(sigs, dirSig{name: rel, size: int64(len(n.body)), mod: n.modTime.UnixNano()})
	}
	return sigs
}

// dirETagLocked is the collection getetag for key ("" = root):
// "dir-<hex>" over its immediate children. Caller holds m.mu.
func (m *MemFS) dirETagLocked(key string) string {
	return dirTokenHex(m.immediateChildren(key))
}

// fileETagLocked is the file getetag: the stored body MD5 (the server
// contract: ETag IS the MD5). Caller holds m.mu.
func (m *MemFS) fileETagLocked(n *memNode) string {
	return md5hex(n.body)
}
