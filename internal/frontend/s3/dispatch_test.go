package s3_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// fsTestBackend is the Backend double for dispatch tests: a real
// fsbackend.FS over a temp dir (the same concrete backend production
// resolves via the config-driven lookup).
type fsTestBackend struct {
	root string
	f    *fsbackend.FS
}

func newFSBackend(t *testing.T) *fsTestBackend {
	t.Helper()
	root := t.TempDir()
	f, err := fsbackend.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return &fsTestBackend{root: root, f: f}
}

func (b *fsTestBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	return b.f.Get(ctx, bucket, key, opts)
}

func (b *fsTestBackend) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	return b.f.Put(ctx, bucket, key, data, size, opts)
}

func (b *fsTestBackend) Delete(ctx context.Context, bucket, key string) error {
	return b.f.Delete(ctx, bucket, key)
}

func (b *fsTestBackend) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	return b.f.Stat(ctx, bucket, key)
}

func (b *fsTestBackend) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	return b.f.List(ctx, bucket, p)
}

func (b *fsTestBackend) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	return b.f.Buckets(ctx)
}

func (b *fsTestBackend) Capabilities() objectmodel.CapabilitySet {
	return b.f.Capabilities()
}

// newTestServer mounts a frontend over a fresh fs backend and installs
// the package seams (config view pointing at the backend's root).
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	b := newFSBackend(t)
	s3.InstallServerConfigView(s3.ServerConfigView{Buckets: map[string]string{}, DataDir: b.root + "/"})
	s3.InstallFSRootResolver(func(bucket string) string {
		return filepath.Join(b.root, bucket)
	})
	f := s3.New(b, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	s3.InstallBackendLookup(func(bucket string) (backend.Backend, error) { return b, nil })
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// signedRequestFor signs for an arbitrary host (the httptest server's).
func signedRequestFor(t *testing.T, method, host, target, body, accessKey, secret string) *http.Request {
	t.Helper()
	payloadHash := hashSHA256Hex([]byte(body))
	req, err := http.NewRequest(method, "http://"+host+target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	now := time.Now().UTC()
	amzDate := now.Format(testISO8601)
	dateStamp := now.Format(testShortDate)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalURI := req.URL.EscapedPath()
	canonicalQuery := req.URL.Query().Encode()
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash}, "\n")
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, testRegion, testService)
	stringToSign := strings.Join([]string{testAlgorithm, amzDate, credentialScope, hashSHA256Hex([]byte(canonicalRequest))}, "\n")
	key := signingKey(secret, dateStamp, testRegion, testService)
	signature := hex.EncodeToString(hmacSHA256Hex(key, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		accessKey, dateStamp, testRegion, testService, signedHeaders, signature))
	return req
}

// doSigned performs a signed request against srv and returns the response.
func doSigned(t *testing.T, srv *httptest.Server, method, target, body string) *http.Response {
	t.Helper()
	return doSignedAs(t, srv, method, target, body, "minioadmin", "minioadmin")
}

// TestS3Frontend_DispatchRouting drives the dispatch through Handler()
// with a real fs backend behind it (routing shapes preserved pre-move).
func TestS3Frontend_DispatchRouting(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		wantStatus int
		wantBody   string // substring; empty = don't check
	}{
		{"PUT bucket", "PUT", "/testbucket", "", http.StatusOK, ""},
		{"GET object missing", "GET", "/testbucket/missing", "", http.StatusNotFound, "NoSuchBucket"},
		{"unsupported method", "BREW", "/testbucket", "", http.StatusMethodNotAllowed, "Method Not Allowed"},
		// Note: the "unauthenticated" case signs for a key the credential
		// source does not know, exercising the InvalidAccessKeyId path.
		{"unknown access key", "GET", "/", "", http.StatusForbidden, "InvalidAccessKeyId"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			creds := []string{"minioadmin", "minioadmin"}
			if tt.name == "unknown access key" {
				creds = []string{"nobody", "nobody"}
			}
			resp := doSignedAs(t, srv, tt.method, tt.target, tt.body, creds[0], creds[1])
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, tt.wantStatus, body)
			}
			if tt.wantBody != "" && !strings.Contains(string(body), tt.wantBody) {
				t.Fatalf("body %q missing %q", body, tt.wantBody)
			}
		})
	}
}

// doSignedAs performs a signed request with explicit credentials.
func doSignedAs(t *testing.T, srv *httptest.Server, method, target, body, accessKey, secret string) *http.Response {
	t.Helper()
	u := strings.TrimPrefix(srv.URL, "http://")
	signed := signedRequestFor(t, method, u, target, body, accessKey, secret)
	resp, err := srv.Client().Do(signed)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

// TestS3Frontend_VersioningDegrades — flipped by s3-versioning leaf 03:
// the ?versioning sub-resource is now a REAL, routed sub-resource
// (SetBucketVersioning), so a signed PUT ?versioning on an existing
// bucket answers 200 and the state echoes back via GET — the opposite
// of the old silent degradation. The never-emulated contract now lives
// in the implemented semantics (status vocabulary, Off echo).
func TestS3Frontend_VersioningDegrades(t *testing.T) {
	srv := newTestServer(t)

	put := doSigned(t, srv, "PUT", "/testbucket", "")
	put.Body.Close()

	resp := doSigned(t, srv, "PUT", "/testbucket?versioning",
		"<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("versioning PUT = %d (body %s); want 200", resp.StatusCode, body)
	}

	get := doSigned(t, srv, "GET", "/testbucket?versioning", "")
	defer get.Body.Close()
	body, _ := io.ReadAll(get.Body)
	if get.StatusCode != http.StatusOK || !strings.Contains(string(body), "<Status>Enabled</Status>") {
		t.Fatalf("versioning GET = %d (body %s); want echoed Enabled", get.StatusCode, body)
	}
}

// TestS3Frontend_PutGetRoundTrip proves the full pipeline through
// Handler() → Backend with real bytes.
func TestS3Frontend_PutGetRoundTrip(t *testing.T) {
	srv := newTestServer(t)

	putBucket := doSigned(t, srv, "PUT", "/rt-bucket", "")
	if putBucket.StatusCode != http.StatusOK {
		putBucket.Body.Close()
		t.Fatalf("create bucket: got %d", putBucket.StatusCode)
	}
	putBucket.Body.Close()

	put := doSigned(t, srv, "PUT", "/rt-bucket/hello.txt", "round-trip-bytes")
	if put.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(put.Body)
		put.Body.Close()
		t.Fatalf("PUT: got %d (body %s)", put.StatusCode, body)
	}
	put.Body.Close()

	get := doSigned(t, srv, "GET", "/rt-bucket/hello.txt", "")
	defer get.Body.Close()
	data, _ := io.ReadAll(get.Body)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("GET: got %d", get.StatusCode)
	}
	if string(data) != "round-trip-bytes" {
		t.Fatalf("GET body = %q, want round-trip bytes", data)
	}
	if ct := get.Header.Get("Content-Type"); ct != "binary/octet-stream" {
		t.Fatalf("GET Content-Type = %q, want binary/octet-stream (default)", ct)
	}
}
