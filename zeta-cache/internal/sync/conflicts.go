package sync

// conflicts.go - the leaf-08 conflict surface: a persistent record of
// every conflict the engine surfaces (matrix 3 conflict copies and
// matrix 5 kept-dirty-on-remote-delete), plus the resolve and restore
// actions the IPC layer calls.
//
// Records are meta rows ("conflict:<path>" -> JSON ConflictRecord) so
// they live in the CLIENT index under the client-state charter - no new
// table, no schema bump, and the list survives daemon restarts (the
// count-only conflict counter in engine.go predates this and is kept).
// A resolve deletes the record; the next scan simply re-records a new
// conflict if reality still disagrees.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// Resolve modes (mirrors the IPC constants without importing it: the
// protocol layer already imports nothing from sync and sync must not
// depend on the wire shapes).
const (
	ResolveKeepLocal  = "keep-local"  // the local copy wins (uploaded)
	ResolveKeepRemote = "keep-remote" // the remote copy wins (downloaded over the local)
)

// ConflictRecord is one pending conflict, persisted under the meta key
// metaConflictPrefix + path.
type ConflictRecord struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "conflict-copy" | "kept-local"
	// CopyPath is the preserved local copy ("" for kept-local).
	CopyPath string `json:"copyPath,omitempty"`
	// DetectedAt is unix seconds when the conflict first surfaced.
	DetectedAt int64 `json:"detectedAt"`
}

const metaConflictPrefix = "conflict:"

// recordConflict persists a conflict record (idempotent: the first
// detection wins so DetectedAt stays stable across rescans). Best-effort:
// a record failure must never fail the sync pass itself.
func (e *Engine) recordConflict(ctx context.Context, rec ConflictRecord) {
	key := metaConflictPrefix + rec.Path
	if _, err := e.store.MetaGet(ctx, key); err == nil {
		return // already recorded
	}
	b, err := json.Marshal(rec)
	if err != nil {
		e.log.Printf("sync: encoding conflict record %s: %v", rec.Path, err)
		return
	}
	if err := e.store.MetaSet(ctx, key, string(b)); err != nil {
		e.log.Printf("sync: recording conflict %s: %v", rec.Path, err)
	}
}

// Conflicts lists the pending conflict records sorted by path.
func (e *Engine) Conflicts(ctx context.Context) ([]ConflictRecord, error) {
	vals, err := e.store.MetaListPrefix(ctx, metaConflictPrefix)
	if err != nil {
		return nil, fmt.Errorf("sync: conflict list: %w", err)
	}
	out := make([]ConflictRecord, 0, len(vals))
	for _, v := range vals {
		var rec ConflictRecord
		if err := json.Unmarshal([]byte(v), &rec); err != nil {
			e.log.Printf("sync: skipping corrupt conflict record: %v", err)
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ResolveConflict applies a user resolution to one conflicted path:
//
//   - keep-local: the LOCAL copy of path is uploaded (If-Match with the
//     row's known-good etag; a row without one uploads as a new file) and
//     the row goes clean. The server copy is overwritten.
//   - keep-remote: the REMOTE copy is downloaded over the local path and
//     the row goes clean. The local divergent bytes are gone except for
//     the preserved conflicted copy.
//
// The conflicted-copy FILE is removed only when removeCopy is true (the
// explicit user flag; the leaf forbids implicit deletion). The conflict
// record is cleared either way - the user decided.
func (e *Engine) ResolveConflict(ctx context.Context, path, mode string, removeCopy bool) error {
	if mode != ResolveKeepLocal && mode != ResolveKeepRemote {
		return fmt.Errorf("sync: resolve mode %q is not keep-local|keep-remote", mode)
	}
	key := metaConflictPrefix + path
	var rec ConflictRecord
	v, err := e.store.MetaGet(ctx, key)
	if errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("sync: no conflict recorded for %s", path)
	} else if err != nil {
		return fmt.Errorf("sync: conflict lookup %s: %w", path, err)
	}
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		return fmt.Errorf("sync: corrupt conflict record for %s: %w", path, err)
	}

	switch mode {
	case ResolveKeepLocal:
		// The user's divergent bytes for a conflict-copy record live
		// in the PRESERVED COPY (matrix 3 applied the remote over the
		// live path during the scan); restore them into the live path
		// first so the upload really pushes the local version. A
		// kept-local record has no copy - the live path already IS the
		// local version.
		if rec.Kind == "conflict-copy" && rec.CopyPath != "" {
			if err := copyFile(e.cachePath(rec.CopyPath), e.cachePath(path)); err != nil {
				return fmt.Errorf("sync: resolve keep-local %s: restoring local copy: %w", path, err)
			}
		}
		// Upload the (now local-version) file. The row's etag (if any)
		// is the If-Match precondition; conflict-copy rows carry no
		// etag and upload as new files.
		row, err := e.store.Get(ctx, path)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return fmt.Errorf("sync: resolve get %s: %w", path, err)
		}
		rep := Report{}
		if err := e.upload(ctx, path, row.ETag, &rep); err != nil {
			return fmt.Errorf("sync: resolve keep-local %s: %w", path, err)
		}
	case ResolveKeepRemote:
		// Download the remote over the local. The next listing's etag
		// is the truth; one Depth-0 probe of the parent gets it.
		entries, err := e.tr.Propfind(ctx, parentDirOf(path), false)
		if err != nil {
			return fmt.Errorf("sync: resolve probe %s: %w", path, err)
		}
		var serverETag string
		found := false
		for i := range entries {
			if entries[i].Key == path && !entries[i].IsDir {
				serverETag, found = entries[i].ETag, true
				break
			}
		}
		if !found {
			return fmt.Errorf("sync: resolve keep-remote %s: the server no longer lists the path", path)
		}
		rep := Report{}
		if err := e.download(ctx, path, serverETag, &rep); err != nil {
			return fmt.Errorf("sync: resolve keep-remote %s: %w", path, err)
		}
	}

	// Explicit flag ONLY: remove the preserved copy file. The record
	// carries the copy path, so a copy made under a different date than
	// the record's naming still resolves correctly.
	if removeCopy && rec.CopyPath != "" {
		if err := os.Remove(e.cachePath(rec.CopyPath)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("sync: resolve %s: removing copy %s: %w", path, rec.CopyPath, err)
		}
		// The copy's own index row (and its tombstone history) goes
		// with the file: a deleted copy must not re-upload from a
		// stale dirty row.
		if err := e.store.DeletePath(ctx, rec.CopyPath, "conflict copy removed by user resolve"); err != nil {
			return fmt.Errorf("sync: resolve %s: unrecording copy %s: %w", path, rec.CopyPath, err)
		}
	}
	return e.store.MetaDelete(ctx, key)
}

// RestoreDeleted re-downloads a tombstoned path if the server still
// serves it (or a version of it). An unrecoverable path (not served
// anymore) is an HONEST error - never a silent ok. Restore flips the
// tombstone back to a clean hydrated row via the download pipeline.
func (e *Engine) RestoreDeleted(ctx context.Context, path string) error {
	row, err := e.store.Get(ctx, path)
	if err != nil {
		return fmt.Errorf("sync: restore %s: no index row: %w", path, err)
	}
	if !row.Deleted {
		return fmt.Errorf("sync: restore %s: not deleted", path)
	}
	entries, err := e.tr.Propfind(ctx, parentDirOf(path), false)
	if err != nil {
		if errors.Is(err, transport.ErrNotExist) && parentDirOf(path) == "" {
			return fmt.Errorf("sync: restore %s: the server no longer serves the path (grace window is the only recovery)", path)
		}
		return fmt.Errorf("sync: restore probe %s: %w", path, err)
	}
	var serverETag string
	found := false
	for i := range entries {
		if entries[i].Key == path && !entries[i].IsDir {
			serverETag, found = entries[i].ETag, true
			break
		}
	}
	if !found {
		return fmt.Errorf("sync: restore %s: the server no longer serves the path (grace window is the only recovery)", path)
	}
	rep := Report{}
	if err := e.download(ctx, path, serverETag, &rep); err != nil {
		return fmt.Errorf("sync: restore %s: %w", path, err)
	}
	return nil
}
