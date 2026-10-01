// neutrality_test.go — the protocol-neutrality proof (pluggable-
// authentication tree leaf 03 Task 3): ONE MultiRegistry driven through TWO
// wire surfaces — the real S3 frontend via SigV4 and a test-only Basic-auth
// HTTP frontend — asserting IDENTICAL identity and grant outcomes across the
// identity × operation × bucket matrix. This IS the issue's "tested from at
// least two different frontends" acceptance criterion.
package frontend_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/frontend"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// neutralityFixture builds the ONE registry both surfaces share.
func neutralityFixture(t *testing.T) *auth.MultiRegistry {
	t.Helper()
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "rw", AccessKey: "AKRW", SecretKey: "sk-rw"}, // wildcard readwrite
		{Name: "ro", AccessKey: "AKRO", SecretKey: "sk-ro", Grants: rawGrants(map[string]string{"neuro": "readonly"})},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// --- shared case matrix ------------------------------------------------------

type neutralityCase struct {
	name      string
	ak, sk    string
	bucket    string
	write     bool
	wantAllow bool
}

func neutralityMatrix() []neutralityCase {
	return []neutralityCase{
		{"AKRW read any", "AKRW", "sk-rw", "any-bucket", false, true},
		{"AKRW write any", "AKRW", "sk-rw", "any-bucket", true, true},
		{"AKRO read neuro", "AKRO", "sk-ro", "neuro", false, true},
		{"AKRO write neuro denied", "AKRO", "sk-ro", "neuro", true, false},
		{"AKRO read other denied", "AKRO", "sk-ro", "other", false, false},
		{"AKRO write other denied", "AKRO", "sk-ro", "other", true, false},
		{"bad password denied", "AKRO", "WRONG", "neuro", false, false},
		{"unknown key denied", "AKNOPE", "sk-x", "neuro", false, false},
	}
}

// --- surface 1: the real S3 frontend over SigV4 ------------------------------

func s3Surface(t *testing.T, reg *auth.MultiRegistry) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	b, err := fsbackend.New(root)
	if err != nil {
		t.Fatal(err)
	}
	s3.InstallServerConfigView(s3.ServerConfigView{Buckets: map[string]string{}, DataDir: root + "/"})
	s3.InstallFSRootResolver(func(bucket string) string { return filepathJoin(root, bucket) })
	s3.InstallBackendLookup(func(bucket string) (backend.Backend, error) { return b, nil })
	s3.InstallIdentityRegistry(reg)
	t.Cleanup(func() { s3.InstallIdentityRegistry(nil) })
	f := s3.New(nil)
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func filepathJoin(a, b string) string {
	if b == "" {
		return a
	}
	return a + "/" + b
}

// signNeutrality is a minimal SigV4 header signer for GET/PUT on path.
// It signs the host header as the request will carry it on the wire.
func signNeutrality(srv *httptest.Server, method, path, ak, sk string) *http.Request {
	amzDate := time.Now().UTC()
	dateStamp := amzDate.Format("20060102")
	host := srv.Listener.Addr().String()
	payloadHash := hex.EncodeToString(func() []byte {
		h := sha256.Sum256([]byte(""))
		return h[:]
	}())
	req := httptest.NewRequest(method, srv.URL+path, nil)
	req.RequestURI = "" // client requests must not carry RequestURI
	req.Host = host
	req.Header.Set("x-amz-date", amzDate.Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, req.Header.Get("x-amz-date"))
	uri := req.URL.EscapedPath()
	canonical := strings.Join([]string{method, uri, "", canonicalHeaders, signedHeaders, payloadHash}, "\n")
	scope := dateStamp + "/us-east-1/s3/aws4_request"
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate.Format("20060102T150405Z"), scope,
		func() string {
			h := sha256.Sum256([]byte(canonical))
			return hex.EncodeToString(h[:])
		}()}, "\n")
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	kDate := mac([]byte("AWS4"+sk), dateStamp)
	kRegion := mac(kDate, "us-east-1")
	kService := mac(kRegion, "s3")
	kSigning := mac(kService, "aws4_request")
	sig := hex.EncodeToString(mac(kSigning, sts))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/us-east-1/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		ak, dateStamp, signedHeaders, sig))
	return req
}

// --- surface 2: the test-only Basic-auth HTTP frontend -----------------------

// basicTestFrontend is the minimal shared-helper consumer: authenticate via
// auth.BasicAuthenticator, authorize via frontend.AuthorizeRequest,
// 200/403. ~30 lines, test-only (no shipped WebDAV frontend in this tree).
func basicTestFrontend(reg auth.IdentityRegistry) http.Handler {
	basic := auth.NewBasicAuthenticator(reg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := basic.Authenticate(r)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		parts := strings.SplitN(strings.Trim(r.URL.Path, "/"), "/", 2)
		bucket := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		write := r.Method != http.MethodGet && r.Method != http.MethodHead
		if err := frontend.AuthorizeRequest(id, bucket, write); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// runBothSurfaces executes one matrix case against both surfaces and
// returns (s3Allowed, basicAllowed, ok).
func runBothSurfaces(t *testing.T, srv *httptest.Server, basic http.Handler, tc neutralityCase) (bool, bool) {
	t.Helper()
	// S3 surface: real wire request through httptest transport with SigV4.
	method := "GET"
	if tc.write {
		method = "PUT"
	}
	req := signNeutrality(srv, method, "/"+tc.bucket+"/obj.txt", tc.ak, tc.sk)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("s3 request: %v", err)
	}
	resp.Body.Close()
	s3Allowed := resp.StatusCode < 400
	if resp.StatusCode == http.StatusNotFound {
		// NoSuchBucket/NoSuchKey are authorization successes (identity
		// passed the grant check; the object/bucket does not exist).
		s3Allowed = true
	}

	// Basic surface: same matrix through the test frontend.
	method2 := method
	path := "/" + tc.bucket + "/obj.txt"
	r := httptest.NewRequest(method2, path, nil)
	r.SetBasicAuth(tc.ak, tc.sk)
	w := httptest.NewRecorder()
	basic.ServeHTTP(w, r)
	basicAllowed := w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden
	return s3Allowed, basicAllowed
}

// TestProtocolNeutrality drives ONE registry through BOTH surfaces and
// asserts identical allow/deny outcomes for every matrix case.
func TestProtocolNeutrality(t *testing.T) {
	reg := neutralityFixture(t)
	srv := s3Surface(t, reg)
	basic := basicTestFrontend(reg)

	for _, tc := range neutralityMatrix() {
		t.Run(tc.name, func(t *testing.T) {
			s3Allowed, basicAllowed := runBothSurfaces(t, srv, basic, tc)
			if s3Allowed != tc.wantAllow || basicAllowed != tc.wantAllow {
				t.Fatalf("outcomes diverge (or are wrong): s3=%v basic=%v want=%v",
					s3Allowed, basicAllowed, tc.wantAllow)
			}
		})
	}
}

// TestNeutralitySameIdentity pins the identity half: the same credential
// resolves to the SAME identity (AccessKeyID + grants) on both surfaces.
func TestNeutralitySameIdentity(t *testing.T) {
	reg := neutralityFixture(t)

	byKey, okAK := reg.LookupByAccessKey("AKRO")
	byBasic, okBasic := reg.LookupByBasicCredential("AKRO", "sk-ro")
	if !okAK || !okBasic {
		t.Fatalf("lookups failed: ak=%v basic=%v", okAK, okBasic)
	}
	if byKey.AccessKeyID != byBasic.AccessKeyID {
		t.Fatalf("AccessKeyID differs: %q vs %q", byKey.AccessKeyID, byBasic.AccessKeyID)
	}
	for _, bucket := range []string{"neuro", "other", "*"} {
		if byKey.CanRead(bucket) != byBasic.CanRead(bucket) ||
			byKey.CanWrite(bucket) != byBasic.CanWrite(bucket) {
			t.Fatalf("grant divergence on %q: %+v vs %+v", bucket, byKey.BucketGrants, byBasic.BucketGrants)
		}
	}
}

// compile-time guards for the fixture types used above.
var (
	_ backend.Backend = (*fsbackend.FS)(nil)
)

// rawGrants adapts the legacy string-literal grant map to the dual-form
// config type (leaf 09): semantics identical, fewer literal bytes.
func rawGrants(m map[string]string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v)
		out[k] = b
	}
	return out
}
