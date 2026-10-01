package metadata

// zmetad_fixture_test.go - smoke test for the e2e fixture builder
// (scripts/e2e/fixtures/zmetad-fixture, zmetad-provider-2026-09 leaf 05
// task 2.2): build the canned database into a temp dir with `go run`,
// open it through the PRODUCTION accessor (OpenZmetadDB), and assert the
// contract values the e2e case pins: GapStats{KnownLost:7, Regressions:0,
// RingSwaps:1}, dataset resolution through the datasets table, the
// recorded-fixture row-form (full_path served verbatim), the NULL
// captured_at wall-clock policy, and the nested-key conservative
// bare-name match on the PARTIAL row.

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// buildFixtureDB compiles + runs the e2e fixture builder against a temp
// path and returns it. Skips the test (not fails) when `go run` cannot
// build on this host - the same graceful-skip convention case 18 uses.
func buildFixtureDB(t *testing.T, mountpoint string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "zmetad-fixture.db")
	cmd := exec.Command("go", "run", "./scripts/e2e/fixtures/zmetad-fixture", dbPath, mountpoint)
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("fixture builder cannot run on this host: %v\n%s", err, out)
	}
	return dbPath
}

func TestZmetadFixtureBuilderSmoke(t *testing.T) {
	mount := "/mnt/testpool/e2e"
	dbPath := buildFixtureDB(t, mount)

	ctx := context.Background()
	db, err := OpenZmetadDB(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenZmetadDB on fixture: %v", err)
	}
	defer db.Close()

	// Dataset resolution: the datasets table maps the bucket mountpoint.
	ds, err := db.ResolveDatasetByPath(mount + "/bucketdir")
	if err != nil {
		t.Fatalf("ResolveDatasetByPath: %v", err)
	}
	if ds != "testpool/e2e" {
		t.Fatalf("dataset = %q, want testpool/e2e", ds)
	}
	if ok, err := db.HasDataset(ds); err != nil || !ok {
		t.Fatalf("HasDataset = %v, %v; want true, nil (sync_state row)", ok, err)
	}

	// Loss classes: known loss 7, no regressions, ONE ring swap - never
	// folded (SCHEMA.md section 4, Contract 5).
	stats, err := db.GapStats(ds)
	if err != nil {
		t.Fatalf("GapStats: %v", err)
	}
	want := GapStats{KnownLost: 7, Regressions: 0, RingSwaps: 1}
	if stats != want {
		t.Fatalf("GapStats = %+v, want %+v", stats, want)
	}

	// Event rows: the recorded nested fixture's row-form (9 rows) plus
	// the synthetic captured_at SETATTR and the PARTIAL nested CREATE.
	rows, err := db.Events(ds, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(rows) != 11 {
		t.Fatalf("event rows = %d, want 11", len(rows))
	}

	// Recorded-fixture rows serve full_path verbatim (drift-guard shape).
	byTxg := map[uint64]EventRow{}
	for _, r := range rows {
		byTxg[r.Txg] = r
	}
	ren := byTxg[32030]
	if ren.FullPath == nil || *ren.FullPath != "edge/inner/renamed.txt" ||
		ren.OldFullPath == nil || *ren.OldFullPath != "edge/inner/deep.txt" {
		t.Fatalf("rename row = %+v, want resolved edge/inner paths", ren)
	}
	tr := byTxg[32022]
	if tr.Size == nil || *tr.Size != 4096 || tr.OldSize == nil || *tr.OldSize != 1024 {
		t.Fatalf("truncate row sizes = %+v, want 4096/1024", tr)
	}

	// captured_at row: wall clock straight off the column.
	sa := byTxg[32055]
	if sa.CapturedAt == nil {
		t.Fatal("SETATTR row must carry captured_at")
	}

	// PARTIAL row: NULL full_path, bare name kept, ancestor absent.
	orphan := byTxg[32060]
	if orphan.FullPath != nil {
		t.Fatalf("PARTIAL row has full_path %q, want NULL", *orphan.FullPath)
	}
	if orphan.Path == nil || *orphan.Path != "orphan.txt" {
		t.Fatalf("PARTIAL row path = %v, want orphan.txt", orphan.Path)
	}
	if orphan.Parent == nil || *orphan.Parent != 999 {
		t.Fatalf("PARTIAL row parent = %v, want 999 (absent from objmap)", orphan.Parent)
	}

	// Row -> wire mapping through the production mapper, then the
	// conservative key match: the nested key resolves the PARTIAL create
	// via bare-name suffix match, never via a fabricated full path.
	set := rowsToEvents(rows)
	var orphanIdx = -1
	for i, e := range set.events {
		if e.Op == "create" && e.Key == "orphan.txt" {
			orphanIdx = i
			if set.resolved[i] {
				t.Fatal("PARTIAL row must map with resolved=false")
			}
			if !e.Timestamp.IsZero() {
				t.Fatalf("NULL captured_at + zero hrtime must map to zero time, got %v", e.Timestamp)
			}
		}
		if e.Op == "setattr" && e.Key == "edge/inner/renamed.txt" {
			if e.Timestamp != time.Unix(1759276800, 0).UTC() {
				t.Fatalf("captured_at row timestamp = %v, want 2025-10-01T00:00:00Z", e.Timestamp)
			}
			if e.SizeNew != 2048 {
				t.Fatalf("captured_at row SizeNew = %d, want 2048", e.SizeNew)
			}
		}
	}
	if orphanIdx < 0 {
		t.Fatal("PARTIAL orphan.txt create missing from mapped events")
	}
	if !rowsMatchKey(set, orphanIdx, "edge/inner/orphan.txt") {
		t.Error("conservative bare-name match must hit nested key edge/inner/orphan.txt")
	}
	if !rowsMatchKey(set, orphanIdx, "orphan.txt") {
		t.Error("conservative bare-name match must hit the bare key")
	}
	if rowsMatchKey(set, orphanIdx, "other/orphan-x.txt") {
		t.Error("conservative match must not hit an unrelated key")
	}
}

// TestZmetadFixtureBuilderProviderChain drives the REAL provider (openDB
// seam left at production, ZETAOBJECT_ASSUME_ZFS bypassing only the
// statfs hint) over the fixture DB: Probe available, bucket History
// carries the Contract 5 detail (recordsLost=7, ringSwaps=1), and the
// key-scoped History returns the PARTIAL create for a nested key.
func TestZmetadFixtureBuilderProviderChain(t *testing.T) {
	mountDir := t.TempDir()
	// The provider EvalSymlinks the bucket path before datasets-table
	// resolution (macOS tempdirs sit behind /var -> /private/var), so
	// the fixture must map the RESOLVED form.
	mount, err := filepath.EvalSymlinks(mountDir)
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	dbPath := buildFixtureDB(t, mount)
	t.Setenv("ZETAOBJECT_ASSUME_ZFS", "1")

	p := NewZmetadEventsProvider(dbPath)
	res, err := p.Probe(context.Background(), mount)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Available || res.Dataset != "testpool/e2e" {
		t.Fatalf("Probe = %+v, want available on testpool/e2e", res)
	}

	events, err := p.History(context.Background(), mount, "", HistoryQuery{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 11 {
		t.Fatalf("bucket History = %d events, want 11", len(events))
	}
	d, ok := p.(interface{ LastDetail() HistoryDetail })
	if !ok {
		t.Fatal("provider must implement the LastDetail detailReporter seam")
	}
	detail := d.LastDetail()
	if detail.Dataset != "testpool/e2e" || detail.RecordsLost != 7 || detail.RingSwaps != 1 {
		t.Fatalf("LastDetail = %+v, want dataset testpool/e2e recordsLost 7 ringSwaps 1", detail)
	}

	// Key-scoped: the nested key gets the PARTIAL create (conservative
	// match) and the recorded fixture's resolved rows for that subtree.
	scoped, err := p.History(context.Background(), mount, "edge/inner/orphan.txt", HistoryQuery{})
	if err != nil {
		t.Fatalf("scoped History: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Op != "create" || scoped[0].Key != "orphan.txt" {
		t.Fatalf("scoped History = %+v, want exactly the PARTIAL orphan create", scoped)
	}
}

// TestZmetadProviderAssumeZFSGate pins the ZETAOBJECT_ASSUME_ZFS env
// gate (leaf 05): unset (production default), Probe fast-fails on the
// statfs hint and the DB is never opened; =1, the statfs hint is
// bypassed and the authoritative datasets/sync_state checks run.
func TestZmetadProviderAssumeZFSGate(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	dbPath := buildFixtureDB(t, resolved)
	p := NewZmetadEventsProvider(dbPath)

	// Production default: hint off -> unavailable, DB never opened.
	t.Setenv("ZETAOBJECT_ASSUME_ZFS", "")
	res, err := p.Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe error = %v, want nil", err)
	}
	if res.Available || res.Reason != "filesystem is not ZFS" {
		t.Fatalf("Probe with hint on (non-ZFS dir) = %+v, want unavailable 'filesystem is not ZFS'", res)
	}

	// =1: hint bypassed; the fixture's datasets + sync_state rows are
	// authoritative and the bucket attaches.
	t.Setenv("ZETAOBJECT_ASSUME_ZFS", "1")
	res, err = p.Probe(context.Background(), dir)
	if err != nil {
		t.Fatalf("Probe error = %v, want nil", err)
	}
	if !res.Available || res.Dataset != "testpool/e2e" {
		t.Fatalf("Probe with assume-ZFS = %+v, want available on testpool/e2e", res)
	}

	// A path the datasets table does NOT cover stays unavailable even
	// with the bypass - the DB remains authoritative.
	res, err = p.Probe(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Probe error = %v, want nil", err)
	}
	if res.Available || res.Reason != "not tracked by zmetad" {
		t.Fatalf("Probe of untracked dir with assume-ZFS = %+v, want unavailable 'not tracked by zmetad'", res)
	}
}
