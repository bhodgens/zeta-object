// dispatch_grants_test.go — grant enforcement in dispatch (pluggable-
// authentication tree leaf 02 Task 2): readonly 403 on writes, scoped-bucket
// 403, ListBuckets filtering, wildcard byte-parity, multipart-initiate as a
// write. httptest against the real Frontend over the seams, fs backend.
package s3_test

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// signPath signs an arbitrary method/path/body with the fixture key. A
// self-contained SigV4 signer (the shared helper is fixed-path by design).
func signPath(t *testing.T, method, path, accessKey, secret, body string) *http.Request {
	t.Helper()
	amzDate := time.Now().UTC()
	dateStamp := amzDate.Format("20060102")
	host := "localhost:8443"
	payloadHash := hashSHA256Hex([]byte(body))

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("x-amz-date", amzDate.Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical URI: the escaped path WITHOUT the query string (query goes
	// into the canonical query string, SigV4 style).
	canonicalURI := req.URL.EscapedPath()

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		host, payloadHash, req.Header.Get("x-amz-date"))
	// Canonical query string: same construction as the server's
	// getCanonicalQueryStringExcluding (sorted keys, URI-escaped k=v).
	canonicalQuery := canonicalQueryStringForTest(req.URL.Query())
	canonicalRequest := strings.Join([]string{
		method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	scope := dateStamp + "/us-east-1/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate.Format("20060102T150405Z"), scope, hashSHA256Hex([]byte(canonicalRequest)),
	}, "\n")
	kDate := hmacSHA256Hex([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256Hex(kDate, "us-east-1")
	kService := hmacSHA256Hex(kRegion, "s3")
	kSigning := hmacSHA256Hex(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256Hex(kSigning, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/us-east-1/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		accessKey, dateStamp, signedHeaders, signature))
	return req
}

// canonicalQueryStringForTest mirrors the server's canonical query
// construction (sorted keys, canonicalQueryEscape semantics; net/url's
// QueryEscape matches for the fixture keys used here).
func canonicalQueryStringForTest(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var params []string
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		for _, v := range values {
			params = append(params, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(params, "&")
}

// newRegistryTestServer wires the full stack (config view, fs backend,
// registry) and returns the httptest server.
func newRegistryTestServer(t *testing.T, reg auth.IdentityRegistry) *httptest.Server {
	t.Helper()
	b := newFSBackend(t)
	s3.InstallServerConfigView(s3.ServerConfigView{Buckets: map[string]string{}, DataDir: b.root + "/"})
	s3.InstallFSRootResolver(func(bucket string) string {
		return b.root + "/" + bucket
	})
	s3.InstallBackendLookup(func(bucket string) (backend.Backend, error) { return b, nil })
	installRegistry(t, reg)
	f := s3.New(nil)
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestDispatchGrantEnforcement(t *testing.T) {
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "ro", AccessKey: "AKRO", SecretKey: "sk-ro", Grants: map[string]string{"bucket-one": "readonly"}},
		{Name: "rw", AccessKey: "AKRW", SecretKey: "sk-rw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := newRegistryTestServer(t, reg)
	_ = srv // subtests drive the frontend Handler directly (signPath covers the full pipeline)

	t.Run("wildcard full access", func(t *testing.T) {
		f := s3.New(nil)
		// Create the bucket, then PUT/GET an object.
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "PUT", "/bucket-one", "AKRW", "sk-rw", ""))
		if w.Code >= 400 {
			t.Fatalf("create bucket: %d %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "PUT", "/bucket-one/obj.txt", "AKRW", "sk-rw", "hello world"))
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("wildcard PUT denied: %d %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "GET", "/bucket-one/obj.txt", "AKRW", "sk-rw", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("wildcard GET denied: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("readonly write denied read allowed", func(t *testing.T) {
		f := s3.New(nil)
		// PUT → 403 AccessDenied.
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "PUT", "/bucket-one/obj.txt", "AKRO", "sk-ro", "x"))
		if w.Code != http.StatusForbidden {
			t.Fatalf("readonly PUT status = %d, want 403: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "AccessDenied") {
			t.Fatalf("body missing AccessDenied: %s", w.Body.String())
		}
		// Multipart initiate (POST ?uploads) is a write → 403.
		w = httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "POST", "/bucket-one/big.bin?uploads", "AKRO", "sk-ro", ""))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "AccessDenied") {
			t.Fatalf("readonly multipart initiate = %d %s", w.Code, w.Body.String())
		}
		// DELETE object → write path → 403.
		w = httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "DELETE", "/bucket-one/obj.txt", "AKRO", "sk-ro", ""))
		if w.Code != http.StatusForbidden {
			t.Fatalf("readonly DELETE = %d, want 403", w.Code)
		}
	})

	t.Run("scoped bucket other bucket denied", func(t *testing.T) {
		f := s3.New(nil)
		// GET on ungranted bucket → 403 AccessDenied (NOT NoSuchBucket).
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "GET", "/bucket-two/obj.txt", "AKRO", "sk-ro", ""))
		if w.Code != http.StatusForbidden {
			t.Fatalf("ungranted GET status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "AccessDenied") || strings.Contains(w.Body.String(), "NoSuchBucket") {
			t.Fatalf("body must be AccessDenied, got: %s", w.Body.String())
		}
	})

	t.Run("ListBuckets filtered to granted buckets", func(t *testing.T) {
		f := s3.New(nil)
		// bucket-one was created by the wildcard subtest above; list as AKRO.
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, signPath(t, "GET", "/", "AKRO", "sk-ro", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("ListBuckets status = %d", w.Code)
		}
		body := w.Body.String()
		if strings.Contains(body, "bucket-two") {
			t.Error("ungranted bucket leaked into ListBuckets")
		}
		if !strings.Contains(body, "bucket-one") {
			t.Error("granted bucket missing from ListBuckets")
		}
	})
}

// TestCopyObjectSourceGrantEnforcement pins bughunt S1: CopyObject must
// enforce the SOURCE bucket grant before the source Get. An identity with
// write on bucket A and no grant on bucket B cannot copy B's bytes into A.
// Drives the full serveHTTP pipeline so the request context carries the
// authenticated identity (the identityOf seam set in serveHTTP).
func TestCopyObjectSourceGrantEnforcement(t *testing.T) {
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "rw-two", AccessKey: "AKRW2", SecretKey: "sk-rw2", Grants: map[string]string{"copy-src": "readwrite", "copy-dst": "readwrite"}},
		{Name: "rw-one", AccessKey: "AKRW1", SecretKey: "sk-rw1", Grants: map[string]string{"copy-dst": "readwrite"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := newRegistryTestServer(t, reg)
	handler := srv.Config.Handler

	// Seed with the fully-granted identity: two buckets, one with an object.
	seed := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, signPath(t, method, path, "AKRW2", "sk-rw2", body))
		if w.Code >= 400 {
			t.Fatalf("seed %s %s = %d: %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	seed("PUT", "/copy-src", "")
	seed("PUT", "/copy-dst", "")
	seed("PUT", "/copy-src/payload.txt", "secret-payload")

	t.Run("copy within granted buckets succeeds", func(t *testing.T) {
		req := signPath(t, "PUT", "/copy-dst/copied.txt", "AKRW2", "sk-rw2", "")
		req.Header.Set("x-amz-copy-source", "copy-src/payload.txt")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("granted copy = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "CopyObjectResult") {
			t.Errorf("body missing CopyObjectResult: %s", w.Body.String())
		}
	})

	t.Run("copy from ungranted source is 403 AccessDenied", func(t *testing.T) {
		// AKRW1 may write copy-dst but holds NO grant on copy-src. The
		// destination grant passes authorizeS3Request; the source check must
		// deny before the backend Get (the source bucket need not even exist
		// — the grant decision precedes existence).
		req := signPath(t, "PUT", "/copy-dst/stolen.txt", "AKRW1", "sk-rw1", "")
		req.Header.Set("x-amz-copy-source", "copy-src/payload.txt")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("ungranted-source copy = %d, want 403: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "AccessDenied") {
			t.Errorf("body missing AccessDenied: %s", w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(s3.GetBucketPathShim("copy-dst"), "stolen.txt")); !os.IsNotExist(err) {
			t.Errorf("denied copy must not create the destination object (err=%v)", err)
		}
	})
}

// TestDispatchWildcardByteParity pins the back-compat guard: with the
// legacy CredentialSource (no registry) every operation behaves exactly as
// pre-tree — the wildcard path must never 403.
func TestDispatchWildcardByteParity(t *testing.T) {
	b := newFSBackend(t)
	s3.InstallServerConfigView(s3.ServerConfigView{Buckets: map[string]string{}, DataDir: b.root + "/"})
	s3.InstallFSRootResolver(func(bucket string) string { return b.root + "/" + bucket })
	s3.InstallBackendLookup(func(bucket string) (backend.Backend, error) { return b, nil })
	s3.InstallIdentityRegistry(nil)
	t.Cleanup(func() { s3.InstallIdentityRegistry(nil) })
	f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))

	// Create the bucket first (wildcard identity — allowed everywhere).
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, signPath(t, "PUT", "/bucket-par", "minioadmin", "minioadmin", ""))
	if w.Code >= 400 {
		t.Fatalf("create bucket: %d %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"PUT", "/bucket-par/obj.txt", 200},
		{"GET", "/bucket-par/obj.txt", 200},
		{"DELETE", "/bucket-par/obj.txt", 204},
		{"GET", "/", 200},
	} {
		body := ""
		if tc.method == "PUT" {
			body = "body"
		}
		req := signPath(t, tc.method, tc.path, "minioadmin", "minioadmin", body)
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (body: %.200s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}
