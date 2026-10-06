// zfssurface_test.go — quic-h3-2026-10 leaf 06: the ZFS enrichment READ
// surfaces over the webdav path (bucket ?events, ?events&versions, file
// ?versions), pinned BYTE-IDENTICAL to the s3 capability endpoints for
// the same bucket data.
//
// h3 needs NO code and NO separate test: the h3 frontend wraps the webdav
// handler (leaf 02), so every surface proven here is served over QUIC
// unchanged — this comment is the leaf's h3 record.
//
// Parity method: the s3 side is driven through the REAL s3 frontend
// pipeline (Frontend.Handler() with a SigV4-signed GET, signed via the s3
// package's exported canonicalization helpers); the webdav side through
// the REAL webdav handler. Bodies are compared as raw byte strings —
// never re-encoded or shape-compared — so any drift on either path fails.
package webdav

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// zfsCreds is the minimal SigV4 credential source for the s3-side peer.
type zfsCreds map[string]string

func (m zfsCreds) SecretKey(accessKeyID string) (string, bool) {
	k, ok := m[accessKeyID]
	return k, ok
}

// stubZFSProvider is the zfs-events provider double, installed through
// the exported InstallMetadataProvider wiring hook (production seam — no
// registry games). It implements the structural LastDetail seam the
// handlers consult.
type stubZFSProvider struct {
	mu     sync.Mutex
	events []metadata.ObjectEvent
	detail metadata.HistoryDetail
	err    error
}

func (s *stubZFSProvider) Name() string { return "zfs-events" }

func (s *stubZFSProvider) Probe(_ context.Context, _ string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{Available: true, Dataset: s.detail.Dataset}, nil
}

func (s *stubZFSProvider) History(_ context.Context, _, _ string, q metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	events := s.events
	if q.MaxEvents > 0 && len(events) > q.MaxEvents {
		events = events[:q.MaxEvents]
	}
	return events, nil
}

func (s *stubZFSProvider) Purge(_ context.Context, _ string) error { return nil }

func (s *stubZFSProvider) LastDetail() metadata.HistoryDetail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detail
}

func (s *stubZFSProvider) configure(events []metadata.ObjectEvent, detail metadata.HistoryDetail, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events, s.detail, s.err = events, detail, err
}

// zfsSurfaceEnv mounts BOTH frontends over ONE fs backend (the s3
// package's own test seams): the webdav frontend in mode B (root = the
// bucket) and a second webdav frontend in mode A (first path segment =
// bucket), plus the s3 frontend for the parity peer.
type zfsSurfaceEnv struct {
	t          *testing.T
	dataDir    string
	bucket     string
	bucketPath string
	dav        *Frontend // mode B: root IS the bucket collection
	davA       *Frontend // mode A: /<bucket>/... paths
	s3h        http.Handler
	stub       *stubZFSProvider
}

func newZFSSurfaceEnv(t *testing.T, bucket string) *zfsSurfaceEnv {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	be := s3.TestBackend()
	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}

	// The event stream, newest-first (stream order, the provider
	// contract): setattr derives NO version entry, remove is a delete
	// marker, recordsLost/ringSwaps are nonzero to exercise the lossy
	// fields, timestamps stay zero (honest "unknown").
	stub := &stubZFSProvider{}
	stub.configure(
		[]metadata.ObjectEvent{
			{Op: "setattr", Key: "doc.txt", Txg: 1003},
			{Op: "truncate", Key: "doc.txt", Txg: 1002, SizeOld: 11, SizeNew: 42},
			{Op: "remove", Key: "gone.log", Txg: 1001},
			{Op: "create", Key: "doc.txt", Txg: 1000, SizeNew: 11},
		},
		metadata.HistoryDetail{Dataset: "stub/ds", RecordsLost: 3, RingSwaps: 1},
		nil,
	)
	s3.InstallMetadataProvider(func(bp string) metadata.MetadataProvider {
		if bp == bucketPath {
			return stub
		}
		return nil
	})
	t.Cleanup(func() { s3.InstallMetadataProvider(nil) })

	resolver := func(b string) string { return filepath.Join(dataDir, b) }
	davB, err := New(be, Config{Bucket: bucket}, WithAuthenticator(newStubAuth()), WithBucketPathResolver(resolver))
	if err != nil {
		t.Fatalf("webdav.New (mode B): %v", err)
	}
	davA, err := New(be, Config{}, WithAuthenticator(newStubAuth()), WithBucketPathResolver(resolver))
	if err != nil {
		t.Fatalf("webdav.New (mode A): %v", err)
	}
	sf := s3.New(be, s3.WithCredentialSource(zfsCreds{"zetaadmin": "zetaadmin"}))

	// One real object for the plain-GET golden asserts (written through
	// the backend so the sidecar exists; the version listing itself
	// derives from the provider events, not the disk).
	if _, err := be.Put(context.Background(), bucket, "doc.txt",
		strings.NewReader("file-bytes-"), int64(len("file-bytes-")), objectmodel.PutOptions{}); err != nil {
		t.Fatalf("backend put doc.txt: %v", err)
	}
	return &zfsSurfaceEnv{t: t, dataDir: dataDir, bucket: bucket, bucketPath: bucketPath,
		dav: davB, davA: davA, s3h: sf.Handler(), stub: stub}
}

// signZFSGet builds a SigV4-signed GET for the s3 pipeline, using the s3
// package's exported canonicalization helpers (the same construction the
// s3 package's own dispatch tests use).
func signZFSGet(t *testing.T, target string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = "localhost:8443"
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := time.Now().UTC().Format("20060102")
	payloadHash := s3.HashSHA256([]byte(""))
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		"GET", req.URL.EscapedPath(), req.URL.Query().Encode(),
		fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", req.Host, payloadHash, amzDate),
		signedHeaders, payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate,
		fmt.Sprintf("%s/us-east-1/s3/aws4_request", dateStamp),
		s3.HashSHA256([]byte(canonicalRequest)),
	}, "\n")
	key := s3.GetSigningKey("zetaadmin", dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(s3.HmacSHA256(key, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=zetaadmin/%s/us-east-1/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		dateStamp, signedHeaders, signature))
	return req
}

// s3Get drives the REAL s3 pipeline (auth included) and returns status,
// headers snapshot, and the raw body.
func (e *zfsSurfaceEnv) s3Get(target string) (int, http.Header, string) {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.s3h.ServeHTTP(w, signZFSGet(e.t, target))
	return w.Code, w.Header(), w.Body.String()
}

// davGet drives the mode-B webdav handler.
func (e *zfsSurfaceEnv) davGet(target string) (int, http.Header, string) {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.dav.Handler().ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	return w.Code, w.Header(), w.Body.String()
}

// davAGet drives the mode-A webdav handler.
func (e *zfsSurfaceEnv) davAGet(target string) (int, http.Header, string) {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.davA.Handler().ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	return w.Code, w.Header(), w.Body.String()
}

// TestZFSSurfaceEventsParity pins byte parity of the collection and file
// ?events surfaces (and the ?events&versions combined extension) against
// the s3 pipeline's responses for the same bucket data.
func TestZFSSurfaceEventsParity(t *testing.T) {
	e := newZFSSurfaceEnv(t, "zfssurface-bkt")

	// Collection ?events: byte-identical from BOTH webdav path shapes
	// (mode B root "/" and mode A "/<bucket>/").
	s3Code, s3Hdr, s3Body := e.s3Get("/" + e.bucket + "?events")
	if s3Code != http.StatusOK || s3Hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("s3 ?events baseline: status %d content-type %q (body %s)", s3Code, s3Hdr.Get("Content-Type"), s3Body)
	}
	for _, f := range []struct {
		name   string
		target string
		get    func(string) (int, http.Header, string)
	}{
		{"modeB root", "/?events", e.davGet},
		{"modeA collection", "/" + e.bucket + "/?events", e.davAGet},
	} {
		code, hdr, body := f.get(f.target)
		if code != s3Code {
			t.Fatalf("%s %s: status = %d, want the s3 status %d", f.name, f.target, code, s3Code)
		}
		if body != s3Body {
			t.Fatalf("%s %s: body not byte-identical to s3:\n dav: %s\n s3:  %s", f.name, f.target, body, s3Body)
		}
		if ct := hdr.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s %s: content-type %q, want application/json", f.name, f.target, ct)
		}
	}

	// Combined ?events&versions resolves to the events extension (the
	// derived XML listing), byte-identical.
	s3Code, _, s3Body = e.s3Get("/" + e.bucket + "?events&versions")
	if s3Code != http.StatusOK || !strings.Contains(s3Body, "ListObjectVersionsExt") {
		t.Fatalf("s3 ?events&versions baseline: status %d body %s", s3Code, s3Body)
	}
	code, _, body := e.davGet("/?events&versions")
	if code != s3Code || body != s3Body {
		t.Fatalf("dav /?events&versions not byte-identical to s3 (status %d vs %d):\n dav: %s\n s3:  %s", code, s3Code, body, s3Body)
	}

	// File ?events: byte-identical.
	s3Code, _, s3Body = e.s3Get("/" + e.bucket + "/doc.txt?events")
	code, _, body = e.davGet("/doc.txt?events")
	if code != s3Code || body != s3Body {
		t.Fatalf("dav /doc.txt?events not byte-identical to s3 (status %d vs %d):\n dav: %s\n s3:  %s", code, s3Code, body, s3Body)
	}
}

// TestZFSSurfaceFileVersionsParity pins the file ?versions surface: the
// s3 derived listing (?events&versions XML) filtered to the key, as
// JSON. The wire shape is pinned LOCALLY here (not via the s3 types) so
// any field-name drift on the wire fails.
func TestZFSSurfaceFileVersionsParity(t *testing.T) {
	e := newZFSSurfaceEnv(t, "zfssurface-bkt")

	// The s3 side of the parity: the whole-bucket derived listing.
	_, _, xmlBody := e.s3Get("/" + e.bucket + "?events&versions")
	var ext s3.ListObjectVersionsExt
	if err := xml.Unmarshal([]byte(xmlBody), &ext); err != nil {
		t.Fatalf("unmarshal s3 listing: %v (body %s)", err, xmlBody)
	}

	// Expected = the s3 entries filtered to the key, same order.
	type wireVersion struct {
		Key            string `json:"key"`
		VersionId      string `json:"versionId"`
		IsLatest       bool   `json:"isLatest"`
		LastModified   string `json:"lastModified"`
		Size           int64  `json:"size"`
		Op             string `json:"op"`
		IsDeleteMarker bool   `json:"isDeleteMarker"`
	}
	type wireEnvelope struct {
		Name        string        `json:"name"`
		IsLossy     bool          `json:"isLossy"`
		RecordsLost uint64        `json:"recordsLost"`
		RingSwaps   uint64        `json:"ringSwaps"`
		Versions    []wireVersion `json:"versions"`
	}
	want := wireEnvelope{Name: e.bucket, IsLossy: ext.IsLossy, RecordsLost: ext.RecordsLost, RingSwaps: ext.RingSwaps}
	for _, v := range ext.Version {
		if v.Key != "doc.txt" {
			continue
		}
		want.Versions = append(want.Versions, wireVersion{
			Key: v.Key, VersionId: v.VersionId, IsLatest: v.IsLatest,
			LastModified: v.LastModified, Size: v.Size, Op: v.Op,
			IsDeleteMarker: v.IsDeleteMarker,
		})
	}
	wantBody, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal expected: %v", err)
	}

	code, hdr, body := e.davGet("/doc.txt?versions")
	if code != http.StatusOK {
		t.Fatalf("file ?versions status = %d (body %s)", code, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("file ?versions content-type %q, want application/json", ct)
	}
	if body != string(wantBody) {
		t.Fatalf("file ?versions not identical to the s3 listing filtered to the key:\n dav: %s\nwant: %s", body, wantBody)
	}

	// Shape sanity against the derivation rules: truncate + create
	// derive entries (setattr does NOT), newest-first stream order,
	// IsLatest on exactly the newest, honest lossy envelope.
	var got wireEnvelope
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal dav body: %v (%s)", err, body)
	}
	if len(got.Versions) != 2 {
		t.Fatalf("want 2 doc.txt versions (truncate+create, setattr derives none), got %d (%s)", len(got.Versions), body)
	}
	if !got.Versions[0].IsLatest || got.Versions[1].IsLatest {
		t.Fatalf("IsLatest must land on exactly the newest entry (%s)", body)
	}
	if got.Versions[0].Op != "truncate" || got.Versions[1].Op != "create" {
		t.Fatalf("stream order broken: ops = %s, %s", got.Versions[0].Op, got.Versions[1].Op)
	}
	if !got.IsLossy || got.RecordsLost != 3 || got.RingSwaps != 1 {
		t.Fatalf("lossy envelope not surfaced honestly: %+v", got)
	}

	// A key with no version events keeps the exact shape (versions
	// null — never fabricated).
	wantEmpty := wireEnvelope{Name: e.bucket, IsLossy: true, RecordsLost: 3, RingSwaps: 1}
	emptyBody, err := json.Marshal(wantEmpty)
	if err != nil {
		t.Fatal(err)
	}
	code, _, body = e.davGet("/nokey?versions")
	if code != http.StatusOK || body != string(emptyBody) {
		t.Fatalf("nokey ?versions: status %d body %s, want 200 %s", code, body, emptyBody)
	}
}

// TestZFSSurfaceNoProviderParity pins the honest 503: a bucket with no
// attached provider answers the SAME body the s3 endpoint answers, on
// the collection ?events surface AND the file ?versions surface.
func TestZFSSurfaceNoProviderParity(t *testing.T) {
	e := newZFSSurfaceEnv(t, "zfssurface-bkt")

	// A second bucket directory with NO provider (the hook returns nil
	// for any other bucket path).
	if err := os.MkdirAll(filepath.Join(e.dataDir, "plainbkt"), 0o755); err != nil {
		t.Fatalf("mkdir plainbkt: %v", err)
	}

	s3Code, s3Hdr, s3Body := e.s3Get("/plainbkt?events")
	if s3Code != http.StatusServiceUnavailable {
		t.Fatalf("s3 no-provider baseline: status %d (body %s)", s3Code, s3Body)
	}

	// Collection ?events, mode A (mode B cannot address another bucket).
	code, hdr, body := e.davAGet("/plainbkt/?events")
	if code != s3Code {
		t.Fatalf("dav /plainbkt/?events status = %d, want %d", code, s3Code)
	}
	if body != s3Body {
		t.Fatalf("dav 503 body not byte-identical to s3:\n dav: %s\n s3:  %s", body, s3Body)
	}
	if ct := hdr.Get("Content-Type"); ct != s3Hdr.Get("Content-Type") {
		t.Fatalf("dav 503 content-type %q, want %q", ct, s3Hdr.Get("Content-Type"))
	}

	// File ?versions on the provider-less bucket: same honest 503 body
	// (compared against the s3 object-level surface, which answers the
	// same contracted error).
	s3Code, s3Hdr, s3Body = e.s3Get("/plainbkt/somekey?events")
	if s3Code != http.StatusServiceUnavailable {
		t.Fatalf("s3 object no-provider baseline: status %d", s3Code)
	}
	code, hdr, body = e.davAGet("/plainbkt/somekey?versions")
	if code != s3Code || body != s3Body || hdr.Get("Content-Type") != s3Hdr.Get("Content-Type") {
		t.Fatalf("dav /plainbkt/somekey?versions: status %d ct %q body %s, want status %d ct %q body %s",
			code, hdr.Get("Content-Type"), body, s3Code, s3Hdr.Get("Content-Type"), s3Body)
	}
}

// TestZFSSurfaceNoParamsUnchanged pins the golden rule: a GET with NO
// query params (or with only UNKNOWN params) is byte-unchanged — the
// pre-leaf collection/file GET behavior, and ?versions alone on a
// collection stays the ignored-param plain GET.
func TestZFSSurfaceNoParamsUnchanged(t *testing.T) {
	e := newZFSSurfaceEnv(t, "zfssurface-bkt")

	// Collection GET, no params: 200, empty body, httpd/unix-directory.
	code, hdr, body := e.davGet("/")
	if code != http.StatusOK || body != "" || hdr.Get("Content-Type") != "httpd/unix-directory" {
		t.Fatalf("golden collection GET drifted: status %d ct %q body %q", code, hdr.Get("Content-Type"), body)
	}

	// File GET, no params: the object bytes.
	code, _, body = e.davGet("/doc.txt")
	if code != http.StatusOK || body != "file-bytes-" {
		t.Fatalf("golden file GET drifted: status %d body %q", code, body)
	}

	// Unknown query params are IGNORED — exactly today's behavior.
	code, hdr, body = e.davGet("/?unknownparam=1")
	if code != http.StatusOK || body != "" || hdr.Get("Content-Type") != "httpd/unix-directory" {
		t.Fatalf("unknown param on collection GET changed behavior: status %d ct %q body %q", code, hdr.Get("Content-Type"), body)
	}
	code, _, body = e.davGet("/doc.txt?unknownparam=1")
	if code != http.StatusOK || body != "file-bytes-" {
		t.Fatalf("unknown param on file GET changed behavior: status %d body %q", code, body)
	}

	// ?versions alone on a COLLECTION stays the plain GET (the param is
	// ignored there — only the file surface resolves it).
	code, hdr, body = e.davGet("/?versions")
	if code != http.StatusOK || body != "" || hdr.Get("Content-Type") != "httpd/unix-directory" {
		t.Fatalf("?versions on collection GET changed behavior: status %d ct %q body %q", code, hdr.Get("Content-Type"), body)
	}
}

// The s3 listing type alias keeps the parity assertions honest about
// what they parse (compile-time guard that the s3 surface shape exists).
var _ = auth.WildcardIdentity // stubAuth supplies identities; this import guard keeps auth explicit
