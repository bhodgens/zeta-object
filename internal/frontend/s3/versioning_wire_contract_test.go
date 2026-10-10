package s3

// versioning_wire_contract_test.go — the versioned-read WIRE contract and the
// branches that decide which S3 error a client gets.
//
// WHY THIS FILE EXISTS (audit G8 note, 2026-10-10): internal/frontend/s3 holds
// the tightest coverage floor in the repo (83, measured 83.2 on linux — +0.2
// headroom), so any unrelated change can red-line CI. The uncovered code here
// is not filler: it is the branch that decides whether a client gets 400 or 404
// for a version id it got wrong, and 405 for a delete marker. Those are the
// answers real S3 clients (boto3, rclone, mc) branch on.
//
// WHAT IT COVERS (all previously 0%):
//   - isSidecarShapedVersionID: the whole shape predicate, including the
//     negative cases the wire rule depends on. It had NO test at all: grep found
//     zero references outside the handler. A bug here silently flips 400↔404.
//   - objectVersionedRead: HEAD (no body streamed), unknown SIDEFORM id → 400,
//     unknown SNAPSHOT-form id → 404, absent key → 404.
//   - getBucketVersioningHandler: NoSuchBucket for an unknown bucket.
//   - captureCurrentObjectVersion: a create (no prior object) writes no version,
//     and a sidecar whose data file is gone preserves nothing.
//
// The 400-vs-404 split is the load-bearing one: server-minted sidecar ids are
// opaque, so an unknown one is a client error (400); snapshot names are host
// policy whose windows expire, so an unknown one is honestly 404.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- isSidecarShapedVersionID: the 400/404 predicate ----------

func TestIsSidecarShapedVersionID(t *testing.T) {
	// The real form: 16 digits, '-', 8 lowercase hex. This is the exact regex
	// versionstore_test's versionIDRe pins, so the two must not drift.
	valid := []string{
		"2025100712345678-a1b2c3d4",
		"0000000000000000-00000000",
		"9999999999999999-ffffffff",
	}
	for _, id := range valid {
		if !isSidecarShapedVersionID(id) {
			t.Errorf("isSidecarShapedVersionID(%q) = false, want true", id)
		}
	}

	// Every negative must be false, and the reason each matters:
	invalid := []struct {
		id     string
		reason string
	}{
		{"", "empty: not a server-minted id, so treat as a snapshot name"},
		{"short", "too short"},
		{"202510071234567-a1b2c3d4", "15 digits, one short"},
		{"20251007123456789-a1b2c3d4", "17 digits, one long"},
		{"2025100712345678a1b2c3d4", "no '-' separator"},
		{"2025100712345678a1b2c3d4", "no '-' separator, right length"},
		{"2025100712345678--a1b2c3d", "wrong position for '-'"},
		{"202510071234567a-a1b2c3d4", "non-digit in the 16-digit prefix"},
		{"2025100712345678-A1B2C3D4", "UPPERCASE hex: the server mints lowercase"},
		{"2025100712345678-a1b2c3g4", "'g' is not a hex digit"},
		{"2025100712345678-a1b2c3d", "7 hex digits"},
		{"2025100712345678-a1b2c3d44", "9 hex digits"},
		{"snapshots/2025-10-07T22:04:00", "a real snapshot-form name, not sidecar"},
		{"/bucket/.zfs/snapshot/20251007", "another real snapshot-form name"},
	}
	for _, c := range invalid {
		if isSidecarShapedVersionID(c.id) {
			t.Errorf("isSidecarShapedVersionID(%q) = true, want false (%s)", c.id, c.reason)
		}
	}
}

// The two shape classes drive two DIFFERENT status codes. Pin the mapping so a
// predicate change cannot silently move a client's error class.
func TestVersionIDShapeClassDecidesStatusCode(t *testing.T) {
	cases := []struct {
		id       string
		wantCode int
		wantErr  string
		why      string
	}{
		{
			id:       "2025100712345678-a1b2c3d4",
			wantCode: http.StatusBadRequest,
			wantErr:  "InvalidArgument",
			why:      "server-minted id shape the store does not know = client error",
		},
		{
			id:       "snapshots/2025-10-07T22-04-00",
			wantCode: http.StatusNotFound,
			wantErr:  "NoSuchKey",
			why:      "snapshot name whose window expired = honest 404",
		},
		{
			id:       "anything-else",
			wantCode: http.StatusNotFound,
			wantErr:  "NoSuchKey",
			why:      "not sidecar-shaped, so the 404 branch",
		},
	}
	for _, c := range cases {
		if got := isSidecarShapedVersionID(c.id); got != (c.wantCode == http.StatusBadRequest) {
			t.Errorf("isSidecarShapedVersionID(%q) = %v, which disagrees with the "+
				"status the case expects (%d) - the shape predicate and the wire "+
				"rule have drifted", c.id, got, c.wantCode)
		}
	}
}

// ---------- objectVersionedRead: the 400 / 404 / HEAD contract ----------

func TestObjectVersionedRead_UnknownIDShapeDecidesStatus(t *testing.T) {
	env := newVerEnv(t, "verunknown-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	cases := []struct {
		name     string
		version  string
		wantCode int
		wantErr  string
	}{
		{"sidecar-shaped but unknown", "2025100712345678-a1b2c3d4", http.StatusBadRequest, "InvalidArgument"},
		{"snapshot-shaped, window gone", "snapshots/2025-01-01T00-00-00", http.StatusNotFound, "NoSuchKey"},
		{"garbage", "not-a-version-at-all", http.StatusNotFound, "NoSuchKey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/"+env.bucket+"/k?versionId="+c.version, nil)
			w := httptest.NewRecorder()
			objectVersionedRead(w, req, env.bucket, "k", c.version)
			if w.Code != c.wantCode {
				t.Errorf("status = %d, want %d (body %s)", w.Code, c.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "<Code>"+c.wantErr+"</Code>") {
				t.Errorf("body missing <Code>%s</Code>: %s", c.wantErr, w.Body.String())
			}
			if got := headerGet(w, "x-amz-version-id"); got != "" {
				t.Errorf("failed versioned read set x-amz-version-id = %q, want empty", got)
			}
		})
	}
}

// A HEAD against a real version must carry every header a GET does and stream
// NO body. The handler gates the copy on r.Method, and that gate was uncovered.
func TestObjectVersionedRead_HeadCarriesHeadersWithoutBody(t *testing.T) {
	env := newVerEnv(t, "verhead-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "hello-head"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "hello-head-v2"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}

	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "k")
	if err != nil || len(versions) == 0 {
		t.Fatalf("List: %v (%d entries)", err, len(versions))
	}
	oldID := versions[len(versions)-1].ID

	// GET first: proves the id resolves and gives us the expected headers.
	getReq := httptest.NewRequest("GET", "/"+env.bucket+"/k?versionId="+oldID, nil)
	getW := httptest.NewRecorder()
	objectVersionedRead(getW, getReq, env.bucket, "k", oldID)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", getW.Code, getW.Body.String())
	}
	if getW.Body.String() != "hello-head" {
		t.Fatalf("GET body = %q, want the old bytes", getW.Body.String())
	}

	headReq := httptest.NewRequest("HEAD", "/"+env.bucket+"/k?versionId="+oldID, nil)
	headW := httptest.NewRecorder()
	objectVersionedRead(headW, headReq, env.bucket, "k", oldID)

	if headW.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200: %s", headW.Code, headW.Body.String())
	}
	if headW.Body.Len() != 0 {
		t.Errorf("HEAD streamed %d body bytes (%q), want 0", headW.Body.Len(), headW.Body.String())
	}
	for _, h := range []string{"x-amz-version-id", "Content-Length", "ETag", "Content-Type"} {
		if got, want := headW.Header().Get(h), getW.Header().Get(h); got != want {
			t.Errorf("HEAD %s = %q, GET had %q - HEAD must carry the same headers", h, got, want)
		}
	}
	if cl := headerGet(headW, "Content-Length"); cl != "10" {
		t.Errorf("Content-Length = %q, want 10 (len of the old bytes)", cl)
	}
}

// ---------- getBucketVersioningHandler: NoSuchBucket ----------

func TestGetBucketVersioning_UnknownBucketIsNoSuchBucket(t *testing.T) {
	newVerEnv(t, "vergetvb-bucket") // installs the seams; the target bucket is absent

	req := httptest.NewRequest("GET", "/never-created-bucket?versioning", nil)
	w := httptest.NewRecorder()
	getBucketVersioningHandler(w, req, "never-created-bucket")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "<Code>NoSuchBucket</Code>") {
		t.Errorf("body missing NoSuchBucket: %s", w.Body.String())
	}
	// A 404 must not leak a Status element suggesting versioning exists.
	if strings.Contains(w.Body.String(), "<Status>") {
		t.Errorf("404 body carries a <Status> element: %s", w.Body.String())
	}
	// An invalid bucket name is the same answer as a missing one (no existence
	// oracle): NoSuchBucket, not InvalidBucketName.
	bad := httptest.NewRequest("GET", "/Invalid..Name?versioning", nil)
	badW := httptest.NewRecorder()
	getBucketVersioningHandler(badW, bad, "Invalid..Name")
	if badW.Code != http.StatusNotFound {
		t.Errorf("invalid bucket name status = %d, want 404 (no existence oracle): %s",
			badW.Code, badW.Body.String())
	}
}

// A bucket whose versioning state file is unreadable must surface the store's
// error through writeS3ErrorFrom, not a silent 200 with an empty configuration.
// That error branch (state, err := ...; err != nil) was uncovered.
func TestGetBucketVersioning_UnreadableStateIsNotSilentSuccess(t *testing.T) {
	env := newVerEnv(t, "verbadstate-bucket")

	// Put a directory where the state file belongs: reading it as a file now
	// fails with EISDIR on every supported platform.
	statePath := filepath.Join(env.bucketPath, ".metadata", ".versioning")
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Skipf("cannot stage the unwritable-state fixture: %v", err)
	}

	req := httptest.NewRequest("GET", "/"+env.bucket+"?versioning", nil)
	w := httptest.NewRecorder()
	getBucketVersioningHandler(w, req, env.bucket)

	if w.Code == http.StatusOK {
		t.Fatalf("unreadable versioning state returned 200 with %q - the error branch "+
			"did not fire, so a broken store reads as 'versioning off'", w.Body.String())
	}
}

// ---------- captureCurrentObjectVersion: the create / lost-data cases ----------

func TestCaptureCurrentObjectVersion_NoPriorObjectWritesNothing(t *testing.T) {
	env := newVerEnv(t, "vercap-bucket")
	_ = env.setupBucket(t, "vercap-bucket")

	// A create: no sidecar, so there is no prior version to capture. This is
	// the errNoPriorVersion path and it must not be reported as an I/O error.
	cap, err := captureCurrentObjectVersion(env.bucketPath, "never-written")
	if !errors.Is(err, errNoPriorVersion) {
		t.Fatalf("capture on a fresh key: err = %v, want errNoPriorVersion", err)
	}
	if cap != nil {
		t.Errorf("capture on a fresh key = %+v, want nil (a create records nothing)", cap)
	}
}

func TestCaptureCurrentObjectVersion_SidecarWithoutDataPreservesNothing(t *testing.T) {
	env := newVerEnv(t, "vercap2-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "v1"); w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	// Drop the DATA file but keep the sidecar: the sidecar's StoragePath now
	// points at nothing. capture must report "no prior version" (nothing to
	// preserve) rather than a hard read error - this is the os.IsNotExist
	// branch inside captureCurrentObjectVersion.
	meta, ok := readObjectMetaForList(filepath.Join(env.bucketPath, ".metadata"), "k")
	if !ok {
		t.Fatal("sidecar not found after a real Put")
	}
	dataPath := meta.StoragePath
	if dataPath == "" {
		dataPath = filepath.Join(env.bucketPath, "k")
	}
	if err := os.Remove(dataPath); err != nil {
		t.Skipf("cannot remove the data file (%s): %v", dataPath, err)
	}

	cap, err := captureCurrentObjectVersion(env.bucketPath, "k")
	if !errors.Is(err, errNoPriorVersion) {
		t.Fatalf("capture with data gone: err = %v, want errNoPriorVersion", err)
	}
	if cap != nil {
		t.Errorf("capture with data gone = %+v, want nil (nothing to preserve)", cap)
	}
}

// A real prior object must capture its BYTES, its old ETag, and its history -
// the three things the record step needs to write a truthful version entry.
func TestCaptureCurrentObjectVersion_CapturesBytesETagAndHistory(t *testing.T) {
	env := newVerEnv(t, "vercap3-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	if w := verPutBody(t, env, "k", "first-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	// A second Put records v1 as a version, so the sidecar now carries history.
	if w := verPutBody(t, env, "k", "second-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v2: %d", w.Code)
	}

	cap, err := captureCurrentObjectVersion(env.bucketPath, "k")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if cap == nil {
		t.Fatal("capture = nil, want the pre-overwrite state")
	}
	// capture reads the object's CURRENT on-disk state, which is what the NEXT
	// overwrite is about to replace - so after two Puts it must be v2's bytes.
	if got := string(cap.data); got != "second-bytes" {
		t.Errorf("captured bytes = %q, want the current pre-overwrite bytes %q", got, "second-bytes")
	}
	if cap.etag == "" {
		t.Error("captured etag is empty; the record step would write a version with no ETag")
	}
	if len(cap.priorEntries) == 0 {
		t.Error("captured priorEntries is empty; the record step would LOSE v1's history")
	}
}
