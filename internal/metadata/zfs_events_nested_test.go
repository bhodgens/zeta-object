package metadata

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// loadFixture parses a testdata replay fixture through the full pipeline.
func loadFixture(t *testing.T, name string) (*eventSet, uint64) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	set, lost, err := parseEventSet(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return set, lost
}

// historyViaRunner drives p.History against a canned `zfs events -j`
// payload through the runner seams (no zfs binary needed — macOS CI).
func historyViaRunner(t *testing.T, canned string, key string) ([]ObjectEvent, error) {
	t.Helper()
	origRunner := zfsRunner
	origResolve := resolveDatasetFn
	t.Cleanup(func() { zfsRunner = origRunner; resolveDatasetFn = origResolve })
	zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
		return []byte(canned), "", nil
	}
	resolveDatasetFn = func(ctx context.Context, path string) (string, error) {
		return "tank/data", nil
	}
	return (&zfsEventsProvider{}).History(context.Background(), "/mnt/tank/data", key, HistoryQuery{})
}

// TestReplayNestedFixtureResolvesPaths is the F-live-1 regression pin: the
// replay fixture replicates the REAL zfs-meta host shapes (dir create for
// 'edge' with its own object id, file create under it with parent=dir
// objid, rename under it, link, remove) and the consumer must resolve
// every nested bare name to its full S3 key.
func TestReplayNestedFixtureResolvesPaths(t *testing.T) {
	set, lost := loadFixture(t, "zfs-events-nested.txt")
	if lost != 2 {
		t.Fatalf("lost = %d, want 2", lost)
	}
	want := map[uint64]ObjectEvent{
		31998: {Op: "create", Key: "top.txt", Txg: 31998},
		32000: {Op: "create", Key: "edge", Txg: 32000},
		32001: {Op: "create", Key: "edge/inner", Txg: 32001},
		32019: {Op: "create", Key: "edge/inner/deep.txt", Txg: 32019},
		32020: {Op: "create", Key: "edge/inner/shallow.txt", Txg: 32020},
		32022: {Op: "truncate", Key: "edge/inner/deep.txt", Txg: 32022, SizeOld: 1024, SizeNew: 4096},
		32030: {Op: "rename", Key: "edge/inner/renamed.txt", OldKey: "edge/inner/deep.txt", Txg: 32030},
		32040: {Op: "link", Key: "edge/inner/hard.txt", Txg: 32040},
		32050: {Op: "remove", Key: "edge/inner/shallow.txt", Txg: 32050},
	}
	if len(set.events) != len(want) {
		t.Fatalf("got %d events, want %d", len(set.events), len(want))
	}
	for i, e := range set.events {
		w, ok := want[e.Txg]
		if !ok {
			t.Fatalf("event[%d] unexpected txg %d", i, e.Txg)
		}
		if e != w { // ObjectEvent is comparable
			t.Fatalf("event[%d] = %+v, want %+v", i, e, w)
		}
		if !set.resolved[i] || (e.OldKey != "" && !set.oldResolved[i]) {
			t.Fatalf("event[%d] txg %d not fully resolved: resolved=%v oldResolved=%v",
				i, e.Txg, set.resolved[i], set.oldResolved[i])
		}
	}
}

// TestNestedHistoryFindsDeepKey pins the endpoint-visible behavior: a
// ?events query for the full S3 key of a nested object returns its history
// (create + truncate + the rename that moved it away), which before
// F-live-1 silently returned [].
func TestNestedHistoryFindsDeepKey(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "zfs-events-nested.txt"))
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	events, err := historyViaRunner(t, string(raw), "edge/inner/deep.txt")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (create, truncate, rename-away): %+v", len(events), events)
	}
	ops := map[string]int{}
	for _, e := range events {
		ops[e.Op]++
		if e.Op == "rename" {
			if e.Key != "edge/inner/renamed.txt" || e.OldKey != "edge/inner/deep.txt" {
				t.Fatalf("rename = %+v, want renamed.txt <- deep.txt (full keys)", e)
			}
		} else if e.Key != "edge/inner/deep.txt" {
			t.Fatalf("event key = %q, want edge/inner/deep.txt", e.Key)
		}
	}
	if ops["create"] != 1 || ops["truncate"] != 1 || ops["rename"] != 1 {
		t.Fatalf("ops = %v, want one each of create/truncate/rename", ops)
	}
}

// TestNestedHistoryExactKeyDoesNotLeakSiblings pins precision: the fully
// resolved key must match exactly, so a sibling at a different depth whose
// bare name does not suffix-match is never shown.
func TestNestedHistoryExactKeyDoesNotLeakSiblings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "zfs-events-nested.txt"))
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	events, err := historyViaRunner(t, string(raw), "top.txt")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// Only the root-level top.txt create; nothing nested leaks in. (The
	// deep.txt rename-away for edge/inner/deep.txt does not match here.)
	if len(events) != 1 || events[0].Key != "top.txt" || events[0].Txg != 31998 {
		t.Fatalf("events = %+v, want only the top.txt create", events)
	}
}

// TestRecordsLostFixtureDocumentsUnresolvableParent pins the documented
// records-lost behavior: the parent dir create (and even the root
// reference) is outside the window, so no unresolvable parent id can be
// corroborated against a known directory and root detection is refused —
// the deep.txt event keeps its BARE name and its history for the full key
// is still returned (conservative broad match), never silently empty.
func TestRecordsLostFixtureDocumentsUnresolvableParent(t *testing.T) {
	set, lost := loadFixture(t, "zfs-events-nested-records-lost.txt")
	if lost != 1 {
		t.Fatalf("lost = %d, want 1", lost)
	}
	if len(set.events) != 3 {
		t.Fatalf("got %d events, want 3", len(set.events))
	}
	// top.txt has parent=0 (legacy/root-record shape): partial, bare name.
	// edge resolves (its parent IS the root); deep.txt's parent 777 is
	// unresolvable: partial, bare name. But top.txt's parent=0 reference
	// does not corroborate 777 against any known dir, so nothing may
	// fabricate an exact "edge/deep.txt" key here.
	for i, e := range set.events {
		switch e.Txg {
		case 31998:
			// Everything stays partial in this window (see below).
			if set.resolved[i] {
				t.Fatalf("no event may claim an exact key in this window: %+v", e)
			}
			if e.Key != "top.txt" && e.Key != "edge" && e.Key != "deep.txt" {
				t.Fatalf("partial event keeps bare name, got %q", e.Key)
			}
		case 32000:
			// edge stays PARTIAL here: root detection is refused (no
			// known dir corroborates any unresolvable parent), so no
			// exact "edge" key may be fabricated even though edge's own
			// parent id (34) is visible — 34 is only referenced, never
			// proven to be the root in this window.
			if set.resolved[i] {
				t.Fatalf("edge must stay partial (root uncorroborated): %+v", e)
			}
			if e.Key != "edge" {
				t.Fatalf("partial event keeps bare name, got %q", e.Key)
			}
		case 32019:
			if set.resolved[i] {
				t.Fatalf("deep.txt must stay partial (parent 777 lost): %+v", e)
			}
			if e.Key != "deep.txt" {
				t.Fatalf("partial event keeps bare name, got %q", e.Key)
			}
		}
	}
	// The endpoint-visible contract for the lost case: querying the full
	// key still surfaces the (partial) create rather than [].
	events, err := historyViaRunner(t,
		"[{\"txg\":31998,\"object\":50,\"op\":\"CREATE\",\"name\":\"top.txt\",\"parent\":34},\n"+
			"{\"txg\":32000,\"object\":129,\"op\":\"CREATE\",\"name\":\"edge\",\"parent\":34},\n"+
			"{\"txg\":32019,\"object\":257,\"op\":\"CREATE\",\"name\":\"deep.txt\",\"parent\":777}]\n",
		"edge/deep.txt")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].Key != "deep.txt" || events[0].Txg != 32019 {
		t.Fatalf("events = %+v, want the partial deep.txt create", events)
	}
}

// TestCorroboratedRootWinsOverLostDirs pins the guard against fabricating
// exact keys: when a mid-level dir create was lost, its id keeps appearing
// as an unresolvable parent; root detection must still land on the true
// root (corroborated by the surviving edge dir) instead of the most
// frequent lost id, so edge/deep.txt resolves EXACTLY while other-branch
// files stay partial.
func TestCorroboratedRootWinsOverLostDirs(t *testing.T) {
	// 'other' (obj 500) was created before the window and lost, but 6 of
	// its events remain — it out-frequents the root (2 refs). The
	// corroborated root must win anyway.
	canned := "[" +
		`{"txg":100,"object":129,"op":"CREATE","name":"edge","parent":34},` +
		`{"txg":110,"object":257,"op":"CREATE","name":"deep.txt","parent":129},` +
		`{"txg":120,"object":258,"op":"CREATE","name":"a","parent":500},` +
		`{"txg":121,"object":259,"op":"CREATE","name":"b","parent":500},` +
		`{"txg":122,"object":260,"op":"CREATE","name":"c","parent":500},` +
		`{"txg":123,"object":261,"op":"CREATE","name":"d","parent":500},` +
		`{"txg":124,"object":262,"op":"CREATE","name":"e","parent":500},` +
		`{"txg":125,"object":263,"op":"CREATE","name":"f","parent":500}]`
	events, err := historyViaRunner(t, canned, "edge/deep.txt")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].Op != "create" || events[0].Key != "edge/deep.txt" {
		t.Fatalf("events = %+v, want exactly the resolved edge/deep.txt create", events)
	}
}

// TestPartialRenameMatchesSuffixKey pins the conservative RENAME case: a
// rename whose parent chain is unresolvable keeps bare names on both ends
// and its history must surface for a query of either full path shape
// (suffix match), never silently vanish.
func TestPartialRenameMatchesSuffixKey(t *testing.T) {
	canned := `[{"txg":10,"object":300,"op":"RENAME","name":"new.txt","old_name":"old.txt","parent":999,"old_parent":999}]`
	events, err := historyViaRunner(t, canned, "some/dir/new.txt")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].OldKey != "old.txt" || events[0].Key != "new.txt" {
		t.Fatalf("events = %+v, want the partial rename", events)
	}
}

// TestNewestObjIDMappingWins pins wraparound object-id reuse: the log is
// newest-first, and when an object id is reused after compaction the NEWER
// record's mapping must win (it is what later records in the window
// reference), never the stale one.
func TestNewestObjIDMappingWins(t *testing.T) {
	// Newest-first stream: obj 300 is currently dir 'fresh' (parent 34),
	// older record shows the same id as file 'stale'.
	canned := "[" +
		`{"txg":50,"object":300,"op":"CREATE","name":"fresh","parent":34},` +
		`{"txg":40,"object":301,"op":"CREATE","name":"f.txt","parent":300},` +
		`{"txg":30,"object":300,"op":"CREATE","name":"stale","parent":0}]`
	events, err := historyViaRunner(t, canned, "fresh/f.txt")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].Key != "fresh/f.txt" {
		t.Fatalf("events = %+v, want fresh/f.txt (newest mapping wins)", events)
	}
}

// TestRenameWithoutParentFallsBackToOldParent tolerates a RENAME record
// that omits `parent` (print_event emits it, but the tolerance is cheap):
// reconstruction uses old_parent, which is the same directory.
func TestRenameWithoutParentFallsBackToOldParent(t *testing.T) {
	canned := "[" +
		`{"txg":60,"object":129,"op":"CREATE","name":"edge","parent":34},` +
		`{"txg":70,"object":300,"op":"RENAME","name":"new.txt","old_name":"old.txt","old_parent":129}]`
	events, err := historyViaRunner(t, canned, "edge/old.txt")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].OldKey != "edge/old.txt" {
		t.Fatalf("events = %+v, want rename matching on resolved OldKey edge/old.txt", events)
	}
}
