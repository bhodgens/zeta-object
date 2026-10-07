// Package transport defines the client-protocol seam between the FUSE
// layer (leaf 02) and the HTTP client (leaf 06): the zeta-cache daemon
// speaks WebDAV to the gateway (plan-tree master decision 3), so the
// verbs are the webdav ones - PROPFIND listing, GET (Range-aware for a
// later partial-hydration stretch), PUT with If-Match (the server
// answers 412 on a stale ETag), MKCOL, DELETE, and MOVE (one atomic
// server-side rename; MOVE enforces If-Match on the destination).
//
// Leaf 02 codes ONLY against this interface and ships memfs.go, an
// in-memory stub for unit tests. Leaf 06 fills in the real HTTP
// implementation behind the same interface.
package transport

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotExist is returned by Get/Propfind when the server answers 404
// (the real transport maps HTTP 404 to it; memfs returns it directly).
// Callers map it to ENOENT.
var ErrNotExist = errors.New("transport: resource does not exist")

// ErrConflict is returned by Put/Move when the server answers 412
// Precondition Failed (stale If-Match ETag). The FUSE layer surfaces it
// as upload-failed and keeps the file dirty; conflict resolution is
// leaf 04's (plan-tree master decision 5).
var ErrConflict = errors.New("transport: precondition failed (412)")

// Entry is one row of a Propfind listing: the collection/file the server
// currently shows for a key. The listing is the reconciliation view
// (delete-marked keys are suppressed server-side; leaf-02 doc).
type Entry struct {
	// Key is the object key relative to the bucket root, "" for the
	// bucket collection itself. Directories end with "/". Reserved keys
	// (.metadata/, .zfs, .uploads) are never returned: the server never
	// lists them and the stub filters them the same way.
	Key       string
	IsDir     bool
	Size      int64
	ModTime   time.Time
	ETag      string // file content MD5 (webdav); dir-<hex> token for collections
	DelMarked bool   // always false: delete-marked keys never surface
}

// Transport is the webdav client surface the FUSE layer mounts over.
//
// Every key is relative to the bucket root, no leading slash, no
// trailing slash (directories are distinct: Mkdir("d"), then Propfind
// returns "d/"). Implementations must be safe for concurrent use.
type Transport interface {
	// Get streams the object body for key. The server answers 200 (full)
	// or 206 (Range; only sent when req is non-empty). io.EOF ends the
	// body. Returns ErrNotExist on 404.
	Get(ctx context.Context, key string, req *RangeRequest) (io.ReadCloser, *ObjectInfo, error)

	// Put uploads body (the transport sends Content-Length; the server
	// rejects chunked PUT bodies) with If-Match etag. An empty etag
	// means "create unconditionally" (If-Match: * on a missing object
	// would 412, so first-write uses no precondition). Returns the
	// server ETag, which IS the stored body MD5.
	Put(ctx context.Context, key string, body io.ReadSeeker, etag string) (serverETag string, err error)

	// Mkdir creates a collection (webdav MKCOL).
	Mkdir(ctx context.Context, key string) error

	// Delete removes a file (or an empty collection) server-side.
	// The real transport maps HTTP 404 to ErrNotExist; a 409/424 on a
	// non-empty collection maps to ErrNotEmpty.
	Delete(ctx context.Context, key string, etag string) error

	// Move renames oldKey to newKey atomically server-side (webdav
	// MOVE), enforcing If-Match destETag on the destination. An empty
	// destETag means no destination precondition. The server answers
	// 412 on a stale destination ETag (ErrConflict) and 409/424 when
	// the destination's parent collection is missing.
	Move(ctx context.Context, oldKey, newKey, destETag string) error

	// Propfind lists key (a collection; "" lists the bucket root)
	// non-recursively, sorted by Key, including the key itself as
	// entry 0 when recursive is false. Reserved names never surface.
	// Directories are entries ending in "/". Returns ErrNotExist on 404.
	Propfind(ctx context.Context, key string, recursive bool) ([]Entry, error)
}

// RangeRequest is a byte-range for partial hydration (leaf-02 stretch;
// the DEFAULT is full-file hydration, so the FUSE layer passes nil).
type RangeRequest struct {
	Start int64
	End   int64 // inclusive
}

// ObjectInfo is the metadata a Get response carries (Content-Length,
// Last-Modified, ETag).
type ObjectInfo struct {
	Size        int64
	ModTime     time.Time
	ETag        string // body MD5; quoted or raw, normalized here
	DelMarked   bool
	ContentType string
}

// ErrNotEmpty is returned by Delete on a non-empty collection.
var ErrNotEmpty = errors.New("transport: collection is not empty")
