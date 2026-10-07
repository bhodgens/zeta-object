package main

// sync_adapter.go - leaf 04's glue between main.go and internal/sync.
// The sync engine owns the status fields (state/lastSync/dirty/
// conflicts); the IPC listener reads them through the StatusSource seam.
// The scheduler (leaf 07) owns the loops; the daemon runs exactly ONE
// initial SyncOnce after the startup probe.
//
// Leaf 06 wires the REAL transport in: the engine is constructed with
// the live webdav client, and the initial SyncOnce runs for real
// (failures logged, never fatal - the scheduler's next pass retries).

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// statusSource adapts the index Store to ipc.StatusSource. The counts are
// live queries (DirtyPaths); state/lastSync come from the persisted meta
// values - the engine stamps last-full-scan-time and conflict-count at
// the end of every pass, so a status request from any process (and
// before/after the engine exists) reads the same truth. When a live
// engine exists mid-run its in-memory syncing state wins (see main).
type statusSource struct {
	db *index.Store
}

// meta keys - must match internal/sync's constants (kept literal here so
// main does not import the engine just to read status; the strings are
// part of the daemon's on-disk contract).
const (
	metaKeyLastSync  = "last-full-scan-time"
	metaKeyConflicts = "conflict-count"
)

// IPCStatus implements ipc.StatusSource.
func (s statusSource) IPCStatus() ipc.StatusData {
	ctx := context.Background()
	data := ipc.StatusData{State: "idle"}
	if last, err := s.db.MetaGet(ctx, metaKeyLastSync); err == nil {
		var t int64
		if _, err := fmt.Sscanf(last, "%d", &t); err == nil {
			data.LastSync = t
		}
	}
	if dirty, err := s.db.DirtyPaths(ctx); err == nil {
		data.Dirty = len(dirty)
	}
	if conf, err := s.db.MetaGet(ctx, metaKeyConflicts); err == nil {
		var n int
		if _, err := fmt.Sscanf(conf, "%d", &n); err == nil {
			data.Conflicts = n
		}
	}
	return data
}

var _ ipc.StatusSource = statusSource{}

// runInitialSync performs the one startup sync (leaf 04; leaf 07 adds
// the scheduler loops) over the REAL transport (leaf 06). The download
// path stages into <cacheDir>/staging, so the dir is created here (the
// FUSE layer creates it only when IT mounts). Failures are logged,
// never fatal: the daemon stays up and the next sync retries.
func runInitialSync(cfg *config.Config, db *index.Store, tr transport.Transport) {
	if cfg == nil || db == nil || tr == nil {
		return
	}
	if err := os.MkdirAll(filepath.Join(cfg.CacheDir, "staging"), 0o700); err != nil {
		log.Printf("zeta-cache: WARNING creating staging dir: %v (initial sync skipped)", err)
		return
	}
	engine, err := sync.NewEngine(sync.Options{
		Transport: tr,
		Store:     db,
		CacheDir:  cfg.CacheDir,
		Logger:    log.Default(),
	})
	if err != nil {
		log.Printf("zeta-cache: WARNING sync engine construction: %v", err)
		return
	}
	if err := engine.SyncOnce(context.Background()); err != nil {
		log.Printf("zeta-cache: initial sync failed (will retry on the next scheduled pass): %v", err)
		return
	}
	rep := engine.LastReport()
	log.Printf("zeta-cache: initial sync complete: scanned=%d downloads=%d uploads=%d conflicts=%d (token-skips=%d fullscans=%d)",
		rep.Scanned, rep.Downloads, rep.Uploads, rep.ConflictCopies, rep.TokenVerified, rep.FullscanVerified)
}
