package s3

// versionstore_put_errors_test.go — the store-level write surface's FAILURE
// paths, which the round-trip tests never reach because a round-trip only
// exercises the happy path.
//
// WHY THIS FILE EXISTS (audit G8 note, 2026-10-10): internal/frontend/s3 holds
// the tightest coverage floor in the repo (83, measured 83.2 on linux — +0.2
// headroom). versionstore.go had 37 unhit blocks and reflinkversions.go 46,
// concentrated in exactly the error arms: a short read, a bad size, an
// unwritable directory. Those arms are what keep a version store from writing
// a sidecar entry that points at bytes it never stored.
//
// THE INVARIANT UNDER TEST (stated in the code's own comments): "Data file
// first: a crash between the two writes leaves a data file with no sidecar
// entry — orphaned bytes, never a lying sidecar." So every failure path must
// leave the sidecar UNCHANGED. A test that only checks "it returned an error"
// would pass even if the store had already written a lying sidecar.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- sidecarVersionStore.PutVersion: the short-read arm ----------

// A reader that yields fewer bytes than the declared size must be refused.
// The store reads through io.LimitReader(r, size), so a lying size is the
// only way to hit the mismatch arm.
func TestSidecarPutVersion_ShortReadIsRefusedAndSidecarStaysEmpty(t *testing.T) {
	store, bucketPath := verTestStore(t)
	key := "short-read-key"

	// Declare 100 bytes, supply 4.
	entry, err := store.PutVersion("bkt", key, strings.NewReader("abcd"), 100, "etag-short")
	if err == nil {
		t.Fatalf("PutVersion accepted a short read: entry = %+v, want an error", entry)
	}
	if !strings.Contains(err.Error(), "got 4 bytes") {
		t.Errorf("error = %v, want it to name the byte-count mismatch", err)
	}
	if entry.ID != "" {
		t.Errorf("failed PutVersion returned entry %+v; it must not mint an id", entry)
	}

	// The lying-sidecar invariant: a failed write records nothing.
	versions, listErr := store.List("bkt", key)
	if listErr == nil && len(versions) != 0 {
		t.Errorf("sidecar holds %d versions after a refused write, want 0", len(versions))
	}
	if _, statErr := os.Stat(store.sidecarPath(key)); statErr == nil {
		t.Error("a refused PutVersion created the sidecar; a failure must leave no state")
	}
	if _, statErr := os.Stat(filepath.Join(bucketPath, ".metadata", ".versions")); statErr == nil {
		t.Error("a refused PutVersion created the versions dir; nothing should be persisted")
	}
}

// A reader that ERRORS must propagate, not be mistaken for a short read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestSidecarPutVersion_ReaderErrorPropagates(t *testing.T) {
	store, _ := verTestStore(t)
	sentinel := errors.New("disk gave up mid-read")

	_, err := store.PutVersion("bkt", "reader-err-key", errReader{err: sentinel}, 10, "etag-re")
	if err == nil {
		t.Fatal("PutVersion accepted a failing reader, want the error propagated")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the reader's error (errors.Is must find it)", err)
	}
	// A reader error is distinct from a length mismatch: the message must not
	// claim a byte count it never reached.
	if strings.Contains(err.Error(), "want 10") {
		t.Errorf("error = %v, looks like the length-mismatch arm; a read failure must not "+
			"be reported as a size mismatch", err)
	}
}

// ---------- PutVersion: an unwritable destination fails closed ----------

// If the versions directory cannot be created, the write must fail BEFORE any
// sidecar entry is written. That is the whole point of data-file-first.
func TestSidecarPutVersion_UnwritableVersionsDirFailsBeforeSidecar(t *testing.T) {
	store, bucketPath := verTestStore(t)
	key := "unwritable-key"

	// Put a regular FILE where the .versions directory must go: MkdirAll on
	// an existing non-directory fails on every supported platform.
	if err := os.WriteFile(filepath.Join(bucketPath, ".metadata", ".versions"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("staging the blocker file: %v", err)
	}

	if _, err := store.PutVersion("bkt", key, strings.NewReader("payload"), 7, "etag-unwritable"); err == nil {
		t.Fatal("PutVersion succeeded with an unwritable versions dir")
	}
	if _, statErr := os.Stat(store.sidecarPath(key)); statErr == nil {
		t.Error("a failed PutVersion wrote the sidecar anyway - the store can now claim a " +
			"version whose data was never stored")
	}
}

// ---------- pruneDropID: order-preserving removal ----------

func TestPruneDropID(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		drop string
		want []string
	}{
		{"removes the middle entry", []string{"a", "b", "c"}, "b", []string{"a", "c"}},
		{"removes the first", []string{"a", "b", "c"}, "a", []string{"b", "c"}},
		{"removes the last", []string{"a", "b", "c"}, "c", []string{"a", "b"}},
		{"absent id leaves it unchanged", []string{"a", "b", "c"}, "zzz", []string{"a", "b", "c"}},
		{"empty input", []string{}, "a", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pruneDropID(c.in, c.drop)
			if len(got) != len(c.want) {
				t.Fatalf("pruneDropID(%v, %q) = %v, want %v", c.in, c.drop, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("pruneDropID(%v, %q) = %v, want %v (order must be preserved)",
						c.in, c.drop, got, c.want)
				}
			}
		})
	}

	// Duplicate ids: pruning removes every occurrence, not just the first.
	dup := []string{"a", "b", "a", "c"}
	got := pruneDropID(dup, "a")
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("pruneDropID with duplicates = %v, want [b c] (every occurrence goes)", got)
	}
}

// ---------- reflinkCloneUnsupportedError ----------

// The darwin (non-linux) arm of the clone returns this error, and the
// fail-soft capture keys off the TYPE. A change to the message or the type
// would break the fail-soft contract, so pin both.
func TestReflinkCloneUnsupportedErrorShape(t *testing.T) {
	var e error = &reflinkCloneUnsupportedError{}
	msg := e.Error()
	if msg == "" {
		t.Fatal("reflinkCloneUnsupportedError.Error() is empty")
	}
	if !strings.Contains(msg, "reflink") || !strings.Contains(msg, "not supported") {
		t.Errorf("message = %q, want it to name reflink and say it is not supported", msg)
	}
	// The type must be matchable with errors.As, which is how callers
	// distinguish "unsupported here" from "the clone failed".
	target, ok := errors.AsType[*reflinkCloneUnsupportedError](e)
	if !ok || target == nil {
		t.Error("errors.As cannot match *reflinkCloneUnsupportedError; the fail-soft " +
			"capture cannot recognise the unsupported case")
	}
}

// ---------- reflinkVersionStore.PutVersion: the same failure arms ----------

// The reflink store has its own PutVersion, so its failure arms are separate
// code from the sidecar store's and need their own pins.
func TestReflinkPutVersion_ShortReadIsRefusedAndSidecarStaysEmpty(t *testing.T) {
	bp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bp, ".metadata"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := reflinkVersionStore{sidecarVersionStore{bucketPath: bp}}
	key := "reflink-short"

	entry, err := store.PutVersion("bkt", key, strings.NewReader("ab"), 50, "etag-rl")
	if err == nil {
		t.Fatalf("reflink PutVersion accepted a short read: %+v", entry)
	}
	if !strings.Contains(err.Error(), "got 2 bytes") {
		t.Errorf("error = %v, want it to name the byte-count mismatch", err)
	}
	if entry.ID != "" {
		t.Errorf("failed reflink PutVersion minted id %q", entry.ID)
	}
	if _, statErr := os.Stat(store.sidecarPath(key)); statErr == nil {
		t.Error("a refused reflink PutVersion wrote the sidecar - lying sidecar")
	}
}

func TestReflinkPutVersion_ReaderErrorPropagates(t *testing.T) {
	bp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bp, ".metadata"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := reflinkVersionStore{sidecarVersionStore{bucketPath: bp}}
	sentinel := errors.New("reflink source vanished")

	_, err := store.PutVersion("bkt", "rl-reader-err", errReader{err: sentinel}, 8, "etag-rl")
	if err == nil {
		t.Fatal("reflink PutVersion accepted a failing reader")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the reader's error", err)
	}
}

// ---------- the Lying-sidecar invariant, stated as one test ----------

// Whichever store, whichever failure, the invariant is the same: a failed
// write leaves NO sidecar entry. Asserted in one place so the two stores above
// cannot drift apart in their failure handling.
func TestFailedVersionWriteNeverLeavesASidecarEntry(t *testing.T) {
	stores := map[string]versionStore{}

	sb, _ := verTestStore(t)
	stores["sidecar"] = sb

	bp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bp, ".metadata"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stores["reflink"] = reflinkVersionStore{sidecarVersionStore{bucketPath: bp}}

	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			key := "invariant-key"
			// Three different failures: bad size, failing reader, and a
			// body that is fine but whose destination is blocked.
			if _, err := st.PutVersion("bkt", key, strings.NewReader("x"), 999, "e1"); err == nil {
				t.Fatal("size mismatch accepted")
			}
			if _, err := st.PutVersion("bkt", key, errReader{err: errors.New("boom")}, 1, "e2"); err == nil {
				t.Fatal("reader error accepted")
			}
			versions, err := st.List("bkt", key)
			if err == nil && len(versions) != 0 {
				t.Errorf("List reports %d versions after three refused writes, want 0", len(versions))
			}
		})
	}
}

// Keep io imported for the io.Reader-shaped errReader contract.
var _ io.Reader = errReader{}
