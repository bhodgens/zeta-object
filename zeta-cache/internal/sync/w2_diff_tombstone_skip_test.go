package sync

// w2_diff_tombstone_skip_test.go - the LOW finding: diffAndApply's
// matrix-6 branch PROMISED a stale-tombstone skip ("A stale tombstone
// (row.ETag empty ...) keeps the tombstone and counts a skip") but never
// implemented it - deleteRemote fired regardless. Pins: an empty-etag
// tombstone over an absent remote is KEPT (no DELETE issued) and counts
// one SkippedDeletes; a tombstone WITH a recorded generation still
// deletes (matrix 6, control).
//
// The tests drive the engine through the DELTA path (a stub feed naming
// exactly one path) so the report reflects a single diffAndApply - the
// full scan's complement and tombstone loops both visit a tombstoned
// key, which is fine for the state assertions but double-counts
// report fields.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// stubDeltaFeed names exactly one path per Delta (deltaScan path: one
// diffAndApply per pass, no full-scan loops).
type stubDeltaFeed struct{ path string }

func (f stubDeltaFeed) Delta(context.Context) ([]string, bool, error) {
	return []string{f.path}, false, nil
}

// TestStaleTombstoneWithNoETagIsKeptAndSkipped: the key was created and
// deleted before any upload ever confirmed a server generation, so the
// tombstone carries etag "". The remote does not list the key. The
// documented skip must fire: no DELETE on the wire, tombstone stands,
// and the report counts exactly one SkippedDeletes.
func TestStaleTombstoneWithNoETagIsKeptAndSkipped(t *testing.T) {
	h := newHarness(t)
	key := "never-uploaded.txt"

	// Local create that never reached the server (no clean etag), then
	// the user deleted it via the mount: DeletePath tombstones with an
	// EMPTY etag.
	if err := h.store.PutPath(bg(), index.Resource{
		Path: key, Size: 3, Hydrated: true, Dirty: true,
	}, index.OpLocalWrite, "local create, never uploaded"); err != nil {
		t.Fatalf("seed dirty row: %v", err)
	}
	if err := h.store.DeletePath(bg(), key, "user delete before first upload"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if row := h.row(key); row.ETag != "" || !row.Deleted {
		t.Fatalf("arrange: tombstone row = %+v, want deleted=1 etag=\"\"", row)
	}

	// Delta pass over the tombstoned key only.
	eng, err := NewEngine(Options{
		Transport: h.fs,
		Store:     h.store,
		CacheDir:  h.dir,
		Feed:      stubDeltaFeed{path: key},
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if _, present := h.fs.ServerBody(key); present {
		t.Fatal("server body appeared - nothing should have been uploaded")
	}
	if row := h.row(key); !row.Deleted {
		t.Fatalf("tombstone row = %+v, want kept (deleted=1)", row)
	}
	// The skip MUST be counted. Asserted as a formatted-report substring
	// (not a field access) so this test still compiles - and FAILS - on
	// the pre-fix diff.go that lacked both the skip and the counter.
	rep := eng.LastReport()
	if got := fmt.Sprintf("%+v", rep); !strings.Contains(got, "SkippedDeletes:1") {
		t.Fatalf("report = %s, want SkippedDeletes:1 (stale tombstone must count a skip)", got)
	}
	if rep.LocalDeletes != 0 {
		t.Fatalf("LocalDeletes = %d, want 0: %+v", rep.LocalDeletes, rep)
	}
}

// TestTombstoneWithRecordedETagStillDeletes (control): with a recorded
// generation and the remote absent, matrix 6's delete still fires - the
// skip is only for the stale/empty tombstone.
func TestTombstoneWithRecordedETagStillDeletes(t *testing.T) {
	h := newHarness(t)
	key := "gone.txt"
	etagV1 := transport.MD5Hex([]byte("v1"))
	h.fs.PutRaw(key, "v1")
	h.putLocal(key, "v1")
	h.seedClean(key, etagV1)
	if err := h.store.DeletePath(bg(), key, "user delete via mount"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	eng, err := NewEngine(Options{
		Transport: h.fs,
		Store:     h.store,
		CacheDir:  h.dir,
		Feed:      stubDeltaFeed{path: key},
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := eng.SyncOnce(bg()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, present := h.fs.ServerBody(key); present {
		t.Fatal("matching generation must still be deleted (matrix 6)")
	}
	rep := eng.LastReport()
	got := fmt.Sprintf("%+v", rep)
	if !strings.Contains(got, "LocalDeletes:1") {
		t.Fatalf("report = %s, want LocalDeletes:1", got)
	}
	// A counted zero formats as "SkippedDeletes:0" and a nonexistent
	// counter is omitted entirely - both mean "no skip counted", so the
	// control passes on both sides of the fix.
	if strings.Contains(got, "SkippedDeletes:1") {
		t.Fatalf("report = %s, want no skip counted for a matching-generation tombstone", got)
	}
}
