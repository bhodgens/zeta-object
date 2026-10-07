package main

// adapter.go - leaf 02's glue between main.go and the fusefs.Index seam,
// sitting on leaf 03's index.Store (internal/index is leaf 03-owned; this
// file only CALLS it, so the two leaves never collide on a file).
// Counters and the prefix listing the Store does not expose as single
// queries yet are derived HERE - the leaf-02 report's "for leaf 03" list
// asks for first-class Count/ListPrefixed methods to replace these walks.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/fusefs"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

// indexAdapter adapts index.Store to the narrow fusefs.Index seam.
type indexAdapter struct {
	store *index.Store
	ctx   context.Context
}

func newIndexAdapter(store *index.Store) fusefs.Index {
	return indexAdapter{store: store, ctx: context.Background()}
}

func (a indexAdapter) CleanETag(key string) (string, error) {
	r, err := a.store.Get(a.ctx, key)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("CleanETag %s: %w", key, err)
	}
	if r.Dirty {
		return "", nil
	}
	return r.ETag, nil
}

func (a indexAdapter) MarkDirty(key string, size, mtime int64) error {
	r := index.Resource{Path: key, Size: size, Mtime: mtime, Dirty: true, Hydrated: true}
	if err := a.store.PutPath(a.ctx, r, "fuse-flush", "dirty=1 at Flush"); err != nil {
		return fmt.Errorf("MarkDirty %s: %w", key, err)
	}
	return nil
}

func (a indexAdapter) MarkClean(key string, etag string, size int64) error {
	r := index.Resource{Path: key, ETag: etag, Size: size, Hydrated: true}
	if err := a.store.PutPath(a.ctx, r, "upload-confirmed", "dirty=0 after verified PUT"); err != nil {
		return fmt.Errorf("MarkClean %s: %w", key, err)
	}
	return nil
}

func (a indexAdapter) RecordHydrated(key, etag string, size, mtime int64) error {
	r := index.Resource{Path: key, ETag: etag, Size: size, Mtime: mtime, Hydrated: true}
	if err := a.store.PutPath(a.ctx, r, "hydrate", "full-file hydration"); err != nil {
		return fmt.Errorf("RecordHydrated %s: %w", key, err)
	}
	return nil
}

func (a indexAdapter) DirtyKeys() ([]string, error) {
	return a.store.DirtyPaths(a.ctx)
}

func (a indexAdapter) DirtyCount() (int, error) {
	keys, err := a.store.DirtyPaths(a.ctx)
	if err != nil {
		return 0, err
	}
	return len(keys), nil
}

func (a indexAdapter) HydratedCount() (int, error) {
	paths, err := a.store.CleanHydratedPaths(a.ctx, nil)
	if err != nil {
		return 0, err
	}
	return len(paths), nil
}

func (a indexAdapter) ListPrefixed(dirKey string) ([]fusefs.IndexRow, error) {
	// The Store has no prefix scan yet (leaf 03 item); walking DirtyPaths
	// is enough because the listing merge only NEEDS local-only dirty
	// rows - clean rows all come from the transport listing anyway.
	keys, err := a.store.DirtyPaths(a.ctx)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if dirKey != "" {
		prefix = dirKey + "/"
	}
	var out []fusefs.IndexRow
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || k == dirKey {
			continue
		}
		rel := strings.TrimSuffix(k[len(prefix):], "/")
		if rel == "" || strings.Contains(rel, "/") {
			continue // direct children only
		}
		out = append(out, fusefs.IndexRow{Key: k, IsDir: false})
	}
	return out, nil
}

func (a indexAdapter) Forget(key string) error {
	if err := a.store.DeletePath(a.ctx, key, "fuse-delete"); err != nil {
		return fmt.Errorf("Forget %s: %w", key, err)
	}
	return nil
}

func (a indexAdapter) MoveKey(oldKey, newKey string) error {
	r, err := a.store.Get(a.ctx, oldKey)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return nil // nothing tracked; the local rename still happened
		}
		return fmt.Errorf("MoveKey get %s: %w", oldKey, err)
	}
	r.Path = newKey
	if err := a.store.PutPath(a.ctx, r, "fuse-rename", "index row re-pointed"); err != nil {
		return fmt.Errorf("MoveKey put %s: %w", newKey, err)
	}
	if err := a.store.DeletePath(a.ctx, oldKey, "fuse-rename"); err != nil {
		return fmt.Errorf("MoveKey forget %s: %w", oldKey, err)
	}
	return nil
}

var _ fusefs.Index = indexAdapter{}
