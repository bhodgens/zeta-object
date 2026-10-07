package main

// sync_adapter.go - leaf 04's glue between main.go and internal/sync.
// The sync engine owns the status fields (state/lastSync/dirty/
// conflicts); the IPC listener reads them through the StatusSource seam.
// The scheduler (leaf 07) owns the loops; the daemon runs exactly ONE
// initial SyncOnce after the startup probe.

import (
	"context"
	"fmt"
	"log"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
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
// the scheduler loops). The transport is still leaf-01's placeholder
// (nil until leaf 06), so until then there is nothing to sync and the
// daemon stays in the documented degraded state. Failures are logged,
// never fatal: the daemon stays up and the next sync retries.
func runInitialSync(cfg *config.Config, db *index.Store) {
	// Leaf 06 wires the real transport here; until then the engine has
	// nothing to talk to. The engine CONSTRUCTION path is exercised in
	// internal/sync's tests against the memfs stub.
	if cfg == nil || db == nil {
		return
	}
	log.Printf("zeta-cache: sync engine ready (transport pending leaf 06; initial sync deferred)")
}
