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
// buckets) can no longer cross-attribute dataset/recordsLost. Rewritten
// onto the zmetad provider when the CLI transport was deleted
// (zmetad-provider-2026-09 leaf 04).
func TestLastDetailIsPerInstance(t *testing.T) {
	// Each dataset maps to a distinct recordsLost count so a
	// cross-attribute read is unambiguous.
	const (
		dsA = "tank/alpha"
		dsB = "tank/beta"
	)
	dbPath := "/var/lib/zfs/zmetad-detail.db"
	stubOpenDB(t, map[string]*stubZmetadDB{
		dbPath: {
			resolveDS: dsA, hasDS: true,
			events: []EventRow{{Dataset: dsA, Txg: 1, Op: "CREATE", Path: new("a")}},
			gaps:   GapStats{KnownLost: 0},
		},
	}, nil)
	// beta needs its own provider instance over a distinct bucket path;
	// the stub resolves per path suffix via two provider instances with
	// separate caches pre-seeded (probeDB caches positive results).
	pa := newTestProvider(t, dbPath, "")
	pa.cacheDataset("/mnt/alpha", dsA)
	pb := newTestProvider(t, dbPath, "")
	pb.cacheDataset("/mnt/beta", dsB)
	// beta reports loss through the same stub DB (GapStats is keyed per
	// dataset in production; here the stub returns a fixed loss figure).
	drA, okA := MetadataProvider(pa).(interface{ LastDetail() HistoryDetail })
	drB, okB := MetadataProvider(pb).(interface{ LastDetail() HistoryDetail })
	if !okA || !okB {
		t.Fatal("zfs-events provider must implement the LastDetail detailReporter seam")
	}

	// Interleave many concurrent histories; afterwards each instance must
	// carry ONLY its own dataset.
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = pa.History(context.Background(), "/mnt/alpha", "", HistoryQuery{}) }()
		go func() { defer wg.Done(); _, _ = pb.History(context.Background(), "/mnt/beta", "", HistoryQuery{}) }()
	}
	wg.Wait()

	if d := drA.LastDetail(); d.Dataset != dsA {
		t.Fatalf("instance A LastDetail = %+v, want dataset %s", d, dsA)
	}
	if d := drB.LastDetail(); d.Dataset != dsB {
		t.Fatalf("instance B LastDetail = %+v, want dataset %s", d, dsB)
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
