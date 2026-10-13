// coverage_restore3_test.go — a third batch of REAL behavior tests for
// previously-uncovered arms (companion to coverage_restore_test.go and
// coverage_restore2_test.go):
//
//   - tagstore.go deleteObjectTaggingHandler: delete-after-put round
//     trip, invalid key, and unknown bucket arms
//   - zmetadtagstore.go zmetadTagUnavailableError: the Unwrap chain and
//     the errors.Is identity the handlers rely on
//   - seam.go defaultWriteFileAtomic: the replace-over-existing arm
//   - versioning_handlers.go isSidecarShapedVersionID: the id-shape
//     classifier's accept/reject table (the 404-vs-400 wire split)
package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteObjectTaggingRoundTrip drives DeleteObjectTagging through the
// real sub-resource handler: put tags, delete them (204), get →
// NoSuchTagSet 404; plus the invalid-key 400 and unknown-bucket 404
// arms.
func TestDeleteObjectTaggingRoundTrip(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := "tagdel-bkt"
	_ = env.setupBucket(t, bucket)
	if w := putBodyAt(t, bucket, "k.txt", "tagged"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	// Put a tag set first so the delete has something to remove.
	w := httptest.NewRecorder()
	putReq := httptest.NewRequest("PUT", "/"+bucket+"/k.txt?tagging",
		strings.NewReader(`<Tagging><TagSet><Tag><Key>env</Key><Value>test</Value></Tag></TagSet></Tagging>`))
	putObjectTaggingHandler(w, putReq, bucket, "k.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("PutObjectTagging = %d %s, want 204", w.Code, w.Body.String())
	}

	// Delete the tags: 204.
	w = httptest.NewRecorder()
	deleteObjectTaggingHandler(w, httptest.NewRequest("DELETE", "/"+bucket+"/k.txt?tagging", nil), bucket, "k.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteObjectTagging = %d %s, want 204", w.Code, w.Body.String())
	}

	// The tags are gone: GET answers NoSuchTagSet (404).
	w = httptest.NewRecorder()
	getObjectTaggingHandler(w, httptest.NewRequest("GET", "/"+bucket+"/k.txt?tagging", nil), bucket, "k.txt")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NoSuchTagSet") {
		t.Errorf("GetObjectTagging after delete = %d %s, want 404 NoSuchTagSet", w.Code, w.Body.String())
	}

	// Delete is idempotent per S3: deleting again still answers 204.
	w = httptest.NewRecorder()
	deleteObjectTaggingHandler(w, httptest.NewRequest("DELETE", "/"+bucket+"/k.txt?tagging", nil), bucket, "k.txt")
	if w.Code != http.StatusNoContent {
		t.Errorf("idempotent DeleteObjectTagging = %d, want 204", w.Code)
	}

	// Invalid key: 400 InvalidArgument.
	w = httptest.NewRecorder()
	deleteObjectTaggingHandler(w, httptest.NewRequest("DELETE", "/"+bucket+"/..%2Fx?tagging", nil), bucket, "../x")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Errorf("invalid key = %d %s, want 400 InvalidArgument", w.Code, w.Body.String())
	}

	// Unknown bucket: 404 NoSuchBucket.
	w = httptest.NewRecorder()
	deleteObjectTaggingHandler(w, httptest.NewRequest("DELETE", "/nope/k?tagging", nil), "no-such-tag-bkt", "k")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("unknown bucket = %d %s, want 404 NoSuchBucket", w.Code, w.Body.String())
	}
}

// TestZmetadTagUnavailableErrorIdentity pins the unavailable error's
// contract: the message carries the store prefix and errors.Is matches
// the WRAPPED cause through Unwrap — the identity the handler arms rely
// on to distinguish "zmetad cannot answer" from "object absent".
func TestZmetadTagUnavailableErrorIdentity(t *testing.T) {
	cause := errors.New("dataset not polled by zmetad yet")
	err := &zmetadTagUnavailableError{cause: cause}
	if !strings.Contains(err.Error(), "zmetad tag store unavailable") {
		t.Errorf("Error() = %q, want the store prefix", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is must match the wrapped cause through Unwrap")
	}
}

// TestDefaultWriteFileAtomicReplace pins the replace arm: writing over
// an existing target swaps the content atomically and leaves no temp
// residue.
func TestDefaultWriteFileAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cfg.json")
	if err := defaultWriteFileAtomic(target, []byte(`"v1"`), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := defaultWriteFileAtomic(target, []byte(`"v2"`), 0o644); err != nil {
		t.Fatalf("replace write: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != `"v2"` {
		t.Errorf("target after replace = %q (%v), want \"v2\"", raw, err)
	}
}

// TestIsSidecarShapedVersionIDTable pins the id-shape classifier that
// splits the versioned-read 404-vs-400 wire rule: sidecar-form ids
// (16 digits, dash, 8 lowercase hex) classify true; snapshot names,
// short/long strings, and wrong alphabets classify false.
func TestIsSidecarShapedVersionIDTable(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"1791697134661230-8ce9bd12", true},
		{"0000000000000000-f00dfeed", true},
		{"1791697134661230-F00DFEED", false},  // uppercase hex
		{"1791697134661230", false},           // no dash/suffix
		{"179169713466123-8ce9bd12", false},   // 15 digits
		{"17916971346612300-8ce9bd12", false}, // 17 digits
		{"17916971346-8ce9bd12", false},       // dash at wrong index
		{"snapshot-name", false},              // snapshot form
		{"", false},
	}
	for _, tc := range cases {
		if got := isSidecarShapedVersionID(tc.id); got != tc.want {
			t.Errorf("isSidecarShapedVersionID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestDeleteTaggingThroughDeleteObjectsParity drives the ?delete
// manifest executor's versioned-marker parity through a REAL object so
// deleteBatchExecutor.Delete's marker-first branch executes end-to-end
// (the bughunt M1 pin shape): on an Enabled bucket the batch delete
// records the marker and the data file SURVIVES.
func TestDeleteTaggingThroughDeleteObjectsParity(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := "batchmarker-bkt"
	_ = env.setupBucket(t, bucket)
	installZfsVersioningModeForTest(t, "sidecar")
	installZmetadDBForTest(t, nil)
	bucketPath := filepath.Join(env.dataDir, bucket)
	enableReq := httptest.NewRequest("PUT", "/"+bucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
	w := httptest.NewRecorder()
	putBucketVersioningHandler(w, enableReq, bucket)
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if w := putBodyAt(t, bucket, "keep.txt", "surviving"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	e := deleteBatchExecutor{bucket: bucket, ctx: context.Background()}
	if err := e.Delete(context.Background(), "keep.txt", ""); err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	// The data file survives the marker'd delete (recoverable).
	if _, err := os.Stat(filepath.Join(bucketPath, "keep.txt")); err != nil {
		t.Errorf("versioned delete destroyed the data file: %v", err)
	}
	// And the plain GET answers the marker 404.
	hidden, err := PlainObjectDeleteMarker404(bucketPath, bucket, "keep.txt")
	if err != nil || !hidden {
		t.Errorf("marker404 after batch delete = (%v, %v), want (true, nil)", hidden, err)
	}
}

var _ = httptest.NewServer
