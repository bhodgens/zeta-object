package main

// fileprovider_handler.go - fileprovider-2026-10 leaf 01's daemon side:
// guiHandler ALSO implements ipc.FileProviderSource, the additive IPC
// capability the FileProvider extension speaks (leaf 02). Same rules as
// the leaf-08 surface: one additive seam, honest capability errors, and
// the data-via-FILE / metadata-via-IPC contract - every entry carries the
// cache-relative backing path so the extension opens bytes directly from
// <cacheDir>/files/<key> without a byte crossing the socket.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
)

var _ ipc.FileProviderSource = (*guiHandler)(nil)

// cacheFilePath mirrors the engine's on-disk layout: <cacheDir>/files/<key>.
func (h *guiHandler) cacheFilePath(key string) string {
	return filepath.Join(h.cfg.CacheDir, "files", filepath.FromSlash(key))
}

// leafName returns the path's last component (dirs without the slash):
// "a/b/c" -> "c", "a/b/" -> "b", "top" -> "top".
func leafName(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// entryFromRow maps one index row to an ItemEntry. isDir comes from the
// trailing-slash path convention (dir rows are recorded with it by the
// sync's token recording); materialized is the DISK truth (os.Stat), not
// the index's hydrated flag - they diverge briefly between the rename
// into files/ and the index commit, and the extension must never promise
// bytes that are not on disk.
func (h *guiHandler) entryFromRow(ctx context.Context, path string, row index.Resource) ipc.ItemEntry {
	entry := ipc.ItemEntry{
		Name:  leafName(path),
		Key:   path,
		IsDir: strings.HasSuffix(path, "/"),
		Size:  row.Size,
		Mtime: row.Mtime,
		Dirty: row.Dirty,
	}
	if entry.IsDir {
		return entry
	}
	if fi, err := os.Stat(h.cacheFilePath(path)); err == nil {
		entry.Materialized = true
		entry.Size = fi.Size()
		if row.Mtime == 0 {
			entry.Mtime = fi.ModTime().Unix()
		}
	}
	entry.CachePath = "files/" + path
	return entry
}

// Enumerate lists the direct children of dirKey ("" = root; a subdir key
// carries its trailing slash) from the INDEX - the sync's PROPFIND-cached
// truth, never the network (charter: enumerate = the daemon's cached
// index). Sources: file rows (direct children), dir-token rows (direct
// subdirectories), and dirty file rows collapsed to their immediate
// subdirectory when the row is deeper (a dirty local-only file two levels
// down must still make its parent dir visible). Tombstoned rows never
// surface (deleted keys do not appear, same rule as the server listing).
func (h *guiHandler) Enumerate(dirKey string) ([]ipc.ItemEntry, error) {
	rows, err := h.db.AllPaths(h.ctx())
	if err != nil {
		return nil, fmt.Errorf("enumerate %q: %w", dirKey, err)
	}
	out := make([]ipc.ItemEntry, 0)
	seen := map[string]bool{}
	for _, p := range rows {
		if !strings.HasPrefix(p, dirKey) || p == dirKey {
			continue
		}
		rel := strings.TrimPrefix(p, dirKey)
		if rel == "" {
			continue
		}
		if isDirectChild(rel) {
			if seen[rel] {
				continue
			}
			seen[rel] = true
			entry, ok, err := h.entryFor(p)
			if err != nil || !ok {
				continue // raced a prune/re-point; next enumerate is truthful
			}
			out = append(out, entry)
			continue
		}
		// Deeper row: collapse to the immediate subdirectory entry.
		sub := rel[:strings.Index(rel, "/")+1] // "a/" from "a/b/c"
		if seen[sub] {
			continue
		}
		seen[sub] = true
		out = append(out, ipc.ItemEntry{Name: strings.TrimSuffix(sub, "/"), Key: dirKey + sub, IsDir: true})
	}
	return out, nil
}

// isDirectChild reports whether rel ("b" or "b/") is one level below the
// enumerate dir.
func isDirectChild(rel string) bool {
	trimmed := strings.TrimSuffix(rel, "/")
	return !strings.Contains(trimmed, "/")
}

// entryFor builds the entry for one direct-child path; ok=false when the
// row is tombstoned or vanished mid-walk.
func (h *guiHandler) entryFor(p string) (ipc.ItemEntry, bool, error) {
	row, err := h.db.Get(h.ctx(), p)
	if errors.Is(err, index.ErrNotFound) {
		return ipc.ItemEntry{}, false, nil
	} else if err != nil {
		return ipc.ItemEntry{}, false, fmt.Errorf("enumerate %s: %w", p, err)
	}
	if row.Deleted {
		return ipc.ItemEntry{}, false, nil
	}
	return h.entryFromRow(h.ctx(), p, row), true, nil
}

// Item is the Lookup analog: the single entry for path, or an error
// naming the path when the index does not know it (the extension maps
// that to NSFileProviderError.noSuchItem).
func (h *guiHandler) Item(path string) (ipc.ItemEntry, error) {
	ctx := h.ctx()
	row, err := h.db.Get(ctx, path)
	if errors.Is(err, index.ErrNotFound) {
		return ipc.ItemEntry{}, fmt.Errorf("no such item: %s", path)
	} else if err != nil {
		return ipc.ItemEntry{}, fmt.Errorf("item %s: %w", path, err)
	}
	if row.Deleted {
		return ipc.ItemEntry{}, fmt.Errorf("no such item: %s (deleted)", path)
	}
	return h.entryFromRow(ctx, path, row), nil
}

// Download hydrates path through the ENGINE's download path. SyncOnce
// runs the matrix-1/7 diff-and-apply (a targeted single-file download is
// the same pipeline; the plan's constraint forbids editing internal/sync,
// so the engine's existing path IS the hydration entry). The request
// BLOCKS until the cache file is on disk or ctx is done - the documented
// poll-until-ready semantics.
func (h *guiHandler) Download(ctx context.Context, path string) (ipc.ItemEntry, error) {
	engine, err := h.requireEngine()
	if err != nil {
		return ipc.ItemEntry{}, err
	}
	// Already on disk: report without touching the network.
	row, err := h.db.Get(h.ctx(), path)
	if err == nil && !row.Deleted {
		if entry := h.entryFromRow(h.ctx(), path, row); entry.Materialized {
			return entry, nil
		}
	} else if err != nil && !errors.Is(err, index.ErrNotFound) {
		return ipc.ItemEntry{}, fmt.Errorf("download %s: %w", path, err)
	}
	// Ensure the dir the engine's pipeline stages into exists (the
	// initial sync normally creates it; a mount-less daemon may not).
	if err := os.MkdirAll(filepath.Join(h.cfg.CacheDir, "staging"), 0o700); err != nil {
		return ipc.ItemEntry{}, fmt.Errorf("download %s: staging: %w", path, err)
	}
	if err := engine.SyncOnce(ctx); err != nil {
		return ipc.ItemEntry{}, fmt.Errorf("download %s: sync: %w", path, err)
	}
	row, err = h.db.Get(h.ctx(), path)
	if errors.Is(err, index.ErrNotFound) {
		return ipc.ItemEntry{}, fmt.Errorf("no such item: %s", path)
	} else if err != nil {
		return ipc.ItemEntry{}, fmt.Errorf("download %s: %w", path, err)
	}
	entry := h.entryFromRow(h.ctx(), path, row)
	if !entry.Materialized {
		return ipc.ItemEntry{}, fmt.Errorf("download %s: not materialized after sync (deadline: %w)", path, ctx.Err())
	}
	return entry, nil
}

// Dehydrate evicts ONE clean file: the same refusals as the leaf-08
// evict (dirty, pinned, tombstoned, unknown), the same two-step contract
// (cache file first, then index demotion via store.Evict).
func (h *guiHandler) Dehydrate(path string) error {
	return h.EvictPath(path)
}

// Mark flags the extension-written cache file dirty so the prompt-upload
// loop uploads it (data via the FILE, state via IPC). The row is created
// or refreshed with the DISK truth (size/mtime from the file itself);
// dirty rows keep their known-good ETag so the sync uses If-Match.
func (h *guiHandler) Mark(path string) (ipc.MarkData, error) {
	ctx := h.ctx()
	fi, err := os.Stat(h.cacheFilePath(path))
	if err != nil {
		return ipc.MarkData{}, fmt.Errorf("mark %s: cache file missing (the extension writes bytes BEFORE mark): %w", path, err)
	}
	prev := index.Resource{}
	row, err := h.db.Get(ctx, path)
	if err == nil {
		prev = row
	} else if !errors.Is(err, index.ErrNotFound) {
		return ipc.MarkData{}, fmt.Errorf("mark %s: %w", path, err)
	}
	next := index.Resource{
		Path: path, ETag: prev.ETag, Mtime: fi.ModTime().Unix(),
		Size: fi.Size(), Hydrated: true, Dirty: true,
		Pinned: prev.Pinned,
	}
	if err := h.db.PutPath(ctx, next, index.OpLocalWrite, "fileprovider mark"); err != nil {
		return ipc.MarkData{}, fmt.Errorf("mark %s: %w", path, err)
	}
	return ipc.MarkData{Path: path, Dirty: true}, nil
}

// Delete tombstones the path: cache file removed + tombstone row (the
// sync's matrix-6 pass issues the remote DELETE). Unknown paths are fine
// (DeletePath tombstones either way, mirroring the FUSE layer).
func (h *guiHandler) Delete(path string) (ipc.DeleteData, error) {
	if err := os.Remove(h.cacheFilePath(path)); err != nil && !os.IsNotExist(err) {
		return ipc.DeleteData{}, fmt.Errorf("delete %s: unlink: %w", path, err)
	}
	if err := h.db.DeletePath(h.ctx(), path, "fileprovider delete"); err != nil {
		return ipc.DeleteData{}, fmt.Errorf("delete %s: %w", path, err)
	}
	return ipc.DeleteData{Path: path}, nil
}

// Move renames the cache file and re-points the index row (the
// adapter.MoveKey shape: new row, then the old key's tombstone - the
// sync's diff deletes the old key remotely and uploads the new one).
// Moving an unknown path is an honest error (the extension only moves
// live items); moving a tombstoned path is refused.
func (h *guiHandler) Move(path, dest string) (ipc.MoveData, error) {
	ctx := h.ctx()
	row, err := h.db.Get(ctx, path)
	if errors.Is(err, index.ErrNotFound) {
		return ipc.MoveData{}, fmt.Errorf("no such item: %s", path)
	} else if err != nil {
		return ipc.MoveData{}, fmt.Errorf("move %s: %w", path, err)
	}
	if row.Deleted {
		return ipc.MoveData{}, fmt.Errorf("move %s: refused, path is tombstoned", path)
	}
	oldPath := h.cacheFilePath(path)
	newPath := h.cacheFilePath(dest)
	if _, err := os.Stat(oldPath); err == nil {
		if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
			return ipc.MoveData{}, fmt.Errorf("move %s: mkdir: %w", path, err)
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return ipc.MoveData{}, fmt.Errorf("move %s -> %s: rename: %w", path, dest, err)
		}
	}
	next := row
	next.Path = dest
	if err := h.db.PutPath(ctx, next, index.OpLocalWrite, "fileprovider rename"); err != nil {
		return ipc.MoveData{}, fmt.Errorf("move %s: %w", path, err)
	}
	if err := h.db.DeletePath(ctx, path, "fileprovider rename (old key)"); err != nil {
		return ipc.MoveData{}, fmt.Errorf("move %s: %w", path, err)
	}
	return ipc.MoveData{From: path, To: dest}, nil
}
