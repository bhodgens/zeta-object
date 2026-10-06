package webdav

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// rangeBody reads the full response body as a string (test helper).
func rangeBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}

func TestGET_Range(t *testing.T) {
	content := "0123456789"
	tests := []struct {
		name string

		method        string
		rng           string
		inm           string
		collection    bool
		wantStatus    int
		wantCL        string // "" = header must be absent
		wantCR        string // "" = header must be absent
		wantAR        string // Accept-Ranges; "bytes" on every object 200/206 (s3 parity)
		wantBody      string
		wantMultipart bool     // multi-span: parse boundary, count parts
		wantParts     []string // per-part Content-Range lines (multipart)
	}{
		{
			name:       "single span 0-4",
			rng:        "bytes=0-4",
			wantStatus: 206,
			wantCL:     "5",
			wantCR:     "bytes 0-4/10",
			wantAR:     "bytes",
			wantBody:   "01234",
		},
		{
			name:       "suffix -3",
			rng:        "bytes=-3",
			wantStatus: 206,
			wantCL:     "3",
			wantCR:     "bytes 7-9/10",
			wantAR:     "bytes",
			wantBody:   "789",
		},
		{
			name:       "open-ended 8-",
			rng:        "bytes=8-",
			wantStatus: 206,
			wantCL:     "2",
			wantCR:     "bytes 8-9/10",
			wantAR:     "bytes",
			wantBody:   "89",
		},
		{
			name:       "malformed spec ignored",
			rng:        "bytes=abc",
			wantStatus: 200,
			wantCL:     "10",
			wantAR:     "bytes",
			wantBody:   content,
		},
		{
			name:       "unsatisfiable 416",
			rng:        "bytes=50-",
			wantStatus: 416,
			wantCR:     "bytes */10",
			wantAR:     "bytes",
			wantBody:   "",
		},
		{
			name:          "multi-span multipart",
			rng:           "bytes=0-1,5-6",
			wantStatus:    206,
			wantAR:        "bytes",
			wantMultipart: true,
			wantParts:     []string{"Content-Range: bytes 0-1/10", "Content-Range: bytes 5-6/10"},
		},
		{
			name:       "HEAD with range",
			method:     "HEAD",
			rng:        "bytes=0-4",
			wantStatus: 206,
			wantCL:     "5",
			wantCR:     "bytes 0-4/10",
			wantAR:     "bytes",
			wantBody:   "",
		},
		{
			// The 304 branch returns before the range-advertising header
			// is set (s3 parity: checkObjectPreconditions runs before
			// Accept-Ranges), so it carries neither.
			name:       "If-None-Match wins over Range",
			rng:        "bytes=0-4",
			inm:        `"abc"`,
			wantStatus: 304,
			wantBody:   "",
		},
		{
			// A collection GET is not an object body: no range support is
			// advertised for the virtual collection listing.
			name:       "Range on collection ignored",
			rng:        "bytes=0-4",
			collection: true,
			wantStatus: 200,
			wantBody:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, be := newTestFrontend(Config{})
			be.seed("photos", "a.txt", []byte(content), func(o *objectmodel.Object) {
				o.ETag = `"abc"`
			})
			be.seed("photos", "dir/", []byte(""), func(o *objectmodel.Object) {})
			path := "/photos/a.txt"
			if tt.collection {
				// Collections resolve with the trailing slash (pinned
				// GET decision: a prefix without the slash is a 404).
				path = "/photos/dir/"
			}
			method := tt.method
			if method == "" {
				method = "GET"
			}
			req := httptest.NewRequest(method, path, nil)
			if tt.rng != "" {
				req.Header.Set("Range", tt.rng)
			}
			if tt.inm != "" {
				req.Header.Set("If-None-Match", tt.inm)
			}
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Range"); got != tt.wantCR {
				t.Fatalf("Content-Range = %q, want %q", got, tt.wantCR)
			}
			// Byte-range support must be discoverable on every object
			// 200/206 (s3 parity: object_handlers.go sets it once for
			// both outcomes). Absent on 304 and on a collection GET.
			if got := rec.Header().Get("Accept-Ranges"); got != tt.wantAR {
				t.Fatalf("Accept-Ranges = %q, want %q", got, tt.wantAR)
			}
			if tt.collection {
				tt.wantCL = "0" // collection always answers Content-Length: 0
			}
			if got := rec.Header().Get("Content-Length"); got != tt.wantCL {
				t.Fatalf("Content-Length = %q, want %q", got, tt.wantCL)
			}
			body := rangeBody(t, rec)
			if !tt.wantMultipart {
				if body != tt.wantBody {
					t.Fatalf("body = %q, want %q", body, tt.wantBody)
				}
				return
			}
			// Multi-span: multipart/byteranges with the requested parts,
			// each carrying its own Content-Range and Content-Type.
			ct := rec.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "multipart/byteranges; boundary=") {
				t.Fatalf("Content-Type = %q, want multipart/byteranges with boundary", ct)
			}
			boundary := strings.TrimPrefix(ct, "multipart/byteranges; boundary=")
			for _, part := range tt.wantParts {
				if !strings.Contains(body, part+"\r\n") {
					t.Fatalf("multipart body missing part header %q:\n%s", part, body)
				}
			}
			if n := strings.Count(body, "--"+boundary+"\r\n"); n != 2 {
				t.Fatalf("multipart part count = %d, want 2:\n%s", n, body)
			}
			if !strings.Contains(body, "Content-Type: text/plain\r\n") {
				t.Fatalf("multipart parts missing object Content-Type:\n%s", body)
			}
			if !strings.Contains(body, "\r\n01\r\n") || !strings.Contains(body, "\r\n56\r\n") {
				t.Fatalf("multipart part payloads wrong:\n%s", body)
			}
		})
	}
}

// markerReadEnv is a webdav GET/HEAD marker test environment: a REAL fs
// backend rooted at a temp dataDir with the s3 process seams installed
// against it, and a webdav Frontend whose bucket-path resolver is wired
// exactly as production wires it (WithLockStoreRoot(getBucketPath) — the
// resolver f.bucketPath reads). The bucket path resolver is what makes
// the read-side delete-marker consult possible at all.
type markerReadEnv struct {
	t          *testing.T
	dataDir    string
	bucket     string
	bucketPath string
	f          *Frontend
}

func newMarkerReadEnv(t *testing.T, bucket string) *markerReadEnv {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	s3.InstallZfsVersioningMode("sidecar")
	t.Cleanup(func() { s3.InstallZfsVersioningMode("") })

	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}

	// Production wiring shape: WithLockStoreRoot IS the bucket-path
	// resolver (frontends.go passes getBucketPath there), so the read-side
	// marker consult resolves through f.bucketPath with no second config
	// view.
	f, err := New(s3.TestBackend(), Config{Bucket: bucket}, WithAuthenticator(newStubAuth()),
		WithLockStoreRoot(func(b string) string { return filepath.Join(dataDir, b) }))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	if got := f.bucketPath(bucket); got != bucketPath {
		t.Fatalf("bucketPath(%q) = %q, want %q (resolver must be non-empty for the marker consult)", bucket, got, bucketPath)
	}
	return &markerReadEnv{t: t, dataDir: dataDir, bucket: bucket, bucketPath: bucketPath, f: f}
}

// enable turns versioning on through the REAL s3 sub-resource handler.
func (e *markerReadEnv) enable() {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+e.bucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
	w := httptest.NewRecorder()
	s3.PutBucketVersioningHandlerForTest(w, req, e.bucket)
	if w.Code != http.StatusOK {
		e.t.Fatalf("enable versioning: status = %d, want 200", w.Code)
	}
}

// davWrite PUTs a key through the webdav handler (the object exists on
// disk and in the sidecar — the exact pre-DELETE state).
func (e *markerReadEnv) davWrite(key, body string) {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+key, strings.NewReader(body))
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated && w.Code != http.StatusNoContent {
		e.t.Fatalf("webdav PUT %s: status = %d, want 201/204", key, w.Code)
	}
}

// markDelete writes the delete marker through the SAME helper the webdav
// DELETE path uses (deleteMarkerOrPlain → the s3 store's PutDeleteMarker)
// and asserts the plain delete is suppressed — i.e. it reproduces the
// state a versioned webdav DELETE leaves behind, independent of whether
// the DELETE handler's own write-side gate currently runs.
func (e *markerReadEnv) markDelete(key string) {
	e.t.Helper()
	suppress, err := deleteMarkerOrPlain(e.bucketPath, e.bucket, key)
	if err != nil {
		e.t.Fatalf("delete marker for %s: %v", key, err)
	}
	if !suppress {
		e.t.Fatalf("delete marker for %s must suppress the plain delete", key)
	}
}

// davGet GETs the key through the webdav handler (mode B: no bucket
// segment) and returns the recorder.
func (e *markerReadEnv) davGet(method, key string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/"+key, nil)
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	return w
}

// TestGET_DeleteMarkedObjectAnswers404 is the HIGH data-plane pin: on a
// versioning-Enabled bucket a DELETE wrote a delete marker and SUPPRESSED
// the plain delete, so the data file survives on disk. A plain GET/HEAD
// over webdav must answer 404 with NO object bytes — the same answer the
// s3 GET gives (there plus x-amz-delete-marker: true). Serving 200 with
// the surviving bytes is the bug this pins.
func TestGET_DeleteMarkedObjectAnswers404(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			e := newMarkerReadEnv(t, "marker-read-bucket")
			e.enable()
			e.davWrite("doc.txt", "secret-payload")
			e.markDelete("doc.txt")

			// Precondition: the data file really does survive the
			// marker delete (that survival is what makes the read-side
			// consult load-bearing).
			if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "doc.txt")); err != nil || string(raw) != "secret-payload" {
				t.Fatalf("data file = %q err=%v, want the surviving secret-payload", raw, err)
			}

			w := e.davGet(method, "doc.txt")
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s on a delete-marked key = %d, want 404 (the marker hides the object)", method, w.Code)
			}
			if body := w.Body.String(); strings.Contains(body, "secret-payload") {
				t.Fatalf("%s leaked the deleted object's bytes: %q", method, body)
			}
			// A marker-hidden key is indistinguishable from a key that was
			// never written: byte-identical 404 (RFC 4918 <D:error>
			// document, which carries no object bytes). Asserted against a
			// live never-written key rather than a hardcoded string.
			absent := e.davGet(method, "never-written.txt")
			if absent.Code != http.StatusNotFound {
				t.Fatalf("%s on a never-written key = %d, want 404 (test fixture sanity)", method, absent.Code)
			}
			if w.Body.String() != absent.Body.String() {
				t.Fatalf("%s marker-hidden body = %q, want the plain 404 body %q (a client cannot tell the two apart)", method, w.Body.String(), absent.Body.String())
			}
			if ar := w.Header().Get("Accept-Ranges"); ar != "" {
				t.Fatalf("a 404 must not advertise Accept-Ranges, got %q", ar)
			}
		})
	}
}

// TestGET_DeleteMarkerHiddenAnswers404OnRange pins the consult ahead of
// the RANGE handling: a ranged GET on a delete-marked key is a 404, never
// a 206 of the hidden bytes (the marker consult runs before any body or
// span decision).
func TestGET_DeleteMarkerHiddenAnswers404OnRange(t *testing.T) {
	e := newMarkerReadEnv(t, "marker-range-bucket")
	e.enable()
	e.davWrite("doc.txt", "secret-payload")
	e.markDelete("doc.txt")

	req := httptest.NewRequest("GET", "/doc.txt", nil)
	req.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("ranged GET on a delete-marked key = %d, want 404", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "secr") {
		t.Fatalf("ranged GET leaked bytes of a delete-marked object: %q", body)
	}
}

// TestGET_NonVersionedBucketUnaffected pins the no-false-positive half:
// on a bucket that was NEVER versioned the consult must not hide an
// existing object (the s3 path's Off short-circuit). The same frontend,
// same resolver, same key — only the versioning state differs.
func TestGET_NonVersionedBucketUnaffected(t *testing.T) {
	e := newMarkerReadEnv(t, "plain-read-bucket") // versioning NEVER enabled
	e.davWrite("doc.txt", "live-payload")

	w := e.davGet("GET", "doc.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("GET on an unversioned bucket = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "live-payload" {
		t.Fatalf("body = %q, want live-payload", body)
	}
	if ar := w.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", ar)
	}
}

// TestGET_MarkerHiddenWithRangeHeader pins the unresolvable-bucketPath
// contract from the other side: with the resolver wired the consult runs;
// the unwired unit-test seam ("" path) keeps today's plain serve. Here we
// pin that a versioned bucket whose marker was written through the s3
// path (not the webdav helper) is hidden identically — the consult reads
// the SHARED store, so a marker written by either frontend hides the
// object over both.
func TestGET_MarkerWrittenByS3HidesOverWebdav(t *testing.T) {
	e := newMarkerReadEnv(t, "cross-frontend-bucket")
	e.enable()
	e.davWrite("doc.txt", "secret-payload")

	// The marker written by the s3 DELETE handler on the same key.
	req := httptest.NewRequest("DELETE", "/"+e.bucket+"/doc.txt", nil)
	w := httptest.NewRecorder()
	s3.DeleteObjectHandlerForTest(w, req, e.bucket, "doc.txt")
	if w.Code != http.StatusNoContent {
		t.Fatalf("s3 DELETE = %d, want 204", w.Code)
	}

	got := e.davGet("GET", "doc.txt")
	if got.Code != http.StatusNotFound {
		t.Fatalf("webdav GET on an s3-delete-marked key = %d, want 404 (one store, one visibility rule)", got.Code)
	}
}

// TestGET_MarkerReadErrorServesPlain pins the s3 EXACTLY: the s3 GET
// treats a marker-read error (mErr != nil) as not-hidden and proceeds
// with the plain path. That is parity, not a fallback we chose — an
// unreadable version sidecar must not turn a readable object into a 404.
// The error is provoked by a sidecar that exists but is not parseable
// JSON, which fails readVersionedSidecar.
func TestGET_MarkerReadErrorServesPlain(t *testing.T) {
	e := newMarkerReadEnv(t, "marker-badstate-bucket")
	e.enable()
	e.davWrite("doc.txt", "live-payload")

	// Corrupt the bucket-level state marker so the store's State read
	// fails: plainObjectDeleteMarker404 returns (false, err) and the s3
	// GET proceeds to the normal path.
	if err := os.WriteFile(filepath.Join(e.bucketPath, ".metadata", ".versioning"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("corrupting the versioning state marker: %v", err)
	}

	w := e.davGet("GET", "doc.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("GET with an unreadable versioning state = %d, want 200 (s3 parity: mErr != nil proceeds)", w.Code)
	}
	if body := w.Body.String(); body != "live-payload" {
		t.Fatalf("body = %q, want live-payload", body)
	}
}
