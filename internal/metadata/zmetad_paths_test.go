package metadata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Test helper: a row builder with option mutators for every pointer field
// of EventRow. Pointer values use Go 1.26 new(expr).

type rowOpt func(*EventRow)

func withTimestamp(ns uint64) rowOpt  { return func(r *EventRow) { r.Timestamp = ns } }
func withCapturedAt(sec int64) rowOpt { return func(r *EventRow) { r.CapturedAt = new(sec) } }
func withOldPath(s *string) rowOpt    { return func(r *EventRow) { r.OldPath = s } }
func withOldFullPath(s *string) rowOpt {
	return func(r *EventRow) { r.OldFullPath = s }
}
func withSize(v int64) rowOpt    { return func(r *EventRow) { r.Size = new(v) } }
func withOldSize(v int64) rowOpt { return func(r *EventRow) { r.OldSize = new(v) } }
func withIoBytes(v uint64) rowOpt {
	return func(r *EventRow) { r.IoBytes = new(v) }
}
func withUID(v uint64) rowOpt { return func(r *EventRow) { r.UID = new(v) } }
func withGID(v uint64) rowOpt { return func(r *EventRow) { r.GID = new(v) } }

func row(op string, txg, objid uint64, path, fullPath *string, opts ...rowOpt) EventRow {
	r := EventRow{Op: op, Txg: txg, ObjectID: objid, Path: path, FullPath: fullPath}
	for _, o := range opts {
		o(&r)
	}
	return r
}

// plausibleNS is a wall-clock-plausible hrtime value (unix ns, > 2001).
const plausibleNS = uint64(1700000000) * 1000000000

func TestRowsToEventsFullPathFirst(t *testing.T) {
	tests := []struct {
		name            string
		row             EventRow
		want            ObjectEvent
		wantResolved    bool
		wantOldResolved bool
	}{
		{
			name:         "CREATE with full_path is resolved and served verbatim",
			row:          row("CREATE", 10, 256, new("deep.txt"), new("edge/deep.txt")),
			want:         ObjectEvent{Op: "create", Key: "edge/deep.txt", Txg: 10},
			wantResolved: true,
		},
		{
			name: "RENAME with both full paths resolves Key and OldKey",
			row: row("RENAME", 11, 300, new("new.txt"), new("edge/new.txt"),
				withOldPath(new("old.txt")), withOldFullPath(new("edge/old.txt"))),
			want:            ObjectEvent{Op: "rename", Key: "edge/new.txt", OldKey: "edge/old.txt", Txg: 11},
			wantResolved:    true,
			wantOldResolved: true,
		},
		{
			name:         "CREATE with NULL full_path stays PARTIAL bare name",
			row:          row("CREATE", 12, 260, new("deep.txt"), nil),
			want:         ObjectEvent{Op: "create", Key: "deep.txt", Txg: 12},
			wantResolved: false,
		},
		{
			name: "RENAME with NULL old_full_path keeps bare OldKey partial",
			row: row("RENAME", 13, 300, new("new.txt"), new("edge/new.txt"),
				withOldPath(new("old.txt"))),
			want:            ObjectEvent{Op: "rename", Key: "edge/new.txt", OldKey: "old.txt", Txg: 13},
			wantResolved:    true,
			wantOldResolved: false,
		},
		{
			name: "op casing is lowercased, unknown ops pass through",
			row:  row("UNKNOWN", 14, 34, nil, nil),
			want: ObjectEvent{Op: "unknown", Txg: 14},
		},
		{
			name: "captured_at wins: unix seconds UTC",
			row: row("CREATE", 15, 256, new("a.txt"), new("a.txt"),
				withCapturedAt(1700000000), withTimestamp(plausibleNS+12345)),
			want: ObjectEvent{
				Op: "create", Key: "a.txt", Txg: 15,
				Timestamp: time.Unix(1700000000, 0).UTC(),
			},
			wantResolved: true,
		},
		{
			name: "captured_at NULL, plausible hrtime -> wall clock",
			row: row("CREATE", 16, 256, new("a.txt"), new("a.txt"),
				withTimestamp(plausibleNS)),
			want: ObjectEvent{
				Op: "create", Key: "a.txt", Txg: 16,
				Timestamp: plausibleWallClockTime(plausibleNS),
			},
			wantResolved: true,
		},
		{
			name:         "no captured_at, implausible hrtime -> zero time",
			row:          row("CREATE", 17, 256, new("a.txt"), new("a.txt"), withTimestamp(42)),
			want:         ObjectEvent{Op: "create", Key: "a.txt", Txg: 17},
			wantResolved: true,
		},
		{
			name:         "no timestamps at all -> zero time",
			row:          row("CREATE", 18, 256, new("a.txt"), nil),
			want:         ObjectEvent{Op: "create", Key: "a.txt", Txg: 18},
			wantResolved: false,
		},
		{
			name: "uid/gid map when present, zero when NULL",
			row: row("CREATE", 19, 256, new("a.txt"), new("a.txt"),
				withUID(1000), withGID(1001)),
			want:         ObjectEvent{Op: "create", Key: "a.txt", Txg: 19, UID: 1000, GID: 1001},
			wantResolved: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := rowsToEvents([]EventRow{tt.row})
			if len(set.events) != 1 {
				t.Fatalf("events len = %d, want 1", len(set.events))
			}
			got := set.events[0]
			if !got.Timestamp.Equal(tt.want.Timestamp) {
				t.Errorf("Timestamp = %v, want %v", got.Timestamp, tt.want.Timestamp)
			}
			gotCmp := got
			wantCmp := tt.want
			gotCmp.Timestamp = time.Time{}
			wantCmp.Timestamp = time.Time{}
			if gotCmp != wantCmp {
				t.Errorf("event = %+v, want %+v", gotCmp, wantCmp)
			}
			if set.resolved[0] != tt.wantResolved {
				t.Errorf("resolved = %v, want %v", set.resolved[0], tt.wantResolved)
			}
			if set.oldResolved[0] != tt.wantOldResolved {
				t.Errorf("oldResolved = %v, want %v", set.oldResolved[0], tt.wantOldResolved)
			}
		})
	}
}

func TestRowsToEventsSizes(t *testing.T) {
	tests := []struct {
		name        string
		row         EventRow
		wantOldSize int64
		wantNewSize int64
	}{
		{
			name:        "TRUNCATE size=0 old_size=100: zero new size is honest",
			row:         row("TRUNCATE", 20, 256, new("a.txt"), new("a.txt"), withSize(0), withOldSize(100)),
			wantOldSize: 100,
			wantNewSize: 0,
		},
		{
			name:        "SETATTR size=50 -> SizeNew=50",
			row:         row("SETATTR", 21, 256, new("a.txt"), new("a.txt"), withSize(50)),
			wantNewSize: 50,
		},
		{
			name:        "WRITE io_bytes=4096 -> SizeNew=4096",
			row:         row("WRITE", 22, 256, new("a.txt"), new("a.txt"), withIoBytes(4096)),
			wantNewSize: 4096,
		},
		{
			name:        "READ io_bytes=512 -> SizeNew=512",
			row:         row("READ", 23, 256, new("a.txt"), new("a.txt"), withIoBytes(512)),
			wantNewSize: 512,
		},
		{
			name: "CREATE with size set -> sizes stay zero (print_event parity)",
			row:  row("CREATE", 24, 256, new("a.txt"), new("a.txt"), withSize(999), withOldSize(888), withIoBytes(777)),
		},
		{
			name: "TRUNCATE with all-NULL sizes -> zero, never fabricated",
			row:  row("TRUNCATE", 25, 256, new("a.txt"), new("a.txt")),
		},
		{
			name:        "WRITE with NULL io_bytes -> zero",
			row:         row("WRITE", 26, 256, new("a.txt"), new("a.txt"), withSize(4096)),
			wantNewSize: 0, // size is not the WRITE source column; io_bytes is
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := rowsToEvents([]EventRow{tt.row})
			got := set.events[0]
			if got.SizeOld != tt.wantOldSize || got.SizeNew != tt.wantNewSize {
				t.Errorf("sizes = (old %d, new %d), want (old %d, new %d)",
					got.SizeOld, got.SizeNew, tt.wantOldSize, tt.wantNewSize)
			}
		})
	}
}

func TestRowsMatchKey(t *testing.T) {
	// One row set covering resolved/partial Key and OldKey combinations.
	rows := []EventRow{
		// 0: resolved CREATE edge/deep.txt
		row("CREATE", 30, 1, new("deep.txt"), new("edge/deep.txt")),
		// 1: partial CREATE deep.txt (NULL full_path)
		row("CREATE", 31, 2, new("deep.txt"), nil),
		// 2: resolved RENAME, partial OldKey
		row("RENAME", 32, 3, new("new.txt"), new("edge/new.txt"), withOldPath(new("old.txt"))),
		// 3: partial RENAME with resolved OldKey
		row("RENAME", 33, 4, new("x.txt"), nil, withOldPath(new("y.txt")), withOldFullPath(new("edge/y.txt"))),
	}
	set := rowsToEvents(rows)

	tests := []struct {
		name string
		i    int
		key  string
		want bool
	}{
		{"resolved matches exact key", 0, "edge/deep.txt", true},
		{"resolved does not match bare name alone", 0, "deep.txt", false},
		{"resolved does not match different key sharing bare name", 0, "other/deep.txt", false},
		{"partial matches bare equality", 1, "deep.txt", true},
		{"partial matches queried key ending /bare", 1, "edge/deep.txt", true},
		{"partial matches deeper queried key ending /bare", 1, "a/b/deep.txt", true},
		{"partial does not match unrelated key", 1, "edge/other.txt", false},
		{"resolved Key matches on rename row", 2, "edge/new.txt", true},
		{"partial OldKey matches suffix", 2, "dir/old.txt", true},
		{"partial OldKey matches bare", 2, "old.txt", true},
		{"resolved OldKey matches exact", 3, "edge/y.txt", true},
		{"resolved OldKey does not match bare", 3, "y.txt", false},
		{"partial Key on rename row matches bare", 3, "x.txt", true},
		{"no match at all", 3, "zzz/q.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsMatchKey(set, tt.i, tt.key); got != tt.want {
				t.Errorf("rowsMatchKey(set, %d, %q) = %v, want %v", tt.i, tt.key, got, tt.want)
			}
		})
	}
}

// fixtureRows converts parsed CLI fixture records into v5-shaped EventRows
// using ONLY information the legacy pipeline had: full_path = whatever
// parseEventsOutput resolved, captured_at NULL. Test-only; the production
// path never builds rows this way.
func fixtureRows(raws []rawEvent, set *eventSet) []EventRow {
	rows := make([]EventRow, len(raws))
	for i, r := range raws {
		rows[i] = EventRow{
			Op:        r.Op, // fixtures store uppercase enum names, same as the DB
			Txg:       r.Txg,
			ObjectID:  r.Object,
			Timestamp: r.TimeNs,
			Path:      r.Name,
			OldPath:   r.OldName,
		}
		if set.resolved[i] {
			k := set.events[i].Key
			rows[i].FullPath = &k
		}
		if set.oldResolved[i] {
			k := set.events[i].OldKey
			rows[i].OldFullPath = &k
		}
		if r.UID != 0 {
			rows[i].UID = new(r.UID)
		}
		if r.GID != 0 {
			rows[i].GID = new(r.GID)
		}
		if strings.EqualFold(r.Op, "TRUNCATE") {
			// The wire's only size-bearing op in the recorded fixtures;
			// the legacy parser honors sizes for truncate only.
			rows[i].Size = new(r.NewSize)
			rows[i].OldSize = new(r.OldSize)
		}
	}
	return rows
}

// TestRowsToEventsDriftGuardFixtures pins that the v5 fast path
// (rowsToEvents over full_path-bearing rows) and the legacy CLI parser
// (parseEventsOutput) produce IDENTICAL ObjectEvent slices on every
// recorded fixture. If upstream resolution ever diverges from the old
// reconstruction on recorded data, this fails.
func TestRowsToEventsDriftGuardFixtures(t *testing.T) {
	fixtures := []string{
		"zfs-events-sample.txt",
		"zfs-events-live-ordcap.txt",
		"zfs-events-nested.txt",
		"zfs-events-nested-records-lost.txt",
	}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("fixture missing: %v", err)
			}
			set, _, err := parseEventSet(string(raw))
			if err != nil {
				t.Fatalf("parseEventSet: %v", err)
			}
			end := strings.LastIndex(string(raw), "]")
			if end < 0 {
				t.Fatalf("fixture has no JSON array")
			}
			var raws []rawEvent
			if err := json.Unmarshal(raw[:end+1], &raws); err != nil {
				t.Fatalf("unmarshal raws: %v", err)
			}
			got := rowsToEvents(fixtureRows(raws, set))
			if len(got.events) != len(set.events) {
				t.Fatalf("event count = %d, want %d", len(got.events), len(set.events))
			}
			if !reflect.DeepEqual(got.events, set.events) {
				for i := range got.events {
					if !reflect.DeepEqual(got.events[i], set.events[i]) {
						t.Errorf("event %d:\n rows = %+v\n  cli = %+v", i, got.events[i], set.events[i])
					}
				}
			}
		})
	}
}
