package s3

// versionstore_test.go — sidecarVersionStore tests (s3-versioning tree
// leaf 01). Covers: state round-trip (Off/Enabled/Suspended marker),
// PutVersion data file + sidecar update (atomic), PutDeleteMarker append,
// List newest-first with IsLatest on the newest entry, Open current + old
// by id, ErrIsDeleteMarker on marker Open, never-versioned bucket zero
// behavior change, sidecar backward compatibility (old sidecars decode
// fine; empty-versioning sidecars stay byte-identical), and the
// versionStoreFor factory mode dispatch.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// verTestStore builds a sidecarVersionStore over a fresh temp bucket.
func verTestStore(t *testing.T) (sidecarVersionStore, string) {
	t.Helper()
	bucketPath := filepath.Join(t.TempDir(), "ver-bucket")
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("mkdir bucket: %v", err)
	}
	return sidecarVersionStore{bucketPath: bucketPath}, bucketPath
}

// verTestStoreWithObject returns a store whose bucket holds one object
// written by the real fsbackend (production sidecar on disk).
func verTestStoreWithObject(t *testing.T, key string) (sidecarVersionStore, string) {
	t.Helper()
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "ver-bucket")
	if _, err := env.b.Put(t.Context(), "ver-bucket", key, strings.NewReader("current-bytes"), int64(len("current-bytes")), objectmodel.PutOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("fsbackend Put: %v", err)
	}
	bucketPath := filepath.Join(env.dataDir, "ver-bucket")
	return sidecarVersionStore{bucketPath: bucketPath}, bucketPath
}

var versionIDRe = regexp.MustCompile(`^[0-9]{16}-[0-9a-f]{8}$`)

func TestSidecarVersionStore_ImplementsContract1(t *testing.T) {
	var _ versionStore = sidecarVersionStore{}
}

// ---------- State / SetState round-trip ----------

func TestSidecarVersionStore_State_DefaultsToOff(t *testing.T) {
	s, _ := verTestStore(t)

	state, err := s.State("ver-bucket")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != versioningOff {
		t.Errorf("default state = %q, want %q", state, versioningOff)
	}
}

func TestSidecarVersionStore_StateRoundTrip(t *testing.T) {
	s, _ := verTestStore(t)

	for _, want := range []string{versioningEnabled, versioningSuspended, versioningOff} {
		if err := s.SetState("ver-bucket", want); err != nil {
			t.Fatalf("SetState(%q): %v", want, err)
		}
		got, err := s.State("ver-bucket")
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if got != want {
			t.Errorf("round-trip state = %q, want %q", got, want)
		}
	}
}

func TestSidecarVersionStore_SetState_RejectsInvalid(t *testing.T) {
	s, _ := verTestStore(t)

	if err := s.SetState("ver-bucket", "Bogus"); err == nil {
		t.Fatal("SetState(Bogus) must fail")
	}
	state, err := s.State("ver-bucket")
	if err != nil || state != versioningOff {
		t.Errorf("after rejected SetState state = %q err=%v, want Off", state, err)
	}
}

// ---------- PutVersion ----------

func TestSidecarVersionStore_PutVersion_WritesDataFileAndSidecar(t *testing.T) {
	s, bucketPath := verTestStore(t)

	entry, err := s.PutVersion("ver-bucket", "docs/a.txt", strings.NewReader("v1-body"), 7, "etag-1")
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}

	if !versionIDRe.MatchString(entry.ID) {
		t.Errorf("version id %q does not match <unix-micros>-<8hex>", entry.ID)
	}
	if !entry.IsLatest || entry.IsDeleteMarker {
		t.Errorf("entry flags wrong: %+v", entry)
	}
	if entry.Size != 7 || entry.ETag != "etag-1" {
		t.Errorf("entry metadata wrong: %+v", entry)
	}

	// Data file at <bucket>/.metadata/.versions/<sha256(key)>/<id>.
	dataPath := filepath.Join(bucketPath, ".metadata", ".versions", keySha("docs/a.txt"), entry.ID)
	raw, err := os.ReadFile(dataPath) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("version data file: %v", err)
	}
	if string(raw) != "v1-body" {
		t.Errorf("data file content = %q, want v1-body", raw)
	}

	// Sidecar updated additively: legacy fields preserved, new fields set.
	sc, err := s.readVersionedSidecar("docs/a.txt")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if sc.Versioning != versioningEnabled {
		t.Errorf("sidecar Versioning = %q, want Enabled", sc.Versioning)
	}
	if len(sc.Versions) != 1 || sc.Versions[0].ID != entry.ID {
		t.Fatalf("sidecar versions = %+v, want exactly [%s]", sc.Versions, entry.ID)
	}
	if sc.CurrentVersionID != entry.ID {
		t.Errorf("CurrentVersionID = %q, want %q", sc.CurrentVersionID, entry.ID)
	}
}

func TestSidecarVersionStore_PutVersion_PreservesLegacySidecarFields(t *testing.T) {
	s, _ := verTestStoreWithObject(t, "obj.txt")

	entry, err := s.PutVersion("ver-bucket", "obj.txt", strings.NewReader("v2"), 2, "etag-2")
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}

	sc, err := s.readVersionedSidecar("obj.txt")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if sc.ContentType != "text/plain" {
		t.Errorf("legacy ContentType lost: %q", sc.ContentType)
	}
	if sc.ContentLength != int64(len("current-bytes")) {
		t.Errorf("legacy ContentLength clobbered: %d", sc.ContentLength)
	}
	if len(sc.Versions) != 1 || sc.Versions[0].ID != entry.ID {
		t.Errorf("versions = %+v", sc.Versions)
	}
}

func TestSidecarVersionStore_PutVersion_TwoVersionsNewestFirstAndIsLatestMoves(t *testing.T) {
	s, _ := verTestStore(t)

	e1, err := s.PutVersion("ver-bucket", "k", strings.NewReader("one"), 3, "e1")
	if err != nil {
		t.Fatalf("PutVersion 1: %v", err)
	}
	e2, err := s.PutVersion("ver-bucket", "k", strings.NewReader("two"), 3, "e2")
	if err != nil {
		t.Fatalf("PutVersion 2: %v", err)
	}

	list, err := s.List("ver-bucket", "k")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	if list[0].ID != e2.ID || !list[0].IsLatest {
		t.Errorf("newest entry = %+v, want %s IsLatest", list[0], e2.ID)
	}
	if list[1].ID != e1.ID || list[1].IsLatest {
		t.Errorf("older entry = %+v, want %s not IsLatest", list[1], e1.ID)
	}
}

func TestSidecarVersionStore_PutVersion_RejectsShortBody(t *testing.T) {
	s, _ := verTestStore(t)

	if _, err := s.PutVersion("ver-bucket", "k", strings.NewReader("ab"), 3, "e"); err == nil {
		t.Fatal("short body must fail")
	}
	// No sidecar side effects on failure.
	if _, err := os.Stat(filepath.Join(s.bucketPath, ".metadata", "k.meta")); !os.IsNotExist(err) {
		t.Errorf("sidecar must not exist after failed PutVersion, stat err = %v", err)
	}
}

// ---------- PutDeleteMarker ----------

func TestSidecarVersionStore_PutDeleteMarker_AppendsNewestFirst(t *testing.T) {
	s, _ := verTestStore(t)

	e1, err := s.PutVersion("ver-bucket", "k", strings.NewReader("body"), 4, "e1")
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	marker, err := s.PutDeleteMarker("ver-bucket", "k")
	if err != nil {
		t.Fatalf("PutDeleteMarker: %v", err)
	}
	if !marker.IsDeleteMarker || !marker.IsLatest || marker.Size != 0 {
		t.Errorf("marker entry wrong: %+v", marker)
	}
	if !versionIDRe.MatchString(marker.ID) || marker.ID == e1.ID {
		t.Errorf("marker id %q wrong", marker.ID)
	}

	list, err := s.List("ver-bucket", "k")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != marker.ID || list[1].ID != e1.ID {
		t.Fatalf("list = %+v", list)
	}
	if list[1].IsLatest {
		t.Error("previous version must lose IsLatest after the marker")
	}

	// Data untouched.
	data, err := os.ReadFile(s.versionDataPath("k", e1.ID)) //nolint:gosec // test path
	if err != nil || string(data) != "body" {
		t.Errorf("marker must not touch data: data=%q err=%v", data, err)
	}
}

func TestSidecarVersionStore_PutDeleteMarker_NoPriorVersions(t *testing.T) {
	s, _ := verTestStore(t)

	marker, err := s.PutDeleteMarker("ver-bucket", "never-existed")
	if err != nil {
		t.Fatalf("PutDeleteMarker: %v", err)
	}
	if !marker.IsDeleteMarker || !marker.IsLatest {
		t.Errorf("marker entry wrong: %+v", marker)
	}
	sc, err := s.readVersionedSidecar("never-existed")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if sc.CurrentVersionID != "" {
		t.Errorf("CurrentVersionID after marker = %q, want empty", sc.CurrentVersionID)
	}
}

// ---------- List ----------

func TestSidecarVersionStore_List_MissingKeyIsNoSuchKey(t *testing.T) {
	s, _ := verTestStore(t)

	_, err := s.List("ver-bucket", "absent")
	var omErr *objectmodel.Error
	if !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
		t.Fatalf("expected NoSuchKey, got %v", err)
	}
}

func TestSidecarVersionStore_List_EntryFieldsRoundTrip(t *testing.T) {
	s, _ := verTestStore(t)

	before := time.Now().Add(-time.Second).UTC()
	entry, err := s.PutVersion("ver-bucket", "k", strings.NewReader("x"), 1, "etag-x")
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	after := time.Now().Add(time.Second).UTC()

	list, err := s.List("ver-bucket", "k")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := list[0]
	if got.ID != entry.ID || got.Size != 1 || got.ETag != "etag-x" || got.IsDeleteMarker {
		t.Errorf("entry = %+v", got)
	}
	if got.LastModified.Before(before) || got.LastModified.After(after) {
		t.Errorf("LastModified %v outside [%v, %v]", got.LastModified, before, after)
	}
}

// ---------- Open ----------

func TestSidecarVersionStore_Open_CurrentAndOldByID(t *testing.T) {
	s, _ := verTestStore(t)

	e1, err := s.PutVersion("ver-bucket", "k", strings.NewReader("one"), 3, "e1")
	if err != nil {
		t.Fatalf("PutVersion 1: %v", err)
	}
	if _, err := s.PutVersion("ver-bucket", "k", strings.NewReader("two"), 3, "e2"); err != nil {
		t.Fatalf("PutVersion 2: %v", err)
	}

	// Old version by id.
	rc, entry, err := s.Open("ver-bucket", "k", e1.ID)
	if err != nil {
		t.Fatalf("Open(old): %v", err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read old version: %v", err)
	}
	if string(body) != "one" {
		t.Errorf("old version body = %q, want one", body)
	}
	if entry.ID != e1.ID || entry.IsLatest {
		t.Errorf("old entry = %+v, must not be IsLatest", entry)
	}

	// Current: empty id.
	rc, entry, err = s.Open("ver-bucket", "k", "")
	if err != nil {
		t.Fatalf("Open(current): %v", err)
	}
	body, _ = io.ReadAll(rc)
	rc.Close()
	if string(body) != "two" {
		t.Errorf("current body = %q, want two", body)
	}
	if !entry.IsLatest {
		t.Error("current entry must be IsLatest")
	}

	// Current: explicit current id.
	rc, _, err = s.Open("ver-bucket", "k", entry.ID)
	if err != nil {
		t.Fatalf("Open(current by id): %v", err)
	}
	body, _ = io.ReadAll(rc)
	rc.Close()
	if string(body) != "two" {
		t.Errorf("current-by-id body = %q, want two", body)
	}
}

func TestSidecarVersionStore_Open_DeleteMarkerIDIsTypedError(t *testing.T) {
	s, _ := verTestStore(t)

	if _, err := s.PutVersion("ver-bucket", "k", strings.NewReader("body"), 4, "e1"); err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	marker, err := s.PutDeleteMarker("ver-bucket", "k")
	if err != nil {
		t.Fatalf("PutDeleteMarker: %v", err)
	}

	rc, entry, err := s.Open("ver-bucket", "k", marker.ID)
	if !errors.Is(err, ErrIsDeleteMarker) {
		t.Fatalf("expected ErrIsDeleteMarker, got %v", err)
	}
	if rc != nil {
		rc.Close()
		t.Error("no reader may be returned for a delete marker")
	}
	if !entry.IsDeleteMarker || entry.ID != marker.ID {
		t.Errorf("marker entry = %+v", entry)
	}
}

func TestSidecarVersionStore_Open_UnknownIDIsNoSuchKey(t *testing.T) {
	s, _ := verTestStore(t)

	if _, err := s.PutVersion("ver-bucket", "k", strings.NewReader("body"), 4, "e1"); err != nil {
		t.Fatalf("PutVersion: %v", err)
	}

	_, _, err := s.Open("ver-bucket", "k", "1234-deadbeef")
	var omErr *objectmodel.Error
	if !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
		t.Fatalf("expected NoSuchKey, got %v", err)
	}
}

// ---------- never-versioned buckets: zero behavior change ----------

func TestSidecarVersionStore_NeverVersionedBucket_ZeroBehaviorChange(t *testing.T) {
	s, bucketPath := verTestStoreWithObject(t, "plain.txt")

	// State reads Off with no marker file on disk.
	state, err := s.State("ver-bucket")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != versioningOff {
		t.Fatalf("state = %q, want Off", state)
	}
	if _, err := os.Stat(filepath.Join(bucketPath, ".metadata", ".versioning")); !os.IsNotExist(err) {
		t.Fatalf(".versioning marker must not exist in a never-versioned bucket: %v", err)
	}

	// The sidecar on disk is byte-identical to the pre-versioning form:
	// no Versioning/Versions/CurrentVersionID fields may appear.
	raw, err := os.ReadFile(filepath.Join(bucketPath, ".metadata", "plain.txt.meta")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	for _, banned := range []string{"Versioning", "Versions", "CurrentVersionID", "isDeleteMarker"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("never-versioned sidecar contains %q: %s", banned, raw)
		}
	}

	// A sidecar with no Versions lists/opens as NoSuchKey (no phantom
	// versions appear for unversioned objects).
	if _, err := s.List("ver-bucket", "plain.txt"); !isNoSuchKeyErr(err) {
		t.Errorf("List on version-less sidecar = %v, want NoSuchKey", err)
	}
}

func TestSidecarVersionStore_OldSidecarWithoutNewFields_DecodesFine(t *testing.T) {
	// HARD requirement: a pre-versioning sidecar (the exact legacy JSON
	// shape) decodes through the versioned envelope with zero values for
	// the new fields.
	s, _ := verTestStore(t)
	legacy := `{"contentType":"text/plain","contentLength":5,"eTag":"abc","customMetadata":{},"lastModified":"2026-01-02T03:04:05Z","storagePath":"plain.txt"}`
	if err := writeFileAtomic(s.sidecarPath("old.txt"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy sidecar: %v", err)
	}

	vs, err := s.readVersionedSidecar("old.txt")
	if err != nil {
		t.Fatalf("legacy sidecar decode: %v", err)
	}
	if vs.ContentType != "text/plain" || vs.ContentLength != 5 || vs.ETag != "abc" {
		t.Errorf("legacy fields lost: %+v", vs.LegacyObjectMetadata)
	}
	if vs.Versioning != "" || vs.Versions != nil || vs.CurrentVersionID != "" {
		t.Errorf("new fields must decode to zero values: %+v", vs)
	}
}

func TestSidecarVersionStore_EmptyVersioningWritesKeepSidecarByteCompat(t *testing.T) {
	// A versionedSidecar with no versioning data must serialize without
	// ANY of the new fields (byte-identical to the legacy form).
	s, _ := verTestStore(t)
	legacyMeta := objectmodel.LegacyObjectMetadata{
		ContentType:   "text/plain",
		ContentLength: 5,
		ETag:          "abc",
		LastModified:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	vs := versionedSidecar{LegacyObjectMetadata: legacyMeta}
	if err := s.writeVersionedSidecar("compat.txt", vs); err != nil {
		t.Fatalf("writeVersionedSidecar: %v", err)
	}

	raw, err := os.ReadFile(s.sidecarPath("compat.txt")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	for _, banned := range []string{"Versioning", "Versions", "CurrentVersionID", "isDeleteMarker"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("byte-compat violation: sidecar contains %q:\n%s", banned, raw)
		}
	}
}

// ---------- versionStoreFor factory ----------

func TestVersionStoreFor_ModeDispatch(t *testing.T) {
	bp := t.TempDir()

	if _, ok := versionStoreFor(bp, nil, "sidecar").(sidecarVersionStore); !ok {
		t.Error("mode sidecar must yield sidecarVersionStore")
	}
	if _, ok := versionStoreFor(bp, nil, "").(sidecarVersionStore); !ok {
		t.Error("empty mode must default to sidecarVersionStore")
	}

	// "snapshots" is wired to zfsSnapshotVersionStore as of leaf 02 —
	// covered by TestVersionStoreFor_SnapshotsModeYieldsZfsSnapshotStore.
	// "both" is wired to the mergeVersionStore as of leaf 03 — covered by
	// TestVersionStoreFor_BothModeYieldsMergeStore below.

	invalid := versionStoreFor(bp, nil, "bogus")
	if _, err := invalid.State("b"); !errors.Is(err, errVersioningUnimplemented) {
		t.Errorf("bogus mode must fail loudly, got %v", err)
	}
}

// ---------- version ID format + concurrency ----------

func TestNewStateVersionID_FormatAndMonotonicUniqueness(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		id, err := newStateVersionID()
		if err != nil {
			t.Fatalf("newStateVersionID: %v", err)
		}
		if !versionIDRe.MatchString(id) {
			t.Fatalf("id %q does not match <unix-micros>-<8hex>", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q across repeated draws", id)
		}
		seen[id] = true
	}
}

func TestSidecarVersionStore_ConcurrentPutsOnOneKey(t *testing.T) {
	s, _ := verTestStore(t)

	const n = 8
	errs := make(chan error, n)
	for i := range n {
		go func() {
			body := fmt.Sprintf("body-%d", i)
			_, err := s.PutVersion("ver-bucket", "shared", strings.NewReader(body), int64(len(body)), fmt.Sprintf("e%d", i))
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent PutVersion: %v", err)
		}
	}

	list, err := s.List("ver-bucket", "shared")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != n {
		t.Fatalf("List len = %d, want %d", len(list), n)
	}
	latest := 0
	for _, e := range list {
		if e.IsLatest {
			latest++
		}
	}
	if latest != 1 {
		t.Errorf("IsLatest count = %d, want exactly 1", latest)
	}
}
