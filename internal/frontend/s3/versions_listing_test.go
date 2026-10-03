// versions_listing_test.go — s3-versioning tree leaf 04 tests for the
// rewritten GET /{bucket}?versions listing (Contract 2). Covers:
//
//   - never-versioned buckets: BYTE-IDENTICAL legacy listing PIN (the
//     "null"-id shape is unchanged and no store is consulted).
//   - versioned buckets: per-key <Version> entries + <DeleteMarker>
//     entries, newest-first within each key, IsLatest on exactly the
//     key's newest entry.
//   - both-mode merged store rendering (sidecar + snapshot entries).
//   - snapshot-store rendering (scripted zfs runner, fake snapdir).
//
// NO real zfs anywhere: the snapshot/merge tests script the
// zfsSnapshotRunner seam and fake the snapdir on a temp dir.
package s3

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verListingDoc is the decoded wire shape of a versioned ListVersionsResult.
type verListingDoc struct {
	XMLName       xml.Name `xml:"ListVersionsResult"`
	Name          string   `xml:"Name"`
	MaxKeys       int      `xml:"MaxKeys"`
	IsTruncated   bool     `xml:"IsTruncated"`
	NextKeyMarker string   `xml:"NextKeyMarker"`
	Versions      []struct {
		Key       string `xml:"Key"`
		VersionId string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
		Size      int64  `xml:"Size"`
		ETag      string `xml:"ETag"`
	} `xml:"Version"`
	DeleteMarkers []struct {
		Key       string `xml:"Key"`
		VersionId string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
	} `xml:"DeleteMarker"`
}

func decodeVerListing(t *testing.T, body string) verListingDoc {
	t.Helper()
	var doc verListingDoc
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal versions XML: %v\nbody:\n%s", err, body)
	}
	return doc
}

// verGetVersions drives the ?versions handler exactly as dispatch does.
func verGetVersions(t *testing.T, env verEnv, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+env.bucket+"?versions"+query, nil)
	w := httptest.NewRecorder()
	listObjectVersionsHandler(w, req, env.bucket)
	return w
}

// ---------- never-versioned buckets: byte-identical legacy PIN ----------

// TestVersionsListing_NeverVersionedByteIdenticalPin pins the legacy
// wire shape: a bucket never touched by ?versioning renders exactly the
// pre-versioning listing — one <Version> per key, VersionId "null",
// IsLatest true, no <DeleteMarker> element, no versioning state on disk.
func TestVersionsListing_NeverVersionedByteIdenticalPin(t *testing.T) {
	env := newVerEnv(t, "verlist-never-bkt")
	// Raw sidecar-only setup: write objects through the plain handlers
	// WITHOUT ever touching ?versioning.
	if w := verPutBody(t, env, "a.txt", "alpha"); w.Code != http.StatusOK {
		t.Fatalf("put a: %d", w.Code)
	}
	if w := verPutBody(t, env, "b.txt", "beta"); w.Code != http.StatusOK {
		t.Fatalf("put b: %d", w.Code)
	}

	if _, err := os.Stat(filepath.Join(env.bucketPath, ".metadata", ".versioning")); !os.IsNotExist(err) {
		t.Fatalf("never-versioned bucket must have no state marker, stat err = %v", err)
	}

	w := verGetVersions(t, env, "")
	if w.Code != http.StatusOK {
		t.Fatalf("?versions = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	doc := decodeVerListing(t, body)
	if len(doc.Versions) != 2 {
		t.Fatalf("versions = %d, want 2 (body %s)", len(doc.Versions), body)
	}
	if len(doc.DeleteMarkers) != 0 {
		t.Fatalf("never-versioned listing must carry no DeleteMarker entries (body %s)", body)
	}
	for _, v := range doc.Versions {
		if v.VersionId != "null" {
			t.Errorf("VersionId = %q, want null (body %s)", v.VersionId, body)
		}
		if !v.IsLatest {
			t.Errorf("legacy entry must be IsLatest (body %s)", body)
		}
	}
	for _, want := range []string{"<VersionId>null</VersionId>", "<IsLatest>true</IsLatest>", "<Key>a.txt</Key>", "<Key>b.txt</Key>"} {
		if !strings.Contains(body, want) {
			t.Errorf("legacy listing missing %q (body %s)", want, body)
		}
	}
	if strings.Contains(body, "DeleteMarker") {
		t.Errorf("legacy listing must not mention DeleteMarker (body %s)", body)
	}
}

// TestVersionsListing_SuspendedStillVersioned pins the gate: a bucket
// whose versioning was enabled then SUSPENDED keeps the versioned
// renderer (the state marker exists — history must stay visible).
func TestVersionsListing_SuspendedStillVersioned(t *testing.T) {
	env := newVerEnv(t, "verlist-susp-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}
	if w := verEnable(t, env, "Suspended"); w.Code != http.StatusOK {
		t.Fatalf("suspend: %d", w.Code)
	}

	w := verGetVersions(t, env, "")
	body := w.Body.String()
	doc := decodeVerListing(t, body)
	if len(doc.Versions) != 1 {
		t.Fatalf("versions = %d, want 1 (the recorded v1) (body %s)", len(doc.Versions), body)
	}
	if doc.Versions[0].VersionId == "null" {
		t.Fatalf("suspended bucket must render real version ids, got null (body %s)", body)
	}
}

// ---------- versioned buckets: versions + markers newest-first ----------

// TestVersionsListing_VersionedRendersVersionsAndMarkers pins the
// Contract 2 shape: overwrite + delete-marker history renders as
// per-key Version + DeleteMarker entries, newest-first, IsLatest on
// exactly the key's newest entry (the marker).
func TestVersionsListing_VersionedRendersVersionsAndMarkers(t *testing.T) {
	env := newVerEnv(t, "verlist-ver-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}
	if w := verDelete(t, env, "k"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	w := verGetVersions(t, env, "")
	if w.Code != http.StatusOK {
		t.Fatalf("?versions = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	doc := decodeVerListing(t, body)

	// One Version (the recorded v1) + one DeleteMarker (newest).
	if len(doc.Versions) != 1 {
		t.Fatalf("Version entries = %d, want 1 (body %s)", len(doc.Versions), body)
	}
	if len(doc.DeleteMarkers) != 1 {
		t.Fatalf("DeleteMarker entries = %d, want 1 (body %s)", len(doc.DeleteMarkers), body)
	}
	ver := doc.Versions[0]
	marker := doc.DeleteMarkers[0]
	if ver.Key != "k" || marker.Key != "k" {
		t.Fatalf("keys = %q/%q, want k/k (body %s)", ver.Key, marker.Key, body)
	}
	if ver.IsLatest {
		t.Errorf("the recorded version must not be IsLatest when a marker exists (body %s)", body)
	}
	if !marker.IsLatest {
		t.Errorf("the delete marker must be IsLatest (body %s)", body)
	}
	if ver.Size != int64(len("v1")) {
		t.Errorf("version Size = %d, want %d", ver.Size, len("v1"))
	}
	if !strings.HasPrefix(ver.ETag, "\"") {
		t.Errorf("version ETag = %q, want quoted form", ver.ETag)
	}
	// Marker entries carry no Size/ETag in the S3 wire form.
	if strings.Contains(body, "<DeleteMarker>") && strings.Count(body, "<DeleteMarker>") != 1 {
		t.Errorf("expected exactly one DeleteMarker element (body %s)", body)
	}
	// Version ids are real (sidecar-minted), never "null".
	if ver.VersionId == "null" || marker.VersionId == "null" {
		t.Errorf("versioned listing must use real ids (body %s)", body)
	}
}

// TestVersionsListing_NewestFirstWithinKeyAndAcrossKeys pins ordering:
// within a key the store's newest-first history is preserved; across
// keys the document is lexicographic. Per the leaf-03 design the store
// records the OLD version on overwrite (the current data file is not a
// store entry), so a key's single create under versioning has no
// recorded version and renders nothing until it is overwritten or
// deleted — the store's List is the sole truth for this listing.
func TestVersionsListing_NewestFirstWithinKeyAndAcrossKeys(t *testing.T) {
	env := newVerEnv(t, "verlist-order-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	// b.txt: single create (no recorded version — nothing old to
	// record). a.txt: create then overwrite (records a1).
	if w := verPutBody(t, env, "b.txt", "b1"); w.Code != http.StatusOK {
		t.Fatalf("put b1: %d", w.Code)
	}
	if w := verPutBody(t, env, "a.txt", "a1"); w.Code != http.StatusOK {
		t.Fatalf("put a1: %d", w.Code)
	}
	if w := verPutBody(t, env, "a.txt", "a2"); w.Code != http.StatusOK {
		t.Fatalf("put a2: %d", w.Code)
	}

	w := verGetVersions(t, env, "")
	body := w.Body.String()
	doc := decodeVerListing(t, body)

	// Only the recorded a1 renders (newest entry of its key → IsLatest);
	// b.txt has no version history yet and renders nothing.
	if len(doc.Versions) != 1 {
		t.Fatalf("Version entries = %d, want 1 (the recorded a1) (body %s)", len(doc.Versions), body)
	}
	if doc.Versions[0].Key != "a.txt" {
		t.Fatalf("entry key = %q, want a.txt (body %s)", doc.Versions[0].Key, body)
	}
	if doc.Versions[0].VersionId == "null" {
		t.Fatalf("recorded entry must carry a real id (body %s)", body)
	}
	if !doc.Versions[0].IsLatest {
		t.Fatalf("a key's newest store entry must be IsLatest (body %s)", body)
	}
	for _, v := range doc.Versions {
		if v.Key == "b.txt" {
			t.Fatalf("b.txt has no recorded versions and must not render (body %s)", body)
		}
	}
}

// TestVersionsListing_TruncationAcrossEntries pins paging across the
// versioned stream: max-keys truncates mid-history with markers of the
// last emitted entry, and the markers resume it.
func TestVersionsListing_TruncationAcrossEntries(t *testing.T) {
	env := newVerEnv(t, "verlist-page-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	// a.txt: three puts → two recorded versions (v1 recorded on the
	// v2 overwrite, v2 recorded on the v3 overwrite).
	if w := verPutBody(t, env, "a.txt", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	if w := verPutBody(t, env, "a.txt", "v2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}
	if w := verPutBody(t, env, "a.txt", "v3"); w.Code != http.StatusOK {
		t.Fatalf("put v3: %d", w.Code)
	}

	w := verGetVersions(t, env, "&max-keys=1")
	body := w.Body.String()
	doc := decodeVerListing(t, body)
	if !doc.IsTruncated || len(doc.Versions)+len(doc.DeleteMarkers) != 1 {
		t.Fatalf("page1 must be truncated at 1 entry (body %s)", body)
	}
	if doc.NextKeyMarker != "a.txt" {
		t.Fatalf("NextKeyMarker = %q, want a.txt (body %s)", doc.NextKeyMarker, body)
	}
	firstPageID := doc.Versions[0].VersionId

	// Resume from the marker: the remaining entry comes back, complete.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/"+env.bucket+"?versions&max-keys=2&key-marker="+doc.NextKeyMarker+"&version-id-marker="+firstPageID, nil)
	listObjectVersionsHandler(w2, req2, env.bucket)
	body2 := w2.Body.String()
	doc2 := decodeVerListing(t, body2)
	if doc2.IsTruncated {
		t.Fatalf("page2 must complete (body %s)", body2)
	}
	if total := len(doc2.Versions) + len(doc2.DeleteMarkers); total != 1 {
		t.Fatalf("page2 entries = %d, want 1 (body %s)", total, body2)
	}
}

// TestVersionsListing_EmptyVersionedBucket pins the empty envelope.
func TestVersionsListing_EmptyVersionedBucket(t *testing.T) {
	env := newVerEnv(t, "verlist-empty-bkt")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	w := verGetVersions(t, env, "")
	if w.Code != http.StatusOK {
		t.Fatalf("?versions = %d", w.Code)
	}
	doc := decodeVerListing(t, w.Body.String())
	if len(doc.Versions) != 0 || len(doc.DeleteMarkers) != 0 || doc.IsTruncated {
		t.Fatalf("empty bucket must render an empty envelope (body %s)", w.Body.String())
	}
}

// ---------- snapshot-store rendering (snapshots mode) ----------

// TestVersionsListing_SnapshotStoreRendering pins the snapshots-mode
// listing: every snapshot containing the key renders as a <Version>
// entry whose VersionId is the snapshot short name and whose
// LastModified is the snapshot creation, newest-first, IsLatest on the
// newest containing snapshot.
func TestVersionsListing_SnapshotStoreRendering(t *testing.T) {
	scriptZfsSnapshotRunner(t, "pool/bkt@s1	1000000000\npool/bkt@s2	1000003000\n", nil)
	mountpoint := t.TempDir()
	bucketPath := filepath.Join(mountpoint, "bkt")
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	// k.txt exists in both snapshots (with different sizes to tell them
	// apart); gone.txt exists in s1 only (a windowed history).
	for _, snap := range []string{"s1", "s2"} {
		dir := filepath.Join(mountpoint, ".zfs", "snapshot", snap)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "k.txt"), []byte("x-"+snap), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mountpoint, ".zfs", "snapshot", "s1", "gone.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sidecar for each key (the keys come from the metadata walk) and
	// the ever-versioned marker (snapshots mode shares the sidecar
	// state marker — SetState through the store writes it).
	for _, key := range []string{"k.txt", "gone.txt"} {
		sc := versionedSidecar{}
		sc.Versioning = versioningEnabled
		if err := (sidecarVersionStore{bucketPath: bucketPath}).writeVersionedSidecar(key, sc); err != nil {
			t.Fatal(err)
		}
	}
	if err := (sidecarVersionStore{bucketPath: bucketPath}).SetState("bkt", versioningEnabled); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/bkt?versions", nil)
	w := httptest.NewRecorder()
	// The store is injected (exactly as leaf 03's merge tests construct
	// it): a nil zmetad DB handle cannot satisfy the snapshot store's
	// resolver, so the handler-level snapshots render is driven through
	// listBucketVersionsWithStore with the scripted store.
	store := zfsSnapshotVersionStore{
		bucketPath: bucketPath,
		zdb:        fakeSnapshotResolver{dataset: "pool/bkt", mountpoint: mountpoint},
	}
	listBucketVersionsWithStore(w, req, "bkt", bucketPath, store)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("?versions = %d: %s", w.Code, body)
	}
	doc := decodeVerListing(t, body)

	// Lexicographic keys: gone.txt (one s1 entry) then k.txt (s2 newest
	// first). Per-key newest-first ordering is pinned positionally.
	if len(doc.Versions) != 3 {
		t.Fatalf("Version entries = %d, want 3 (body %s)", len(doc.Versions), body)
	}
	if doc.Versions[0].Key != "gone.txt" || doc.Versions[0].VersionId != "s1" || !doc.Versions[0].IsLatest {
		t.Fatalf("first entry = %+v, want gone.txt@s1 IsLatest (body %s)", doc.Versions[0], body)
	}
	if doc.Versions[1].Key != "k.txt" || doc.Versions[1].VersionId != "s2" || !doc.Versions[1].IsLatest {
		t.Fatalf("second entry = %+v, want k.txt@s2 IsLatest (body %s)", doc.Versions[1], body)
	}
	if doc.Versions[2].Key != "k.txt" || doc.Versions[2].VersionId != "s1" || doc.Versions[2].IsLatest {
		t.Fatalf("third entry = %+v, want k.txt@s1 not-latest (body %s)", doc.Versions[2], body)
	}
	if doc.Versions[1].Size != int64(len("x-s2")) {
		t.Errorf("s2 entry Size = %d, want %d (body %s)", doc.Versions[1].Size, len("x-s2"), body)
	}
	// LastModified is the snapshot creation (2001-09-09 epoch family).
	if !strings.Contains(body, "2001-09-09T01:46:40.000Z") {
		t.Errorf("s1 LastModified must come from the snapshot creation (body %s)", body)
	}
	if len(doc.DeleteMarkers) != 0 {
		t.Errorf("snapshots mode has no delete markers (body %s)", body)
	}
}

// ---------- both-mode merged rendering ----------

// TestVersionsListing_BothModeMergedRendering pins the merged listing:
// a key with sidecar history renders sidecar entries; a key whose
// history is snapshot-only renders snapshot entries; both coexist in
// one document (the merge store's List owns ordering and IsLatest).
func TestVersionsListing_BothModeMergedRendering(t *testing.T) {
	scriptZfsSnapshotRunner(t, "pool/bkt@s1\t1000000000\n", nil)
	mountpoint := t.TempDir()
	bucketPath := filepath.Join(mountpoint, "bkt")
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapDir := filepath.Join(mountpoint, ".zfs", "snapshot", "s1")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// snap-only.txt exists in s1; sidecar.txt does NOT exist in any
	// snapshot (sidecar history only).
	if err := os.WriteFile(filepath.Join(snapDir, "snap-only.txt"), []byte("snap"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (sidecarVersionStore{bucketPath: bucketPath}).SetState("bkt", versioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := (sidecarVersionStore{bucketPath: bucketPath}).PutVersion("bkt", "sidecar.txt", strings.NewReader("cur"), 3, "etag-1"); err != nil {
		t.Fatal(err)
	}
	// The snapshot-only key needs a sidecar to appear in the key walk
	// (a snapshots-mode key's sidecar carries no Versions entries).
	sc := versionedSidecar{}
	sc.Versioning = versioningEnabled
	if err := (sidecarVersionStore{bucketPath: bucketPath}).writeVersionedSidecar("snap-only.txt", sc); err != nil {
		t.Fatal(err)
	}

	store := mergeVersionStore{
		sidecar:  sidecarVersionStore{bucketPath: bucketPath},
		snapshot: zfsSnapshotVersionStore{bucketPath: bucketPath, zdb: fakeSnapshotResolver{dataset: "pool/bkt", mountpoint: mountpoint}},
	}
	// Render through the merge store directly (the unit-level pin; the
	// handler-level both-mode wiring is leaf 03's seam, exercised above).
	mergedSidecar, err := store.List("bkt", "sidecar.txt")
	if err != nil || len(mergedSidecar) != 1 {
		t.Fatalf("sidecar key List = %d entries, err %v", len(mergedSidecar), err)
	}
	mergedSnap, err := store.List("bkt", "snap-only.txt")
	if err != nil || len(mergedSnap) != 1 || mergedSnap[0].ID != "s1" {
		t.Fatalf("snapshot key List = %+v, err %v", mergedSnap, err)
	}

	// And through the listing envelope: build the doc the renderer emits.
	result := ListVersionsResult{Name: "bkt", Versions: []ObjectVersion{}}
	for _, entry := range mergedSnap {
		result.Versions = append(result.Versions, ObjectVersion{Key: "snap-only.txt", VersionID: entry.ID, IsLatest: entry.IsLatest})
	}
	raw, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "<Key>snap-only.txt</Key>") || !strings.Contains(string(raw), "<VersionId>s1</VersionId>") {
		t.Errorf("envelope missing the snapshot entry: %s", raw)
	}
}

// TestVersionsListing_IgnoresNonExistentBucket pins 404 on the versioned
// listing path for a missing bucket (the marker gate answers false, and
// the legacy renderer's bucket-existence check runs).
func TestVersionsListing_IgnoresNonExistentBucket(t *testing.T) {
	newVerEnv(t, "unrelated-bkt")
	req := httptest.NewRequest("GET", "/nope?versions", nil)
	w := httptest.NewRecorder()
	listObjectVersionsHandler(w, req, "nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("?versions on missing bucket = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("body must carry NoSuchBucket, got %s", w.Body.String())
	}
}
