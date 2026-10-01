package metadata

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplayFixtureEndToEnd parses the committed record-and-replay fixture
// of real zfs-events CLI output and validates every event against the
// frozen op vocabulary (master Contract 3).
func TestReplayFixtureEndToEnd(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "zfs-events-sample.txt"))
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	events, lost, err := parseEventsOutput(string(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("fixture parsed to zero events")
	}
	// Every event satisfies the frozen op vocabulary.
	for _, e := range events {
		switch e.Op {
		case "create", "remove", "rename", "link", "symlink", "truncate", "setattr":
		default:
			t.Fatalf("unknown op in fixture: %q", e.Op)
		}
	}
	if lost > 0 {
		t.Logf("fixture reports %d lost records (lossy ring buffer) — expected", lost)
	}
	// The fixture exercises every op shape Contract 3 pins; assert the
	// per-op field mappings survived end-to-end parsing.
	byOp := map[string][]ObjectEvent{}
	for _, e := range events {
		byOp[e.Op] = append(byOp[e.Op], e)
	}
	for _, op := range []string{"create", "remove", "rename", "link", "symlink", "truncate", "setattr"} {
		if len(byOp[op]) == 0 {
			t.Fatalf("fixture must cover op %q", op)
		}
	}
	// RENAME must carry old_name; TRUNCATE must carry sizes.
	for _, e := range byOp["rename"] {
		if e.OldKey == "" {
			t.Fatalf("rename event missing OldKey: %+v", e)
		}
	}
	for _, e := range byOp["truncate"] {
		if e.SizeOld == 0 && e.SizeNew == 0 {
			t.Fatalf("truncate event missing sizes: %+v", e)
		}
	}
}
