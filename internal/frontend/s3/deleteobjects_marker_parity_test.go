// deleteobjects_marker_parity_test.go — the DeleteObjects-vs-DELETE parity
// pin (bughunt 2026-10-06 M1).
//
// THE DEFECT: on a versioning-Enabled bucket, a single DELETE
// (deleteObjectHandler) writes a recoverable delete marker and SUPPRESSES the
// plain delete, so the bytes survive on disk and GET answers 404. The S3 XML
// batch surface (POST /{bucket}?delete -> deleteBatchExecutor.Delete) called
// deleteObjectCore DIRECTLY, skipping the marker step, so it HARD-DELETED the
// same key on the same bucket. Measured before the fix, on one Enabled bucket:
//
//	single DELETE -> "Delete marker written" status=204, 1 version recorded
//	?delete       -> "Successfully deleted object" status=200, 0 versions
//
// One bucket, one key, two delete semantics, one of them unrecoverable. The
// JSON ?batch surface (batchDelete) already had the marker step, which is why
// the S3 manifest surface was the outlier.
package s3

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDeleteObjectsRecordsDeleteMarkerOnVersionedBucket is the M1 pin: both
// delete surfaces must leave the same recoverable state.
func TestDeleteObjectsRecordsDeleteMarkerOnVersionedBucket(t *testing.T) {
	const enableXML = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`

	// --- arm 1: the single DELETE, as the reference behavior ---------------
	env := setupS3TestEnv(t)
	env.setupBucket(t, "parity-bucket")
	installZfsVersioningModeForTest(t, "sidecar")

	req := httptest.NewRequest("PUT", "/parity-bucket?versioning", strings.NewReader(enableXML))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	putBucketVersioningHandler(w, req, "parity-bucket")
	if w.Code != 200 {
		t.Fatalf("enable versioning = %d, want 200: %s", w.Code, w.Body.String())
	}
	_ = verPutBody(t, verEnv{testS3Env: env, bucket: "parity-bucket"}, "single.txt", "original bytes")

	state, err := versionStoreForBucket(getBucketPath("parity-bucket")).State("parity-bucket")
	if err != nil || state != versioningEnabled {
		t.Fatalf("arm setup: bucket state = %q err=%v, want Enabled", state, err)
	}

	// --- arm 2: the same store state through POST /{bucket}?delete ----------
	env2 := setupS3TestEnv(t)
	env2.setupBucket(t, "parity-bucket")
	installZfsVersioningModeForTest(t, "sidecar")
	req2 := httptest.NewRequest("PUT", "/parity-bucket?versioning", strings.NewReader(enableXML))
	req2.Header.Set("Content-Type", "application/xml")
	w2 := httptest.NewRecorder()
	putBucketVersioningHandler(w2, req2, "parity-bucket")
	if w2.Code != 200 {
		t.Fatalf("arm 2 enable versioning = %d: %s", w2.Code, w2.Body.String())
	}
	_ = verPutBody(t, verEnv{testS3Env: env2, bucket: "parity-bucket"}, "batch.txt", "original bytes")

	body := `<Delete><Object><Key>batch.txt</Key></Object></Delete>`
	preq := httptest.NewRequest("POST", "/parity-bucket?delete", strings.NewReader(body))
	pw := httptest.NewRecorder()
	deleteObjectsHandler(pw, preq, "parity-bucket")
	if pw.Code != 200 {
		t.Fatalf("?delete status = %d, want 200: %s", pw.Code, pw.Body.String())
	}
	if !strings.Contains(pw.Body.String(), "<Deleted>") {
		t.Fatalf("?delete did not report the key Deleted: %s", pw.Body.String())
	}

	// THE PIN: a delete marker must exist for the batch-deleted key.
	st := versionStoreForBucket(getBucketPath("parity-bucket"))
	versions, lerr := st.List("parity-bucket", "batch.txt")
	if lerr != nil {
		t.Fatalf("version List after ?delete = %v (a marker-less key means the plain delete ran)", lerr)
	}
	if len(versions) == 0 {
		t.Fatalf("M1 REGRESSION: ?delete recorded no version for a versioning-Enabled bucket; " +
			"the batch surface hard-deleted bytes the single DELETE would have preserved")
	}
	if !versions[0].IsDeleteMarker {
		t.Errorf("M1: the newest version entry is not a delete marker (IsDeleteMarker=%v); "+
			"the batch surface recorded the wrong kind of history", versions[0].IsDeleteMarker)
	}

	// And the plain GET must answer the marker 404, proving the object is
	// logically gone while its bytes survive.
	greq := httptest.NewRequest("GET", "/parity-bucket/batch.txt", nil)
	gw := httptest.NewRecorder()
	getObjectHandler(gw, greq, "parity-bucket", "batch.txt")
	if gw.Code != 404 {
		t.Errorf("GET after ?delete = %d, want 404 (the marker hides the key)", gw.Code)
	}
}

// TestDeleteObjectsUnversionedBucketStillHardDeletes is the OTHER arm: on an
// OFF/Suspended bucket the marker step reports no suppression and the plain
// delete must still run. This pins that the M1 fix did not turn every batch
// delete into a no-op.
func TestDeleteObjectsUnversionedBucketStillHardDeletes(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "plain-bucket")
	installZfsVersioningModeForTest(t, "sidecar")
	_ = verPutBody(t, verEnv{testS3Env: env, bucket: "plain-bucket"}, "gone.txt", "bytes")

	body := `<Delete><Object><Key>gone.txt</Key></Object></Delete>`
	preq := httptest.NewRequest("POST", "/plain-bucket?delete", strings.NewReader(body))
	pw := httptest.NewRecorder()
	deleteObjectsHandler(pw, preq, "plain-bucket")
	if pw.Code != 200 {
		t.Fatalf("?delete status = %d: %s", pw.Code, pw.Body.String())
	}

	// The object must be GONE (a plain delete ran), not merely hidden.
	breq := httptest.NewRequest("GET", "/plain-bucket/gone.txt", nil)
	bw := httptest.NewRecorder()
	getObjectHandler(bw, breq, "plain-bucket", "gone.txt")
	if bw.Code != 404 {
		t.Errorf("GET after ?delete on an unversioned bucket = %d, want 404 (the delete must be real)", bw.Code)
	}
	// And no delete marker was fabricated on an unversioned bucket.
	st := versionStoreForBucket(getBucketPath("plain-bucket"))
	if _, lerr := st.List("plain-bucket", "gone.txt"); lerr == nil {
		t.Errorf("an unversioned bucket recorded version history for a plain delete")
	}
}
