package metadata

// The zmetad-backed "zfs-events" MetadataProvider (leaf 03 of
// docs/plans/zmetad-provider-2026-09). It serves ?events exclusively
// from the zmetad SQLite export database (SCHEMA.md v5): dataset
// resolution is DB-only (ZERO kernel execs on the Probe/History paths),
// row -> ObjectEvent mapping goes through rowsToEvents (full_path-first,
// no graph walk), and purge runs `zmetad --purge` - the ONLY purge
// mechanism (SCHEMA.md section 9: it clears the DB rows AND the kernel
// ring; a hand-rolled SQL delete that drops sync_state would cause a
// full re-import of an uncleared ring, so SQL purge is forbidden here).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// assumeZFSEnv is the escape hatch for the Probe statfs fast-fail
// (ZETAOBJECT_ASSUME_ZFS=1). DetectZFS is a HINT, never authoritative
// (master plan: "a non-polled dataset is unavailable regardless of fs
// type") - the authoritative checks are the zmetad DB's datasets table
// (tracked) and sync_state (polled). ZFS-less test hosts (e2e harness,
// dev machines) set this so the real provider chain can run against a
// fixture database; production ZFS hosts never need it. Unset/any other
// value keeps the statfs fast-fail.
const assumeZFSEnv = "ZETAOBJECT_ASSUME_ZFS"

// assumeZFS reports whether the statfs hint is bypassed for this process.
func assumeZFS() bool { return os.Getenv(assumeZFSEnv) == "1" }

// zmetadCmdTimeout bounds the `zmetad --purge` invocation. Purge is a
// coordinated DB + kernel-ring wipe; 10s is generous for either.
const zmetadCmdTimeout = 10 * time.Second

// dbHandle is the accessor surface this provider consumes from the
// zmetad database. *ZmetadDB (leaf 01) implements it; tests stub it.
type dbHandle interface {
	Events(dataset string, max int) ([]EventRow, error)
	GapStats(dataset string) (GapStats, error)
	ResolveDatasetByPath(path string) (string, error)
	HasDataset(dataset string) (bool, error)
	Close() error
}

// openDB is the seam tests replace. Production opens the real
// read-only accessor; the handle is opened per call and closed by the
// caller (defer Close) so readers never pin the WAL against the live
// daemon.
var openDB = func(ctx context.Context, path string) (dbHandle, error) {
	return OpenZmetadDB(ctx, path)
}

// zmetadRunner is the seam tests replace for purge. Production execs
// the zmetad binary with a 10s timeout, argv-only (the dataset name
// comes from the DB, never from user input), stdout/stderr captured.
var zmetadRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, zmetadCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...) // #nosec G204 // binary is provider config (default "zmetad" on PATH); args are argv-only, dataset names come from the zmetad DB, never user input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

// zmetadEventsProvider is the "zfs-events" MetadataProvider reading the
// zmetad SQLite export database. The registry name is UNCHANGED - the
// frontend constant and wiring depend on it.
//
// detail carries the HistoryDetail (dataset + recordsLost + ringSwaps)
// of the most recent completed History call ON THIS INSTANCE, guarded by
// detailMu (the bughunt A2/C2 pattern): the frontend reads it via the
// structural detailReporter interface (capability_endpoints.go
// historyDetailFor), so concurrent ?events on different providers/buckets
// cannot cross-attribute. There is no package-global last-detail record
// (it was deleted with the CLI transport, leaf 04).
//
// datasets caches positive bucketPath -> dataset resolutions (mutex-
// guarded). Only positive results are cached: a dataset can appear in
// the zmetad `datasets` table after the first poll, so
// DatasetNotTrackedError and every other Probe failure must stay
// retryable.
type zmetadEventsProvider struct {
	dbPath string
	// binary is the zmetad executable for --purge. It defaults to
	// "zmetad" (PATH lookup); leaf 04's config pass-through (config key
	// `zmetad_binary`) sets it via setZmetadBinary. The constructor
	// signature stays single-arg per master Contract 3.
	binary string

	detailMu sync.Mutex
	detail   HistoryDetail

	cacheMu  sync.Mutex
	datasets map[string]string
}

// Compile-time assertions: the concrete provider must satisfy the frozen
// MetadataProvider seam (master Contract 1 - the interface must not gain
// methods) and the frontend's structural detailReporter seam
// (LastDetail() HistoryDetail, capability_endpoints.go).
var (
	_ MetadataProvider                        = (*zmetadEventsProvider)(nil)
	_ interface{ LastDetail() HistoryDetail } = (*zmetadEventsProvider)(nil)
)

// NewZmetadEventsProvider returns the zmetad-backed "zfs-events"
// provider reading the database at dbPath. The zmetad binary for purge
// defaults to "zmetad" on PATH; see setZmetadBinary for leaf 04's
// config pass-through.
func NewZmetadEventsProvider(dbPath string) MetadataProvider {
	return &zmetadEventsProvider{
		dbPath:   dbPath,
		binary:   "zmetad",
		datasets: make(map[string]string),
	}
}

// setZmetadBinary overrides the purge binary path. Unexported by
// design: the constructor signature is frozen (Contract 3), and leaf 04
// wires config inside this package's API surface or promotes an exported
// option then.
func (p *zmetadEventsProvider) setZmetadBinary(binary string) {
	if binary != "" {
		p.binary = binary
	}
}

// SetZmetadBinary applies the config-driven purge-binary override
// (leaf 04, Contract 4: config key `zmetad_binary`) to a provider
// returned by NewZmetadEventsProvider. The constructor signature stays
// frozen single-arg (Contract 3), so wiring promotes the unexported
// setter through this exported package function. A no-op for providers
// that are not the zmetad events provider (e.g. test fakes). Empty
// binary keeps the "zmetad" PATH default.
func SetZmetadBinary(p MetadataProvider, binary string) {
	if z, ok := p.(*zmetadEventsProvider); ok {
		z.setZmetadBinary(binary)
	}
}

func (p *zmetadEventsProvider) Name() string { return "zfs-events" }

// LastDetail returns the detail of the most recent completed History
// call on this provider instance (the frontend's detailReporter seam).
func (p *zmetadEventsProvider) LastDetail() HistoryDetail {
	p.detailMu.Lock()
	defer p.detailMu.Unlock()
	return p.detail
}

func (p *zmetadEventsProvider) setDetail(d HistoryDetail) {
	p.detailMu.Lock()
	defer p.detailMu.Unlock()
	p.detail = d
}

func (p *zmetadEventsProvider) cachedDataset(bucketPath string) (string, bool) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	ds, ok := p.datasets[bucketPath]
	return ds, ok
}

func (p *zmetadEventsProvider) cacheDataset(bucketPath, dataset string) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.datasets[bucketPath] = dataset
}

// dataset resolves bucketPath to its zmetad dataset: warm cache first,
// else DB-only resolution (EvalSymlinks -> openDB ->
// ResolveDatasetByPath). ZERO process execs. Positive results are
// cached; DatasetNotTrackedError and open failures are not.
func (p *zmetadEventsProvider) dataset(ctx context.Context, bucketPath string) (string, error) {
	if ds, ok := p.cachedDataset(bucketPath); ok {
		return ds, nil
	}
	resolved, err := filepath.EvalSymlinks(bucketPath)
	if err != nil {
		return "", fmt.Errorf("metadata: resolve bucket path %s: %w", bucketPath, err)
	}
	db, err := openDB(ctx, p.dbPath)
	if err != nil {
		return "", fmt.Errorf("metadata: open zmetad database %s: %w", p.dbPath, err)
	}
	defer db.Close()
	ds, err := db.ResolveDatasetByPath(resolved)
	if err != nil {
		return "", err // DatasetNotTrackedError et al. already carry the "metadata: " prefix
	}
	p.cacheDataset(bucketPath, ds)
	return ds, nil
}

// Probe reports whether the bucket path sits on a ZFS dataset that
// zmetad tracks and has polled. It performs ZERO process execs: the
// chain is EvalSymlinks -> DetectZFS (statfs fast-fail HINT only - a
// non-polled dataset is unavailable regardless of fs type) -> openDB ->
// ResolveDatasetByPath -> HasDataset. Every failure mode degrades to
// ProbeResult{Available:false, Reason} with a NIL error - "unavailable"
// is a status, not a failure.
func (p *zmetadEventsProvider) Probe(ctx context.Context, bucketPath string) (ProbeResult, error) {
	resolved, err := filepath.EvalSymlinks(bucketPath)
	if err != nil {
		return ProbeResult{Available: false, Reason: "path resolve: " + err.Error()}, nil //nolint:nilerr // unavailable is a status, not a failure
	}
	isZFS, err := DetectZFS(resolved)
	if err != nil && !assumeZFS() {
		return ProbeResult{Available: false, Reason: "statfs: " + err.Error()}, nil //nolint:nilerr // unavailable is a status, not a failure
	}
	if !isZFS && !assumeZFS() {
		return ProbeResult{Available: false, Reason: "filesystem is not ZFS"}, nil
	}
	return p.probeDB(ctx, bucketPath, resolved), nil
}

// probeDB is the GOOS-independent heart of Probe (DetectZFS is
// statfs-bound and dev hosts have no ZFS, so tests drive this directly).
// It never fails: unavailability is a ProbeResult status, not an error.
func (p *zmetadEventsProvider) probeDB(ctx context.Context, bucketPath, resolved string) ProbeResult {
	// Warm positive cache: resolution already proved tracked+polled.
	if ds, ok := p.cachedDataset(bucketPath); ok {
		return ProbeResult{Available: true, Dataset: ds}
	}
	db, err := openDB(ctx, p.dbPath)
	if err != nil {
		return ProbeResult{Available: false, Reason: "db: " + err.Error()}
	}
	defer db.Close()
	ds, err := db.ResolveDatasetByPath(resolved)
	if err != nil {
		if _, ok := errors.AsType[*DatasetNotTrackedError](err); ok {
			return ProbeResult{Available: false, Reason: "not tracked by zmetad"}
		}
		return ProbeResult{Available: false, Reason: "db: " + err.Error()}
	}
	polled, err := db.HasDataset(ds)
	if err != nil {
		return ProbeResult{Available: false, Reason: "db: " + err.Error()}
	}
	if !polled {
		// No sync_state row yet: zmetad knows the dataset but has not
		// completed a poll. NOT cached - the next probe may succeed.
		return ProbeResult{Available: false, Reason: "not polled by zmetad yet", Dataset: ds}
	}
	p.cacheDataset(bucketPath, ds)
	return ProbeResult{Available: true, Dataset: ds}
}

// History returns events for the dataset behind bucketPath, scoped to
// key when key is non-empty. Rows come from the DB in (txg ASC, id ASC)
// order (SCHEMA.md section 7); no MaxEvents fetch headroom is needed -
// the CLI path's MaxEvents*3 hack is gone because filtering happens
// over the full row set here.
//
// Filter semantics replicate the deleted CLI provider's History EXACTLY:
//   - the key filter (rowsMatchKey: exact on resolved full_path keys,
//     conservative bare-name match on PARTIAL rows) runs BEFORE the
//     Since filter - rowsMatchKey indexes into the pristine
//     rowEventSet, and the Since pass re-slices, which would desync the
//     indices (the legacy zfs_events.go index-desync rule);
//   - Since keeps events with Timestamp.After(Since) OR a zero
//     Timestamp: rows with NULL captured_at AND implausible hrtime map
//     to zero time, and "unknown time" is not "older than Since";
//   - MaxEvents caps post-filter.
//
// Loss detail (RecordsLost = GapStats.KnownLost, RingSwaps =
// GapStats.RingSwaps - never folded, SCHEMA.md section 4) is recorded on
// this provider instance and read via LastDetail (detailReporter seam)
// immediately after this call returns.
func (p *zmetadEventsProvider) History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error) {
	ds, err := p.dataset(ctx, bucketPath)
	if err != nil {
		return nil, err
	}
	db, err := openDB(ctx, p.dbPath)
	if err != nil {
		return nil, fmt.Errorf("metadata: open zmetad database %s: %w", p.dbPath, err)
	}
	defer db.Close()
	rows, err := db.Events(ds, 0)
	if err != nil {
		return nil, err
	}
	stats, err := db.GapStats(ds)
	if err != nil {
		return nil, err
	}
	set := rowsToEvents(rows)
	// Per-instance detail (bughunt A2/C2): the endpoint reads this via
	// the detailReporter interface, so concurrent ?events on different
	// buckets/providers cannot cross-attribute dataset or loss stats.
	p.setDetail(HistoryDetail{Dataset: ds, RecordsLost: stats.KnownLost, RingSwaps: stats.RingSwaps})

	events := set.events
	// Key filter BEFORE Since filter (index-desync rule from the legacy
	// CLI provider).
	if key != "" {
		filtered := make([]ObjectEvent, 0, len(events))
		for i := range events {
			if rowsMatchKey(set, i, key) {
				filtered = append(filtered, events[i])
			}
		}
		events = filtered
	}
	if !q.Since.IsZero() {
		filtered := make([]ObjectEvent, 0, len(events))
		for _, e := range events {
			if e.Timestamp.After(q.Since) || e.Timestamp.IsZero() {
				filtered = append(filtered, e) // zero timestamps pass: unknown, not old
			}
		}
		events = filtered
	}
	// Cursor filter (gateway issue #15): strictly-after semantics over
	// the monotonic row id, applied AFTER the key/Since passes (same
	// post-filter ordering as MaxEvents, which still caps last).
	if q.SinceID > 0 {
		filtered := make([]ObjectEvent, 0, len(events))
		for _, e := range events {
			if e.ID > q.SinceID {
				filtered = append(filtered, e)
			}
		}
		events = filtered
	}
	if q.MaxEvents > 0 && len(events) > q.MaxEvents {
		events = events[:q.MaxEvents]
	}
	return events, nil
}

// Purge clears the dataset's event history via `zmetad --purge`: the
// coordinated wipe removes BOTH the DB copy (events, gaps, sync_state
// rows) AND the kernel ring buffer (SCHEMA.md section 9). Gap rows are
// removed too - the loss history (recordsLost/ringSwaps) resets. This is
// the ONLY purge mechanism: SQL-level purge in mini-s3 is forbidden
// (dropping sync_state without clearing the ring causes a full
// re-import). Purge is destructive: it runs only from the authenticated
// endpoint (leaf 04 wiring) and NEVER at server startup. ZERO execs
// happen anywhere else in this provider.
func (p *zmetadEventsProvider) Purge(ctx context.Context, bucketPath string) error {
	ds, err := p.dataset(ctx, bucketPath)
	if err != nil {
		return err
	}
	_, stderr, err := zmetadRunner(ctx, p.binary, "--purge", ds)
	if err != nil {
		return fmt.Errorf("metadata: zmetad --purge %s: %w (stderr: %s)", ds, err, stderr)
	}
	return nil
}
