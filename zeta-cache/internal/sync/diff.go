package sync

// diff.go - the diff engine: the locked conflict matrix, per path.
//
// Inputs per file key: the SERVER row (from the PROPFIND listing), the
// INDEX row (dirty flag + last-known-clean ETag), and the DISK file
// (the FUSE-visible cache file under files/<key>).
//
// THE LOCKED CONFLICT MATRIX (leaf 04; implement EXACTLY this):
//
//	| # | server            | index / disk            | action                        |
//	|---|-------------------|-------------------------|-------------------------------|
//	| 1 | present, changed  | clean local (hydrated)  | DOWNLOAD                      |
//	| 2 | present, unchanged| dirty local             | UPLOAD (If-Match known ETag)  |
//	| 3 | present, changed  | dirty local             | CONFLICT COPY (remote wins;   |
//	|   |                   |                         | local -> "f (conflicted copy  |
//	|   |                   |                         | YYYY-MM-DD).ext"; never merge)|
//	| 4 | deleted           | clean local             | delete locally + tombstone    |
//	| 5 | deleted           | dirty local             | KEEP local + surface conflict |
//	| 6 | present, unchanged| local deleted (tombstone)| DELETE remote (If-Match)     |
//	| 7 | present (new)     | no index row            | DOWNLOAD                      |
//	| 8 | absent            | no index row, file on disk| UPLOAD (If-None-Match: *)   |
//
// "changed remote" = listed ETag != index ETag; "unchanged" = equal.
// "dirty" is the index's dirty flag (FUSE writes); "clean local" is
// hydrated & !dirty. "local deleted" = index tombstone (deleted=1,
// set by the FUSE layer's Forget) with the remote still listing the key.

import (
	"context"
	"crypto/md5" // #nosec G401 -- the wire ETag IS the body MD5 (server contract); not a security use
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// Report summarizes one sync pass (counts surface in logs; the conflict
// counters feed the IPC status).
type Report struct {
	Scanned          int // file keys diffed
	Downloads        int // matrix 1, 7
	Uploads          int // matrix 2, 6, 8
	SkippedUploads   int // uploads avoided: MD5 already matches the index ETag
	RemoteDeletes    int // matrix 4 (tombstones written)
	LocalDeletes     int // remote DELETEs issued (matrix 6)
	ConflictCopies   int // matrix 3
	RemoteDeleteKept int // matrix 5 (kept-dirty conflicts surfaced)
	TokenVerified    int // subtrees skipped by matching collection token
	FullscanVerified int // subtrees actually listed
}

// diffAndApply classifies one file key and performs the action. listed
// is the server's listing row when the caller just got it from a walk
// (nil = probe the parent listing, the delta/unknown path).
func (e *Engine) diffAndApply(ctx context.Context, key string, listed *transport.Entry, rep *Report) error {
	rep.Scanned++

	row, err := e.store.Get(ctx, key)
	hasRow := err == nil
	if err != nil && !errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("sync: index get %s: %w", key, err)
	}
	_, onDisk := e.localStat(key)

	// Server row: use the freshly listed entry when we have it;
	// otherwise one Depth-0 probe of the parent collection.
	var serverETag string
	serverExists := false
	if listed != nil {
		serverETag, serverExists = listed.ETag, true
	} else {
		entries, err := e.tr.Propfind(ctx, parentDirOf(key), false)
		if err != nil {
			return fmt.Errorf("sync: propfind parent of %s: %w", key, err)
		}
		for i := range entries {
			if entries[i].Key == key && !entries[i].IsDir {
				serverETag, serverExists = entries[i].ETag, true
				break
			}
		}
	}

	switch {
	case !serverExists && !hasRow:
		// Matrix 8: new local, server never saw it.
		if !onDisk {
			return nil // nothing anywhere (race/cleanup): no action
		}
		return e.upload(ctx, key, "" /* If-None-Match: * */, rep)

	case serverExists && !hasRow:
		// Matrix 7: new remote.
		return e.download(ctx, key, serverETag, rep)

	case !serverExists && hasRow:
		if row.Deleted {
			// Matrix 6: local delete (tombstone) + remote unchanged
			// (still listing the old key) -> DELETE remote. A missing
			// remote row with a tombstone = the delete already won:
			// nothing to do.
			return e.deleteRemote(ctx, key, row.ETag, rep)
		}
		if row.Dirty {
			if row.ETag == "" {
				// Dirty with NO known-good etag = never uploaded (e.g.
				// a fresh conflict copy): it is a NEW LOCAL file ->
				// upload If-None-Match:* (matrix 8 semantics).
				return e.upload(ctx, key, "", rep)
			}
			// Matrix 5: remote deleted + dirty local (uploaded before,
			// server row gone) -> KEEP local, surface the conflict.
			rep.RemoteDeleteKept++
			e.log.Printf("sync: conflict: %s deleted remotely but has local edits - kept local", key)
			return nil
		}
		// Matrix 4: remote deleted + clean local -> delete locally +
		// tombstone.
		return e.deleteLocal(ctx, key, "remote deleted", rep)

	default: // serverExists && hasRow
		if row.Deleted {
			// Tombstone still fresh and remote STILL present: matrix 6.
			return e.deleteRemote(ctx, key, row.ETag, rep)
		}
		unchanged := row.ETag != "" && serverETag == row.ETag
		switch {
		case !row.Dirty && !unchanged:
			// Matrix 1: clean local + changed remote -> download.
			return e.download(ctx, key, serverETag, rep)
		case row.Dirty && unchanged:
			// Matrix 2: dirty local + unchanged remote -> upload
			// (If-Match with the index's known-good ETag).
			return e.upload(ctx, key, row.ETag, rep)
		case row.Dirty && !unchanged:
			// Matrix 3: dirty local + changed remote -> CONFLICT COPY.
			return e.conflictCopy(ctx, key, serverETag, row, rep)
		default:
			// Clean + unchanged: nothing to do (the common steady state).
			return nil
		}
	}
}

// download = the remote-apply pipeline: GET to temp+rename, index update
// (clean, etag=serverETag), journal hydrate row.
func (e *Engine) download(ctx context.Context, key, serverETag string, rep *Report) error {
	rc, info, err := e.tr.Get(ctx, key, nil)
	if err != nil {
		if errors.Is(err, transport.ErrNotExist) {
			return nil // vanished mid-scan; next sync reconciles
		}
		return fmt.Errorf("sync: get %s: %w", key, err)
	}
	defer rc.Close()
	if info != nil && info.ETag != "" {
		serverETag = info.ETag // the GET's own ETag is fresher than the listing's
	}
	dst := e.cachePath(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("sync: cache dir for %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(filepath.Join(e.cache, "staging"), "dl-*")
	if err != nil {
		return fmt.Errorf("sync: staging for %s: %w", key, err)
	}
	tmpName := tmp.Name()
	clean := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	size, err := io.Copy(tmp, rc)
	if err != nil {
		clean()
		return fmt.Errorf("sync: writing %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync: closing %s: %w", key, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync: rename into %s: %w", dst, err)
	}
	mtime := int64(0)
	if info != nil {
		mtime = info.ModTime.Unix()
	}
	// Index update + journal hydrate row in one transaction (PutPath).
	r := index.Resource{
		Path: key, ETag: serverETag, Mtime: mtime, Size: size,
		Hydrated: true, Dirty: false,
	}
	if err := e.store.PutPath(ctx, r, index.OpHydrate, "sync download"); err != nil {
		return fmt.Errorf("sync: index after download %s: %w", key, err)
	}
	rep.Downloads++
	return nil
}

// upload = the upload pipeline: read file -> MD5 (unchanged content
// skips the PUT) -> PUT with If-Match / If-None-Match -> journal
// upload-ok + clean index row. 412 = conflict pipeline; other 5xx-class
// errors leave the row dirty.
func (e *Engine) upload(ctx context.Context, key, ifMatch string, rep *Report) error {
	local := e.cachePath(key)
	etag, size, err := md5File(local)
	if err != nil {
		if os.IsNotExist(err) {
			// Dirty row whose disk file vanished: reconcile R1 could not
			// run (no diskCheck); the scan drops the row to not-hydrated
			// and leaves the decision to the next pass.
			e.log.Printf("sync: %s marked dirty but no local file - demoting hydration", key)
			return e.store.PutPath(ctx, index.Resource{Path: key, ETag: ifMatch}, "", "sync: dirty row without disk file demoted")
		}
		return fmt.Errorf("sync: hashing %s: %w", key, err)
	}
	// The server ETag IS the MD5 of the body: unchanged content = skip
	// the PUT entirely (idempotent re-sync).
	if ifMatch != "" && etag == ifMatch {
		rep.SkippedUploads++
		return e.store.PutPath(ctx, index.Resource{
			Path: key, ETag: etag, Size: size, Hydrated: true, Dirty: false,
		}, index.OpUploadOK, "sync: content unchanged, marked clean")
	}
	serverETag, err := e.tr.Put(ctx, key, mustOpen(local), ifMatch)
	if err != nil {
		if errors.Is(err, transport.ErrConflict) {
			if ifMatch == "" {
				// Create-race: someone else created the file between our
				// listing and the PUT. NOT a conflict-copy case (there is
				// no remote generation we are clobbering that our copy
				// logic could pair with) - leave dirty, the next pass
				// re-diffs against the now-existing row.
				return fmt.Errorf("sync: put %s: create raced an existing object (412, left dirty): %w", key, err)
			}
			// 412 on If-Match: our known-good ETag is stale - escalate
			// to the conflict pipeline.
			row, gerr := e.store.Get(ctx, key)
			if gerr != nil {
				return fmt.Errorf("sync: conflict lookup %s: %w", key, gerr)
			}
			return e.conflictCopy(ctx, key, "", row, rep)
		}
		// Any other failure (5xx, network): LEAVE DIRTY, the scheduler
		// retries per its backoff.
		return fmt.Errorf("sync: put %s (left dirty): %w", key, err)
	}
	if serverETag != etag {
		// Stored bytes are not what we sent: keep dirty, re-diff next pass.
		return fmt.Errorf("sync: put %s: server etag %q != sent md5 %q (left dirty)", key, serverETag, etag)
	}
	if err := e.store.PutPath(ctx, index.Resource{
		Path: key, ETag: serverETag, Size: size, Hydrated: true, Dirty: false,
	}, index.OpUploadOK, "sync upload"); err != nil {
		return fmt.Errorf("sync: index after upload %s: %w", key, err)
	}
	rep.Uploads++
	return nil
}

// deleteLocal implements matrix 4: remove the cache file, write the
// tombstone (DeletePath couples the index flip and the journal row).
func (e *Engine) deleteLocal(ctx context.Context, key, reason string, rep *Report) error {
	if err := os.Remove(e.cachePath(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sync: removing local %s: %w", key, err)
	}
	if err := e.store.DeletePath(ctx, key, reason); err != nil {
		return fmt.Errorf("sync: tombstone %s: %w", key, err)
	}
	rep.RemoteDeletes++
	return nil
}

// deleteRemote implements matrix 6: DELETE with If-Match (the tombstone's
// kept etag is the known-good value), then confirm the tombstone.
func (e *Engine) deleteRemote(ctx context.Context, key, etag string, rep *Report) error {
	if err := e.tr.Delete(ctx, key, etag); err != nil {
		if errors.Is(err, transport.ErrNotExist) {
			// Already gone server-side: the tombstone stands.
			return nil
		}
		if errors.Is(err, transport.ErrConflict) {
			return fmt.Errorf("sync: delete %s: remote changed under us (412): %w", key, err)
		}
		return fmt.Errorf("sync: delete %s: %w", key, err)
	}
	rep.LocalDeletes++
	return nil
}

// conflictCopy implements matrix 3 (and the 412 escalation): the REMOTE
// generation wins (download over the local path) and the local bytes are
// preserved as "file (conflicted copy YYYY-MM-DD).ext" next to it. Never
// merged, never silently overwritten.
func (e *Engine) conflictCopy(ctx context.Context, key, serverETag string, row index.Resource, rep *Report) error {
	local := e.cachePath(key)
	copyKey := conflictedCopyKey(key, e.nowFn())
	if err := copyFile(local, e.cachePath(copyKey)); err != nil {
		return fmt.Errorf("sync: preserving conflict copy of %s: %w", key, err)
	}
	// Record the copy as its own clean hydrated file; the index etag is
	// unknown until its own upload - keep it dirty so the scheduler
	// pushes it as a NEW file (If-None-Match: *).
	if err := e.store.PutPath(ctx, index.Resource{
		Path: copyKey, Size: row.Size, Mtime: e.nowFn().Unix(), Hydrated: true, Dirty: true,
	}, index.OpUploadConflict, "conflict copy of "+key); err != nil {
		return fmt.Errorf("sync: index conflict copy %s: %w", copyKey, err)
	}
	if err := e.download(ctx, key, serverETag, rep); err != nil {
		return err
	}
	rep.ConflictCopies++
	e.log.Printf("sync: conflict on %s: local preserved as %s, remote applied", key, copyKey)
	return nil
}

// conflictedCopyKey builds "dir/file (conflicted copy 2026-10-07).ext".
// The date uses the engine clock (tests pin it); the extension is kept.
func conflictedCopyKey(key string, now time.Time) string {
	date := now.Format("2006-01-02")
	dir, name := parentOf(key)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	copyName := base + " (conflicted copy " + date + ")" + ext
	if dir == "" {
		return copyName
	}
	return dir + "/" + copyName
}

// localStat stats the cache file (size, ok).
func (e *Engine) localStat(key string) (int64, bool) {
	fi, err := os.Stat(e.cachePath(key))
	if err != nil {
		return 0, false
	}
	return fi.Size(), true
}

// cachePath mirrors the leaf-02 data plane: <cacheDir>/files/<key>.
func (e *Engine) cachePath(key string) string {
	return filepath.Join(e.cache, "files", filepath.FromSlash(key))
}

// parentOf splits "a/b/c" -> ("a/b", "c"); top level -> ("", "c").
func parentOf(key string) (string, string) {
	if parent, leaf, found := strings.CutLast(key, "/"); found {
		return parent, leaf
	}
	return "", key
}

// parentDirOf returns the collection key containing key ("" for top level).
func parentDirOf(key string) string {
	parent, _ := parentOf(key)
	return parent
}

// md5File hashes a local file (client-side MD5; the server ETag IS the
// body MD5).
func md5File(path string) (string, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- constructed cache path
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := md5.New() // #nosec G401 -- ETag contract, not a security use
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// mustOpen wraps a path in a ReadSeeker for Put (Content-Length comes
// from seeking; chunked bodies are never sent).
func mustOpen(path string) io.ReadSeeker {
	f, err := os.Open(path) // #nosec G304 -- constructed cache path
	if err != nil {
		return errReader{err}
	}
	return f
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error)       { return 0, e.err }
func (e errReader) Seek(int64, int) (int64, error) { return 0, e.err }

// copyFile copies local bytes to the conflict-copy cache path.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src) // #nosec G304 -- constructed cache path
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil { //nolint:gosec // local file copy
		out.Close()
		return err
	}
	return out.Close()
}

// discardLogger is the no-op logger for tests.
func discardLogger() *log.Logger { return log.New(discardWriter{}, "", 0) }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
