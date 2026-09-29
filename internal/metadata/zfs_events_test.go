package metadata

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRunZFSCmdUsesRunner pins the seam: every zfs invocation in the
// zfs-events provider must dispatch through the zfsRunner var so tests
// can record-and-replay without the binary (macOS CI has no zfs).
func TestRunZFSCmdUsesRunner(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	zfsRunner = func(ctx context.Context, args ...string) (stdout []byte, stderr string, err error) {
		if args[0] != "events" {
			t.Fatalf("args = %v, want events first", args)
		}
		return []byte("[]"), "", nil
	}
	out, _, err := runZFS(context.Background(), "events", "-j", "tank/data")
	if err != nil || string(out) != "[]" {
		t.Fatalf("runZFS = %q, %v", out, err)
	}
}

// TestParseEventsOutput covers every shape of the pinned print_event wire
// format (zfs-metadata zfs_main.c:8332) plus the forward-compatible
// optional kernel fields, per master Contract 3.
func TestParseEventsOutput(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     []ObjectEvent
		wantLost uint64
		wantErr  bool
	}{
		{
			name: "empty array no loss",
			in:   "[]\n",
			want: []ObjectEvent{},
		},
		{
			name: "create with name",
			in:   `[{"txg":1234,"object":256,"op":"CREATE","name":"report.pdf"}]` + "\n",
			want: []ObjectEvent{{Op: "create", Key: "report.pdf", Txg: 1234}},
		},
		{
			name: "rename carries old_name",
			in:   `[{"txg":1251,"object":300,"op":"RENAME","name":"new.txt","old_name":"old.txt","old_parent":2}]` + "\n",
			want: []ObjectEvent{{Op: "rename", Key: "new.txt", OldKey: "old.txt", Txg: 1251}},
		},
		{
			name: "truncate sizes",
			in:   `[{"txg":1240,"object":256,"op":"TRUNCATE","name":"f","old_size":1024,"new_size":2048}]` + "\n",
			want: []ObjectEvent{{Op: "truncate", Key: "f", Txg: 1240, SizeOld: 1024, SizeNew: 2048}},
		},
		{
			name: "symlink target",
			in:   `[{"txg":1255,"object":301,"op":"SYMLINK","name":"link","target":"a.txt"}]` + "\n",
			want: []ObjectEvent{{Op: "symlink", Key: "link", Txg: 1255}},
		},
		{
			name:     "records lost line after array",
			in:       "[{\"txg\":1,\"object\":2,\"op\":\"REMOVE\"}]\n7 record(s) lost to log wraparound\n",
			want:     []ObjectEvent{{Op: "remove", Txg: 1}},
			wantLost: 7, // ordinary path: exact count preserved
		},
		{
			name:     "records lost count overflows uint64",
			in:       "[{\"txg\":1,\"object\":2,\"op\":\"REMOVE\"}]\n99999999999999999999999999 record(s) lost to log wraparound\n",
			want:     []ObjectEvent{{Op: "remove", Txg: 1}},
			wantLost: 1,
		},
		{
			name: "records lost count zero still surfaces lossy",
			in:   "[{\"txg\":1,\"object\":2,\"op\":\"REMOVE\"}]\n0 record(s) lost to log wraparound\n",
			want: []ObjectEvent{{Op: "remove", Txg: 1}},
			// A trailer that says 0 is itself anomalous (upstream only
			// prints it when records were lost); surface the conservative
			// sentinel rather than a silent lossless claim.
			wantLost: 1,
		},
		{
			name: "future fields tolerated and mapped",
			in:   `[{"txg":9,"object":2,"op":"SETATTR","name":"x","time":500,"uid":1000,"gid":100,"mode":420,"attrs":7}]` + "\n",
			want: []ObjectEvent{{Op: "setattr", Key: "x", Txg: 9, UID: 1000, GID: 100,
				Timestamp: time.Unix(0, 500)}},
		},
		{
			name:    "garbage output",
			in:      "this is not json\n",
			wantErr: true,
		},
		{
			name:    "truncated json",
			in:      `[{"txg":1,`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, lost, err := parseEventsOutput(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseEventsOutput err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if lost != tt.wantLost {
				t.Fatalf("lost = %d, want %d", lost, tt.wantLost)
			}
			if len(events) != len(tt.want) {
				t.Fatalf("got %d events, want %d", len(events), len(tt.want))
			}
			for i := range events {
				if events[i] != tt.want[i] { // ObjectEvent is comparable
					t.Fatalf("event[%d] = %+v, want %+v", i, events[i], tt.want[i])
				}
			}
		})
	}
}

func TestHistoryFilteringAndMaxEvents(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	var gotArgs []string
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		gotArgs = args
		return []byte(`[` +
			`{"txg":1,"object":10,"op":"CREATE","name":"a"},` +
			`{"txg":2,"object":11,"op":"CREATE","name":"b"},` +
			`{"txg":3,"object":10,"op":"TRUNCATE","name":"a","old_size":1,"new_size":2}]`), "", nil
	}
	// Inject a dataset resolver so History never shells out to the real
	// zfs binary (the runner seam covers the events call; resolveDatasetFn
	// covers the zfs get name,mountpoint call leaf 01 owns).
	origResolve := resolveDatasetFn
	defer func() { resolveDatasetFn = origResolve }()
	resolveDatasetFn = func(ctx context.Context, path string) (string, error) {
		return "tank/data", nil
	}

	p := &zfsEventsProvider{}
	events, err := p.History(context.Background(), "/mnt/tank/data", "",
		HistoryQuery{MaxEvents: 2})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// Newest-first (ring buffer returns newest first); MaxEvents caps.
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Txg != 1 || events[1].Txg != 2 {
		t.Fatalf("MaxEvents cap kept wrong slice: %+v", events)
	}
	// The fetch adds -n headroom so the post-parse cap can still fill.
	if len(gotArgs) < 3 || gotArgs[2] != "-n" {
		t.Fatalf("args = %v, want -n headroom flag", gotArgs)
	}

	// Key filter: only object 10 / key "a" events (both create and truncate).
	events, err = p.History(context.Background(), "/mnt/tank/data", "a", HistoryQuery{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 2 || events[0].Key != "a" || events[1].Key != "a" {
		t.Fatalf("key filter: %+v", events)
	}
}

func TestHistoryPropagatesRecordsLost(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		return []byte("[{\"txg\":1,\"object\":2,\"op\":\"REMOVE\",\"name\":\"gone\"}]\n" +
			"3 record(s) lost to log wraparound\n"), "", nil
	}
	origResolve := resolveDatasetFn
	defer func() { resolveDatasetFn = origResolve }()
	resolveDatasetFn = func(ctx context.Context, path string) (string, error) {
		return "tank/data", nil
	}

	p := &zfsEventsProvider{}
	if _, err := p.History(context.Background(), "/mnt/tank/data", "", HistoryQuery{}); err != nil {
		t.Fatalf("History: %v", err)
	}
	d := LastHistoryDetail()
	if d.Dataset != "tank/data" || d.RecordsLost != 3 {
		t.Fatalf("LastHistoryDetail = %+v, want dataset tank/data, 3 lost", d)
	}
}

// TestHistorySurfacesExecFailure pins that a zfs exec failure surfaces the
// dataset and stderr, not a silent empty history.
func TestHistorySurfacesExecFailure(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		return nil, "no event log found for 'tank/data'", context.DeadlineExceeded
	}
	origResolve := resolveDatasetFn
	defer func() { resolveDatasetFn = origResolve }()
	resolveDatasetFn = func(ctx context.Context, path string) (string, error) {
		return "tank/data", nil
	}

	p := &zfsEventsProvider{}
	_, err := p.History(context.Background(), "/mnt/tank/data", "", HistoryQuery{})
	if err == nil {
		t.Fatal("History must error when the zfs exec fails")
	}
	if !strings.Contains(err.Error(), "tank/data") || !strings.Contains(err.Error(), "no event log") {
		t.Fatalf("err = %v, want dataset + stderr context", err)
	}
}

// TestProbeDegradesGracefully pins the degradation path: on a non-ZFS
// filesystem Probe reports Available=false + Reason with a nil error, and
// never execs zfs.
func TestProbeDegradesGracefully(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("degradation path pinned on hosts without ZFS statfs magic")
	}
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		t.Fatalf("zfs must not be exec'd when DetectZFS fails first")
		return nil, "", nil
	}
	p := NewZFSEventsProvider()
	res, err := p.Probe(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Probe returned error: %v (must degrade, not error)", err)
	}
	if res.Available {
		t.Fatalf("Probe = available on non-ZFS fs: %+v", res)
	}
	if res.Reason == "" {
		t.Fatal("Probe must record a Reason when unavailable")
	}
}

// TestProbeAvailableWhenFeatureOn drives the GOOS-independent probeCore
// directly (DetectZFS is statfs-bound and macOS has no ZFS), faking the
// `zfs get -H -o value events <ds>` output through the runner seam.
func TestProbeAvailableWhenFeatureOn(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	var gotArgs []string
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		gotArgs = args
		return []byte("on\n"), "", nil
	}
	p := &zfsEventsProvider{}
	res, err := p.probeCore(context.Background(), "/mnt/tank/data", "tank/data")
	if err != nil {
		t.Fatalf("probeCore error: %v", err)
	}
	if !res.Available || res.Dataset != "tank/data" || res.Reason != "" {
		t.Fatalf("probeCore = %+v, %v", res, err)
	}
	if len(gotArgs) < 5 || gotArgs[0] != "get" || gotArgs[len(gotArgs)-1] != "tank/data" {
		t.Fatalf("args = %v, want zfs get ... events tank/data", gotArgs)
	}
}

func TestProbeFeatureOffDegrades(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		return []byte("off\n"), "", nil
	}
	p := &zfsEventsProvider{}
	res, err := p.probeCore(context.Background(), "/mnt/tank/data", "tank/data")
	if err != nil {
		t.Fatalf("probeCore error: %v (feature-off must degrade, not error)", err)
	}
	if res.Available || res.Reason == "" || res.Dataset != "tank/data" {
		t.Fatalf("probeCore = %+v, want unavailable with Reason", res)
	}
}

func TestProbeFeatureCheckFailureDegrades(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		return nil, "property 'events' not found", context.DeadlineExceeded
	}
	p := &zfsEventsProvider{}
	res, err := p.probeCore(context.Background(), "/mnt/tank/data", "tank/data")
	if err != nil {
		t.Fatalf("probeCore error: %v (exec failure must degrade, not error)", err)
	}
	if res.Available || res.Reason == "" {
		t.Fatalf("probeCore = %+v, want unavailable with Reason", res)
	}
}

// TestPurgeClearsLog pins Purge: `zfs events -c <dataset>` with stderr in
// the error on failure.
func TestPurgeClearsLog(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	var gotArgs []string
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		gotArgs = args
		return []byte("cleared event log for 'tank/data'\n"), "", nil
	}
	origResolve := resolveDatasetFn
	defer func() { resolveDatasetFn = origResolve }()
	resolveDatasetFn = func(ctx context.Context, path string) (string, error) {
		return "tank/data", nil
	}

	p := &zfsEventsProvider{}
	if err := p.Purge(context.Background(), "/mnt/tank/data"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if len(gotArgs) != 3 || gotArgs[0] != "events" || gotArgs[1] != "-c" || gotArgs[2] != "tank/data" {
		t.Fatalf("args = %v, want events -c tank/data", gotArgs)
	}
}

func TestPurgeFailureIncludesStderr(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		return nil, "no event log found for 'tank/data'", context.DeadlineExceeded
	}
	origResolve := resolveDatasetFn
	defer func() { resolveDatasetFn = origResolve }()
	resolveDatasetFn = func(ctx context.Context, path string) (string, error) {
		return "tank/data", nil
	}

	p := &zfsEventsProvider{}
	err := p.Purge(context.Background(), "/mnt/tank/data")
	if err == nil || !strings.Contains(err.Error(), "no event log") {
		t.Fatalf("Purge err = %v, want stderr context", err)
	}
}

// TestNamePinsProviderName guards the registry key ("zfs-events") that
// config wiring in later leaves depends on.
func TestNamePinsProviderName(t *testing.T) {
	if got := NewZFSEventsProvider().Name(); got != "zfs-events" {
		t.Fatalf("Name() = %q, want zfs-events", got)
	}
}

// TestRealZFSSkipsWithoutBinary is the real-host integration path; it must
// skip cleanly on macOS dev hosts where the zfs binary is absent.
func TestRealZFSSkipsWithoutBinary(t *testing.T) {
	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs binary not on PATH (macOS dev host) — integration skipped")
	}
	ds := os.Getenv("MINIS3_ZFS_TEST_DATASET")
	if ds == "" {
		t.Skip("set MINIS3_ZFS_TEST_DATASET to run real zfs integration")
	}
	// Real-host path: exercise Probe + History + Purge against the real
	// dataset via the provider, no seams.
	p := NewZFSEventsProvider()
	res, err := p.Probe(context.Background(), "/"+ds)
	if err != nil || !res.Available {
		t.Fatalf("Probe = %+v, %v on real ZFS host", res, err)
	}
	events, err := p.History(context.Background(), "/"+ds, "", HistoryQuery{MaxEvents: 10})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, e := range events {
		if e.Timestamp.After(time.Now()) {
			t.Logf("event txg %d has future timestamp (hrtime is not wall-clock)", e.Txg)
		}
	}
	if err := p.Purge(context.Background(), "/"+ds); err != nil {
		t.Fatalf("Purge: %v", err)
	}
}

// silence unused-import if filepath drops out of the real-host path later
var _ = filepath.Join
