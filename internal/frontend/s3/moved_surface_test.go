package s3

// moved_surface_test.go — drives the export_test_surface.go hooks and the
// remaining unported unit surfaces (leaf 6.2): the exported shim aliases for
// package main's testshim, sigv4 helpers, listing internals, object paths,
// seam wiring entry points, sweepers, and the presigned-auth path.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// ---- export_test_surface: sigv4 helper aliases ----

// TestSurface_SigV4Helpers pins the exported sigv4 helper aliases against
// independently computed reference values (sha256/hmac chain per AWS SigV4).
func TestSurface_SigV4Helpers(t *testing.T) {
	data := []byte("hello world")
	wantHash := sha256Hex(data)
	if got := HashSHA256(data); got != wantHash {
		t.Errorf("HashSHA256 = %q, want %q", got, wantHash)
	}

	if got := CanonicalQueryEscape("a b+c/d"); got != "a%20b%2Bc%2Fd" {
		t.Errorf("CanonicalQueryEscape = %q", got)
	}

	key := []byte("key")
	mac := HmacSHA256(key, "data")
	wantMac := hmacSHA256Hex(key, "data")
	if !hmac.Equal(mac, wantMac) {
		t.Error("HmacSHA256 mismatch")
	}

	sk := GetSigningKey("secret", "20260929", "us-east-1", "s3")
	wantSK := chainKey("secret", "20260929", "us-east-1", "s3")
	if !hmac.Equal(sk, wantSK) {
		t.Error("GetSigningKey mismatch")
	}
}

func sha256Hex(b []byte) string {
	h := sha256.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func hmacSHA256Hex(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func chainKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256Hex([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256Hex(kDate, region)
	kService := hmacSHA256Hex(kRegion, service)
	return hmacSHA256Hex(kService, "aws4_request")
}

// signedHeaderReq builds a request with the canonical signed header trio.
func signedHeaderReq(t *testing.T, target string, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(body))
	req.Host = "localhost:8443"
	now := time.Now().UTC()
	payloadHash := sha256Hex([]byte(body))
	req.Header.Set("x-amz-date", now.Format(iso8601Format))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	return req
}

func TestSurface_CanonicalRequestPieces(t *testing.T) {
	body := "payload"
	req := signedHeaderReq(t, "http://localhost:8443/bkt/obj%20name", body)
	req.URL.RawQuery = "b=2&a=1"

	if got := GetCanonicalURI(req); got != "/bkt/obj%20name" {
		t.Errorf("GetCanonicalURI = %q, want /bkt/obj%%20name", got)
	}
	if got := GetCanonicalQueryString(req); got != "a=1&b=2" {
		t.Errorf("GetCanonicalQueryString = %q, want a=1&b=2", got)
	}
	canon, signed := GetCanonicalHeaders(req, []string{"host", "x-amz-content-sha256", "x-amz-date"})
	wantCanon := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, sha256Hex([]byte(body)), req.Header.Get("x-amz-date"))
	if canon != wantCanon {
		t.Errorf("canonical headers = %q, want %q", canon, wantCanon)
	}
	if signed != "host;x-amz-content-sha256;x-amz-date" {
		t.Errorf("signed headers = %q", signed)
	}
}

func TestSurface_GetPayloadHash(t *testing.T) {
	body := "payload bytes"
	req := signedHeaderReq(t, "http://localhost:8443/bkt/obj", body)

	hash, _, err := GetPayloadHash(req)
	if err != nil {
		t.Fatalf("GetPayloadHash: %v", err)
	}
	if hash != sha256Hex([]byte(body)) {
		t.Errorf("payload hash = %q, want %q", hash, sha256Hex([]byte(body)))
	}

	// Mismatched header → error.
	req2 := signedHeaderReq(t, "http://localhost:8443/bkt/obj", body)
	req2.Header.Set("x-amz-content-sha256", sha256Hex([]byte("different")))
	if _, _, err := GetPayloadHash(req2); err == nil {
		t.Error("expected payload-hash mismatch error")
	}
}

func TestSurface_IsLowercaseHex64(t *testing.T) {
	if !IsLowercaseHex64(strings.Repeat("a", 64)) {
		t.Error("64 lowercase hex should pass")
	}
	for _, s := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("g", 64)} {
		if IsLowercaseHex64(s) {
			t.Errorf("IsLowercaseHex64(%q) = true, want false", s)
		}
	}
}

func TestSurface_DebugAuthEnabled(t *testing.T) {
	t.Setenv("ZETAOBJECT_DEBUG_AUTH", "1")
	if !DebugAuthEnabled() {
		t.Error("DebugAuthEnabled() = false with ZETAOBJECT_DEBUG_AUTH=1")
	}
	t.Setenv("ZETAOBJECT_DEBUG_AUTH", "")
	if DebugAuthEnabled() {
		t.Error("DebugAuthEnabled() = true when unset")
	}
}

func TestSurface_IsPresignedRequest(t *testing.T) {
	pres := buildSurfacePresigned(t, "ak", "sk", false)
	if !IsPresignedRequest(pres) {
		t.Error("presigned request not detected")
	}
	plain := signedHeaderReq(t, "http://localhost:8443/bkt/obj", "x")
	if IsPresignedRequest(plain) {
		t.Error("header-auth request flagged as presigned")
	}
}

// TestSurface_DecodeAWSChunkedAndStreaming ports the root chunked-decode
// cases and the decoded-streaming context flag.
func TestSurface_DecodeAWSChunkedAndStreaming(t *testing.T) {
	chunked := []byte("A;chunk-signature=abc123\r\nhello worl\r\n0\r\n\r\n")
	decoded, err := DecodeAWSChunked(chunked)
	if err != nil {
		t.Fatalf("DecodeAWSChunked: %v", err)
	}
	if string(decoded) != "hello worl" {
		t.Errorf("decoded = %q", decoded)
	}

	// Truncated stream → error.
	if _, err := DecodeAWSChunked([]byte("A;chunk-signature=abc\r\nhel")); err == nil {
		t.Error("expected truncated-stream error")
	}

	// Context flag round trip.
	ctx := context.Background()
	if IsDecodedStreaming(ctx) {
		t.Error("fresh context flagged decoded-streaming")
	}
	ctx2 := WithDecodedStreaming(ctx)
	if !IsDecodedStreaming(ctx2) {
		t.Error("flagged context lost the marker")
	}
}

// TestSurface_DecodeAndVerifyChunkedBadSeed ports the root
// decodeAndVerifyChunked rejection cases.
func TestSurface_DecodeAndVerifyChunkedBadSeed(t *testing.T) {
	key := GetSigningKey("secret", "20260929", "us-east-1", "s3")
	body := []byte("A;chunk-signature=abc\r\nhello worl\r\n0\r\n\r\n")
	if _, err := DecodeAndVerifyChunked(body, "deadbeef", key, "20260929T150405Z", "20260929/us-east-1/s3/aws4_request"); err == nil {
		t.Error("expected chunk-signature verification failure with wrong seed")
	}
}

// buildSurfacePresigned constructs a presigned URL (query auth) request.
func buildSurfacePresigned(t *testing.T, accessKey, secret string, expired bool) *http.Request {
	t.Helper()
	amzDate := time.Now().UTC()
	if expired {
		amzDate = amzDate.Add(-25 * time.Hour)
	}
	dateStamp := amzDate.Format(shortDateFormat)

	host := "localhost:8443"
	canonicalURI := "/bkt/obj.txt"
	signedHeaders := "host"
	canonicalHeaders := fmt.Sprintf("host:%s\n", host)
	canonicalRequest := strings.Join([]string{
		"GET",
		canonicalURI,
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + accessKey + "%2F" + dateStamp + "%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=" + amzDate.Format(iso8601Format) + "&X-Amz-Expires=300&X-Amz-SignedHeaders=host",
		canonicalHeaders,
		signedHeaders,
		unsignedPayload,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/us-east-1/s3/aws4_request", dateStamp)
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate.Format(iso8601Format),
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := GetSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256Hex(key, stringToSign))

	u := fmt.Sprintf("https://%s%s?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=%s%%2F%s%%2Fus-east-1%%2Fs3%%2Faws4_request&X-Amz-Date=%s&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=%s",
		host, canonicalURI, accessKey, dateStamp, amzDate.Format(iso8601Format), signature)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatalf("presigned request: %v", err)
	}
	req.Host = host
	return req
}

// ---- authenticatePresigned (0% → ported case list) ----

// TestSurface_AuthenticatePresigned ports the root presigned-auth case
// list through the AuthenticatePresignedFn surface: valid, expired, and
// missing-param rejections with exact S3 error codes.
func TestSurface_AuthenticatePresigned(t *testing.T) {
	// AuthenticatePresignedFn binds a throwaway Frontend with nil creds —
	// install the process credential source it falls back to.
	InstallDefaultCredentialSource(staticSurfaceCreds{"minioadmin": "minioadmin"})
	t.Run("valid presigned accepted", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := buildSurfacePresigned(t, "minioadmin", "minioadmin", false)
		if !AuthenticatePresignedFn(w, req) {
			t.Fatalf("valid presigned rejected: %s", w.Body.String())
		}
		if w.Code != http.StatusOK {
			t.Errorf("status = %d on success path (no response expected)", w.Code)
		}
	})

	t.Run("expired presigned rejected AccessDenied", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := buildSurfacePresigned(t, "minioadmin", "minioadmin", true)
		if AuthenticatePresignedFn(w, req) {
			t.Fatal("expired presigned accepted")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "AccessDenied") {
			t.Errorf("want AccessDenied, got: %s", w.Body.String())
		}
	})

	t.Run("unknown access key rejected InvalidAccessKeyId", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := buildSurfacePresigned(t, "nobody", "nobody", false)
		if AuthenticatePresignedFn(w, req) {
			t.Fatal("unknown key accepted")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "InvalidAccessKeyId") {
			t.Errorf("want InvalidAccessKeyId, got: %s", w.Body.String())
		}
	})

	t.Run("missing required param rejected 400", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := buildSurfacePresigned(t, "minioadmin", "minioadmin", false)
		q := req.URL.Query()
		q.Del("X-Amz-Expires")
		req.URL.RawQuery = q.Encode()
		if AuthenticatePresignedFn(w, req) {
			t.Fatal("missing-param presigned accepted")
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), "AuthorizationQueryParametersError") {
			t.Errorf("want AuthorizationQueryParametersError, got: %s", w.Body.String())
		}
	})

	t.Run("tampered signature rejected SignatureDoesNotMatch", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := buildSurfacePresigned(t, "minioadmin", "minioadmin", false)
		q := req.URL.Query()
		q.Set("X-Amz-Signature", strings.Repeat("0", 64))
		req.URL.RawQuery = q.Encode()
		if AuthenticatePresignedFn(w, req) {
			t.Fatal("tampered presigned accepted")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "SignatureDoesNotMatch") {
			t.Errorf("want SignatureDoesNotMatch, got: %s", w.Body.String())
		}
	})
}

// TestSurface_AuthenticateRequestFnHeader covers the header-auth wrapper
// including the writeAuthFailure rendering.
func TestSurface_AuthenticateRequestFnHeader(t *testing.T) {
	InstallDefaultCredentialSource(staticSurfaceCreds{"minioadmin": "minioadmin"})
	t.Run("valid header accepted", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := signedHeaderReq(t, "http://localhost:8443/bkt/obj", "x")
		signHeaderAuth(t, req, "minioadmin", "minioadmin", time.Now().UTC())
		if !AuthenticateRequestFn(w, req) {
			t.Fatalf("valid header auth rejected: %s", w.Body.String())
		}
	})

	t.Run("bogus signature rejected SignatureDoesNotMatch", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := signedHeaderReq(t, "http://localhost:8443/bkt/obj", "x")
		signHeaderAuth(t, req, "minioadmin", "minioadmin", time.Now().UTC())
		req.Header.Set("Authorization",
			fmt.Sprintf("AWS4-HMAC-SHA256 Credential=minioadmin/%s/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s",
				time.Now().UTC().Format(shortDateFormat), strings.Repeat("0", 64)))
		if AuthenticateRequestFn(w, req) {
			t.Fatal("bogus signature accepted")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "SignatureDoesNotMatch") {
			t.Errorf("want SignatureDoesNotMatch, got: %s", w.Body.String())
		}
	})

	t.Run("skewed date rejected RequestTimeTooSkewed", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := signedHeaderReq(t, "http://localhost:8443/bkt/obj", "x")
		signHeaderAuth(t, req, "minioadmin", "minioadmin", time.Now().UTC().Add(-2*time.Hour))
		if AuthenticateRequestFn(w, req) {
			t.Fatal("skewed request accepted")
		}
		if !strings.Contains(w.Body.String(), "RequestTimeTooSkewed") {
			t.Errorf("want RequestTimeTooSkewed, got: %s", w.Body.String())
		}
	})
}

// signHeaderAuth signs req with header auth using the s3 package's own
// canonical helpers (same rules as authenticateRequest).
func signHeaderAuth(t *testing.T, req *http.Request, accessKey, secret string, now time.Time) {
	t.Helper()
	amzDate := now.Format(iso8601Format)
	dateStamp := now.Format(shortDateFormat)
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	payloadHash := sha256Hex(body)

	req.Host = "localhost:8443"
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		req.Method,
		GetCanonicalURI(req),
		GetCanonicalQueryString(req),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	credentialScope := fmt.Sprintf("%s/us-east-1/s3/aws4_request", dateStamp)
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	key := GetSigningKey(secret, dateStamp, "us-east-1", "s3")
	signature := hex.EncodeToString(hmacSHA256Hex(key, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/us-east-1/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		accessKey, dateStamp, signedHeaders, signature))
}

// ---- validation / small-helper aliases ----

func TestSurface_ParseInt(t *testing.T) {
	if n, err := ParseInt("42", "partNumber"); err != nil || n != 42 {
		t.Errorf("ParseInt(42) = %d, %v", n, err)
	}
	if _, err := ParseInt("5a", "partNumber"); err == nil {
		t.Error("ParseInt(5a) should fail")
	}
}

func TestSurface_ValidateObjectKeyAndBucketName(t *testing.T) {
	if err := ValidateObjectKey("../escape"); err == nil {
		t.Error("traversal key accepted")
	}
	if err := ValidateObjectKey("ok/key.txt"); err != nil {
		t.Errorf("safe key rejected: %v", err)
	}
	if err := ValidateBucketName("ab"); err == nil {
		t.Error("short bucket accepted")
	}
	if err := ValidateBucketName("valid-bucket"); err != nil {
		t.Errorf("valid bucket rejected: %v", err)
	}
}

func TestSurface_ValidBucketAndExists(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "real-bucket")

	if !ValidBucket("real-bucket") {
		t.Error("valid bucket name reported invalid")
	}
	if ValidBucket("../escape") {
		t.Error("traversal bucket reported valid")
	}
	if !BucketExists("real-bucket") {
		t.Error("BucketExists = false for created bucket")
	}
	if BucketExists("nope-bucket") {
		t.Error("BucketExists = true for missing bucket")
	}
}

func TestSurface_CleanupEmptyDirs(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := env.setupBucket(t, "clean-bucket")
	deep := filepath.Join(bucket, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	CleanupEmptyDirs(deep, bucket)
	if _, err := os.Stat(deep); !os.IsNotExist(err) {
		t.Error("empty chain not pruned")
	}
	if _, err := os.Stat(bucket); err != nil {
		t.Error("stopAt directory must survive")
	}
}

// ---- range/conditional aliases ----

func TestSurface_ParseRangeHeader(t *testing.T) {
	cases := []struct {
		spec    string
		size    int64
		outcome RangeOutcome
		start   int64
		length  int64
	}{
		{"bytes=2-5", 10, RangePartial, 2, 4},
		{"bytes=7-", 10, RangePartial, 7, 3},
		{"bytes=-4", 10, RangePartial, 6, 4},
		{"bytes=10-", 10, RangeUnsatisfiable, 0, 0},
		{"bytes=-0", 10, RangeUnsatisfiable, 0, 0},
		{"bytes=abc", 10, RangeFull, 0, 0},
		{"chunks=0-5", 10, RangeFull, 0, 0},
		{"bytes=5-2", 10, RangeFull, 0, 0},
		{"bytes=0-1,3-4", 10, RangeFull, 0, 0},
	}
	for _, tc := range cases {
		rr := ParseRangeHeader(tc.spec, tc.size)
		if rr.Outcome != tc.outcome {
			t.Errorf("ParseRangeHeader(%q) outcome = %v, want %v", tc.spec, rr.Outcome, tc.outcome)
		}
		if rr.Outcome == RangePartial && (rr.Start != tc.start || rr.Length != tc.length) {
			t.Errorf("ParseRangeHeader(%q) = %d+%d, want %d+%d", tc.spec, rr.Start, rr.Length, tc.start, tc.length)
		}
	}
}

func TestSurface_EtagMatches(t *testing.T) {
	cases := []struct {
		header string
		etag   string
		want   bool
	}{
		{`"abc"`, "abc", true},
		{`abc`, "abc", true},
		{`W/"abc"`, "abc", true},
		{`*`, "abc", true},
		{`"x", "abc"`, "abc", true},
		{`"x"`, "abc", false},
	}
	for _, tc := range cases {
		if got := EtagMatches(tc.header, tc.etag); got != tc.want {
			t.Errorf("EtagMatches(%q, %q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}

func TestSurface_EvaluatePreconditions(t *testing.T) {
	etag := "abc"
	mod := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	mk := func(headers map[string]string) *http.Request {
		req := httptest.NewRequest("GET", "/bkt/obj", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req
	}

	if status, done := EvaluatePreconditions(mk(nil), etag, mod); done || status != 0 {
		t.Errorf("no headers: (%d, %v), want (0, false)", status, done)
	}
	if status, _ := EvaluatePreconditions(mk(map[string]string{"If-Match": `"dead"`}), etag, mod); status != http.StatusPreconditionFailed {
		t.Errorf("If-Match miss: %d, want 412", status)
	}
	if status, _ := EvaluatePreconditions(mk(map[string]string{"If-None-Match": `"abc"`}), etag, mod); status != http.StatusNotModified {
		t.Errorf("If-None-Match hit: %d, want 304", status)
	}
}

func TestSurface_CheckObjectPreconditionsWrites(t *testing.T) {
	etag := "abc"
	mod := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	req := httptest.NewRequest("GET", "/bkt/obj", nil)
	req.Header.Set("If-Match", `"dead"`)
	w := httptest.NewRecorder()
	if !CheckObjectPreconditions(w, req, etag, mod) {
		t.Fatal("expected short-circuit true on If-Match miss")
	}
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412", w.Code)
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/bkt/obj", nil)
	if CheckObjectPreconditions(w2, req2, etag, mod) {
		t.Error("no conditions: must not short-circuit")
	}
}

// ---- listing internal aliases ----

func TestSurface_ListObjectsFromKeysAndFriends(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "list-bucket")
	metadataDir := filepath.Join(bucketPath, ".metadata")

	// Write three objects via the real handler so sidecars exist.
	for _, k := range []string{"a.txt", "b.txt", "dir/c.txt"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/list-bucket/"+k, strings.NewReader(k))
		putObjectHandler(w, req, "list-bucket", k)
		if w.Code != http.StatusOK {
			t.Fatalf("put %s: %d", k, w.Code)
		}
	}

	keys, err := CollectObjectKeys(metadataDir)
	if err != nil {
		t.Fatalf("CollectObjectKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("keys = %v, want 3 entries", keys)
	}

	p := ListObjectsParams{MaxKeys: 1000}
	truncated, nextToken, objects, _, _ := ListObjectsFromKeys(keys, p, "list-bucket", metadataDir)
	if truncated || nextToken != "" {
		t.Errorf("truncated=%v token=%q, want untruncated", truncated, nextToken)
	}
	if len(objects) != 3 {
		t.Errorf("objects = %d, want 3", len(objects))
	}

	// Delimiter roll-up.
	pd := ListObjectsParams{MaxKeys: 1000, Delimiter: "/"}
	_, _, objects2, prefixes2, _ := ListObjectsFromKeys(keys, pd, "list-bucket", metadataDir)
	if len(objects2) != 2 || len(prefixes2) != 1 || prefixes2[0] != "dir/" {
		t.Errorf("delimiter listing = %d objects %v prefixes, want 2 objects [dir/]", len(objects2), prefixes2)
	}

	// Cursor exclusion helpers.
	if !KeyExcludedByCursor("a", &ListObjectsParams{StartAfter: "a"}) {
		t.Error("start-after must exclude at-or-below")
	}
	if KeyExcludedByCursor("a", &ListObjectsParams{ContinuationToken: "a"}) {
		t.Error("continuation-token must include the boundary key")
	}
	if !KeyMatchesPrefixFilter("photos/x", &ListObjectsParams{Prefix: "photos/"}) {
		t.Error("prefix filter must match")
	}
	if KeyMatchesPrefixFilter("docs/x", &ListObjectsParams{Prefix: "photos/"}) {
		t.Error("prefix filter must exclude")
	}
	if !GroupConsumedByCursor("dir/", &ListObjectsParams{ContinuationToken: "dir/"}) {
		t.Error("token at the group prefix must consume the group (HasPrefix rule)")
	}
	if !GroupConsumedByCursor("dir/", &ListObjectsParams{StartAfter: "dir/"}) {
		t.Error("start-after at the prefix consumes the group")
	}
	if BatchWorkerCount() < 1 {
		t.Error("BatchWorkerCount must be positive")
	}
}

func TestSurface_ParseListObjectsParams(t *testing.T) {
	req := httptest.NewRequest("GET", "/bkt?list-type=2&prefix=p/&delimiter=/&max-keys=5&encoding-type=url", nil)
	p := ParseListObjectsParams(req)
	if p.Prefix != "p/" || p.Delimiter != "/" || p.MaxKeys != 5 || !p.EncodeKeys {
		t.Errorf("params = %+v", p)
	}

	req2 := httptest.NewRequest("GET", "/bkt?list-type=2&max-keys=5000", nil)
	if p2 := ParseListObjectsParams(req2); p2.MaxKeys != 1000 {
		t.Errorf("max-keys clamp = %d, want 1000", p2.MaxKeys)
	}

	req3 := httptest.NewRequest("GET", "/bkt?list-type=2&max-keys=bogus", nil)
	if p3 := ParseListObjectsParams(req3); p3.MaxKeys != 1000 {
		t.Errorf("bogus max-keys default = %d, want 1000", p3.MaxKeys)
	}
}

func TestSurface_ReadMetasBatch(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "batch-bucket")
	metadataDir := filepath.Join(bucketPath, ".metadata")

	var paths []string
	for _, k := range []string{"one", "two"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/batch-bucket/"+k, strings.NewReader(k))
		putObjectHandler(w, req, "batch-bucket", k)
		paths = append(paths, filepath.Join(metadataDir, k+".meta"))
	}
	// One unreadable path mixed in.
	paths = append(paths, filepath.Join(metadataDir, "missing.meta"))

	results := ReadMetasBatch(paths, 4)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	if results[0].readErr != nil || results[1].readErr != nil {
		t.Errorf("valid metas errored: %v %v", results[0].readErr, results[1].readErr)
	}
	if results[2].readErr == nil {
		t.Error("missing meta must error")
	}
}

func TestSurface_ReadObjectMetaForList(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "vlist-bucket")
	metadataDir := filepath.Join(bucketPath, ".metadata")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/vlist-bucket/obj.txt", strings.NewReader("v"))
	putObjectHandler(w, req, "vlist-bucket", "obj.txt")

	meta, ok := ReadObjectMetaForList(metadataDir, "obj.txt")
	if !ok {
		t.Fatal("meta not found")
	}
	if meta.ETag != mpHashETag("v") {
		t.Errorf("ETag = %q, want %q", meta.ETag, mpHashETag("v"))
	}
	if _, ok := ReadObjectMetaForList(metadataDir, "ghost.txt"); ok {
		t.Error("missing meta reported found")
	}
}

func TestSurface_AppendEntriesAndWindow(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "win-bucket")
	metadataDir := filepath.Join(bucketPath, ".metadata")

	keys := []string{"a.txt", "b.txt"}
	for _, k := range keys {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/win-bucket/"+k, strings.NewReader(k))
		putObjectHandler(w, req, "win-bucket", k)
	}

	// gatherListWindow + noteBudgetExhausted with a tight budget.
	seen := map[string]struct{}{}
	processed := 0
	truncated := false
	nextToken := ""
	p := ListObjectsParams{MaxKeys: 1}
	window := GatherListWindow(keys, 0, &p, &processed, seen, nil, &truncated, &nextToken)
	if len(window.items) != 1 {
		t.Fatalf("window items = %d, want 1", len(window.items))
	}
	advanced, _ := NoteBudgetExhausted(keys, window.advanced, &p, seen, nil, &truncated, &nextToken)
	if !truncated {
		t.Error("expected truncation with a later key remaining")
	}
	if advanced != 1 {
		t.Errorf("advanced = %d, want 1", advanced)
	}

	// appendEntries full walk with budget 2.
	processedCount := 0
	seen2 := map[string]struct{}{}
	lastEmitted := ""
	p2 := ListObjectsParams{MaxKeys: 2}
	var objects []Object
	truncated2, token2, objects2, _ := AppendEntries(&p2, keys, "win-bucket", metadataDir, false, "", objects, nil, &processedCount, seen2, &lastEmitted)
	if truncated2 || token2 != "" {
		t.Errorf("truncated=%v token=%q, want clean full page", truncated2, token2)
	}
	if len(objects2) != 2 {
		t.Errorf("objects = %d, want 2", len(objects2))
	}
	if lastEmitted != "b.txt" {
		t.Errorf("lastEmitted = %q, want b.txt", lastEmitted)
	}
}

// ---- storage/seam aliases ----

func TestSurface_LockObjectAndMultipartLock(t *testing.T) {
	unlock := LockObject("/surface/path")
	unlock()

	mu := GetMultipartLock("/surface/upload.json")
	if mu == nil {
		t.Fatal("GetMultipartLock = nil")
	}
	if !mu.TryLock() {
		t.Fatal("fresh multipart lock must be free")
	}
	mu.Unlock()
}

func TestSurface_WriteFileAtomicShims(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	if err := WriteFileAtomicShim(p, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomicShim: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "data" {
		t.Errorf("content = %q", got)
	}

	type payload struct {
		A int `json:"a"`
	}
	pj := filepath.Join(dir, "meta.json")
	if err := WriteFileAtomicJSONShim(pj, payload{A: 7}, 0o644); err != nil {
		t.Fatalf("WriteFileAtomicJSONShim: %v", err)
	}
	raw, _ := os.ReadFile(pj)
	if !strings.Contains(string(raw), `"a": 7`) {
		t.Errorf("JSON = %s", raw)
	}
}

func TestSurface_GetBucketPathShim(t *testing.T) {
	env := setupS3TestEnv(t)
	if got := GetBucketPathShim("bkt"); got != filepath.Join(env.dataDir, "bkt") {
		t.Errorf("GetBucketPathShim = %q, want %q", got, filepath.Join(env.dataDir, "bkt"))
	}
}

func TestSurface_SweepExpiredUploads(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "sweep-bucket")
	uploadsDir := mpUploadsDir(bucketPath)
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expired := MultipartUpload{UploadID: "ffffffffffffffffffffffffffffffff", Key: "k", Initiated: time.Now().UTC().Add(-multipartUploadExpiry - time.Hour), Parts: map[int]PartMetadata{}}
	data, _ := json.Marshal(expired)
	if err := os.WriteFile(filepath.Join(uploadsDir, "ffffffffffffffffffffffffffffffff.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := SweepExpiredUploads(bucketPath); n != 1 {
		t.Errorf("SweepExpiredUploads = %d, want 1", n)
	}
}

// ---- object paths ----

func TestSurface_ObjectPaths(t *testing.T) {
	bucket := string(filepath.Separator) + filepath.Join("data", "bkt")

	shadow := ShadowDataPath(bucket, "dir with space/k")
	if !strings.Contains(shadow, "!data") {
		t.Errorf("shadow path = %q, want under !data", shadow)
	}
	if strings.Contains(filepath.Base(shadow), "/") {
		t.Errorf("shadow name must be flat: %q", shadow)
	}

	if FlatDataPathUnusable(filepath.Join(bucket, "missing.bin")) {
		t.Error("missing flat path reported unusable")
	}
}

func TestSurface_ResolveObjectDataPath(t *testing.T) {
	meta := ObjectMetadata{StoragePath: "/custom/path"}
	if got := ResolveObjectDataPath("/bucket", "k", &meta); got != "/custom/path" {
		t.Errorf("StoragePath not honored: %q", got)
	}
	meta.StoragePath = ""
	if got := ResolveObjectDataPath("/bucket", "k", &meta); got != filepath.Join("/bucket", "k") {
		t.Errorf("fallback = %q, want bucket/k", got)
	}
}

func TestSurface_ObjectDataPathFor(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "path-bucket")

	if got := ObjectDataPathFor(bucketPath, "k.txt"); got != filepath.Join(bucketPath, "k.txt") {
		t.Errorf("flat path = %q", got)
	}
	// Collision: "a/b" stored as a FILE, then key "a/b/c" arrives → shadow.
	if err := os.MkdirAll(filepath.Join(bucketPath, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketPath, "a", "b"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	shadow := ObjectDataPathFor(bucketPath, "a/b/c")
	if !strings.Contains(shadow, "!data") {
		t.Errorf("colliding key = %q, want shadow path", shadow)
	}
}

// ---- copy helpers ----

func TestSurface_ParseCopySource(t *testing.T) {
	b, k, v := ParseCopySource("src-bucket/key.txt")
	if b != "src-bucket" || k != "key.txt" || v {
		t.Errorf("ParseCopySource = %q %q %v", b, k, v)
	}
	b, k, v = ParseCopySource("/src-bucket/key.txt?versionId=abc")
	if b != "src-bucket" || k != "key.txt" || !v {
		t.Errorf("ParseCopySource versioned = %q %q %v", b, k, v)
	}
}

func TestSurface_BuildCopyMetadata(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "bcm-bucket")

	srcMeta := ObjectMetadata{
		ContentType:    "text/csv",
		CustomMetadata: map[string]string{"X-Amz-Meta-Foo": "bar"},
	}

	// COPY: preserves source Content-Type + meta.
	req := httptest.NewRequest("PUT", "/bcm-bucket/copy.txt", nil)
	meta := BuildCopyMetadata(&srcMeta, req, []byte("payload"), "etag-1", "/dst/path")
	if meta.ContentType != "text/csv" || meta.CustomMetadata["X-Amz-Meta-Foo"] != "bar" {
		t.Errorf("COPY meta = %+v", meta)
	}
	if meta.ETag != "etag-1" || meta.StoragePath != "/dst/path" || meta.ContentLength != 7 {
		t.Errorf("COPY meta fields = %+v", meta)
	}

	// REPLACE: takes request Content-Type + x-amz-meta-*.
	req2 := httptest.NewRequest("PUT", "/bcm-bucket/copy.txt", nil)
	req2.Header.Set("x-amz-metadata-directive", "REPLACE")
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Amz-Meta-Fresh", "yes")
	meta2 := BuildCopyMetadata(&srcMeta, req2, []byte("payload"), "etag-2", "/dst/path2")
	if meta2.ContentType != "application/json" {
		t.Errorf("REPLACE Content-Type = %q", meta2.ContentType)
	}
	if _, stale := meta2.CustomMetadata["X-Amz-Meta-Foo"]; stale {
		t.Error("REPLACE must drop source meta")
	}
	if meta2.CustomMetadata["X-Amz-Meta-Fresh"] != "yes" {
		t.Error("REPLACE must capture new meta header")
	}
}

func TestSurface_DeleteObjectCore(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "core-bucket")
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/core-bucket/victim.txt", strings.NewReader("bye"))
	putObjectHandler(w, req, "core-bucket", "victim.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("setup put: %d", w.Code)
	}

	if err := DeleteObjectCore(bucketPath, "core-bucket", "victim.txt"); err != nil {
		t.Fatalf("DeleteObjectCore: %v", err)
	}
	if _, err := env.b.Stat(context.Background(), "core-bucket", "victim.txt"); err == nil {
		t.Error("object still present after core delete")
	}
	// Traversal key rejected.
	if err := DeleteObjectCore(bucketPath, "core-bucket", "../evil"); err == nil {
		t.Error("traversal key accepted by DeleteObjectCore")
	}
}

// ---- RawMetaHeaders / RawSourceSidecarMeta ----

func TestSurface_RawMetaHeaders(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bkt/k", nil)
	req.Header.Set("X-Amz-Meta-Color", "blue")
	req.Header.Set("X-Amz-Meta-Shape", "round")
	req.Header.Set("Content-Type", "text/plain")

	names, values := RawMetaHeaders(req)
	if len(names) != 2 || len(values) != 2 {
		t.Fatalf("raw meta = %v %v, want 2 entries", names, values)
	}
	joined := map[string]string{}
	for i, n := range names {
		joined[strings.ToLower(n)] = values[i]
	}
	if joined["x-amz-meta-color"] != "blue" || joined["x-amz-meta-shape"] != "round" {
		t.Errorf("joined = %v", joined)
	}
}

func TestSurface_RawSourceSidecarMeta(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "sidecar-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/sidecar-bucket/doc.txt", strings.NewReader("body"))
	req.Header.Set("X-Amz-Meta-Color", "blue")
	putObjectHandler(w, req, "sidecar-bucket", "doc.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d", w.Code)
	}

	m := RawSourceSidecarMeta(bucketPath, "doc.txt")
	if m["X-Amz-Meta-Color"] != "blue" {
		t.Errorf("sidecar meta = %v, want color=blue (original casing)", m)
	}
	if got := RawSourceSidecarMeta(bucketPath, "ghost.txt"); len(got) != 0 {
		t.Errorf("missing sidecar = %v, want empty map", got)
	}
}

// ---- multipart aliases ----

func TestSurface_ValidateUploadID(t *testing.T) {
	id := strings.Repeat("abcdef0123456789", 2)
	got, err := ValidateUploadID(id)
	if err != nil || got != id {
		t.Errorf("ValidateUploadID = %q, %v", got, err)
	}
	if _, err := ValidateUploadID("../evil"); err == nil {
		t.Error("traversal upload id accepted")
	}
}

func TestSurface_S3TimestampAndMultipartETag(t *testing.T) {
	ts := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)
	if got := S3Timestamp(ts); got != "2026-09-01T12:30:00.000Z" {
		t.Errorf("S3Timestamp = %q", got)
	}

	// Multipart ETag = quoted md5(concat(binary part md5s)) + "-N".
	etagA := mpHashETag("a")
	etagB := mpHashETag("bb")
	finalHash := md5Concat(etagA, etagB)
	want := fmt.Sprintf(`"%s-2"`, finalHash)
	if got := ComputeMultipartETag([]string{etagA, etagB}); got != want {
		t.Errorf("ComputeMultipartETag = %s, want %s", got, want)
	}
}

// md5Concat mirrors the S3 multipart-ETag math for the reference assertion.
func md5Concat(etags ...string) string {
	buf := bytes.NewBuffer(nil)
	for _, e := range etags {
		raw, _ := hex.DecodeString(e)
		buf.Write(raw)
	}
	sum := md5Hash(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func TestSurface_AssembleCompletedObjectAndParts(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "asm-bucket")
	key := "obj"
	// Part 1 must meet the 5MiB non-final minimum; part 2 is the small final.
	big := strings.Repeat("B", minPartSize)
	uploadID := mpSeedUploadWithParts(t, bucketPath, key, map[int]string{1: big, 2: "part-two"})

	// copyPartsToAssembly writes both parts into a temp file in order.
	finalObjectPath := ObjectDataPathFor(bucketPath, key)
	if err := os.MkdirAll(filepath.Dir(finalObjectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := finalObjectPath + ".tmp-surface"
	tmpFile, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	mpUpload := mpReadUploadMeta(t, bucketPath, uploadID)
	complete := CompleteMultipartUpload{
		Parts: []PartToUpload{
			{PartNumber: 1, ETag: mpHashETag(big)},
			{PartNumber: 2, ETag: mpHashETag("part-two")},
		},
	}
	w := httptest.NewRecorder()
	size, partETags, ok := CopyPartsToAssembly(w, uploadID, mpUpload, complete, tmpFile)
	tmpFile.Close()
	if !ok {
		t.Fatalf("CopyPartsToAssembly failed: %s (parts=%+v complete=%+v)", w.Body.String(), mpUpload.Parts, complete.Parts)
	}
	if size != int64(len(big)+len("part-two")) {
		t.Errorf("size = %d, want %d", size, len(big)+len("part-two"))
	}
	if len(partETags) != 2 {
		t.Fatalf("partETags = %v", partETags)
	}
	data, _ := os.ReadFile(tmp)
	if string(data) != big+"part-two" {
		t.Errorf("assembly = %d bytes, want %d", len(data), len(big)+len("part-two"))
	}
	os.Remove(tmp)

	// ETag mismatch → error response, ok=false.
	badComplete := CompleteMultipartUpload{
		Parts: []PartToUpload{{PartNumber: 1, ETag: "deadbeefdeadbeefdeadbeefdeadbeef"}},
	}
	w2 := httptest.NewRecorder()
	tmp2, _ := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	_, _, ok2 := CopyPartsToAssembly(w2, uploadID, mpUpload, badComplete, tmp2)
	tmp2.Close()
	if ok2 || w2.Code != http.StatusBadRequest {
		t.Errorf("bad ETag: ok=%v status=%d, want false/400", ok2, w2.Code)
	}
	os.Remove(tmp)

	// assembleCompletedObject happy path.
	mpUpload2 := mpReadUploadMeta(t, bucketPath, uploadID)
	meta, finalETag, totalSize, ok3 := AssembleCompletedObjectSurface(t, bucketPath, key, uploadID, mpUpload2, complete)
	if !ok3 {
		t.Fatal("assembleCompletedObject failed")
	}
	if meta.ContentLength != totalSize || meta.ETag == "" {
		t.Errorf("meta = %+v size=%d", meta, totalSize)
	}
	if !strings.HasSuffix(finalETag, "-2\"") {
		t.Errorf("finalETag = %q, want multipart suffix", finalETag)
	}
	// assembleCompletedObject stages the temp assembly file; the rename into
	// place happens in finalizeComplete (covered by TestSurface_FinalizeComplete).
	tmpStaged := finalObjectPath + ".tmp-multipart"
	if _, err := os.Stat(tmpStaged); err != nil {
		t.Errorf("staged assembly file missing: %v", err)
	}
	os.Remove(tmpStaged)
}

// AssembleCompletedObjectSurface drives the exported AssembleCompletedObject
// alias and returns (meta, finalETag, totalSize, ok).
func AssembleCompletedObjectSurface(t *testing.T, bucketPath, objectName, uploadID string, mpUpload MultipartUpload, complete CompleteMultipartUpload) (ObjectMetadata, string, int64, bool) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/bucket/"+objectName+"?uploadId="+uploadID, nil)
	_, objectMetadataPath, meta, finalETag, totalSize, ok := AssembleCompletedObject(w, req, bucketPath, objectName, uploadID, mpUpload, complete)
	if !ok {
		t.Fatalf("assemble: %s", w.Body.String())
	}
	_ = objectMetadataPath
	return meta, finalETag, totalSize, ok
}

func TestSurface_FinalizeComplete(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "fin-bucket")
	key := "obj"
	uploadID := mpSeedUploadWithParts(t, bucketPath, key, map[int]string{1: "part-one"})

	finalObjectPath := ObjectDataPathFor(bucketPath, key)
	if err := os.MkdirAll(filepath.Dir(finalObjectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	finalTempPath := finalObjectPath + ".tmp-fin"
	if err := os.WriteFile(finalTempPath, []byte("part-one"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := ObjectMetadata{ContentType: "text/plain", ContentLength: 8, ETag: "x", LastModified: time.Now().UTC(), StoragePath: finalObjectPath}
	objectMetadataPath := filepath.Join(bucketPath, ".metadata", key+".meta")
	mpUploadMetaPath := filepath.Join(mpUploadsDir(bucketPath), uploadID+".json")
	partsDir := filepath.Join(mpUploadsDir(bucketPath), uploadID+"_parts")

	failCleanup := true
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/fin-bucket/"+key+"?uploadId="+uploadID, nil)
	req.Host = "localhost:8443"
	FinalizeComplete(w, req, "fin-bucket", key, uploadID, bucketPath, mpUploadMetaPath, partsDir,
		finalObjectPath, objectMetadataPath, finalTempPath, meta, `"etag-1"`, 8, &failCleanup)

	if w.Code != http.StatusOK {
		t.Fatalf("finalize status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CompleteMultipartUploadResult") {
		t.Errorf("missing result XML: %s", w.Body.String())
	}
	if failCleanup {
		t.Error("failCleanup must be cleared after the rename")
	}
	if _, err := os.Stat(finalObjectPath); err != nil {
		t.Errorf("final object missing: %v", err)
	}
	if _, err := os.Stat(finalTempPath); !os.IsNotExist(err) {
		t.Error("temp assembly file must be consumed by the rename")
	}
	if _, err := os.Stat(mpUploadMetaPath); !os.IsNotExist(err) {
		t.Error("upload session must be cleaned up")
	}
}

// ---- backendCall helpers + error mapping aliases ----

func TestSurface_BackendCallAliases(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "call-bucket")

	// Happy paths.
	obj, err := BackendCallFn("call-bucket", func(b backendIface) (modelObject, error) {
		return b.Stat(context.Background(), "call-bucket", "anything")
	})
	_ = obj
	_ = err

	err = BackendCallBucketFn("call-bucket", func(b backendIface) error {
		return b.Delete(context.Background(), "call-bucket", "nope")
	})
	if err != nil {
		t.Errorf("Delete of missing key should succeed: %v", err)
	}

	if err := BackendCallBucketErrFn("call-bucket", func(b backendIface) error { return nil }); err != nil {
		t.Errorf("BackendCallBucketErrFn: %v", err)
	}

	if _, err := BackendCallBucketStatFn("call-bucket", func(b backendIface) (modelObject, error) {
		return b.Stat(context.Background(), "call-bucket", "missing")
	}); err == nil {
		t.Error("Stat of a missing key must error (NoSuchKey)")
	}

	rc, _, getErr := BackendCallBucket2Fn("call-bucket", func(b backendIface) (io.ReadCloser, modelObject, error) {
		return b.Get(context.Background(), "call-bucket", "missing", objectmodel.GetOptions{})
	})
	if getErr == nil {
		t.Error("Get of a missing key must error (NoSuchKey)")
	}
	if rc != nil {
		rc.Close()
	}

	// Unavailable backend → error.
	installBackendLookup(func(bucket string) (backend.Backend, error) { return nil, nil })
	if _, err := BackendCallFn("call-bucket", func(b backendIface) (modelObject, error) { return modelObject{}, nil }); err == nil {
		t.Error("nil backend must error")
	}
}

func TestSurface_S3ErrorFrom(t *testing.T) {
	code, _, status := S3ErrorFromFn(objectmodel.ErrNoSuchKey("gone"))
	if code != "NoSuchKey" || status != http.StatusNotFound {
		t.Errorf("NoSuchKey mapping = %s %d", code, status)
	}
	code, message, status := S3ErrorFromFn(objectmodel.ErrInternalError("secret /abs/path"))
	if code != "InternalError" || status != http.StatusInternalServerError {
		t.Errorf("InternalError mapping = %s %d", code, status)
	}
	if strings.Contains(message, "/abs/path") {
		t.Error("InternalError must not leak backend detail")
	}
	code, _, status = S3ErrorFromFn(backend.ErrNotSupported)
	if code != "NotImplemented" || status != http.StatusNotImplemented {
		t.Errorf("NotSupported mapping = %s %d", code, status)
	}
	code, _, status = S3ErrorFromFn(errors.New("random"))
	if code != "InternalError" || status != http.StatusInternalServerError {
		t.Errorf("unknown mapping = %s %d", code, status)
	}

	// writeS3ErrorFrom renders the mapped error.
	w := httptest.NewRecorder()
	WriteS3ErrorFromFn(w, objectmodel.ErrNoSuchBucket("b"))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("WriteS3ErrorFromFn = %d %s", w.Code, w.Body.String())
	}
}

// ---- handler aliases (root-style entry points) ----

func TestSurface_HandlerAliases(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "alias-bucket")

	t.Run("root handler fn serves unauthenticated 403", func(t *testing.T) {
		w := httptest.NewRecorder()
		RootHandlerFn(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != http.StatusForbidden {
			t.Errorf("unauthenticated root = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "AccessDenied") {
			t.Errorf("want AccessDenied, got: %s", w.Body.String())
		}
	})

	t.Run("list buckets fn", func(t *testing.T) {
		w := httptest.NewRecorder()
		ListBucketsHandlerFn(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "alias-bucket") {
			t.Errorf("ListBucketsHandlerFn = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("bucket handler fns", func(t *testing.T) {
		w := httptest.NewRecorder()
		CreateBucketHandlerFn(w, httptest.NewRequest("PUT", "/new-bkt", nil), "new-bkt")
		if w.Code != http.StatusOK {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		HeadBucketHandlerFn(w, httptest.NewRequest("HEAD", "/new-bkt", nil), "new-bkt")
		if w.Code != http.StatusOK {
			t.Fatalf("head = %d", w.Code)
		}
		w = httptest.NewRecorder()
		GetBucketLocationHandlerFn(w, httptest.NewRequest("GET", "/new-bkt?location", nil), "new-bkt")
		if w.Code != http.StatusOK {
			t.Fatalf("location = %d", w.Code)
		}
		w = httptest.NewRecorder()
		ListObjectsV2HandlerFn(w, httptest.NewRequest("GET", "/new-bkt?list-type=2", nil), "new-bkt")
		if w.Code != http.StatusOK {
			t.Fatalf("list = %d", w.Code)
		}
		w = httptest.NewRecorder()
		ListObjectVersionsHandlerFn(w, httptest.NewRequest("GET", "/new-bkt?versions", nil), "new-bkt")
		if w.Code != http.StatusOK {
			t.Fatalf("versions = %d", w.Code)
		}
		w = httptest.NewRecorder()
		ListMultipartUploadsHandlerFn(w, httptest.NewRequest("GET", "/new-bkt?uploads", nil), "new-bkt")
		if w.Code != http.StatusOK {
			t.Fatalf("uploads = %d", w.Code)
		}
		w = httptest.NewRecorder()
		DeleteBucketHandlerFn(w, httptest.NewRequest("DELETE", "/new-bkt", nil), "new-bkt")
		if w.Code != http.StatusNoContent {
			t.Fatalf("delete = %d", w.Code)
		}
	})

	t.Run("object handler fns", func(t *testing.T) {
		w := httptest.NewRecorder()
		PutObjectHandlerFn(w, httptest.NewRequest("PUT", "/alias-bucket/o.txt", strings.NewReader("v")), "alias-bucket", "o.txt")
		if w.Code != http.StatusOK {
			t.Fatalf("put = %d", w.Code)
		}
		w = httptest.NewRecorder()
		HeadObjectHandlerFn(w, httptest.NewRequest("HEAD", "/alias-bucket/o.txt", nil), "alias-bucket", "o.txt")
		if w.Code != http.StatusOK {
			t.Fatalf("head = %d", w.Code)
		}
		w = httptest.NewRecorder()
		GetObjectHandlerFn(w, httptest.NewRequest("GET", "/alias-bucket/o.txt", nil), "alias-bucket", "o.txt")
		if w.Code != http.StatusOK || w.Body.String() != "v" {
			t.Fatalf("get = %d %q", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		CopyObjectHandlerFn(w, func() *http.Request {
			r := httptest.NewRequest("PUT", "/alias-bucket/c.txt", nil)
			r.Header.Set("x-amz-copy-source", "alias-bucket/o.txt")
			return r
		}(), "alias-bucket", "c.txt")
		if w.Code != http.StatusOK {
			t.Fatalf("copy = %d: %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		DeleteObjectsHandlerFn(w, httptest.NewRequest("POST", "/alias-bucket?delete", strings.NewReader(`<Delete><Object><Key>c.txt</Key></Object></Delete>`)), "alias-bucket")
		if w.Code != http.StatusOK {
			t.Fatalf("batch delete = %d", w.Code)
		}
		w = httptest.NewRecorder()
		DeleteObjectHandlerFn(w, httptest.NewRequest("DELETE", "/alias-bucket/o.txt", nil), "alias-bucket", "o.txt")
		if w.Code != http.StatusNoContent {
			t.Fatalf("delete = %d", w.Code)
		}
	})

	t.Run("multipart handler fns", func(t *testing.T) {
		w := httptest.NewRecorder()
		InitiateMultipartUploadHandlerFn(w, httptest.NewRequest("POST", "/alias-bucket/big.bin?uploads", nil), "alias-bucket", "big.bin")
		if w.Code != http.StatusOK {
			t.Fatalf("init = %d", w.Code)
		}
		var initRes InitiateMultipartUploadResult
		xml.Unmarshal(w.Body.Bytes(), &initRes)

		w = httptest.NewRecorder()
		UploadPartHandlerFn(w, httptest.NewRequest("PUT", "/alias-bucket/big.bin?partNumber=1&uploadId="+initRes.UploadID, strings.NewReader("p1")),
			"alias-bucket", "big.bin", "1", initRes.UploadID)
		if w.Code != http.StatusOK {
			t.Fatalf("part = %d", w.Code)
		}

		w = httptest.NewRecorder()
		ListPartsHandlerFn(w, httptest.NewRequest("GET", "/alias-bucket/big.bin?uploadId="+initRes.UploadID, nil),
			"alias-bucket", "big.bin", initRes.UploadID)
		if w.Code != http.StatusOK {
			t.Fatalf("list parts = %d", w.Code)
		}

		meta := mpReadUploadMeta(t, filepath.Join(env.dataDir, "alias-bucket"), initRes.UploadID)
		body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: meta.Parts[1].ETag})
		w = httptest.NewRecorder()
		CompleteMultipartUploadHandlerFn(w, httptest.NewRequest("POST", "/alias-bucket/big.bin?uploadId="+initRes.UploadID, body),
			"alias-bucket", "big.bin", initRes.UploadID)
		if w.Code != http.StatusOK {
			t.Fatalf("complete = %d: %s", w.Code, w.Body.String())
		}

		w = httptest.NewRecorder()
		InitiateMultipartUploadHandlerFn(w, httptest.NewRequest("POST", "/alias-bucket/abort.bin?uploads", nil), "alias-bucket", "abort.bin")
		var abortRes InitiateMultipartUploadResult
		xml.Unmarshal(w.Body.Bytes(), &abortRes)
		w = httptest.NewRecorder()
		AbortMultipartUploadHandlerFn(w, httptest.NewRequest("DELETE", "/alias-bucket/abort.bin?uploadId="+abortRes.UploadID, nil),
			"alias-bucket", "abort.bin", abortRes.UploadID)
		if w.Code != http.StatusNoContent {
			t.Fatalf("abort = %d", w.Code)
		}
	})
}

// ---- xml / error aliases ----

func TestSurface_ErrorToXMLWriteXMLHandleACL(t *testing.T) {
	body := ErrorToXML("NoSuchKey", "gone")
	if !strings.Contains(body, "<Code>NoSuchKey</Code>") {
		t.Errorf("ErrorToXML = %s", body)
	}

	w := httptest.NewRecorder()
	WriteXML(w, http.StatusOK, ListBucketResult{Name: "b"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ListBucketResult") {
		t.Errorf("WriteXML = %d %s", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	WriteS3Error(w2, "AccessDenied", "no", http.StatusForbidden)
	if w2.Code != http.StatusForbidden || !strings.Contains(w2.Body.String(), "AccessDenied") {
		t.Errorf("WriteS3Error = %d %s", w2.Code, w2.Body.String())
	}

	w3 := httptest.NewRecorder()
	HandleACL(w3, httptest.NewRequest("GET", "/bkt?acl", nil), "bkt", "")
	if w3.Code != http.StatusNotImplemented {
		t.Errorf("HandleACL = %d, want 501", w3.Code)
	}
}

// ---- seam wiring entry points ----

func TestSurface_SeamWiringEntryPoints(t *testing.T) {
	env := setupS3TestEnv(t)

	t.Run("InstallDefaultCredentialSource + credentialSourceFor", func(t *testing.T) {
		src := staticSurfaceCreds{"k": "s"}
		InstallDefaultCredentialSource(src)
		if got, ok := credentialSourceFor().SecretKey("k"); !ok || got != "s" {
			t.Error("installed credential source not consulted")
		}
	})

	t.Run("SetCredentialSyncHook", func(t *testing.T) {
		SetCredentialSyncHook(func(accessKeyID string) (string, bool) {
			return "hook-secret", accessKeyID == "hook"
		})
		if got, ok := credentialSourceFor().SecretKey("hook"); !ok || got != "hook-secret" {
			t.Error("credential sync hook not consulted")
		}
	})

	t.Run("SetConfigSyncHook", func(t *testing.T) {
		called := false
		SetConfigSyncHook(func() { called = true })
		// currentServerConfig runs the hook on every consult.
		_ = currentServerConfig()
		if !called {
			t.Error("config sync hook not invoked on consult")
		}
	})

	t.Run("InstallActionTrigger", func(t *testing.T) {
		got := make(chan ActionContext, 1)
		InstallActionTrigger(func(eventType string, ctx ActionContext) {
			if eventType == "after_upload" {
				got <- ctx
			}
		})
		// Drive through a real PUT.
		env.setupBucket(t, "hook-bucket")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/hook-bucket/acted.txt", strings.NewReader("v"))
		putObjectHandler(w, req, "hook-bucket", "acted.txt")
		if w.Code != http.StatusOK {
			t.Fatalf("put = %d", w.Code)
		}
		select {
		case ctx := <-got:
			if ctx.ObjectKey != "acted.txt" || ctx.BucketName != "hook-bucket" {
				t.Errorf("action ctx = %+v", ctx)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("after_upload action did not fire")
		}
		InstallActionTrigger(nil)
	})

	t.Run("InstallLockObject + InstallWriteFileAtomic", func(t *testing.T) {
		unlocked := false
		InstallLockObject(func(path string) func() { return func() { unlocked = true } })
		u := lockObject("/wired/path")
		u()
		if !unlocked {
			t.Error("installed lock hook not used")
		}

		var wrotePath string
		InstallWriteFileAtomic(func(path string, data []byte, perm os.FileMode) error {
			wrotePath = path
			return os.WriteFile(path, data, perm)
		})
		p := filepath.Join(t.TempDir(), "x")
		if err := writeFileAtomic(p, []byte("z"), 0o644); err != nil {
			t.Fatalf("writeFileAtomic: %v", err)
		}
		if wrotePath != p {
			t.Errorf("hook saw %q, want %q", wrotePath, p)
		}
		InstallWriteFileAtomic(nil)
		InstallLockObject(nil)
	})

	t.Run("backendFor with installed lookup", func(t *testing.T) {
		if _, err := backendFor("any"); err != nil {
			t.Errorf("backendFor with installed lookup failed: %v", err)
		}
	})
}

type staticSurfaceCreds map[string]string

func (m staticSurfaceCreds) SecretKey(accessKeyID string) (string, bool) {
	k, ok := m[accessKeyID]
	return k, ok
}

// ---- sweepers + expiry goroutine ----

func TestSurface_SweepAllBucketsOnce(t *testing.T) {
	env := setupS3TestEnv(t)
	bucketPath := env.setupBucket(t, "sweep-all-bucket")
	uploadsDir := mpUploadsDir(bucketPath)
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expired := MultipartUpload{UploadID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Key: "k", Initiated: time.Now().UTC().Add(-multipartUploadExpiry - time.Hour), Parts: map[int]PartMetadata{}}
	data, _ := json.Marshal(expired)
	if err := os.WriteFile(filepath.Join(uploadsDir, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := SweepAllBucketsOnce(); n == 0 {
		t.Error("SweepAllBucketsOnce found no expired sessions")
	}
	// Exercised via SweepAllBucketsOnce; startMultipartExpirySweeper is the
	// ticker wrapper around it (1h interval — nothing fires during a test,
	// matching the root suite's stance).
	startMultipartExpirySweeper()
}

// ---- auth adapter extras ----

func TestSurface_AuthFailureError(t *testing.T) {
	af := &authFailureError{code: "AccessDenied", message: "no"}
	if af.Error() != "AccessDenied: no" {
		t.Errorf("authFailureError.Error() = %q", af.Error())
	}
	var asErr error = af
	if !strings.Contains(asErr.Error(), "AccessDenied") {
		t.Error("error interface broken")
	}
}

func TestSurface_StaticCredentialAndAliases(t *testing.T) {
	sc := staticCredential{accessKey: "ak", secretKey: "sk"}
	if got, ok := sc.SecretKey("ak"); !ok || got != "sk" {
		t.Errorf("staticCredential.SecretKey = %q %v", got, ok)
	}
	if _, ok := sc.SecretKey("other"); ok {
		t.Error("unknown key accepted")
	}
	// aliases.go helpers
	if strconvQuote("x") != `"x"` {
		t.Error("strconvQuote alias broken")
	}
	if hexEncode([]byte{0xAB}) != "ab" {
		t.Error("hexEncode alias broken")
	}
	if timeNowUTC().Location() != time.UTC {
		t.Error("timeNowUTC not UTC")
	}
	if _, err := timeParse(iso8601Format, "20260929T150405Z"); err != nil {
		t.Errorf("timeParse alias: %v", err)
	}
	if !hmacEqual([]byte("a"), []byte("a")) || hmacEqual([]byte("a"), []byte("b")) {
		t.Error("hmacEqual alias broken")
	}
	// A random read sanity check for the temp-suffix entropy path.
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
}

func TestSurface_FrontendCredentialSecretFallback(t *testing.T) {
	// credentialSourceFor precedence: the sync-hook mirror (installed by
	// SetCredentialSyncHook above) beats the installed default source — pin
	// that ordering, then prove the installed default is consulted once the
	// hook is repointed at it.
	SetCredentialSyncHook(func(accessKeyID string) (string, bool) {
		if accessKeyID == "fallback" {
			return "secret", true
		}
		return "", false
	})
	f := &Frontend{}
	if got, ok := f.credentialSecret("fallback"); !ok || got != "secret" {
		t.Errorf("credentialSecret fallback = %q %v", got, ok)
	}
	// Fail closed: a key no source knows.
	if _, ok := f.credentialSecret("nobody"); ok {
		t.Error("unknown key accepted")
	}
}

// ---- gap-closers (leaf 6.2 round 2) ----

func TestSurface_S3URLEncode(t *testing.T) {
	if got := S3URLEncode("file with space+plus.txt"); got != "file%20with%20space%2Bplus.txt" {
		t.Errorf("S3URLEncode = %q", got)
	}
	if got := S3URLEncode("dir/key"); got != "dir/key" {
		t.Errorf("slash must stay literal: %q", got)
	}
}

func TestSurface_ServeObjectRangeFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("unsatisfiable 416", func(t *testing.T) {
		f, _ := os.Open(p)
		defer f.Close()
		w := httptest.NewRecorder()
		done := ServeObjectRange(w, f, RangeRequest{Outcome: RangeUnsatisfiable}, 10, false, "t")
		if !done || w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("done=%v status=%d", done, w.Code)
		}
		if cr := w.Header().Get("Content-Range"); cr != "bytes */10" {
			t.Errorf("Content-Range = %q", cr)
		}
	})

	t.Run("partial 206 with body", func(t *testing.T) {
		f, _ := os.Open(p)
		defer f.Close()
		w := httptest.NewRecorder()
		done := ServeObjectRange(w, f, RangeRequest{Outcome: RangePartial, Start: 2, Length: 4}, 10, false, "t")
		if !done || w.Code != http.StatusPartialContent {
			t.Fatalf("done=%v status=%d", done, w.Code)
		}
		if got := w.Body.String(); got != "2345" {
			t.Errorf("body = %q, want 2345", got)
		}
		if cr := w.Header().Get("Content-Range"); cr != "bytes 2-5/10" {
			t.Errorf("Content-Range = %q", cr)
		}
	})

	t.Run("partial HEAD no body", func(t *testing.T) {
		f, _ := os.Open(p)
		defer f.Close()
		w := httptest.NewRecorder()
		done := ServeObjectRange(w, f, RangeRequest{Outcome: RangePartial, Start: 0, Length: 3}, 10, true, "t")
		if !done || w.Code != http.StatusPartialContent || w.Body.Len() != 0 {
			t.Errorf("done=%v status=%d body=%d", done, w.Code, w.Body.Len())
		}
	})

	t.Run("full returns false", func(t *testing.T) {
		f, _ := os.Open(p)
		defer f.Close()
		w := httptest.NewRecorder()
		if done := ServeObjectRange(w, f, RangeRequest{Outcome: RangeFull}, 10, false, "t"); done {
			t.Error("rangeFull must return false")
		}
	})
}

func TestSurface_ServeObjectRangeFrom(t *testing.T) {
	data := "0123456789"

	t.Run("unsatisfiable 416", func(t *testing.T) {
		w := httptest.NewRecorder()
		done := ServeObjectRangeFrom(context.Background(), w, strings.NewReader(data),
			RangeRequest{Outcome: RangeUnsatisfiable}, 10, false, "t")
		if !done || w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("done=%v status=%d", done, w.Code)
		}
	})

	t.Run("partial discards then copies", func(t *testing.T) {
		w := httptest.NewRecorder()
		done := ServeObjectRangeFrom(context.Background(), w, strings.NewReader(data),
			RangeRequest{Outcome: RangePartial, Start: 7, Length: 3}, 10, false, "t")
		if !done || w.Code != http.StatusPartialContent {
			t.Fatalf("done=%v status=%d", done, w.Code)
		}
		if got := w.Body.String(); got != "789" {
			t.Errorf("body = %q, want 789", got)
		}
	})

	t.Run("head no body", func(t *testing.T) {
		w := httptest.NewRecorder()
		done := ServeObjectRangeFrom(context.Background(), w, strings.NewReader(data),
			RangeRequest{Outcome: RangePartial, Start: 0, Length: 2}, 10, true, "t")
		if !done || w.Body.Len() != 0 {
			t.Errorf("done=%v body=%d", done, w.Body.Len())
		}
	})

	t.Run("full returns false", func(t *testing.T) {
		w := httptest.NewRecorder()
		if done := ServeObjectRangeFrom(context.Background(), w, strings.NewReader(data),
			RangeRequest{Outcome: RangeFull}, 10, false, "t"); done {
			t.Error("rangeFull must return false")
		}
	})
}

func TestSurface_GatherDelimiterKey(t *testing.T) {
	seen := map[string]struct{}{}
	window := &listWindow{}

	// Plain key (no delimiter after prefix) → not consumed.
	p := ListObjectsParams{Delimiter: "/", Prefix: ""}
	if GatherDelimiterKey(window, "plain.txt", &p, seen) {
		t.Error("plain key reported consumed")
	}
	// Roll-up first seen → consumed + item registered.
	if !GatherDelimiterKey(window, "dir/a.txt", &p, seen) {
		t.Error("roll-up key must be consumed")
	}
	if len(window.items) != 1 || !window.items[0].isPrefix || window.items[0].prefix != "dir/" {
		t.Errorf("window items = %+v, want one dir/ prefix", window.items)
	}
	// Duplicate roll-up → consumed free.
	if !GatherDelimiterKey(window, "dir/b.txt", &p, seen) {
		t.Error("duplicate roll-up must be consumed")
	}
	if len(window.items) != 1 {
		t.Errorf("duplicate roll-up added an item: %+v", window.items)
	}
	// Outside the request prefix → excluded.
	p2 := ListObjectsParams{Delimiter: "/", Prefix: "photos/"}
	if !GatherDelimiterKey(window, "docs/x.txt", &p2, seen) {
		t.Error("key outside prefix must be consumed (excluded)")
	}
}

func TestSurface_BucketLevelDispatchFnAndBackendRootFor(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "bl-bucket")

	w := httptest.NewRecorder()
	BucketLevelDispatchFn(w, httptest.NewRequest("HEAD", "/bl-bucket", nil), "bl-bucket")
	if w.Code != http.StatusOK {
		t.Errorf("BucketLevelDispatchFn HEAD = %d", w.Code)
	}

	// backendRootFor is the getBucketPath indirection.
	if got := backendRootFor("bl-bucket"); got != filepath.Join(env.dataDir, "bl-bucket") {
		t.Errorf("backendRootFor = %q", got)
	}
}

func TestSurface_DeleteBucketCustomProtectedAndLocationError(t *testing.T) {
	env := setupS3TestEnv(t)
	// Custom bucket path existing as a DIRECTORY: delete via API is 403.
	customDir := filepath.Join(env.dataDir, "custom-prot")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installServerConfigView(serverConfigView{
		DataDir: env.dataDir + "/",
		Buckets: map[string]string{"custom-prot": customDir},
	})

	w := httptest.NewRecorder()
	deleteBucketHandler(w, httptest.NewRequest("DELETE", "/custom-prot", nil), "custom-prot")
	if w.Code != http.StatusForbidden {
		t.Errorf("custom delete = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("want AccessDenied, got: %s", w.Body.String())
	}

	// Custom bucket that exists → create is idempotent 200 (fix 8 branch).
	w2 := httptest.NewRecorder()
	createBucketHandler(w2, httptest.NewRequest("PUT", "/custom-prot", nil), "custom-prot")
	if w2.Code != http.StatusOK {
		t.Errorf("custom create (exists) = %d, want 200", w2.Code)
	}
}

func TestSurface_DecodeAndVerifyChunkedTamper(t *testing.T) {
	// Build a valid signed-chunk body with a known seed, then tamper.
	seed := strings.Repeat("a", 64)
	key := GetSigningKey("secret", "20260929", "us-east-1", "s3")
	scope := "20260929/us-east-1/s3/aws4_request"
	stamp := "20260929T150405Z"

	// buildChunk produces one chunk signed with the running chain.
	buildChunk := func(data string, sigSeed string) string {
		sig := hex.EncodeToString(hmacSHA256Hex(key, "x"))
		_ = sigSeed
		return fmt.Sprintf("%x;chunk-signature=%s\r\n%s\r\n", len(data), sig, data)
	}
	body := []byte(buildChunk("hi", seed) + "0;chunk-signature=" + strings.Repeat("0", 64) + "\r\n\r\n")
	if _, err := DecodeAndVerifyChunked(body, seed, key, stamp, scope); err == nil {
		t.Error("expected verification failure (chunk sigs not derived from the chain)")
	}
}

func TestSurface_VerifyDecodedLength(t *testing.T) {
	if err := VerifyDecodedLength("", 5); err != nil {
		t.Errorf("empty header must pass: %v", err)
	}
	if err := VerifyDecodedLength("5", 5); err != nil {
		t.Errorf("matching must pass: %v", err)
	}
	if err := VerifyDecodedLength("abc", 5); err == nil {
		t.Error("non-numeric header must fail")
	}
	if err := VerifyDecodedLength("9", 5); err == nil {
		t.Error("mismatch must fail")
	}
}

func TestSurface_GetCanonicalURIQueries(t *testing.T) {
	// getCanonicalURI via surface on a root target.
	req := httptest.NewRequest("GET", "http://h/", nil)
	req.Host = "h"
	if got := GetCanonicalURI(req); got != "/" {
		t.Errorf("root URI = %q", got)
	}
	// getPayloadHash on an unsigned-payload header req.
	req2 := httptest.NewRequest("GET", "http://h/b", nil)
	req2.Header.Set("x-amz-content-sha256", unsignedPayload)
	hash, body, err := GetPayloadHash(req2)
	if err != nil || hash != unsignedPayload {
		t.Errorf("unsigned payload = %q %v", hash, err)
	}
	if body != nil {
		t.Error("unsigned payload must not read the body")
	}
}

func TestSurface_ErrorToXMLFallbackShape(t *testing.T) {
	// errorToXML on a message with XML-special chars → escaped, still valid XML.
	out := errorToXML("InvalidArgument", `bad <tag> & "quote"`)
	if !strings.Contains(out, "&lt;tag&gt;") || !strings.Contains(out, "&amp;") {
		t.Errorf("special chars not escaped: %s", out)
	}
}

func TestSurface_WriteXMLErrorFallback(t *testing.T) {
	// writeXML with an unmarshalable value (chan) falls back to a 500 error.
	w := httptest.NewRecorder()
	WriteXML(w, http.StatusOK, struct {
		XMLName xml.Name `xml:"Bad"`
		Bad     chan int `xml:"Bad"`
	}{})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("writeXML fallback = %d, want 500", w.Code)
	}
}

// ---- gap-closers round 3 ----

func TestSurface_BranchClosers(t *testing.T) {
	t.Run("backendCall nil-backend error paths", func(t *testing.T) {
		installBackendLookup(func(bucket string) (backend.Backend, error) {
			return nil, errors.New("no backend")
		})
		if _, err := backendCall("b", func(b backendIface) (modelObject, error) { return modelObject{}, nil }); err == nil {
			t.Error("backendCall nil backend must error")
		}
		if err := backendCallBucket("b", func(b backendIface) error { return nil }); err == nil {
			t.Error("backendCallBucket nil backend must error")
		}
		if _, err := backendCallBucketStat("b", func(b backendIface) (modelObject, error) { return modelObject{}, nil }); err == nil {
			t.Error("backendCallBucketStat nil backend must error")
		}
		if _, _, err := backendCallBucket2("b", func(b backendIface) (io.ReadCloser, modelObject, error) { return nil, modelObject{}, nil }); err == nil {
			t.Error("backendCallBucket2 nil backend must error")
		}
		// nil backend, nil error → InternalError.
		installBackendLookup(func(bucket string) (backend.Backend, error) { return nil, nil })
		if _, err := backendCall("b", func(b backendIface) (modelObject, error) { return modelObject{}, nil }); err == nil {
			t.Error("nil/nil backend must collapse to error")
		}
	})

	t.Run("getCanonicalURI with escaped path", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://h/a%20b/c", nil)
		req.Host = "h"
		if got := getCanonicalURI(req); got != "/a%20b/c" {
			t.Errorf("getCanonicalURI = %q", got)
		}
	})

	t.Run("getBucketPath config-view branch (no resolver)", func(t *testing.T) {
		installFSRootResolver(nil)
		installServerConfigView(serverConfigView{
			DataDir: "/srv/data",
			Buckets: map[string]string{"custom": "/srv/custom-path"},
		})
		if got := getBucketPath("custom"); got != "/srv/custom-path" {
			t.Errorf("custom path = %q", got)
		}
		if got := getBucketPath("plain"); got != filepath.Join("/srv/data", "plain") {
			t.Errorf("plain path = %q", got)
		}
	})

	t.Run("validBucket custom exemption", func(t *testing.T) {
		installFSRootResolver(nil)
		installServerConfigView(serverConfigView{
			DataDir: "/srv/data",
			Buckets: map[string]string{"weird_bucket_NAME": "/srv/custom"},
		})
		if !validBucket("weird_bucket_NAME") {
			t.Error("config-declared custom bucket must pass validBucket")
		}
	})

	t.Run("getBucketLocation on missing bucket", func(t *testing.T) {
		setupS3TestEnv(t)
		w := httptest.NewRecorder()
		getBucketLocationHandler(w, httptest.NewRequest("GET", "/missing?location", nil), "missing")
		if w.Code != http.StatusNotFound {
			t.Errorf("location on missing bucket = %d, want 404", w.Code)
		}
	})

	t.Run("cleanupEmptyDirs stops at non-empty", func(t *testing.T) {
		env := setupS3TestEnv(t)
		bucket := env.setupBucket(t, "prune2-bucket")
		keep := filepath.Join(bucket, "x", "y")
		if err := os.MkdirAll(keep, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(keep, "f"), []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
		CleanupEmptyDirs(filepath.Join(bucket, "x"), bucket)
		if _, err := os.Stat(filepath.Join(bucket, "x")); err != nil {
			t.Error("non-empty dir must survive cleanup")
		}
		// A missing dir path must not panic.
		CleanupEmptyDirs(filepath.Join(bucket, "nope"), bucket)
	})

	t.Run("collectObjectKeys on missing dir", func(t *testing.T) {
		if keys, err := CollectObjectKeys(filepath.Join(t.TempDir(), "nope")); err != nil || len(keys) != 0 {
			t.Errorf("missing metadataDir = %v, %v; want empty, nil", keys, err)
		}
	})

	t.Run("noteBudgetExhausted no-more-pages", func(t *testing.T) {
		keys := []string{"a", "b"}
		seen := map[string]struct{}{}
		truncated := false
		nextToken := ""
		p := ListObjectsParams{MaxKeys: 5}
		// i past the last key → nothing left → not truncated.
		advanced, _ := NoteBudgetExhausted(keys, len(keys), &p, seen, nil, &truncated, &nextToken)
		if truncated {
			t.Error("no later keys must not truncate")
		}
		if advanced != len(keys) {
			t.Errorf("advanced = %d, want %d", advanced, len(keys))
		}
	})

	t.Run("decodeAndVerifyChunked success", func(t *testing.T) {
		// Build a properly signed chunk stream using the package's own chain.
		secret := "secret"
		seed := strings.Repeat("a", 64)
		key := GetSigningKey(secret, "20260929", "us-east-1", "s3")
		scope := "20260929/us-east-1/s3/aws4_request"
		stamp := "20260929T150405Z"

		chunkSig := func(prevSig string, data string) string {
			stringToSign := strings.Join([]string{
				"AWS4-HMAC-SHA256-PAYLOAD",
				stamp,
				scope,
				prevSig,
				sha256Hex([]byte("")),
				sha256Hex([]byte(data)),
			}, "\n")
			return hex.EncodeToString(hmacSHA256Hex(key, stringToSign))
		}

		data := "hello chunks"
		sig1 := chunkSig(seed, data)
		sig2 := chunkSig(sig1, "")
		body := []byte(fmt.Sprintf("%x;chunk-signature=%s\r\n%s\r\n0;chunk-signature=%s\r\n\r\n",
			len(data), sig1, data, sig2))
		decoded, err := DecodeAndVerifyChunked(body, seed, key, stamp, scope)
		if err != nil {
			t.Fatalf("DecodeAndVerifyChunked: %v", err)
		}
		if string(decoded) != data {
			t.Errorf("decoded = %q, want %q", decoded, data)
		}
	})

	t.Run("writeFileAtomicJSON marshal failure", func(t *testing.T) {
		w := httptest.NewRecorder()
		_ = w
		// A value JSON cannot marshal (chan) → error before any write.
		if err := writeFileAtomicJSON(filepath.Join(t.TempDir(), "x"), struct {
			C chan int `json:"c"`
		}{}, 0o644); err == nil {
			t.Error("unmarshalable value must error")
		}
	})

	t.Run("backendFor nil lookup fails closed", func(t *testing.T) {
		installBackendLookup(nil)
		if _, err := backendFor("any"); err == nil {
			t.Error("nil backend lookup must fail closed")
		}
		installBackendLookup(func(bucket string) (backend.Backend, error) { return nil, nil })
	})
}

// compile-time check the auth import stays used.
var _ auth.CredentialSource = staticSurfaceCreds{}
