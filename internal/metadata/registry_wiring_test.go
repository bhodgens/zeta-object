package metadata

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
)

// TestLastDetailIsPerInstance pins the A2/C2 race fix: the zfs-events
// provider carries its HistoryDetail on the instance (read via the
// LastDetail seam the frontend's detailReporter interface uses), so
// concurrent History calls on DIFFERENT provider instances (different
// buckets) can no longer cross-attribute dataset/recordsLost through the
// package-global lastDetail.
func TestLastDetailIsPerInstance(t *testing.T) {
	orig := zfsRunner
	defer func() { zfsRunner = orig }()
	origResolve := resolveDatasetFn
	defer func() { resolveDatasetFn = origResolve }()

	// Each dataset name maps to a distinct recordsLost count so a
	// cross-attribute read is unambiguous.
	const (
		dsA = "tank/alpha"
		dsB = "tank/beta"
	)
	resolveDatasetFn = func(ctx context.Context, path string) (string, error) {
		if strings.HasSuffix(path, "alpha") {
			return dsA, nil
		}
		return dsB, nil
	}
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		ds := args[len(args)-1]
		if ds == dsA {
			return []byte(`[{"txg":1,"object":2,"op":"CREATE","name":"a"}]`), "", nil
		}
		// beta reports ring-buffer loss.
		return []byte("[{\"txg\":2,\"object\":3,\"op\":\"REMOVE\"}]\n" +
			"9 record(s) lost to log wraparound\n"), "", nil
	}

	pa := NewZFSEventsProvider()
	pb := NewZFSEventsProvider()
	drA, okA := pa.(interface{ LastDetail() HistoryDetail })
	drB, okB := pb.(interface{ LastDetail() HistoryDetail })
	if !okA || !okB {
		t.Fatal("zfs-events provider must implement the LastDetail detailReporter seam")
	}

	// Interleave many concurrent histories; afterwards each instance must
	// carry ONLY its own dataset/recordsLost.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = pa.History(context.Background(), "/mnt/alpha", "", HistoryQuery{}) }()
		go func() { defer wg.Done(); _, _ = pb.History(context.Background(), "/mnt/beta", "", HistoryQuery{}) }()
	}
	wg.Wait()

	if d := drA.LastDetail(); d.Dataset != dsA || d.RecordsLost != 0 {
		t.Fatalf("instance A LastDetail = %+v, want dataset %s, 0 lost", d, dsA)
	}
	if d := drB.LastDetail(); d.Dataset != dsB || d.RecordsLost != 9 {
		t.Fatalf("instance B LastDetail = %+v, want dataset %s, 9 lost", d, dsB)
	}
}

// TestProbeAndAttachLogsUnavailable pins the A5 fix: probe failures and
// unavailability reasons are LOGGED (with provider name and bucket path),
// while the never-abort contract is preserved (loop continues, available
// providers still attach).
func TestProbeAndAttachLogsUnavailable(t *testing.T) {
	Register(&fakeProvider{name: "log-hit", probeRes: ProbeResult{Available: true}})
	Register(&fakeProvider{name: "log-miss", probeRes: ProbeResult{Available: false, Reason: "filesystem is not ZFS"}})
	Register(&fakeProvider{name: "log-err", probeErr: context.DeadlineExceeded})

	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(origOut); log.SetFlags(origFlags) }()

	got := ProbeAndAttach(context.Background(), "/some/bucket")

	// The registry is shared process-wide (other tests register their own
	// providers), so assert on OUR providers only: the hit attaches, the
	// unavailable/failed ones never do — the never-abort contract holds.
	gotSet := map[string]bool{}
	for _, name := range got {
		gotSet[name] = true
	}
	if !gotSet["log-hit"] {
		t.Fatalf("ProbeAndAttach = %v, want log-hit attached", got)
	}
	if gotSet["log-miss"] || gotSet["log-err"] {
		t.Fatalf("ProbeAndAttach = %v, want log-miss/log-err never attached", got)
	}
	out := buf.String()
	for _, want := range []string{
		`provider "log-miss" not attached`,
		"filesystem is not ZFS",
		`probe of provider "log-err" for bucket /some/bucket failed`,
		"context deadline exceeded",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("probe log missing %q:\n%s", want, out)
		}
	}
}
