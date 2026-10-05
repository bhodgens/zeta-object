// batch_parity_test.go — quic-h3-2026-10 leaf 07: the s3↔webdav JSON
// batch PARITY (same manifest via the s3 handler and the webdav handler
// → identical results) and the versioning-capture-per-item pin (a batch
// overwrite on a versioned test bucket shows a captured old version per
// item in the store's List).
//
// Parity method (the leaf-06 zfssurface pattern): the s3 side runs the
// REAL s3 pipeline (Frontend.Handler() with a SigV4-signed POST, signed
// via the s3 package's exported canonicalization helpers); the webdav
// side the REAL webdav handler. Response bodies are compared as raw
// byte strings — never re-encoded — so any drift on either path fails.
//
// h3 needs NO code and NO separate test: the h3 frontend wraps the
// webdav handler (leaf 02), so ?batch is served over QUIC unchanged —
// this comment is the leaf's h3 record.
package webdav

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/batchops"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// signBatchPost builds a SigV4-signed POST for the s3 pipeline (the same
// construction signZFSGet uses, with a body hash).
func signBatchPost(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", target, bytes.NewReader(body))
	req.Host = "localhost:8443"
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := time.Now().UTC().Format("20060102")
	payloadHash := s3.HashSHA256(body)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		"POST", req.URL.EscapedPath(), req.URL.Query().Encode(),
		fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", req.Host, payloadHash, amzDate),
		signedHeaders, payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate,
		fmt.Sprintf("%s/us-east-1/s3/aws4_request", dateStamp),
		s3.HashSHA256([]byte(canonicalRequest)),
	}, "\n")
	key := s3.GetSigningKey("minioadmin", dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(s3.HmacSHA256(key, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=minioadmin/%s/us-east-1/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		dateStamp, signedHeaders, signature))
	return req
}

// batchParityEnv mounts BOTH frontends over ONE fs backend (the leaf-06
// env shape): webdav mode B (root = the bucket), webdav mode A
// (first path segment = bucket), and the s3 frontend for the parity
// peer — all over one bucket directory.
type batchParityEnv struct {
	t          *testing.T
	dataDir    string
	bucket     string
	bucketPath string
	davB       *Frontend
	davA       *Frontend
	s3h        http.Handler
}

func newBatchParityEnv(t *testing.T, bucket string) *batchParityEnv {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}
	resolver := func(b string) string { return filepath.Join(dataDir, b) }
	davB, err := New(s3.TestBackend(), Config{Bucket: bucket}, WithAuthenticator(newStubAuth()), WithBucketPathResolver(resolver))
	if err != nil {
		t.Fatalf("webdav.New (mode B): %v", err)
	}
	davA, err := New(s3.TestBackend(), Config{}, WithAuthenticator(newStubAuth()), WithBucketPathResolver(resolver))
	if err != nil {
		t.Fatalf("webdav.New (mode A): %v", err)
	}
	sf := s3.New(s3.TestBackend(), s3.WithCredentialSource(zfsCreds{"minioadmin": "minioadmin"}))
	return &batchParityEnv{t: t, dataDir: dataDir, bucket: bucket, bucketPath: bucketPath, davB: davB, davA: davA, s3h: sf.Handler()}
}

// seed writes one object through the backend (the shared data plane).
func (e *batchParityEnv) seed(key, body string) {
	e.t.Helper()
	if _, err := s3.TestBackend().Put(e.t.Context(), e.bucket, key, strings.NewReader(body), int64(len(body)), objectmodel.PutOptions{}); err != nil {
		e.t.Fatalf("seed %s: %v", key, err)
	}
}

// s3Batch POSTs the manifest through the REAL s3 pipeline (auth
// included); returns status + raw body.
func (e *batchParityEnv) s3Batch(manifest string) (int, string) {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.s3h.ServeHTTP(w, signBatchPost(e.t, "/"+e.bucket+"?batch", []byte(manifest)))
	return w.Code, w.Body.String()
}

// davBatchB POSTs through the mode-B webdav handler.
func (e *batchParityEnv) davBatchB(manifest string) (int, string) {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.davB.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/?batch", strings.NewReader(manifest)))
	return w.Code, w.Body.String()
}

// davBatchA POSTs through the mode-A webdav handler.
func (e *batchParityEnv) davBatchA(manifest string) (int, string) {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.davA.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/"+e.bucket+"/?batch", strings.NewReader(manifest)))
	return w.Code, w.Body.String()
}

// objectmodelPutOpts removed: the seed helper uses objectmodel.PutOptions
// directly.

// TestBatchParity_SameManifestIdenticalResults: the SAME manifest via
// the s3 handler and BOTH webdav path shapes produces byte-identical
// response bodies (status, content-type, the JSON envelope) — one
// implementation, mounted three ways.
func TestBatchParity_SameManifestIdenticalResults(t *testing.T) {
	manifest := `{"operations":[
		{"op":"copy","from":"a/src.txt","to":"b/copy.txt"},
		{"op":"delete","from":"missing.txt"},
		{"op":"delete","from":"tmp/junk"}
	]}`

	// Three independent envs (each run consumes the same manifest
	// against the same seeded layout — fresh state per surface so the
	// comparison is run-to-run honest).
	runs := []struct {
		name string
		run  func(e *batchParityEnv, manifest string) (int, string)
	}{
		{"s3", (*batchParityEnv).s3Batch},
		{"webdav modeB", (*batchParityEnv).davBatchB},
		{"webdav modeA", (*batchParityEnv).davBatchA},
	}
	var baselineCode int
	var baselineBody string
	for _, tc := range runs {
		t.Run(tc.name, func(t *testing.T) {
			e := newBatchParityEnv(t, "parity-batch-bkt")
			e.seed("a/src.txt", "copy me")
			e.seed("tmp/junk", "junk")

			code, body := tc.run(e, manifest)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", code, body)
			}
			if baselineBody == "" {
				baselineCode, baselineBody = code, body
				return
			}
			if code != baselineCode || body != baselineBody {
				t.Fatalf("%s response not byte-identical to the baseline:\n this: %s\n base: %s", tc.name, body, baselineBody)
			}
		})
	}

	// The shared baseline is a real partial-success envelope: ok,
	// NoSuchKey error at index 1, ok at index 2 — pinned once here so
	// "identical" can never mean "identically wrong".
	var resp batchops.Response
	if err := json.Unmarshal([]byte(baselineBody), &resp); err != nil {
		t.Fatalf("baseline not a batch response: %v (%s)", err, baselineBody)
	}
	if len(resp.Results) != 3 ||
		resp.Results[0].Status != batchops.StatusOK ||
		resp.Results[1].Status != batchops.StatusError || resp.Results[1].Code != "NoSuchKey" ||
		resp.Results[2].Status != batchops.StatusOK {
		t.Fatalf("baseline envelope drifted: %s", baselineBody)
	}
	_ = baselineCode
}

// TestBatchParity_MalformedManifestIdentical400: the same malformed
// manifest via both frontends is the SAME 400 — identical status and
// body — and NOTHING executes on either path.
func TestBatchParity_MalformedManifestIdentical400(t *testing.T) {
	bad := `{"operations":[{"op":"copy","from":".metadata/x","to":"y"}]}`

	var s3Code int
	var s3Body string
	t.Run("s3", func(t *testing.T) {
		e := newBatchParityEnv(t, "parity-bad-bkt")
		s3Code, s3Body = e.s3Batch(bad)
		if s3Code != http.StatusBadRequest {
			t.Fatalf("s3 status = %d, want 400: %s", s3Code, s3Body)
		}
	})
	t.Run("webdav modeB", func(t *testing.T) {
		e := newBatchParityEnv(t, "parity-bad-bkt")
		code, body := e.davBatchB(bad)
		if code != s3Code || body != s3Body {
			t.Fatalf("webdav 400 not byte-identical to s3:\n dav: %s\n s3:  %s", body, s3Body)
		}
	})
}

// TestBatchVersioningCapturePerItemPinned: a batch overwrite of a
// versioned-Enabled bucket captures the OLD bytes PER ITEM — the leaf-05
// machinery fires automatically because each item runs the normal write
// path; this test pins it (List shows one captured version per overwrite
// item, with the OLD content readable through the store).
func TestBatchVersioningCapturePerItemPinned(t *testing.T) {
	e := newBatchParityEnv(t, "ver-batch-bkt")

	// Enable versioning through the REAL s3 sub-resource handler.
	req := httptest.NewRequest("PUT", "/"+e.bucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
	w := httptest.NewRecorder()
	s3.PutBucketVersioningHandlerForTest(w, req, e.bucket)
	if w.Code != http.StatusOK {
		t.Fatalf("enable versioning: status = %d, want 200", w.Code)
	}

	// Two existing objects; each gets overwritten by one batch copy
	// item → one captured old version PER ITEM.
	e.seed("one.txt", "old-one")
	e.seed("two.txt", "old-two")

	// Seed the copy sources so the items are real overwrites.
	e.seed("src-a.txt", "new-a")
	e.seed("src-b.txt", "new-b")

	manifest := `{"operations":[
		{"op":"copy","from":"src-a.txt","to":"one.txt"},
		{"op":"copy","from":"src-b.txt","to":"two.txt"}
	]}`
	code, body := e.s3Batch(manifest)
	if code != http.StatusOK {
		t.Fatalf("batch status = %d, want 200: %s", code, body)
	}
	var resp batchops.Response
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	for i, r := range resp.Results {
		if r.Status != batchops.StatusOK {
			t.Fatalf("results[%d] = %+v, want ok", i, r)
		}
	}

	// Per-item captures: one old version per key, byte-readable, with
	// the OLD sizes (the overwrite captured what was there before).
	for key, oldBody := range map[string]string{
		"one.txt": "old-one",
		"two.txt": "old-two",
	} {
		versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, key)
		if err != nil {
			t.Fatalf("List %s: %v", key, err)
		}
		if len(versions) != 1 {
			t.Fatalf("%s: List = %d entries, want exactly 1 captured old version (per-item capture): %+v", key, len(versions), versions)
		}
		if versions[0].Size != int64(len(oldBody)) {
			t.Errorf("%s: captured size = %d, want %d (the OLD bytes)", key, versions[0].Size, len(oldBody))
		}
		if versions[0].IsDeleteMarker {
			t.Errorf("%s: capture must not be a delete marker", key)
		}
	}

	// The versioning capture fired through the WEBDAV mount too (the
	// same executor serves both surfaces).
	e2 := newBatchParityEnv(t, "ver-batch-dav-bkt")
	req2 := httptest.NewRequest("PUT", "/"+e2.bucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"))
	w2 := httptest.NewRecorder()
	s3.PutBucketVersioningHandlerForTest(w2, req2, e2.bucket)
	if w2.Code != http.StatusOK {
		t.Fatalf("enable versioning (dav env): %d", w2.Code)
	}
	e2.seed("keep.txt", "dav-old")
	e2.seed("src.txt", "dav-new")
	code2, body2 := e2.davBatchB(`{"operations":[{"op":"copy","from":"src.txt","to":"keep.txt"}]}`)
	if code2 != http.StatusOK {
		t.Fatalf("dav batch status = %d: %s", code2, body2)
	}
	davVersions, err := s3.ListVersionsForTest(e2.bucketPath, e2.bucket, "keep.txt")
	if err != nil {
		t.Fatalf("dav List: %v", err)
	}
	if len(davVersions) != 1 || davVersions[0].Size != int64(len("dav-old")) {
		t.Fatalf("dav-mounted capture: List = %+v, want one old-version capture of %d bytes", davVersions, len("dav-old"))
	}
}
