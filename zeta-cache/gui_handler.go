package main

// gui_handler.go - leaf 08's daemon side: ONE handler implementing the
// full IPC surface (leaf-07 Handler + leaf-08 ConflictResolver) and the
// live StatusSource, wired in main.go. The GUI talks ONLY to this socket;
// every network access happens behind the transport the daemon owns.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/scheduler"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
)

// Compile-time capability checks: the daemon handler must satisfy BOTH
// IPC seams.
var (
	_ ipc.Handler          = (*guiHandler)(nil)
	_ ipc.ConflictResolver = (*guiHandler)(nil)
)

// guiHandler implements the full state-changing IPC surface. Every
// component is optional: a nil scheduler (mount failed, scheduler
// disabled) or nil engine degrades that capability to an honest error
// instead of wedging the socket.
type guiHandler struct {
	cfg    *config.Config
	db     *index.Store
	engine *zsync.Engine
	sched  *scheduler.Scheduler
}

func newGUIHandler(cfg *config.Config, db *index.Store, engine *zsync.Engine, sched *scheduler.Scheduler) *guiHandler {
	return &guiHandler{cfg: cfg, db: db, engine: engine, sched: sched}
}

func (h *guiHandler) ctx() context.Context { return context.Background() }

// require returns the engine + scheduler or an honest capability error.
func (h *guiHandler) require() (*zsync.Engine, *scheduler.Scheduler, error) {
	if h.engine == nil || h.sched == nil {
		return nil, nil, errors.New("not supported: daemon is running without the sync engine")
	}
	return h.engine, h.sched, nil
}

// requireEngine returns the engine only (conflict/restore/evict methods
// need no scheduler; the daemon degrades those independently).
func (h *guiHandler) requireEngine() (*zsync.Engine, error) {
	if h.engine == nil {
		return nil, errors.New("not supported: daemon is running without the sync engine")
	}
	return h.engine, nil
}

// --- ipc.Handler (leaf-07 surface) ---

func (h *guiHandler) Pause() (bool, error) {
	_, sched, err := h.require()
	if err != nil {
		return false, err
	}
	return sched.Pause()
}

func (h *guiHandler) Resume() (bool, error) {
	_, sched, err := h.require()
	if err != nil {
		return false, err
	}
	return sched.Resume()
}

func (h *guiHandler) SetPin(path string, pinned bool) error {
	_, sched, err := h.require()
	if err != nil {
		return err
	}
	if _, err := sched.Quota().SetPin(h.ctx(), path, pinned); err != nil {
		return fmt.Errorf("pin %s: %w", path, err)
	}
	return nil
}

func (h *guiHandler) Pins() ([]string, error) {
	_, sched, err := h.require()
	if err != nil {
		return nil, err
	}
	return sched.Quota().Pins(h.ctx())
}

// Tombstones lists the deletion-grace rows (leaf-07 request shape).
func (h *guiHandler) Tombstones() ([]ipc.Tombstone, error) {
	return h.tombstones()
}

// DeletedPaths lists the tombstoned rows (deleted.list). One truth, two
// request names: tombstones is the leaf-07 name, deleted.list is the
// leaf-08 GUI-facing name. The truth lives in the INDEX (deleted=1
// rows), not the quota engine, so this works without a scheduler.
func (h *guiHandler) DeletedPaths() ([]ipc.Tombstone, error) {
	return h.tombstones()
}

func (h *guiHandler) tombstones() ([]ipc.Tombstone, error) {
	paths, err := h.db.TombstonedPaths(h.ctx())
	if err != nil {
		return nil, fmt.Errorf("tombstones: %w", err)
	}
	retentionSecs := int64(0)
	if h.cfg.Quota.TombstoneDays > 0 {
		retentionSecs = int64(time.Duration(h.cfg.Quota.TombstoneDays) * 24 * time.Hour / time.Second)
	}
	out := make([]ipc.Tombstone, 0, len(paths))
	for _, p := range paths {
		row, err := h.db.Get(h.ctx(), p)
		if err != nil {
			continue // raced a prune; the next list is truthful
		}
		// The tombstone's mtime is the deletion timestamp (index.go's
		// grace-window contract).
		out = append(out, ipc.Tombstone{
			Path: p, DeletedAt: row.Mtime, ExpiresAt: row.Mtime + retentionSecs,
		})
	}
	return out, nil
}

// --- ipc.ConflictResolver (leaf-08 surface) ---

// Conflicts lists the pending conflict records.
func (h *guiHandler) Conflicts() ([]ipc.ConflictItem, error) {
	engine, err := h.requireEngine()
	if err != nil {
		return nil, err
	}
	recs, err := engine.Conflicts(h.ctx())
	if err != nil {
		return nil, err
	}
	out := make([]ipc.ConflictItem, 0, len(recs))
	for _, r := range recs {
		out = append(out, ipc.ConflictItem{Path: r.Path, Kind: r.Kind, CopyPath: r.CopyPath})
	}
	return out, nil
}

// ResolveConflict applies keep-local (upload the local copy) or
// keep-remote (download the remote over the local). The conflicted-copy
// file is removed ONLY when the request carries the explicit user flag.
func (h *guiHandler) ResolveConflict(path, mode string, removeCopy bool) error {
	engine, err := h.requireEngine()
	if err != nil {
		return err
	}
	return engine.ResolveConflict(h.ctx(), path, mode, removeCopy)
}

// RestoreDeleted re-downloads a tombstoned path via the transport when
// the server still serves it; otherwise the engine returns the honest
// not-recoverable error (surfaced verbatim to the GUI).
func (h *guiHandler) RestoreDeleted(path string) error {
	engine, err := h.requireEngine()
	if err != nil {
		return err
	}
	return engine.RestoreDeleted(h.ctx(), path)
}

// EvictPath evicts ONE clean file now. Locked refusals: dirty (unsaved
// local bytes), pinned, tombstoned, and unknown paths are never evicted;
// the error names the reason.
func (h *guiHandler) EvictPath(path string) error {
	if h.engine == nil {
		return errors.New("not supported: daemon is running without the sync engine")
	}
	ctx := h.ctx()
	row, err := h.db.Get(ctx, path)
	if errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("evict %s: unknown path", path)
	} else if err != nil {
		return fmt.Errorf("evict %s: %w", path, err)
	}
	switch {
	case row.Dirty:
		return fmt.Errorf("evict %s: refused, file is dirty (upload it first)", path)
	case row.Pinned:
		return fmt.Errorf("evict %s: refused, file is pinned", path)
	case row.Deleted:
		return fmt.Errorf("evict %s: refused, path is tombstoned", path)
	case !row.Hydrated:
		return fmt.Errorf("evict %s: refused, not hydrated (nothing cached)", path)
	}
	// evictOne is unexported in scheduler; the same two-step contract
	// (cache file first, then index demotion) runs here via the store.
	return evictOne(ctx, h.cfg, h.db, path)
}

// evictOne mirrors scheduler.QuotaEngine.evictOne: remove the cache file
// (the remote copy is authoritative for a clean file), then demote the
// index row + journal row in one transaction (store.Evict).
func evictOne(ctx context.Context, cfg *config.Config, db *index.Store, path string) error {
	if err := removeCacheFile(cfg.CacheDir, path); err != nil {
		return err
	}
	return db.Evict(ctx, path, "evict (ipc)")
}

// guiStatusSource is the live StatusSource: engine status (state,
// lastSync, dirty, conflicts, lastError) + quota numbers + scheduler
// liveness, layered over the persisted-meta fallback the leaf-01
// statusSource provides (the fallback covers the window before the
// engine/scheduler exist).
type guiStatusSource struct {
	db     *index.Store
	cfg    *config.Config
	engine *zsync.Engine
	sched  *scheduler.Scheduler
}

var _ ipc.StatusSource = guiStatusSource{}

// IPCStatus implements ipc.StatusSource. It must not block on the
// network: every read is a local SQLite query or in-memory state.
func (s guiStatusSource) IPCStatus() ipc.StatusData {
	// The persisted-meta fallback (leaf-01 behavior) first.
	data := statusSource{db: s.db}.IPCStatus()
	data.Server = "" // identity fields are stamped by the listener
	data.Bucket = ""
	ctx := context.Background()

	if s.engine != nil {
		st := s.engine.Status()
		data.State = st.State
		data.LastSync = st.LastSync
		data.Dirty = st.Dirty
		data.Conflicts = st.Conflicts
		data.LastErr = st.LastErr
	}
	if s.sched != nil {
		usage, cap, overflow, evicted, lastEvict := s.sched.Quota().Stats(ctx)
		data.UsageBytes = usage
		data.CapBytes = cap
		data.Overflow = overflow
		data.Evicted = evicted
		if !lastEvict.IsZero() {
			data.LastEvictAt = lastEvict.Unix()
		}
		data.Cached = s.cachedCount(ctx)
	}
	return data
}

// cachedCount counts hydrated live rows (the GUI's "files cached" line).
func (s guiStatusSource) cachedCount(ctx context.Context) int {
	paths, err := s.db.CleanHydratedPaths(ctx, nil)
	if err != nil {
		log.Printf("zeta-cache: status cached count: %v", err)
		return 0
	}
	return len(paths)
}
