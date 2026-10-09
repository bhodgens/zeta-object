package ipc

// fileprovider.go - the fileprovider-2026-10 leaf-01 protocol additions:
// the namespace surface a FileProvider extension speaks. Everything here
// is ADDITIVE to the leaf-01/07/08 wire shapes: one new optional Request
// field (to, the move destination), responses ride the existing Extra
// field, and the {v, ok, data|error} envelope is untouched. A daemon
// whose handler does not implement FileProviderSource answers the new
// types with a capability error (the same rule leaf 08 uses for
// ConflictResolver).
//
// THE DATA/METADATA CONTRACT (leaf 01): data flows via the FILE, metadata
// via IPC. Every entry carries cachePath - the path of the backing file
// RELATIVE to the daemon's cacheDir (slash-separated, "files/<key>") - so
// the extension (same user) opens the bytes directly at
// <cacheDir>/<cachePath> without any byte ever crossing the socket.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DownloadTimeout bounds one IPC download request: the request blocks
// until the engine's hydration finishes or this deadline fires.
const DownloadTimeout = 30 * time.Second

// ItemEntry is one namespace row (enumerate/item payloads): everything a
// FileProvider item needs. Key is the server key (the item identity);
// Name is the leaf component; CachePath is the cache-relative backing
// file path ("files/<key>", or "" for dirs - dirs have no bytes).
type ItemEntry struct {
	Name string `json:"name"`
	Key  string `json:"key"`
	IsDir bool  `json:"isDir"`
	Size int64  `json:"size"`
	// Mtime is unix seconds (0 = unknown).
	Mtime int64 `json:"mtime"`
	// Materialized reports that the backing cache file exists on disk
	// (dir entries are never materialized).
	Materialized bool `json:"materialized"`
	// Dirty reports unsynced local edits (upload pending).
	Dirty bool `json:"dirty"`
	// CachePath is relative to the daemon's cacheDir, slash-separated.
	CachePath string `json:"cachePath"`
}

// EnumerateData is the enumerate payload.
type EnumerateData struct {
	Entries []ItemEntry `json:"entries"`
}

// ItemData is the item (lookup) payload.
type ItemData struct {
	Item ItemEntry `json:"item"`
}

// DownloadData is the download (hydrate) ack.
type DownloadData struct {
	Path         string `json:"path"`
	Materialized bool   `json:"materialized"`
	CachePath    string `json:"cachePath"`
}

// MarkData is the mark (dirty + upload-trigger) ack.
type MarkData struct {
	Path  string `json:"path"`
	Dirty bool   `json:"dirty"`
}

// DeleteData is the delete ack (tombstone recorded; the sync pushes the
// remote DELETE).
type DeleteData struct {
	Path string `json:"path"`
}

// MoveData is the move (rename) ack.
type MoveData struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// FileProviderSource is the additive capability seam for the leaf-01
// (fileprovider-2026-10) requests. A Handler may ALSO implement it; the
// server type-asserts per request and answers the capability error when
// it does not. All methods must be safe for concurrent use.
type FileProviderSource interface {
	// Enumerate lists the DIRECT children of dirKey ("" = the root;
	// a subdirectory key carries its trailing slash) from the daemon's
	// index (the sync's PROPFIND-cached truth; never the network).
	Enumerate(dirKey string) ([]ItemEntry, error)
	// Item returns the single entry for path (the Lookup analog) or an
	// error naming the path when it is unknown to the index.
	Item(path string) (ItemEntry, error)
	// Download hydrates path through the engine's download path and
	// BLOCKS until the cache file is materialized or ctx is done. The
	// returned entry reflects the post-hydration state.
	Download(ctx context.Context, path string) (ItemEntry, error)
	// Dehydrate evicts ONE clean file (refuses dirty, pinned,
	// tombstoned, and unknown paths with an error naming the reason).
	Dehydrate(path string) error
	// Mark flags the (already written) cache file at path as dirty so
	// the prompt-upload loop pushes it. The extension's create flow
	// writes the bytes into the cache dir itself (same user) and calls
	// this - the data via the file, the state via IPC.
	Mark(path string) (MarkData, error)
	// Delete tombstones path: the cache file is removed and the sync's
	// matrix-6 pass issues the remote DELETE.
	Delete(path string) (DeleteData, error)
	// Move renames path to dest: cache file rename + index row
	// re-point; the sync then deletes the old key remotely and uploads
	// the new one.
	Move(path, dest string) (MoveData, error)
}

// normalizeDirKey canonicalizes an enumerate path: no leading slash, a
// non-root dir key always ends with "/".
func normalizeDirKey(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// normalizePath canonicalizes a file path: no leading or trailing slash.
func normalizePath(p string) string {
	return strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/")
}

// handleFileProvider serves the fileprovider-2026-10 request types against
// the additive FileProviderSource capability. handler may be nil or lack
// the capability; both answer the capability error.
func handleFileProvider(req Request, handler Handler) Response {
	src, ok := handler.(FileProviderSource)
	if handler == nil || !ok {
		return Response{V: Version, OK: false, Error: "not supported: no file provider handler"}
	}
	switch req.Type {
	case "enumerate":
		entries, err := src.Enumerate(normalizeDirKey(req.Path))
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		if entries == nil {
			entries = []ItemEntry{} // empty dir, never null (Swift arrays)
		}
		return Response{V: Version, OK: true, Extra: EnumerateData{Entries: entries}}
	case "item":
		if req.Path == "" {
			return Response{V: Version, OK: false, Error: "item requires path"}
		}
		entry, err := src.Item(normalizePath(req.Path))
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: ItemData{Item: entry}}
	case "download":
		if req.Path == "" {
			return Response{V: Version, OK: false, Error: "download requires path"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), DownloadTimeout)
		defer cancel()
		entry, err := src.Download(ctx, normalizePath(req.Path))
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: DownloadData{
			Path: entry.Key, Materialized: entry.Materialized, CachePath: entry.CachePath,
		}}
	case "dehydrate":
		if req.Path == "" {
			return Response{V: Version, OK: false, Error: "dehydrate requires path"}
		}
		if err := src.Dehydrate(normalizePath(req.Path)); err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: EvictData{Path: normalizePath(req.Path)}}
	case "mark":
		if req.Path == "" {
			return Response{V: Version, OK: false, Error: "mark requires path"}
		}
		data, err := src.Mark(normalizePath(req.Path))
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: data}
	case "delete":
		if req.Path == "" {
			return Response{V: Version, OK: false, Error: "delete requires path"}
		}
		data, err := src.Delete(normalizePath(req.Path))
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: data}
	case "move":
		if req.Path == "" || req.Dest == "" {
			return Response{V: Version, OK: false, Error: "move requires path and to fields"}
		}
		data, err := src.Move(normalizePath(req.Path), normalizePath(req.Dest))
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: data}
	}
	// Unreachable: the caller only routes the seven leaf types here.
	return Response{V: Version, OK: false, Error: fmt.Sprintf("unhandled file provider request %q", req.Type)}
}
