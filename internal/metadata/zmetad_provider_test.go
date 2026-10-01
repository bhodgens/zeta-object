package metadata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubZmetadDB is a scriptable dbHandle for the provider tests. All
// counters are atomic so the -race concurrency test can assert call
// counts (cache behavior) safely.
type stubZmetadDB struct {
	events     []EventRow
	eventsErr  error
	gaps       GapStats
	gapErr     error
	resolveDS  string
	resolveErr error
	hasDS      bool
	hasErr     error
	closeErr   error

	resolveCalls atomic.Int64
	eventsCalls  atomic.Int64
	closeCalls   atomic.Int64
}

func (s *stubZmetadDB) Events(dataset string, max int) ([]EventRow, error) {
	s.eventsCalls.Add(1)
	if s.eventsErr != nil {
		return nil, s.eventsErr
	}
	if max > 0 && len(s.events) > max {
		return s.events[:max], nil
	}
	return s.events, nil
}

func (s *stubZmetadDB) GapStats(dataset string) (GapStats, error) {
	return s.gaps, s.gapErr
}

func (s *stubZmetadDB) ResolveDatasetByPath(path string) (string, error) {
	s.resolveCalls.Add(1)
	if s.resolveErr != nil {
		return "", s.resolveErr
	}
	return s.resolveDS, nil
}

func (s *stubZmetadDB) HasDataset(dataset string) (bool, error) {
	return s.hasDS, s.hasErr
}

func (s *stubZmetadDB) Close() error {
	s.closeCalls.Add(1)
	return s.closeErr
}

// stubOpenDB replaces the openDB seam with a path-keyed map of stubs and
// counts opens. Restored via t.Cleanup.
func stubOpenDB(t *testing.T, dbs map[string]*stubZmetadDB, openErr error) *atomic.Int64 {
	t.Helper()
	opens := &atomic.Int64{}
	old := openDB
	openDB = func(ctx context.Context, path string) (dbHandle, error) {
		opens.Add(1)
		if openErr != nil {
			return nil, openErr
		}
		db, ok := dbs[path]
		if !ok {
			return nil, fmt.Errorf("metadata: no stub db for path %s", path)
		}
		return db, nil
	}
	t.Cleanup(func() { openDB = old })
	return opens
}

// stubZmetadRunner replaces the purge runner seam and records invocations.
type runnerCall struct {
	binary string
	args   []string
}

func stubZmetadRunner(t *testing.T, stderr string, err error) (*[]runnerCall, *sync.Mutex) {
	t.Helper()
	var calls []runnerCall
	var mu sync.Mutex
	old := zmetadRunner
	zmetadRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
		mu.Lock()
		calls = append(calls, runnerCall{binary: binary, args: args})
		mu.Unlock()
		return []byte("out"), stderr, err
	}
	t.Cleanup(func() { zmetadRunner = old })
	return &calls, &mu
}

// newTestProvider builds a provider with the given dbPath and binary.
func newTestProvider(t *testing.T, dbPath, binary string) *zmetadEventsProvider {
	t.Helper()
	p := NewZmetadEventsProvider(dbPath).(*zmetadEventsProvider)
	if binary != "" {
		p.setZmetadBinary(binary)
	}
	return p
}

// ---------------------------------------------------------------------------
// Task 1: Probe matrix
// ---------------------------------------------------------------------------

// TestZmetadProviderProbeMatrix drives probeDB directly (DetectZFS is
// statfs-bound and dev hosts have no ZFS - same rationale as the
// probeCore tests in zfs_events_test.go).
func TestZmetadProviderProbeMatrix(t *testing.T) {
	dbPath := "/var/lib/zfs/zmetad.db"
	notTracked := &DatasetNotTrackedError{Path: "/mnt/tank/data"}

	tests := []struct {
		name          string
		db            *stubZmetadDB
		openErr       error
		wantAvailable bool
		wantReason    string // substring; "" = none expected
		wantDataset   string
	}{
		{
			name:          "happy path: tracked and polled",
			db:            &stubZmetadDB{resolveDS: "tank/data", hasDS: true},
			wantAvailable: true,
			wantDataset:   "tank/data",
		},
		{
			name:       "not tracked by zmetad",
			db:         &stubZmetadDB{resolveErr: notTracked},
			wantReason: "not tracked by zmetad",
		},
		{
			name:        "tracked but not polled yet",
			db:          &stubZmetadDB{resolveDS: "tank/data", hasDS: false},
			wantReason:  "not polled by zmetad yet",
			wantDataset: "tank/data",
		},
		{
			name:       "openDB failure degrades with db: prefix",
			openErr:    errors.New("metadata: zmetad database not found at /var/lib/zfs/zmetad.db"),
			wantReason: "db: metadata: zmetad database not found",
		},
		{
			name:       "resolve query failure degrades with db: prefix",
			db:         &stubZmetadDB{resolveErr: errors.New("metadata: query datasets table: boom")},
			wantReason: "db: metadata: query datasets table",
		},
		{
			name:       "HasDataset failure degrades with db: prefix",
			db:         &stubZmetadDB{resolveDS: "tank/data", hasErr: errors.New("metadata: query sync_state: boom")},
			wantReason: "db: metadata: query sync_state",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbs := map[string]*stubZmetadDB{}
			if tt.db != nil {
				dbs[dbPath] = tt.db
			}
			stubOpenDB(t, dbs, tt.openErr)
			runnerCalls, runnerMu := stubZmetadRunner(t, "", nil)
			p := newTestProvider(t, dbPath, "")

			res := p.probeDB(context.Background(), "/mnt/tank/data", "/mnt/tank/data")
			if res.Available != tt.wantAvailable {
				t.Errorf("Available = %v, want %v (res=%+v)", res.Available, tt.wantAvailable, res)
			}
			if tt.wantReason != "" && !strings.Contains(res.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want substring %q", res.Reason, tt.wantReason)
			}
			if tt.wantReason == "" && res.Reason != "" {
				t.Errorf("Reason = %q, want empty", res.Reason)
			}
			if res.Dataset != tt.wantDataset {
				t.Errorf("Dataset = %q, want %q", res.Dataset, tt.wantDataset)
			}
			runnerMu.Lock()
			n := len(*runnerCalls)
			runnerMu.Unlock()
			if n != 0 {
				t.Errorf("purge runner called %d times during Probe; Probe must be exec-free", n)
			}
		})
	}
}

// TestZmetadProviderProbeStatfsHint: DetectZFS false (a temp dir on any
// dev host) fast-fails with the statfs Reason and nil error - the DB is
// never opened.
func TestZmetadProviderProbeStatfsHint(t *testing.T) {
	db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true}
	opens := stubOpenDB(t, map[string]*stubZmetadDB{"/no/such/db": db}, nil)
	p := newTestProvider(t, "/no/such/db", "")

	dir := t.TempDir()
	res, err := p.Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe error = %v, want nil (unavailable is a status)", err)
	}
	if res.Available {
		t.Fatalf("Probe = available on a non-ZFS temp dir, want unavailable: %+v", res)
	}
	if res.Reason != "filesystem is not ZFS" {
		t.Errorf("Reason = %q, want %q", res.Reason, "filesystem is not ZFS")
	}
	if opens.Load() != 0 {
		t.Errorf("openDB called %d times after statfs fast-fail, want 0", opens.Load())
	}
}

// TestZmetadProviderProbePathResolve: a nonexistent bucket path degrades
// to "path resolve:" with nil error, before any DB open.
func TestZmetadProviderProbePathResolve(t *testing.T) {
	opens := stubOpenDB(t, map[string]*stubZmetadDB{}, nil)
	p := newTestProvider(t, "/no/such/db", "")

	res, err := p.Probe(context.Background(), "/definitely/not/here")
	if err != nil {
		t.Fatalf("Probe error = %v, want nil", err)
	}
	if res.Available || !strings.HasPrefix(res.Reason, "path resolve:") {
		t.Fatalf("Probe = %+v, want unavailable with 'path resolve:' Reason", res)
	}
	if opens.Load() != 0 {
		t.Errorf("openDB called %d times after path-resolve failure, want 0", opens.Load())
	}
}

// TestZmetadProviderProbeCache: resolution runs ONCE across two probes;
// failures (not-tracked, not-polled) are NOT cached.
func TestZmetadProviderProbeCache(t *testing.T) {
	dbPath := "/var/lib/zfs/zmetad.db"
	db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true}
	stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
	p := newTestProvider(t, dbPath, "")
	ctx := context.Background()

	for i := range 2 {
		res := p.probeDB(ctx, "/mnt/tank/data", "/mnt/tank/data")
		if !res.Available || res.Dataset != "tank/data" {
			t.Fatalf("probe %d = %+v; want available tank/data", i, res)
		}
	}
	if got := db.resolveCalls.Load(); got != 1 {
		t.Errorf("ResolveDatasetByPath called %d times across 2 probes, want 1 (positive cache)", got)
	}

	// A subsequent History on the warm cache must not re-resolve.
	if _, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{}); err != nil {
		t.Fatalf("History: %v", err)
	}
	if got := db.resolveCalls.Load(); got != 1 {
		t.Errorf("ResolveDatasetByPath called %d times after warm-cache History, want 1", got)
	}
}

// TestZmetadProviderProbeFailuresNotCached: not-tracked and not-polled
// probes stay retryable (a dataset can appear after the first poll).
func TestZmetadProviderProbeFailuresNotCached(t *testing.T) {
	dbPath := "/var/lib/zfs/zmetad.db"
	db := &stubZmetadDB{resolveErr: &DatasetNotTrackedError{Path: "/mnt/tank/data"}}
	stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
	p := newTestProvider(t, dbPath, "")
	ctx := context.Background()

	if res := p.probeDB(ctx, "/mnt/tank/data", "/mnt/tank/data"); res.Available {
		t.Fatal("want unavailable")
	}
	// Second probe must re-resolve (failure not cached) - and now succeeds.
	db.resolveErr = nil
	db.resolveDS = "tank/data"
	db.hasDS = true
	res := p.probeDB(ctx, "/mnt/tank/data", "/mnt/tank/data")
	if !res.Available || res.Dataset != "tank/data" {
		t.Fatalf("second probe = %+v, want available tank/data (failures must not be cached)", res)
	}
	if got := db.resolveCalls.Load(); got != 2 {
		t.Errorf("ResolveDatasetByPath called %d times, want 2 (failure path re-resolves)", got)
	}

	// Same for the not-polled state: HasDataset flips true after a poll.
	db2 := &stubZmetadDB{resolveDS: "tank/x", hasDS: false}
	stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db2}, nil)
	p2 := newTestProvider(t, dbPath, "")
	if r := p2.probeDB(ctx, "/mnt/tank/x", "/mnt/tank/x"); r.Available {
		t.Fatal("not-polled probe must be unavailable")
	}
	db2.hasDS = true
	if r := p2.probeDB(ctx, "/mnt/tank/x", "/mnt/tank/x"); !r.Available {
		t.Fatalf("probe after poll = %+v, want available", r)
	}
	if got := db2.resolveCalls.Load(); got != 2 {
		t.Errorf("ResolveDatasetByPath called %d times, want 2", got)
	}
}

// TestZmetadProviderProbeNoExec: the probe path must not touch the
// purge exec seam (zmetadRunner). The CLI zfsRunner seam no longer
// exists - the zfs exec transport was deleted (leaf 04).
func TestZmetadProviderProbeNoExec(t *testing.T) {
	runnerCalls, runnerMu := stubZmetadRunner(t, "", nil)

	dbPath := "/var/lib/zfs/zmetad.db"
	stubOpenDB(t, map[string]*stubZmetadDB{dbPath: {resolveDS: "tank/data", hasDS: true}}, nil)
	p := newTestProvider(t, dbPath, "")
	res := p.probeDB(context.Background(), "/mnt/tank/data", "/mnt/tank/data")
	if !res.Available {
		t.Fatalf("probeDB = %+v; want available", res)
	}
	runnerMu.Lock()
	n := len(*runnerCalls)
	runnerMu.Unlock()
	if n != 0 {
		t.Errorf("zmetadRunner called %d times during Probe, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Task 2: History filtering chain
// ---------------------------------------------------------------------------

func TestZmetadProviderName(t *testing.T) {
	p := NewZmetadEventsProvider("/db")
	if got := p.Name(); got != "zfs-events" {
		t.Errorf("Name() = %q, want %q (registry name is frozen)", got, "zfs-events")
	}
}

func TestZmetadProviderHistoryFiltering(t *testing.T) {
	dbPath := "/var/lib/zfs/zmetad.db"
	// Rows (txg order): old CREATE of a resolved key, recent TRUNCATE of
	// the same key, a PARTIAL (NULL full_path) row for "obj", an unrelated
	// resolved key, and a zero-timestamp row (NULL captured_at +
	// implausible hrtime) for the queried key.
	zeroTime := time.Time{}
	rows := []EventRow{
		row("CREATE", 10, 100, new("obj"), new("dir/obj"),
			withCapturedAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())),
		row("TRUNCATE", 20, 100, new("obj"), new("dir/obj"),
			withCapturedAt(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Unix()),
			withSize(5), withOldSize(9)),
		row("CREATE", 30, 200, new("obj"), nil, // PARTIAL bare name
			withCapturedAt(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Unix())),
		row("REMOVE", 40, 300, new("other"), new("dir/other"),
			withCapturedAt(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix())),
		row("SETATTR", 50, 100, new("obj"), new("dir/obj"),
			withTimestamp(12345)), // implausible hrtime, NULL captured_at -> zero time
	}
	gaps := GapStats{KnownLost: 5, Regressions: 2, RingSwaps: 1}
	db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true, events: rows, gaps: gaps}
	stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
	runnerCalls, runnerMu := stubZmetadRunner(t, "", nil)
	p := newTestProvider(t, dbPath, "")
	// Warm the positive cache: /mnt/... paths do not exist on dev hosts,
	// and the cache is the tested fast path anyway (EvalSymlinks skipped).
	p.cacheDataset("/mnt/tank/data", "tank/data")
	ctx := context.Background()

	t.Run("empty DB yields empty slice and nil error", func(t *testing.T) {
		emptyDB := &stubZmetadDB{resolveDS: "tank/empty", hasDS: true, events: nil}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: emptyDB}, nil)
		q := newTestProvider(t, dbPath, "")
		q.cacheDataset("/mnt/tank/empty", "tank/empty")
		events, err := q.History(ctx, "/mnt/tank/empty", "", HistoryQuery{})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("events = %v, want empty", events)
		}
	})

	t.Run("no key no Since returns all rows mapped", func(t *testing.T) {
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		events, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(events) != len(rows) {
			t.Fatalf("got %d events, want %d", len(events), len(rows))
		}
		if events[0].Key != "dir/obj" || events[0].Op != "create" {
			t.Errorf("events[0] = %+v, want resolved key dir/obj op create", events[0])
		}
		if events[2].Key != "obj" {
			t.Errorf("PARTIAL row Key = %q, want bare name %q", events[2].Key, "obj")
		}
		if !events[4].Timestamp.IsZero() {
			t.Errorf("implausible-hrtime row Timestamp = %v, want zero", events[4].Timestamp)
		}
		d := p.LastDetail()
		if d.Dataset != "tank/data" || d.RecordsLost != 5 || d.RingSwaps != 1 {
			t.Errorf("LastDetail = %+v, want {tank/data 5 1} (KnownLost->RecordsLost, RingSwaps separate, never folded)", d)
		}
	})

	t.Run("key filter: exact on resolved, conservative on PARTIAL", func(t *testing.T) {
		events, err := p.History(ctx, "/mnt/tank/data", "dir/obj", HistoryQuery{})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		// Resolved rows 10/20/50 match exactly; PARTIAL row 30 matches
		// conservatively ("dir/obj" ends in "/obj"); row 40 does not.
		if len(events) != 4 {
			t.Fatalf("got %d events, want 4: %+v", len(events), events)
		}
		for _, e := range events {
			if e.Key == "dir/other" {
				t.Errorf("unrelated resolved key leaked through filter: %+v", e)
			}
		}
	})

	t.Run("Since drops only older rows, zero-timestamp passes", func(t *testing.T) {
		since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		events, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{Since: since})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		// Row 10 (Jan 2026) is older -> dropped. Rows 20/30/40 pass.
		// Row 50 has zero Timestamp -> passes (unknown is not old).
		if len(events) != 4 {
			t.Fatalf("got %d events, want 4: %+v", len(events), events)
		}
		if events[0].Txg != 20 {
			t.Errorf("first event txg = %d, want 20 (older-than-Since row dropped)", events[0].Txg)
		}
		if !events[len(events)-1].Timestamp.IsZero() {
			t.Errorf("zero-timestamp row was dropped or altered: %+v", events[len(events)-1])
		}
		if events[len(events)-1].Timestamp != zeroTime {
			t.Errorf("last event Timestamp = %v, want zero", events[len(events)-1].Timestamp)
		}
	})

	t.Run("key filter runs BEFORE Since (index desync rule)", func(t *testing.T) {
		since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		events, err := p.History(ctx, "/mnt/tank/data", "dir/obj", HistoryQuery{Since: since})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		// key "dir/obj": rows 10,20,30(partial),50. Since drops row 10.
		// Remaining: 20, 30, 50(zero passes).
		if len(events) != 3 {
			t.Fatalf("got %d events, want 3: %+v", len(events), events)
		}
		wantTxgs := []uint64{20, 30, 50}
		for i, e := range events {
			if e.Txg != wantTxgs[i] {
				t.Errorf("events[%d].Txg = %d, want %d", i, e.Txg, wantTxgs[i])
			}
		}
	})

	t.Run("MaxEvents caps post-filter", func(t *testing.T) {
		events, err := p.History(ctx, "/mnt/tank/data", "dir/obj", HistoryQuery{MaxEvents: 2})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		// 4 rows match the key; cap applies AFTER filtering.
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2 (post-filter cap)", len(events))
		}
		if events[0].Txg != 10 || events[1].Txg != 20 {
			t.Errorf("capped events txgs = %d,%d, want 10,20", events[0].Txg, events[1].Txg)
		}
		// Fetch headroom hack is GONE: Events must be called with max=0.
	})

	t.Run("detail exposes KnownLost and RingSwaps separately", func(t *testing.T) {
		// SCHEMA.md section 4: gaps rows lost=5 (sum) and lost=-1 (swap
		// sentinel, count 1) must never fold: RecordsLost=5, RingSwaps=1,
		// NOT RecordsLost=4 or 6.
		_, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		d := p.LastDetail()
		if d.RecordsLost != 5 {
			t.Errorf("RecordsLost = %d, want 5 (KnownLost only)", d.RecordsLost)
		}
		if d.RingSwaps != 1 {
			t.Errorf("RingSwaps = %d, want 1 (separate class)", d.RingSwaps)
		}
	})

	t.Run("History performs ZERO execs", func(t *testing.T) {
		runnerMu.Lock()
		n := len(*runnerCalls)
		runnerMu.Unlock()
		if n != 0 {
			t.Errorf("zmetadRunner called %d times during History, want 0", n)
		}
	})

	t.Run("DB opened per call and closed", func(t *testing.T) {
		opensBefore := db.eventsCalls.Load()
		if _, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{}); err != nil {
			t.Fatalf("History: %v", err)
		}
		if db.eventsCalls.Load() != opensBefore+1 {
			t.Errorf("Events call count did not advance")
		}
		if db.closeCalls.Load() == 0 {
			t.Errorf("stub DB was never closed (want defer Close per call)")
		}
	})
}

func TestZmetadProviderHistoryErrors(t *testing.T) {
	dbPath := "/var/lib/zfs/zmetad.db"
	ctx := context.Background()

	t.Run("unresolvable dataset propagates error", func(t *testing.T) {
		db := &stubZmetadDB{resolveErr: &DatasetNotTrackedError{Path: "/mnt/tank/data"}}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		p := newTestProvider(t, dbPath, "")
		// Cold cache + nonexistent /mnt path: resolution fails (path
		// resolve, then would be DatasetNotTrackedError on a real host);
		// the error must propagate, never degrade to empty history.
		if _, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{}); err == nil {
			t.Fatal("History on untracked path: want error")
		}
	})

	t.Run("openDB failure propagates wrapped error", func(t *testing.T) {
		stubOpenDB(t, nil, errors.New("metadata: zmetad database not found"))
		p := newTestProvider(t, dbPath, "")
		// Warm the cache first so the open in History (not dataset) fails.
		p.cacheDataset("/mnt/tank/data", "tank/data")
		_, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{})
		if err == nil || !strings.Contains(err.Error(), "metadata: open zmetad database") {
			t.Fatalf("History err = %v, want wrapped metadata: open error", err)
		}
	})

	t.Run("Events error propagates", func(t *testing.T) {
		db := &stubZmetadDB{resolveDS: "tank/data", eventsErr: errors.New("metadata: query events: boom")}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		p := newTestProvider(t, dbPath, "")
		p.cacheDataset("/mnt/tank/data", "tank/data")
		if _, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{}); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("GapStats error propagates", func(t *testing.T) {
		db := &stubZmetadDB{resolveDS: "tank/data", gapErr: errors.New("metadata: query gap stats: boom")}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		p := newTestProvider(t, dbPath, "")
		p.cacheDataset("/mnt/tank/data", "tank/data")
		if _, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{}); err == nil {
			t.Fatal("want error")
		}
	})
}

// ---------------------------------------------------------------------------
// Task 3: concurrency (-race; bughunt A2/C2 cross-attribution)
// ---------------------------------------------------------------------------

func TestZmetadProviderConcurrentHistoryNoCrossAttribution(t *testing.T) {
	dbA := "/var/lib/zfs/a.db"
	dbB := "/var/lib/zfs/b.db"
	rowsA := []EventRow{row("CREATE", 1, 10, new("a"), new("a"), withCapturedAt(1700000000))}
	rowsB := []EventRow{row("CREATE", 1, 20, new("b"), new("b"), withCapturedAt(1700000001))}
	stubA := &stubZmetadDB{resolveDS: "tank/a", hasDS: true, events: rowsA,
		gaps: GapStats{KnownLost: 11, RingSwaps: 1}}
	stubB := &stubZmetadDB{resolveDS: "tank/b", hasDS: true, events: rowsB,
		gaps: GapStats{KnownLost: 22, RingSwaps: 2}}
	stubOpenDB(t, map[string]*stubZmetadDB{dbA: stubA, dbB: stubB}, nil)

	// One provider per bucket path is the production shape, but the A2/C2
	// failure mode is per-instance detail under concurrent History on the
	// SAME instance across two datasets - test both shapes.
	shared := newTestProvider(t, dbA, "")
	shared.cacheDataset("/mnt/tank/a", "tank/a")

	pa := newTestProvider(t, dbA, "")
	pb := newTestProvider(t, dbB, "")
	pa.cacheDataset("/mnt/tank/a", "tank/a")
	pb.cacheDataset("/mnt/tank/b", "tank/b")

	var wg sync.WaitGroup
	const goroutines = 8
	errCh := make(chan error, goroutines*2)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			p := pa
			path, wantDS, wantLost, wantSwaps := "/mnt/tank/a", "tank/a", uint64(11), uint64(1)
			if i%2 == 1 {
				p, path, wantDS, wantLost, wantSwaps = pb, "/mnt/tank/b", "tank/b", uint64(22), uint64(2)
			}
			events, err := p.History(ctx, path, "", HistoryQuery{})
			if err != nil {
				errCh <- fmt.Errorf("History(%s): %w", path, err)
				return
			}
			if len(events) != 1 {
				errCh <- fmt.Errorf("History(%s) returned %d events, want 1", path, len(events))
			}
			// Detail read immediately after, per the endpoint contract:
			// must belong to THIS provider instance, never the other.
			d := p.LastDetail()
			if d.Dataset != wantDS || d.RecordsLost != wantLost || d.RingSwaps != wantSwaps {
				errCh <- fmt.Errorf("cross-attributed detail on %s: %+v, want {%s %d %d}",
					path, d, wantDS, wantLost, wantSwaps)
			}
			// The shared instance concurrently histories dataset A only.
			if _, err := shared.History(ctx, "/mnt/tank/a", "", HistoryQuery{}); err != nil {
				errCh <- fmt.Errorf("shared History: %w", err)
				return
			}
			if d := shared.LastDetail(); d.Dataset != "tank/a" || d.RecordsLost != 11 || d.RingSwaps != 1 {
				errCh <- fmt.Errorf("shared provider cross-attributed: %+v", d)
			}
		}(i)
	}
	// Concurrent Probe + History on the shared instance (cache race).
	for range 4 {
		wg.Go(func() {
			shared.probeDB(context.Background(), "/mnt/tank/a", "/mnt/tank/a")
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// ---------------------------------------------------------------------------
// Task 4: Purge via the zmetad binary
// ---------------------------------------------------------------------------

func TestZmetadProviderPurge(t *testing.T) {
	dbPath := "/var/lib/zfs/zmetad.db"
	ctx := context.Background()

	t.Run("success passes --purge dataset with the configured binary", func(t *testing.T) {
		db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		calls, mu := stubZmetadRunner(t, "", nil)
		p := newTestProvider(t, dbPath, "/usr/local/bin/zmetad")
		p.cacheDataset("/mnt/tank/data", "tank/data")
		if err := p.Purge(ctx, "/mnt/tank/data"); err != nil {
			t.Fatalf("Purge: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(*calls) != 1 {
			t.Fatalf("runner called %d times, want 1", len(*calls))
		}
		c := (*calls)[0]
		if c.binary != "/usr/local/bin/zmetad" {
			t.Errorf("binary = %q, want /usr/local/bin/zmetad", c.binary)
		}
		if len(c.args) != 2 || c.args[0] != "--purge" || c.args[1] != "tank/data" {
			t.Errorf("args = %v, want [--purge tank/data]", c.args)
		}
	})

	t.Run("default binary is zmetad on PATH", func(t *testing.T) {
		db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		calls, mu := stubZmetadRunner(t, "", nil)
		p := newTestProvider(t, dbPath, "")
		p.cacheDataset("/mnt/tank/data", "tank/data")
		if err := p.Purge(ctx, "/mnt/tank/data"); err != nil {
			t.Fatalf("Purge: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(*calls) != 1 || (*calls)[0].binary != "zmetad" {
			t.Errorf("calls = %+v, want one call with binary %q", *calls, "zmetad")
		}
	})

	t.Run("non-zero exit propagates wrapped error with dataset and stderr", func(t *testing.T) {
		db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		stubZmetadRunner(t, "zmetad: ring busy", errors.New("exit status 1"))
		p := newTestProvider(t, dbPath, "")
		p.cacheDataset("/mnt/tank/data", "tank/data")
		err := p.Purge(ctx, "/mnt/tank/data")
		if err == nil {
			t.Fatal("want error on non-zero exit")
		}
		for _, want := range []string{"metadata: ", "zmetad --purge tank/data", "exit status 1", "ring busy"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err.Error(), want)
			}
		}
	})

	t.Run("timeout propagates wrapped context error", func(t *testing.T) {
		db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		stubZmetadRunner(t, "", context.DeadlineExceeded)
		p := newTestProvider(t, dbPath, "")
		p.cacheDataset("/mnt/tank/data", "tank/data")
		err := p.Purge(ctx, "/mnt/tank/data")
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Purge err = %v, want wrapped context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "tank/data") {
			t.Errorf("error %q missing dataset name", err.Error())
		}
	})

	t.Run("runner never called at Probe or History", func(t *testing.T) {
		db := &stubZmetadDB{resolveDS: "tank/data", hasDS: true}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		calls, mu := stubZmetadRunner(t, "", nil)
		p := newTestProvider(t, dbPath, "")
		p.probeDB(ctx, "/mnt/tank/data", "/mnt/tank/data")
		if _, err := p.History(ctx, "/mnt/tank/data", "", HistoryQuery{}); err != nil {
			t.Fatalf("History: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(*calls) != 0 {
			t.Errorf("zmetadRunner invoked %d times during Probe/History, want 0 (purge-only)", len(*calls))
		}
	})

	t.Run("unresolvable dataset fails before any exec", func(t *testing.T) {
		db := &stubZmetadDB{resolveErr: &DatasetNotTrackedError{Path: "/mnt/tank/data"}}
		stubOpenDB(t, map[string]*stubZmetadDB{dbPath: db}, nil)
		calls, mu := stubZmetadRunner(t, "", nil)
		p := newTestProvider(t, dbPath, "")
		if err := p.Purge(ctx, "/mnt/tank/data"); err == nil {
			t.Fatal("want error")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(*calls) != 0 {
			t.Errorf("runner called on unresolvable dataset, want 0")
		}
	})
}
