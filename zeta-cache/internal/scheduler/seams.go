package scheduler

// seams.go - the narrow interfaces the loops code against. *index.Store
// and *sync.Engine satisfy them; tests use in-memory fakes. Keeping the
// seams here (not in index/sync) means leaf 07 never edits the sibling
// packages' files beyond the additive index migration.

import (
	"context"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
)

// StoreView is the index surface the loops need.
type StoreView interface {
	Get(ctx context.Context, path string) (index.Resource, error)
	DirtyPaths(ctx context.Context) ([]string, error)
}

// Compile-time: *index.Store satisfies StoreView.
var _ StoreView = (*index.Store)(nil)

// SyncRunner is the sync-engine surface the sync loop drives
// (*sync.Engine implements both).
type SyncRunner interface {
	SyncOnce(ctx context.Context) error
	FullRescan(ctx context.Context) error
}

// Sync loop constants (leaf: ~15min cadence, cap 1h).
const (
	syncInterval  = 15 * time.Minute
	backoffBase   = 30 * time.Second
	backoffCap    = time.Hour
	jitterFrac    = 0.25 // +[0,25%) jitter
	nightlyPeriod = 24 * time.Hour
	// syncInProgressRetry is the retry delay after a BENIGN
	// ErrSyncInProgress skip (W2-M4): another pass is already live and
	// covers the same index, so this is not a failure - no backoff
	// increment, no failure count, no error state. The loop re-checks
	// after one short interval instead of growing toward the 1h cap.
	syncInProgressRetry = time.Minute
)
