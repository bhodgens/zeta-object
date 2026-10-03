// zfssnapshots_test.go — zfsSnapshotVersionStore + ListSnapshots tests
// (s3-versioning tree leaf 02). NO real zfs: the zfsSnapshotRunner seam is
// scripted per test and snapdirs are faked as
// <tempdir>/.zfs/snapshot/<snapShort>/<relPath>. Covers: ListSnapshots
// parsing/sorting (newest first, epoch and formatted creation forms),
// SnapPath, List newest-first existence walk, Open reads snapshot bytes,
// unknown/expired/traversal snapshot IDs 404-class, PutVersion
// ErrSnapshotsReadOnly, PutDeleteMarker ErrDeleteMarkersUnsupported, and
// State/SetState round-trip through the shared bucket-level marker.

package s3

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// fakeSnapshotResolver scripts the zmetad DB handle (dataset +
// mountpoint resolution) without SQLite.
type fakeSnapshotResolver struct {
	dataset    string
	dsErr      error
	mountpoint string
	mpErr      error
}

func (f fakeSnapshotResolver) ResolveDatasetByPath(string) (string, error) {
	return f.dataset, f.dsErr
}

func (f fakeSnapshotResolver) ResolveMountpointByPath(string) (string, error) {
	return f.mountpoint, f.mpErr
}

// scriptZfsSnapshotRunner replaces the zfsSnapshotRunner seam with a
// scripted stdout (and optional error), returning the captured args via
// the channel for argv assertions. Restores the seam on cleanup.
func scriptZfsSnapshotRunner(t *testing.T, stdout string, err error) *[][]string {
	t.Helper()
	gotArgs := &[][]string{}
	orig := zfsSnapshotRunner
	zfsSnapshotRunner = func(_ context.Context, args ...string) ([]byte, string, error) {
		*gotArgs = append(*gotArgs, args)
		return []byte(stdout), "", err
	}
	t.Cleanup(func() { zfsSnapshotRunner = orig })
	return gotArgs
}

// snapTestEnv builds a fake ZFS layout: mountpoint with two snapshots
// (s1 older, s2 newer) under .zfs/snapshot, where relPath exists in both
// snapshots unless omitted, and returns the wired store.
type snapTestEnv struct {
	store      zfsSnapshotVersionStore
	mountpoint string
	relPath    string
}

func snapTestStore(t *testing.T, s2Has, s1Has bool) snapTestEnv {
	t.Helper()
	mountpoint := t.TempDir()
	bucketPath := filepath.Join(mountpoint, "bkt")
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("mkdir bucket: %v", err)
	}
	relPath := "docs/report.txt"
	writeSnap := func(short string, has bool) {
		dir := filepath.Join(mountpoint, ".zfs", "snapshot", short, "docs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir snapdir: %v", err)
		}
		if !has {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("bytes-"+short), 0o644); err != nil {
			t.Fatalf("write snap file: %v", err)
		}
	}
	writeSnap("s1", s1Has)
	writeSnap("s2", s2Has)
	// zfs list output: name<TAB>creation, epoch form; s2 newer. zfs
	// default sort is by name — the parser must sort by creation.
	stdout := "pool/bkt@s1	1000000000\npool/bkt@s2	2000000000\n"
	scriptZfsSnapshotRunner(t, stdout, nil)
	store := zfsSnapshotVersionStore{
		bucketPath: bucketPath,
		zdb: fakeSnapshotResolver{
			dataset:    "pool/bkt",
			mountpoint: mountpoint,
		},
	}
	return snapTestEnv{store: store, mountpoint: mountpoint, relPath: relPath}
}

// ---------- contract + pure helpers ----------

func TestZfsSnapshotVersionStore_ImplementsContract1(t *testing.T) {
	var _ versionStore = zfsSnapshotVersionStore{}
}

func TestSnapPath_PureHelper(t *testing.T) {
	got := SnapPath("/mnt/tank/bkt", "auto-20261002-143000", "docs/report.txt")
	want := "/mnt/tank/bkt/.zfs/snapshot/auto-20261002-143000/docs/report.txt"
	if got != want {
		t.Errorf("SnapPath = %q, want %q", got, want)
	}
}

// ---------- ListSnapshots (Contract 3 exec) ----------

func TestListSnapshots_ArgvNewestFirstAndFields(t *testing.T) {
	argsChan := scriptZfsSnapshotRunner(t,
		"pool/bkt@old	Fri Oct  2 10:00:00 2026\npool/bkt@new	Fri Oct  2 17:30:00 2026\n", nil)

	snaps, err := ListSnapshots(t.Context(), "pool/bkt")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}
	// Newest first regardless of zfs's name-sorted output order.
	if snaps[0].Name != "pool/bkt@new" || snaps[0].Short != "new" {
		t.Errorf("snaps[0] = {%q %q}, want pool/bkt@new / new", snaps[0].Name, snaps[0].Short)
	}
	if snaps[1].Name != "pool/bkt@old" {
		t.Errorf("snaps[1] = %q, want pool/bkt@old", snaps[1].Name)
	}
	if snaps[0].Creation.Before(snaps[1].Creation) {
		t.Errorf("creation order not newest-first: %v before %v", snaps[0].Creation, snaps[1].Creation)
	}

	if len(*argsChan) != 1 {
		t.Fatalf("runner called %d times, want 1 exec", len(*argsChan))
	}
	wantArgs := []string{"list", "-H", "-t", "snapshot", "-o", "name,creation", "-d", "1", "pool/bkt"}
	got := (*argsChan)[0]
	if len(got) != len(wantArgs) {
		t.Fatalf("argv = %v, want %v", got, wantArgs)
	}
	for i := range wantArgs {
		if got[i] != wantArgs[i] {
			t.Fatalf("argv = %v, want %v", got, wantArgs)
		}
	}
}

func TestListSnapshots_EmptyAndErrorForms(t *testing.T) {
	scriptZfsSnapshotRunner(t, "", nil)
	snaps, err := ListSnapshots(t.Context(), "pool/bkt")
	if err != nil {
		t.Fatalf("empty output: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("empty output: got %d snapshots, want 0", len(snaps))
	}

	scriptZfsSnapshotRunner(t, "", errors.New("exit 1"))
	if _, err := ListSnapshots(t.Context(), "pool/bkt"); err == nil {
		t.Error("runner error must propagate")
	}

	// Garbage lines are skipped, not fatal (zfs pads blank lines on some
	// builds); a snapshot with an unparseable creation keeps a zero time.
	scriptZfsSnapshotRunner(t, "\nnotalist\npool/bkt@weird\tgarbage\n", nil)
	snaps, err = ListSnapshots(t.Context(), "pool/bkt")
	if err != nil {
		t.Fatalf("garbage output: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Short != "weird" {
		t.Fatalf("garbage output: got %+v, want one snapshot weird", snaps)
	}
	if !snaps[0].Creation.IsZero() {
		t.Errorf("unparseable creation = %v, want zero time", snaps[0].Creation)
	}
}

// TestListSnapshots_MinuteResolutionCreationForm pins the REAL zfs
// `list -o creation` wire form (found by the zfs-validate section 10
// probe on zfs-meta's zfs 2.4.1): ctime() at MINUTE resolution —
// "Sat Oct  3  2:11 2026", no seconds, single-digit hour space-padded.
// The seconds-only layouts parsed this to zero, collapsing the
// newest-first ordering to zfs name order (older snapshots first on
// ties). Name-sorted input must still come back newest-first.
func TestListSnapshots_MinuteResolutionCreationForm(t *testing.T) {
	scriptZfsSnapshotRunner(t,
		"pool/bkt@s1val	Sat Oct  3  2:09 2026\n"+
			"pool/bkt@s2val	Sat Oct  3  2:11 2026\n", nil)
	snaps, err := ListSnapshots(t.Context(), "pool/bkt")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}
	if snaps[0].Short != "s2val" || snaps[1].Short != "s1val" {
		t.Errorf("order = [%s %s], want [s2val s1val] (minute-resolution creation must sort)",
			snaps[0].Short, snaps[1].Short)
	}
	if snaps[0].Creation.IsZero() || snaps[1].Creation.IsZero() {
		t.Errorf("creations parsed to zero: %v / %v", snaps[0].Creation, snaps[1].Creation)
	}
}

// ---------- List: newest-first existence walk ----------

func TestZfsSnapshotVersionStore_List_NewestFirstExistenceWalk(t *testing.T) {
	env := snapTestStore(t, true, true)

	entries, err := env.store.List("bkt", "docs/report.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].ID != "s2" || !entries[0].IsLatest {
		t.Errorf("entries[0] = %+v, want s2 IsLatest", entries[0])
	}
	if entries[1].ID != "s1" || entries[1].IsLatest {
		t.Errorf("entries[1] = %+v, want s1 not-latest", entries[1])
	}
	if entries[0].Size != int64(len("bytes-s2")) {
		t.Errorf("entries[0].Size = %d, want %d", entries[0].Size, len("bytes-s2"))
	}
	if entries[0].LastModified.IsZero() {
		t.Error("entries[0].LastModified must carry the snapshot creation")
	}
	if entries[0].IsDeleteMarker {
		t.Error("snapshot entries never carry delete markers")
	}
}

func TestZfsSnapshotVersionStore_List_WindowPartial(t *testing.T) {
	// Key exists only in the OLDER snapshot: still listed, and it is the
	// latest snapshot containing the key.
	env := snapTestStore(t, false, true)

	entries, err := env.store.List("bkt", "docs/report.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "s1" || !entries[0].IsLatest {
		t.Fatalf("entries = %+v, want single s1 IsLatest", entries)
	}
}

func TestZfsSnapshotVersionStore_List_NoSnapshotPresenceIsNoSuchKey(t *testing.T) {
	env := snapTestStore(t, false, false)

	_, err := env.store.List("bkt", "docs/report.txt")
	if !isNoSuchKeyErr(err) {
		t.Fatalf("List err = %v, want NoSuchKey 404-class", err)
	}
}

func TestZfsSnapshotVersionStore_List_DatasetNotTracked(t *testing.T) {
	mountpoint := t.TempDir()
	store := zfsSnapshotVersionStore{
		bucketPath: filepath.Join(mountpoint, "bkt"),
		zdb:        fakeSnapshotResolver{dsErr: &metadata.DatasetNotTrackedError{Path: mountpoint + "/bkt"}},
	}
	scriptZfsSnapshotRunner(t, "", nil)

	if _, err := store.List("bkt", "k"); err == nil {
		t.Fatal("untracked bucket must error, got nil")
	}
}

// ---------- Open: snapshot bytes only, never current data ----------

func TestZfsSnapshotVersionStore_Open_ReadsSnapshotBytes(t *testing.T) {
	env := snapTestStore(t, true, true)

	rc, entry, err := env.store.Open("bkt", "docs/report.txt", "s1")
	if err != nil {
		t.Fatalf("Open(s1): %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "bytes-s1" {
		t.Errorf("read %q, want %q", data, "bytes-s1")
	}
	if entry.ID != "s1" || entry.IsLatest {
		t.Errorf("entry = %+v, want s1 not-latest (s2 is newest with the key)", entry)
	}
}

func TestZfsSnapshotVersionStore_Open_EmptyIDNeverSubstitutesCurrentData(t *testing.T) {
	env := snapTestStore(t, true, true)

	// No snapshot ID: there is no "current version" concept in snapshot
	// mode — reads of the live object bypass the version store entirely.
	if _, _, err := env.store.Open("bkt", "docs/report.txt", ""); !isNoSuchKeyErr(err) {
		t.Fatalf("Open(\"\") err = %v, want NoSuchKey", err)
	}
}

func TestZfsSnapshotVersionStore_Open_UnknownAndExpiredIDs(t *testing.T) {
	env := snapTestStore(t, true, true)

	for _, id := range []string{"nope", "pool/other@s2", "../..", "s2/../../escape"} {
		if _, _, err := env.store.Open("bkt", "docs/report.txt", id); !isNoSuchKeyErr(err) {
			t.Errorf("Open(%q) err = %v, want NoSuchKey 404-class", id, err)
		}
	}
}

func TestZfsSnapshotVersionStore_Open_KeyMissingInSnapshot(t *testing.T) {
	// s2 exists (of the dataset) but does not contain the key: honest 404.
	env := snapTestStore(t, false, true)

	if _, _, err := env.store.Open("bkt", "docs/report.txt", "s2"); !isNoSuchKeyErr(err) {
		t.Fatalf("Open(s2) err = %v, want NoSuchKey", err)
	}
}

// ---------- read-only semantics ----------

func TestZfsSnapshotVersionStore_PutVersion_ReadOnly(t *testing.T) {
	env := snapTestStore(t, true, true)

	_, err := env.store.PutVersion("bkt", "k", strings.NewReader("x"), 1, "etag")
	if !errors.Is(err, ErrSnapshotsReadOnly) {
		t.Fatalf("PutVersion err = %v, want ErrSnapshotsReadOnly", err)
	}
}

func TestZfsSnapshotVersionStore_PutDeleteMarker_Unsupported(t *testing.T) {
	env := snapTestStore(t, true, true)

	_, err := env.store.PutDeleteMarker("bkt", "k")
	if !errors.Is(err, ErrDeleteMarkersUnsupported) {
		t.Fatalf("PutDeleteMarker err = %v, want ErrDeleteMarkersUnsupported", err)
	}
}

// ---------- State/SetState: shared bucket-level marker ----------

func TestZfsSnapshotVersionStore_StateSharedMarker(t *testing.T) {
	env := snapTestStore(t, true, true)

	state, err := env.store.State("bkt")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != versioningOff {
		t.Errorf("default state = %q, want Off", state)
	}

	// The sidecar store writes the marker; the snapshot store reads the
	// SAME bucket-level mechanism (path-independent, mechanism-independent).
	sidecar := sidecarVersionStore{bucketPath: filepath.Join(env.mountpoint, "bkt")}
	if err := sidecar.SetState("bkt", versioningEnabled); err != nil {
		t.Fatalf("sidecar SetState: %v", err)
	}
	state, err = env.store.State("bkt")
	if err != nil {
		t.Fatalf("State after sidecar SetState: %v", err)
	}
	if state != versioningEnabled {
		t.Errorf("state = %q, want Enabled (shared marker)", state)
	}

	if err := env.store.SetState("bkt", versioningSuspended); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	state, err = sidecar.State("bkt")
	if err != nil {
		t.Fatalf("sidecar State: %v", err)
	}
	if state != versioningSuspended {
		t.Errorf("sidecar State = %q, want Suspended (shared marker)", state)
	}

	if err := env.store.SetState("bkt", "bogus"); err == nil {
		t.Error("invalid state must be rejected")
	}
}

// ---------- factory wiring ----------

func TestVersionStoreFor_SnapshotsModeYieldsZfsSnapshotStore(t *testing.T) {
	bp := t.TempDir()
	st := versionStoreFor(bp, nil, "snapshots")
	if _, ok := st.(zfsSnapshotVersionStore); !ok {
		t.Fatalf("snapshots mode yielded %T, want zfsSnapshotVersionStore", st)
	}
}

// compile-time guard on the objectmodel 404-class identity used by the
// 404 contract.
var _ = objectmodel.ErrNoSuchKey
