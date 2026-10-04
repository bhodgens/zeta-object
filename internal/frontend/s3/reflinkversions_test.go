// reflinkversions_test.go — reflink (block-clone) versioning mode tests
// (s3-versioning tree leaf 06). Covers: factory dispatch (reflink /
// both), capture fail-soft (the darwin dev arm returns
// errReflinkUnsupported — the PUT must proceed, no version record, no
// orphaned dest), the clone-seam round-trip (fake reflinkClone via the
// seam variable), both-mode merge (reflink bytes first, sidecar-layout
// fallback, delete markers), and delete-marker suppression through the
// versionStoreForBucket dispatch.
package s3

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cloneSeam replaces reflinkClone for the test's duration.
func withCloneSeam(t *testing.T, fn func(dstPath, srcPath string) error) {
	t.Helper()
	old := reflinkCloneFn
	reflinkCloneFn = fn
	t.Cleanup(func() { reflinkCloneFn = old })
}

// installReflinkMode installs the reflink mode seam (retention lands
// with the pruning commit — 0/unlimited here).
func installReflinkMode(t *testing.T, retention int) {
	t.Helper()
	if retention != 0 {
		t.Fatal("commit-2 tests run at unlimited retention; pruning tests own the retention seam")
	}
	installZfsVersioningModeForTest(t, "reflink")
}

func TestVersionStoreFor_ReflinkModes(t *testing.T) {
	bp := t.TempDir()
	if _, ok := versionStoreFor(bp, nil, "reflink").(reflinkVersionStore); !ok {
		t.Fatalf("reflink mode yielded %T, want reflinkVersionStore", versionStoreFor(bp, nil, "reflink"))
	}
	if _, ok := versionStoreFor(bp, nil, "both").(reflinkBothVersionStore); !ok {
		t.Fatalf("both mode yielded %T, want reflinkBothVersionStore", versionStoreFor(bp, nil, "both"))
	}
	// The zero-value seam state ("") still maps to the sidecar store
	// (unit tests / unwired servers keep pre-leaf-06 behavior).
	if _, ok := versionStoreFor(bp, nil, "").(sidecarVersionStore); !ok {
		t.Fatalf("empty mode yielded %T, want sidecarVersionStore", versionStoreFor(bp, nil, ""))
	}
}

// TestCaptureReflink_FailSoft pins the leaf-06 fail-soft contract: on
// the GOOS fallback arm (darwin dev) the capture returns a NON-NIL
// capture with cloneOK=false and nil error; the record step then writes
// NOTHING (no sidecar entry, no .versions-r file left behind).
func TestCaptureReflink_FailSoft(t *testing.T) {
	env := setupS3TestEnv(t)
	bkt := "reflink-failsoft"
	_ = env.setupBucket(t, bkt)
	bucketPath := filepath.Join(env.dataDir, bkt)
	installReflinkMode(t, 0)
	// Default seam = the real GOOS arm (on darwin: errReflinkUnsupported).
	if err := os.WriteFile(filepath.Join(bucketPath, "obj.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The object needs a sidecar to count as existing.
	if err := writeTestSidecar(t, bucketPath, "obj.txt"); err != nil {
		t.Fatal(err)
	}
	captured, err := captureReflinkObjectVersion(bucketPath, "obj.txt")
	if err != nil {
		t.Fatalf("fail-soft capture must not error, got: %v", err)
	}
	if captured.cloneOK {
		t.Fatal("capture on the unsupported-GOOS arm must report cloneOK=false")
	}
	// No orphaned dest FILE (the empty per-key directory may remain).
	vrDir := filepath.Join(bucketPath, ".metadata", reflinkVersionsDirName)
	leftovers := 0
	if entries, err := os.ReadDir(vrDir); err == nil {
		for _, e := range entries {
			sub, _ := os.ReadDir(filepath.Join(vrDir, e.Name()))
			leftovers += len(sub)
		}
	}
	if leftovers != 0 {
		t.Fatalf(".versions-r must hold no data files after a failed clone, got %d", leftovers)
	}
	// The record step is a silent no-op on cloneOK=false.
	if err := recordCapturedObjectVersion(bucketPath, bkt, "obj.txt", &capturedObjectVersion{reflink: captured}); err != nil {
		t.Fatalf("record with cloneOK=false must be nil, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bucketPath, "obj.txt.meta")); err == nil {
		raw, _ := os.ReadFile(filepath.Join(bucketPath, "obj.txt.meta"))
		if strings.Contains(string(raw), "Versions") {
			t.Fatalf("fail-soft record must not write a Versions entry: %s", raw)
		}
	}
}

// fakeClone emulates FICLONE by copying bytes — the round-trip path the
// linux arm exercises with the real ioctl (proven live by zfs-validate
// section 11).
func fakeClone(t *testing.T) {
	t.Helper()
	withCloneSeam(t, func(dstPath, srcPath string) error {
		raw, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		return os.WriteFile(dstPath, raw, 0o644)
	})
}

// writeTestSidecar creates a minimal sidecar + data file for key.
func writeTestSidecar(t *testing.T, bucketPath, key string) error {
	t.Helper()
	metaDir := filepath.Join(bucketPath, ".metadata")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return err
	}
	sidecar := `{"contentType":"binary/octet-stream","contentLength":2,"eTag":"abc","lastModified":"2026-10-02T00:00:00Z","storagePath":` +
		`"` + bucketPath + `/` + key + `"` + `}`
	return os.WriteFile(filepath.Join(metaDir, key+".meta"), []byte(sidecar), 0o644)
}

// TestCaptureReflink_RoundTrip exercises the success path through the
// clone seam: capture clones the old bytes, record lands the sidecar
// entry, Open reads the OLD bytes back by id (the round-trip the live
// ioctl proves on linux/ZFS).
func TestCaptureReflink_RoundTrip(t *testing.T) {
	env := setupS3TestEnv(t)
	bkt := "reflink-roundtrip"
	_ = env.setupBucket(t, bkt)
	bucketPath := filepath.Join(env.dataDir, bkt)
	installReflinkMode(t, 0)
	fakeClone(t)

	if err := os.WriteFile(filepath.Join(bucketPath, "obj.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeTestSidecar(t, bucketPath, "obj.txt"); err != nil {
		t.Fatal(err)
	}
	captured, err := captureReflinkObjectVersion(bucketPath, "obj.txt")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !captured.cloneOK {
		t.Fatal("clone through the fake seam must succeed")
	}
	if captured.size != 2 || captured.etag != "abc" {
		t.Fatalf("capture metadata: size=%d etag=%q, want 2/abc", captured.size, captured.etag)
	}
	// The clone lands under .versions-r/<key-sha>/<id> with the OLD bytes.
	dataPath := filepath.Join(bucketPath, ".metadata", reflinkVersionsDirName, keySha("obj.txt"), captured.id)
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("clone file missing: %v", err)
	}
	if string(raw) != "v1" {
		t.Fatalf("clone bytes = %q, want v1", raw)
	}
	// Record (simulating post-Put) + full read-back through the store.
	if err := recordCapturedObjectVersion(bucketPath, bkt, "obj.txt", &capturedObjectVersion{reflink: captured}); err != nil {
		t.Fatalf("record: %v", err)
	}
	store := versionStoreFor(bucketPath, nil, "reflink")
	rc, entry, err := store.Open(bkt, "obj.txt", captured.id)
	if err != nil {
		t.Fatalf("Open by cloned id: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Fatalf("Open bytes = %q, want v1", got)
	}
	if entry.ID != captured.id || entry.ETag != "abc" || entry.Size != 2 {
		t.Fatalf("entry metadata: %+v", entry)
	}
}

// TestReflinkBoth_Merge pins the leaf-06 "both" semantics: writes go to
// the reflink layout; an id whose bytes live ONLY in the plain
// .versions layout (history written under sidecar mode) still opens.
func TestReflinkBoth_Merge(t *testing.T) {
	bp := t.TempDir()
	m := reflinkBothVersionStore{
		reflink: reflinkVersionStore{sidecarVersionStore{bucketPath: bp}},
		sidecar: sidecarVersionStore{bucketPath: bp},
	}
	// A version written through the reflink store.
	e1, err := m.PutVersion("b", "k.txt", strings.NewReader("reflink-bytes"), 13, "etag-r")
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	// A version written through the plain sidecar layout (simulating
	// pre-switch history): data under .versions + entry in the shared
	// sidecar array.
	legacyID, err := newStateVersionID()
	if err != nil {
		t.Fatal(err)
	}
	s := sidecarVersionStore{bucketPath: bp}
	if err := os.MkdirAll(s.versionsDir("k.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(s.versionDataPath("k.txt", legacyID), []byte("sidecar-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock := lockObject(s.sidecarPath("k.txt"))
	vs, err := s.readVersionedSidecar("k.txt")
	if err != nil && !isNoSuchKeyErr(err) {
		t.Fatal(err)
	}
	vs.Versions = append(vs.Versions, sidecarVersionEntry{ID: legacyID, ETag: "etag-s", LastModified: e1.LastModified})
	if err := s.writeVersionedSidecar("k.txt", vs); err != nil {
		t.Fatal(err)
	}
	unlock()

	// List shows BOTH entries (the shared array).
	list, err := m.List("b", "k.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %d entries, want 2", len(list))
	}
	// Open resolves the reflink-layout id...
	rc, _, err := m.Open("b", "k.txt", e1.ID)
	if err != nil {
		t.Fatalf("Open reflink id: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "reflink-bytes" {
		t.Fatalf("reflink id bytes = %q", b)
	}
	// ...AND the sidecar-layout id (the merge contract).
	rc, _, err = m.Open("b", "k.txt", legacyID)
	if err != nil {
		t.Fatalf("Open sidecar-layout id: %v", err)
	}
	b, _ = io.ReadAll(rc)
	rc.Close()
	if string(b) != "sidecar-bytes" {
		t.Fatalf("sidecar-layout id bytes = %q", b)
	}
	// A delete marker through the both store hides the current version.
	if _, err := m.PutDeleteMarker("b", "k.txt"); err != nil {
		t.Fatalf("PutDeleteMarker: %v", err)
	}
	if _, _, err := m.Open("b", "k.txt", ""); !errors.Is(err, ErrIsDeleteMarker) {
		t.Fatalf("Open after marker = %v, want ErrIsDeleteMarker", err)
	}
}

// TestReflink_RetentionPrune pins the leaf-06 retention contract: with
// retention=2, a 4th recorded version prunes the OLDEST version data
// file and its sidecar entry; delete markers are never counted and
// never pruned; the crash-safety invariant holds (every remaining
// sidecar entry's data file exists — never a lying sidecar).
func TestReflink_RetentionPrune(t *testing.T) {
	env := setupS3TestEnv(t)
	bkt := "reflink-retain"
	_ = env.setupBucket(t, bkt)
	bucketPath := filepath.Join(env.dataDir, bkt)
	installReflinkMode(t, 0)
	InstallZfsVersioningReflinkRetention(2)
	t.Cleanup(func() { InstallZfsVersioningReflinkRetention(0) })
	fakeClone(t)

	if err := os.WriteFile(filepath.Join(bucketPath, "k.txt"), []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeTestSidecar(t, bucketPath, "k.txt"); err != nil {
		t.Fatal(err)
	}
	store := versionStoreFor(bucketPath, nil, "reflink")

	// Four captures+records (simulating 4 overwrites on the wire). Each
	// capture rides the captured history (the backend Put wipes the
	// sidecar; the record step restores priorEntries beneath the new
	// entry — the live bug this pins).
	var ids []string
	var history []sidecarVersionEntry
	for _, v := range []string{"v1", "v2", "v3", "v4"} {
		captured, err := captureReflinkObjectVersion(bucketPath, "k.txt")
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		if !captured.cloneOK {
			t.Fatalf("clone failed unexpectedly")
		}
		// Simulate the new write landing before the record step.
		if err := os.WriteFile(filepath.Join(bucketPath, "k.txt"), []byte(v+"-new"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := recordCapturedObjectVersion(bucketPath, bkt, "k.txt", &capturedObjectVersion{reflink: captured, priorEntries: history}); err != nil {
			t.Fatalf("record: %v", err)
		}
		history = append([]sidecarVersionEntry{{
			ID: captured.id, Size: captured.size, ETag: captured.etag,
		}}, history...)
		ids = append(ids, captured.id)
	}

	// A delete marker exists in the history: never counted, never pruned.
	if _, err := store.PutDeleteMarker(bkt, "k.txt"); err != nil {
		t.Fatal(err)
	}

	list, err := store.List(bkt, "k.txt")
	if err != nil {
		t.Fatal(err)
	}
	markers, versions := 0, 0
	for _, e := range list {
		if e.IsDeleteMarker {
			markers++
		} else {
			versions++
		}
	}
	if markers != 1 {
		t.Fatalf("markers = %d, want 1 (retention never prunes markers)", markers)
	}
	if versions != 2 {
		t.Fatalf("versions = %d, want 2 (retention=2)", versions)
	}

	// The OLDEST data files are gone (v1, v2 clones), the newest two remain.
	for _, gone := range ids[:2] {
		if _, err := os.Stat(filepath.Join(bucketPath, ".metadata", reflinkVersionsDirName, keySha("k.txt"), gone)); !os.IsNotExist(err) {
			t.Fatalf("oldest version data %s must be pruned", gone)
		}
	}
	for _, keep := range ids[2:] {
		if _, err := os.Stat(filepath.Join(bucketPath, ".metadata", reflinkVersionsDirName, keySha("k.txt"), keep)); err != nil {
			t.Fatalf("kept version data %s must exist: %v", keep, err)
		}
	}

	// Crash-safety invariant: EVERY remaining sidecar entry either is a
	// marker (no data) or has its data file on disk — never a lying
	// sidecar.
	vs, err := (reflinkVersionStore{sidecarVersionStore{bucketPath: bucketPath}}).readVersionedSidecar("k.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range vs.Versions {
		if e.IsDeleteMarker {
			continue
		}
		if _, err := os.Stat(filepath.Join(bucketPath, ".metadata", reflinkVersionsDirName, keySha("k.txt"), e.ID)); err != nil {
			t.Fatalf("lying sidecar: entry %s has no data file: %v", e.ID, err)
		}
	}
}

// TestReflink_RetentionZeroUnlimited pins 0/unset = unlimited (no
// pruning at any count).
func TestReflink_RetentionZeroUnlimited(t *testing.T) {
	if reflinkRetentionFor() != 0 {
		t.Fatalf("default retention = %d, want 0 (unlimited)", reflinkRetentionFor())
	}
	bp := t.TempDir()
	s := reflinkVersionStore{sidecarVersionStore{bucketPath: bp}}
	for range 5 {
		if _, err := s.PutVersion("b", "k.txt", strings.NewReader("x"), 1, "e"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List("b", "k.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("versions = %d, want 5 (unlimited)", len(list))
	}
}

// TestReflink_DeleteMarkerSuppression pins the delete contract through
// the real dispatch: versionStoreForBucket in reflink mode + Enabled
// state suppresses the plain delete (deleteObjectVersionedMarker).
func TestReflink_DeleteMarkerSuppression(t *testing.T) {
	env := setupS3TestEnv(t)
	bkt := "reflink-delete"
	_ = env.setupBucket(t, bkt)
	bucketPath := filepath.Join(env.dataDir, bkt)
	installReflinkMode(t, 0)

	store := versionStoreForBucket(bucketPath)
	if err := store.SetState(bkt, versioningEnabled); err != nil {
		t.Fatal(err)
	}
	// A data file that must SURVIVE the delete.
	if err := os.WriteFile(filepath.Join(bucketPath, "doomed.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeTestSidecar(t, bucketPath, "doomed.txt"); err != nil {
		t.Fatal(err)
	}
	suppress, err := deleteObjectVersionedMarker(bucketPath, bkt, "doomed.txt")
	if err != nil {
		t.Fatalf("deleteObjectVersionedMarker: %v", err)
	}
	if !suppress {
		t.Fatal("Enabled + reflink mode must suppress the plain delete")
	}
	if _, err := os.Stat(filepath.Join(bucketPath, "doomed.txt")); err != nil {
		t.Fatal("data file must survive a versioned delete")
	}
	list, err := store.List(bkt, "doomed.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || !list[0].IsDeleteMarker {
		t.Fatalf("List after delete = %+v, want one delete marker", list)
	}
}

// TestReflinkRetention_PerBucketOverride (leaf 07): a bucket's own
// reflinkRetention overrides the server-wide fallback; buckets without
// one fall back; an explicit per-bucket 0 keeps zero version copies even
// when the server-wide key is nonzero.
func TestReflinkRetention_PerBucketOverride(t *testing.T) {
	env := setupS3TestEnv(t)
	InstallZfsVersioningReflinkRetention(2) // server-wide fallback
	t.Cleanup(func() { InstallZfsVersioningReflinkRetention(0) })
	InstallServerConfigView(ServerConfigView{
		Buckets:          map[string]string{"b-3": env.dataDir + "/b-3", "b-0": env.dataDir + "/b-0", "b-fb": env.dataDir + "/b-fb"},
		DataDir:          env.dataDir,
		ReflinkRetention: map[string]int{"b-3": 3, "b-0": 0}, // b-fb absent → fallback 2
	})
	t.Cleanup(func() { InstallServerConfigView(ServerConfigView{DataDir: env.dataDir}) })
	fakeClone(t)

	for _, b := range []string{"b-3", "b-0", "b-fb"} {
		if err := os.MkdirAll(env.dataDir+"/"+b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(env.dataDir, b, "k.txt"), []byte("v0"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeTestSidecar(t, env.dataDir+"/"+b, "k.txt"); err != nil {
			t.Fatal(err)
		}
	}

	overwrite := func(bucket string) {
		captured, err := captureReflinkObjectVersion(env.dataDir+"/"+bucket, "k.txt")
		if err != nil {
			t.Fatalf("%s capture: %v", bucket, err)
		}
		if !captured.cloneOK {
			t.Fatalf("%s clone failed", bucket)
		}
		if err := os.WriteFile(filepath.Join(env.dataDir, bucket, "k.txt"), []byte("new-"+bucket), 0o644); err != nil {
			t.Fatal(err)
		}
		// Prior history rides the capture (the backend Put wipes the
		// sidecar; the record step restores it — the live-bug invariant).
		st := reflinkVersionStore{sidecarVersionStore{bucketPath: env.dataDir + "/" + bucket}}
		prior, err := st.readVersionedSidecar("k.txt")
		if err != nil && !isNoSuchKeyErr(err) {
			t.Fatalf("%s read sidecar: %v", bucket, err)
		}
		if err := recordCapturedObjectVersion(env.dataDir+"/"+bucket, bucket, "k.txt", &capturedObjectVersion{reflink: captured, priorEntries: prior.Versions}); err != nil {
			t.Fatalf("%s record: %v", bucket, err)
		}
	}

	// b-0 has per-bucket 0: the first recorded version is pruned
	// immediately (keep zero copies) even though the fallback is 2.
	overwrite("b-0")
	s0 := reflinkVersionStore{sidecarVersionStore{bucketPath: env.dataDir + "/b-0"}}
	vs0, err := s0.readVersionedSidecar("k.txt")
	if err != nil {
		t.Fatal(err)
	}
	if n := countVersionEntries(vs0); n != 0 {
		t.Fatalf("b-0 (per-bucket 0): got %d version entries, want 0 (explicit zero overrides fallback)", n)
	}

	// b-3 keeps 3 versions despite overwriting 3 times (fallback would
	// have pruned to 2).
	for range 3 {
		overwrite("b-3")
	}
	sbs := reflinkVersionStore{sidecarVersionStore{bucketPath: env.dataDir + "/b-3"}}
	vs3, err := sbs.readVersionedSidecar("k.txt")
	if err != nil {
		t.Fatal(err)
	}
	if n := countVersionEntries(vs3); n != 3 {
		t.Fatalf("b-3 (per-bucket 3): got %d version entries, want 3", n)
	}

	// b-fb (no per-bucket value) falls back to the server-wide 2.
	for range 3 {
		overwrite("b-fb")
	}
	sfb := reflinkVersionStore{sidecarVersionStore{bucketPath: env.dataDir + "/b-fb"}}
	vsfb, err := sfb.readVersionedSidecar("k.txt")
	if err != nil {
		t.Fatal(err)
	}
	if n := countVersionEntries(vsfb); n != 2 {
		t.Fatalf("b-fb (fallback 2): got %d version entries, want 2", n)
	}
}

// countVersionEntries counts non-marker version entries.
func countVersionEntries(vs versionedSidecar) int {
	n := 0
	for _, e := range vs.Versions {
		if !e.IsDeleteMarker {
			n++
		}
	}
	return n
}
