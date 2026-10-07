package sync

// engine.go - the two-way sync engine (leaf 04). The engine owns the
// IPC status fields (state/lastSync/dirty/conflicts), exposes
// SyncOnce(ctx) and FullRescan(ctx) for leaf 07's scheduler, and never
// touches FUSE internals: sync and FUSE share the index Store through
// SQLite WAL (single writer, concurrent readers), so no ad-hoc locking
// between them is needed.
//
// The scan is the universal ETag-diff PROPFIND walk:
//
//	walk root (Depth 1)
//	  for each child entry:
//	    dir row: token == index token -> SKIP subtree (record "token")
//	             token mismatch / any doubt -> walk it (record "fullscan")
//	    file row: diff (server row, index row, disk file) -> action
//
// Every conflict decision uses FILE ETags / If-Match; the collection
// token is a skip HINT only (master decision 8). A wrong skip is
// recovered by the periodic full scan (leaf 07).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// State values surfaced in the IPC status (status.state).
const (
	StateIdle    = "idle"
	StateSyncing = "syncing"
	StateError   = "error"
)

// meta keys owned by this package.
const (
	metaLastSync      = "last-full-scan-time"
	metaConflictCount = "conflict-count"
)

// Engine is the two-way sync engine. Construct with NewEngine.
type Engine struct {
	tr    transport.Transport
	store *index.Store
	feed  ChangeFeed
	cache string // cache dir (files/ lives under it)
	nowFn func() time.Time
	log   *log.Logger

	mu        sync.Mutex // guards the status snapshot below
	state     string
	lastSync  time.Time
	lastErr   string
	conflicts int
	listAll   bool // FullRescan: bypass the collection-token skip
	lastRep   Report
}

// LastReport returns the Report of the most recent completed pass (the
// scheduler logs it; tests assert on it).
func (e *Engine) LastReport() Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRep
}

// Options configures the engine.
type Options struct {
	// Transport is the webdav seam (required).
	Transport transport.Transport
	// Store is the client index (required).
	Store *index.Store
	// CacheDir is the cache root: hydrated files live under CacheDir/files
	// (the leaf-02 layout). Required.
	CacheDir string
	// Feed is the change-delta seam; nil = FullScanFeed (leaf 05 replaces).
	Feed ChangeFeed
	// Logger is optional; defaults to a discard logger.
	Logger *log.Logger
	// Now overrides the clock (tests); defaults to time.Now.
	Now func() time.Time
}

// NewEngine validates options and returns a ready engine.
func NewEngine(o Options) (*Engine, error) {
	if o.Transport == nil {
		return nil, fmt.Errorf("sync: Transport is required")
	}
	if o.Store == nil {
		return nil, fmt.Errorf("sync: Store is required")
	}
	if o.CacheDir == "" {
		return nil, fmt.Errorf("sync: CacheDir is required")
	}
	feed := o.Feed
	if feed == nil {
		feed = FullScanFeed{}
	}
	logger := o.Logger
	if logger == nil {
		logger = discardLogger()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	e := &Engine{
		tr:    o.Transport,
		store: o.Store,
		feed:  feed,
		cache: o.CacheDir,
		nowFn: now,
		log:   logger,
		state: StateIdle,
	}
	e.loadConflicts(context.Background())
	return e, nil
}

// SyncOnce runs one reconcile pass: delta (ChangeFeed) -> diff -> apply.
// This is what leaf 07's scheduler calls on connect + backoff.
func (e *Engine) SyncOnce(ctx context.Context) error {
	return e.run(ctx, false)
}

// FullRescan forces the whole-tree walk regardless of what the feed says
// (leaf 07's nightly full-scan cadence, and the token-mismatch fallback).
func (e *Engine) FullRescan(ctx context.Context) error {
	return e.run(ctx, true)
}

func (e *Engine) run(ctx context.Context, forceFull bool) error {
	e.setState(StateSyncing)
	e.mu.Lock()
	e.listAll = forceFull
	e.mu.Unlock()

	paths, feedFull, err := e.feed.Delta(ctx)
	if err != nil {
		// Feed failure is "on any doubt": degrade to the full scan
		// rather than skipping work (the feed is an optimization).
		e.log.Printf("sync: feed delta failed (%v); falling back to full scan", err)
		paths, feedFull = nil, true
	}
	full := forceFull || feedFull

	var rep Report
	if full {
		rep, err = e.fullScan(ctx)
	} else {
		rep, err = e.deltaScan(ctx, paths)
	}
	if err != nil {
		e.setStateErr(err)
		return err
	}

	now := e.nowFn()
	e.mu.Lock()
	e.state = StateIdle
	e.lastSync = now
	e.lastErr = "" // recovered
	e.listAll = false
	e.conflicts = max(rep.ConflictCopies+rep.RemoteDeleteKept, 0)
	e.lastRep = rep
	e.mu.Unlock()
	if err := e.store.MetaSet(ctx, metaLastSync, fmt.Sprintf("%d", now.Unix())); err != nil {
		e.log.Printf("sync: stamping last-sync time: %v", err)
	}
	if err := e.store.MetaSet(ctx, metaConflictCount, fmt.Sprintf("%d", e.snapshotConflicts())); err != nil {
		e.log.Printf("sync: stamping conflict count: %v", err)
	}
	e.log.Printf("sync: done (scanned=%d downloads=%d uploads=%d deletes=%d conflicts=%d, verified token=%d fullscan=%d)",
		rep.Scanned, rep.Downloads, rep.Uploads, rep.RemoteDeletes, rep.ConflictCopies, rep.TokenVerified, rep.FullscanVerified)
	return nil
}

// setState flips the running state (StateSyncing has no timestamp side
// effects; StateError records the message).
func (e *Engine) setState(s string) {
	e.mu.Lock()
	e.state = s
	e.mu.Unlock()
}

func (e *Engine) setStateErr(err error) {
	e.mu.Lock()
	e.state = StateError
	e.lastErr = err.Error()
	e.mu.Unlock()
}

// snapshotConflicts reads the current conflict counter.
func (e *Engine) snapshotConflicts() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conflicts
}

// listingAll reports whether the running pass must list every subtree
// (FullRescan bypasses the token skip).
func (e *Engine) listingAll() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listAll
}

// loadConflicts restores the persisted conflict count at construction.
func (e *Engine) loadConflicts(ctx context.Context) {
	v, err := e.store.MetaGet(ctx, metaConflictCount)
	if err != nil {
		return // fresh DB: zero conflicts
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
		e.mu.Lock()
		e.conflicts = n
		e.mu.Unlock()
	}
}

// Status is the engine-owned slice of the IPC status payload: state
// (idle/syncing/sync-error), lastSync (unix seconds, 0 = never), dirty
// count (live index rows with dirty=1) and conflicts count.
type Status struct {
	State     string `json:"state"`
	LastSync  int64  `json:"lastSync"` // unix seconds; 0 = never synced
	Dirty     int    `json:"dirty"`
	Conflicts int    `json:"conflicts"`
	LastErr   string `json:"lastError,omitempty"`
}

// Status returns the live status values. It is safe for concurrent use
// and is what main.go's IPC wiring reads.
func (e *Engine) Status() Status {
	dirty, err := e.store.DirtyPaths(context.Background())
	if err != nil {
		e.log.Printf("sync: DirtyPaths for status: %v", err)
		dirty = nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	last := int64(0)
	if !e.lastSync.IsZero() {
		last = e.lastSync.Unix()
	}
	return Status{
		State:     e.state,
		LastSync:  last,
		Dirty:     len(dirty),
		Conflicts: e.conflicts,
		LastErr:   e.lastErr,
	}
}

// fullScan walks the whole tree from the root, then diffs the index-side
// keys the listing did NOT show (remote deletions, local-only files,
// pending tombstones) - the walk alone can only see what the server
// lists.
func (e *Engine) fullScan(ctx context.Context) (Report, error) {
	seen := map[string]bool{}
	rep, err := e.walk(ctx, "", seen)
	if err != nil {
		return rep, err
	}
	// Index rows (files only; dir-token rows carry the trailing slash)
	// the listing never mentioned: the remote deleted them (matrix 4/5)
	// or they are local-only (matrix 8 uploads).
	paths, err := e.store.AllPaths(ctx)
	if err != nil {
		return rep, fmt.Errorf("sync: index paths: %w", err)
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "/") || seen[p] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if err := e.diffAndApply(ctx, p, nil, &rep); err != nil {
			return rep, err
		}
	}
	// Orphan scan: files on disk with NO index row and NO server row are
	// new local files (matrix 8: upload If-None-Match:*). The FUSE layer
	// normally tracks its own writes; the orphan pass is the safety net
	// for writes that raced the index (crash between rename and Put).
	if err := e.orphans(ctx, seen, &rep); err != nil {
		return rep, err
	}
	// Tombstones (local deletes awaiting the remote DELETE, matrix 6).
	tombs, err := e.store.TombstonedPaths(ctx)
	if err != nil {
		return rep, fmt.Errorf("sync: index tombstones: %w", err)
	}
	for _, p := range tombs {
		if seen[p] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if err := e.diffAndApply(ctx, p, nil, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// orphans walks the local cache files/ tree and uploads every file that
// has neither an index row nor a server row (matrix 8). Files the server
// still lists were already diffed by the walk; conflict copies and dirty
// rows have index rows.
func (e *Engine) orphans(ctx context.Context, seen map[string]bool, rep *Report) error {
	root := filepath.Join(e.cache, "files")
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // no files/ dir yet: nothing local at all
			}
			return fmt.Errorf("sync: orphan walk: %w", err)
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return fmt.Errorf("sync: orphan rel %s: %w", path, rerr)
		}
		key := filepath.ToSlash(rel)
		if seen[key] {
			return nil
		}
		// Seen by the listing but never reached? diffAndApply is
		// idempotent, but skipping the double work is cheap: check the
		// index once.
		if _, gerr := e.store.Get(ctx, key); gerr == nil {
			return nil // has a row: already diffed or scheduled
		}
		return e.diffAndApply(ctx, key, nil, rep)
	})
}

// deltaScan visits only the (file) paths the feed named, diffing each
// against the server row and the index. Parent-collection tokens are NOT
// consulted (the feed already named the paths); the per-path diff and
// pipelines are exactly the full scan's.
func (e *Engine) deltaScan(ctx context.Context, paths []string) (Report, error) {
	rep := Report{}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if err := e.diffAndApply(ctx, p, nil, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// walk lists dir ("" = root, Depth 1) and recurses into child
// collections unless the collection-token skip fires, running the
// per-file diff for every listed file. seen accumulates the file keys
// the SERVER still lists (fullScan uses the complement).
func (e *Engine) walk(ctx context.Context, dir string, seen map[string]bool) (Report, error) {
	rep := Report{}
	entries, err := e.tr.Propfind(ctx, dir, false)
	if err != nil {
		return rep, fmt.Errorf("sync: propfind %q: %w", dir, err)
	}
	for i := range entries {
		entry := entries[i]
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		key := entry.Key
		if key == dir {
			continue // entry 0: the collection itself
		}
		if entry.IsDir {
			if err := e.walkDir(ctx, key, entry.ETag, &rep); err != nil {
				return rep, err
			}
			continue
		}
		seen[key] = true
		if err := e.diffAndApply(ctx, key, &entry, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// walkDir applies the collection-token skip: when the listed dir token
// equals the index's stored token for the dir row, the whole subtree is
// skipped (verified=token); otherwise it is walked (verified=fullscan)
// and the fresh token recorded. Dir rows live in the resources table
// with the leaf's trailing-slash path convention. On ANY doubt (missing
// or empty token, index error) the walk happens - the token is a hint,
// never a correctness input.
func (e *Engine) walkDir(ctx context.Context, dirKey, listedToken string, rep *Report) error {
	rowKey := dirKey + "/" // dir rows carry the trailing slash
	if e.listingAll() {
		// FullRescan: every subtree is listed regardless of the token.
		rep.FullscanVerified++
		sub, err := e.walk(ctx, dirKey, map[string]bool{})
		mergeReport(rep, sub)
		if err != nil {
			return err
		}
		if listedToken != "" {
			if err := e.store.PutPath(ctx, index.Resource{
				Path: rowKey, ETag: listedToken,
			}, "", "dir token recorded"); err != nil {
				return fmt.Errorf("sync: storing dir token %s: %w", rowKey, err)
			}
		}
		return nil
	}
	stored, err := e.store.Get(ctx, rowKey)
	switch {
	case err == nil && !stored.Deleted && listedToken != "" && stored.ETag == listedToken:
		// Skip: the server's derived immediate-children token matches
		// what we recorded last scan - nothing in this subtree changed.
		rep.TokenVerified++
		return nil
	case err != nil && !errors.Is(err, index.ErrNotFound):
		return fmt.Errorf("sync: index get %s: %w", rowKey, err)
	default:
		// Mismatch, first sighting, tombstoned dir, or empty token:
		// walk the subtree (and remember the new token afterwards).
		rep.FullscanVerified++
		sub, err := e.walk(ctx, dirKey, map[string]bool{})
		mergeReport(rep, sub)
		if err != nil {
			return err
		}
		if listedToken != "" {
			if err := e.store.PutPath(ctx, index.Resource{
				Path: rowKey, ETag: listedToken,
			}, "", "dir token recorded"); err != nil {
				return fmt.Errorf("sync: storing dir token %s: %w", rowKey, err)
			}
		}
		return nil
	}
}

func mergeReport(dst *Report, src Report) {
	dst.Scanned += src.Scanned
	dst.Downloads += src.Downloads
	dst.Uploads += src.Uploads
	dst.SkippedUploads += src.SkippedUploads
	dst.RemoteDeletes += src.RemoteDeletes
	dst.LocalDeletes += src.LocalDeletes
	dst.ConflictCopies += src.ConflictCopies
	dst.RemoteDeleteKept += src.RemoteDeleteKept
	dst.TokenVerified += src.TokenVerified
	dst.FullscanVerified += src.FullscanVerified
}
