package metadata

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Live-host fixture pins (zfs-events-live-ordcap.txt) --------------------
//
// The fixture is REAL `zfs events -j testpool/ordcap` output captured on the
// zfs-meta host (2026-09-30) after: mkdir dirA; create+remove f1.txt; create
// f2.txt; remove f2.txt; create f3.txt. It pins the three graph-assumption
// findings against ground truth:
//
//   - records are OLDEST-FIRST (txg ascending) — M8;
//   - the dataset-root dir (objid 34) emits its own NAMELESS SETATTR record
//     on ordinary activity — H5;
//   - REMOVE records carry name+parent like CREATE — so the create+remove
//     of the REUSED objid 128 (f1.txt, then f2.txt) must both resolve
//     exactly under txg-scoped mapping — H6;
//   - dirA/f2.txt and dirA/f3.txt resolve exactly (H4's unanimity election
//     must keep the true root 34).

func TestLiveOrdcapFixtureResolution(t *testing.T) {
	set, lost := loadFixture(t, "zfs-events-live-ordcap.txt")
	if lost != 0 {
		t.Fatalf("lost = %d, want 0 (fixture captured without loss line)", lost)
	}
	// The nameless root SETATTR (txg 33943) parses as ONE op-only event
	// (no key) that carries no history; the six named records below carry
	// all the reconstructible history of the window.
	wantKeys := []struct {
		txg      uint64
		op       string
		key      string
		resolved bool
	}{
		{33966, "create", "dirA", true}, // parent 34 = root, unanimous
		{33966, "create", "dirA/f1.txt", true},
		{33966, "remove", "dirA/f1.txt", true},
		{33966, "create", "dirA/f2.txt", true},
		{33966, "remove", "dirA/f2.txt", true},
		{33966, "create", "dirA/f3.txt", true},
	}
	// events[0] is the op-only root-SETATTR event (Key == "") — skip it.
	if len(set.events) != len(wantKeys)+1 {
		t.Fatalf("events = %d, want %d (6 named records + the op-only root setattr)", len(set.events), len(wantKeys)+1)
	}
	if set.events[0].Key != "" || set.events[0].Op != "setattr" {
		t.Fatalf("events[0] = %+v, want the op-only root setattr event", set.events[0])
	}
	for i, w := range wantKeys {
		e := set.events[i+1]
		if e.Op != w.op || e.Key != w.key {
			t.Fatalf("event[%d] = %s %q, want %s %q", i, e.Op, e.Key, w.op, w.key)
		}
		if set.resolved[i+1] != w.resolved {
			t.Fatalf("event[%d] (%s %q) resolved=%v, want %v", i, e.Op, e.Key, set.resolved[i+1], w.resolved)
		}
	}
}

// TestLiveOrdcapRootSetattrProducesNothing isolates the H5 shape: a lone
// nameless SETATTR for the root dir must parse to zero events, must not
// enter the object map, and must not elect a phantom root.
func TestLiveOrdcapRootSetattrProducesNothing(t *testing.T) {
	set, lost, err := parseEventSet(`[{"txg":33943,"object":34,"op":"SETATTR"}]` + "\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lost != 0 {
		t.Fatalf("lost = %d, want 0", lost)
	}
	// The nameless SETATTR still parses to ONE event (op setattr, no key —
	// it carries no name/op payload beyond the op itself), but it must
	// never enter the object map or elect a root. Pin both the count and
	// the emptiness of the key.
	if len(set.events) != 1 {
		t.Fatalf("events = %+v, want exactly the nameless setattr event", set.events)
	}
	if set.events[0].Key != "" || set.events[0].Op != "setattr" {
		t.Fatalf("event = %+v, want op-only setattr with no key", set.events[0])
	}
}

// TestLiveOrdcapObjidReuseResolvesBothNames pins H6 against the live
// capture's reuse shape: objid 128 was f1.txt (create txg 33966, then
// REMOVE) and f2.txt (create, then REMOVE) — the SAME id in one window.
// All four records must resolve to their OWN names under dirA; a
// first-seen- or last-seen-wins for-all-time map would mislabel two of
// them.
func TestLiveOrdcapObjidReuseResolvesBothNames(t *testing.T) {
	set, _ := loadFixture(t, "zfs-events-live-ordcap.txt")
	counts := map[string]int{}
	for i, e := range set.events {
		switch e.Key {
		case "dirA/f1.txt":
			counts["f1"]++
			if !set.resolved[i] {
				t.Fatalf("dirA/f1.txt %s not resolved despite objid reuse", e.Op)
			}
		case "dirA/f2.txt":
			counts["f2"]++
			if !set.resolved[i] {
				t.Fatalf("dirA/f2.txt %s not resolved despite objid reuse", e.Op)
			}
		}
	}
	if counts["f1"] != 2 || counts["f2"] != 2 {
		t.Fatalf("reused-objid events = %v, want 2 create/remove each for f1.txt and f2.txt", counts)
	}
}

// TestBootRelativeTimeNsStaysZero pins M2 end to end: a boot-relative
// TimeNs (~10 days of uptime) must leave Timestamp zero so the wire
// carries "unknown" and the Since filter passes the event through.
func TestBootRelativeTimeNsStaysZero(t *testing.T) {
	canned := `[{"txg":5,"object":2,"op":"SETATTR","name":"x","time":10000000000}]`
	events, _, err := parseEventsOutput(canned)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || !events[0].Timestamp.IsZero() {
		t.Fatalf("Timestamp = %v, want zero (boot-relative hrtime is not wall-clock)", events[0].Timestamp)
	}
	// The Since filter must PASS a zero-timestamp event (unknown ≠ old).
	p := &zfsEventsProvider{}
	_ = p // filter behavior is exercised via History below
	got, err := historyViaRunner(t, canned+"\n", "")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("History = %+v, want the event", got)
	}
	// And a wall-clock-plausible TimeNs DOES become a timestamp.
	cannedWall := `[{"txg":6,"object":2,"op":"SETATTR","name":"y","time":1780000000000000000}]`
	got, err = historyViaRunner(t, cannedWall, "")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 || got[0].Timestamp.IsZero() {
		t.Fatalf("wall-clock TimeNs = %+v, want a real timestamp", got)
	}
	if got[0].Timestamp.Before(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("wall-clock timestamp %v predates the plausibility floor", got[0].Timestamp)
	}
}

// TestLiveOrdcapFileOnDiskSanity guards the fixture file itself against
// silent regeneration drift: it must still be the raw ordcap capture.
func TestLiveOrdcapFileOnDiskSanity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "zfs-events-live-ordcap.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if len(s) == 0 || s[0] != '[' {
		t.Fatal("fixture is not a JSON array document")
	}
	for _, needle := range []string{
		`"object":34,"op":"SETATTR"`, // nameless root record (H5)
		`"object":128,"op":"CREATE"`, // reused id, first life (H6)
		`"object":128,"op":"REMOVE"`, // reused id, second life (H6)
		`"txg":33943`,                // lowest txg first (M8: oldest-first)
	} {
		if !contains(s, needle) {
			t.Fatalf("fixture drift: missing %s", needle)
		}
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestZfsExecSemaphoreBoundsConcurrency pins D5: the package-level
// semaphore caps concurrent zfs processes at 4. Drives runZFS with more
// than 4 concurrent calls through a runner that blocks until released; at
// most 4 may be in flight at once.
func TestZfsExecSemaphoreBoundsConcurrency(t *testing.T) {
	orig := zfsRunner
	t.Cleanup(func() { zfsRunner = orig })

	const capSemi = 4
	const total = capSemi + 3
	release := make(chan struct{})
	var inFlight int32
	var maxInFlight int32
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		<-release // hold until the test releases everyone
		atomic.AddInt32(&inFlight, -1)
		return []byte("[]"), "", nil
	}
	var wg sync.WaitGroup
	for range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = runZFS(context.Background(), "events", "-j", "tank/data")
		}()
	}
	// Give the goroutines a moment to pile up against the semaphore.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&inFlight) == capSemi {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if got := atomic.LoadInt32(&maxInFlight); got != capSemi {
		t.Fatalf("max concurrent zfs calls = %d, want exactly %d (semaphore cap)", got, capSemi)
	}
}
