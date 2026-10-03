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
