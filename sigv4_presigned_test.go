package main

// sigv4_presigned_test.go — leaf 3.2: SigV4 presigned URL (query-string) auth.
//
// A presigned request carries X-Amz-* query params (Algorithm, Credential,
// Date, Expires, SignedHeaders, Signature) and no Authorization header. The
// canonical request uses UNSIGNED-PAYLOAD and excludes X-Amz-Signature from
// the canonical query string.

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// presignOpts controls how a presigned request is built (and corrupted).
type presignOpts struct {
	amzDate   time.Time  // signing time (default: now)
	expires   string     // X-Amz-Expires (default "300")
	accessKey string     // override credential access key (default: server's)
	dropParam string     // remove this X-Amz-* param entirely (missing-param case)
	extraQ    url.Values // appended AFTER signing (tamper case)
}

// buildPresignedRequest builds an httptest request with a correctly-signed
// presigned query string, per the leaf 3.2 canonical-request rules.
func buildPresignedRequest(t *testing.T, method, target string, body string, opts presignOpts) *http.Request {
	t.Helper()

	amzDate := time.Now().UTC()
	if !opts.amzDate.IsZero() {
		amzDate = opts.amzDate
	}
	expires := "300"
	if opts.expires != "" {
		expires = opts.expires
	}
	accessKey := serverCredentials.AccessKeyID
	if opts.accessKey != "" {
		accessKey = opts.accessKey
	}

	dateStamp := amzDate.UTC().Format(shortDateFormat)
	q := url.Values{
		"X-Amz-Algorithm":     {awsAlgorithm},
		"X-Amz-Credential":    {accessKey + "/" + dateStamp + "/" + defaultRegion + "/" + serviceName + "/aws4_request"},
		"X-Amz-Date":          {amzDate.UTC().Format(iso8601Format)},
		"X-Amz-Expires":       {expires},
		"X-Amz-SignedHeaders": {"host"},
	}
	if opts.dropParam != "" {
		q.Del(opts.dropParam)
	}

	// Canonical query: all params EXCEPT X-Amz-Signature, SigV4 escaping, sorted.
	var parts []string
	for _, k := range sortedKeysOf(q) {
		for _, v := range q[k] {
			parts = append(parts, canonicalQueryEscape(k)+"="+canonicalQueryEscape(v))
		}
	}
	canonicalQuery := strings.Join(parts, "&")

	const host = "localhost:8443"

	targetURL := mustParseURL(t, target)
	canonicalHeaders := fmt.Sprintf("host:%s\n", host)
	canonicalRequest := strings.Join([]string{
		method,
		targetURL.EscapedPath(),
		canonicalQuery,
		canonicalHeaders,
		"host",
		unsignedPayload,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, defaultRegion, serviceName)
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate.UTC().Format(iso8601Format),
		credentialScope,
		hashSHA256([]byte(canonicalRequest)),
	}, "\n")
	signingKey := getSigningKey(serverCredentials.SecretAccessKey, dateStamp, defaultRegion, serviceName)
	signature := hexEncode(hmacSHA256(signingKey, stringToSign))

	q.Set("X-Amz-Signature", signature)
	for k, vs := range opts.extraQ {
		for _, v := range vs {
			q.Add(k, v)
		}
	}

	full := target + "?" + q.Encode()
	req := httptest.NewRequest(method, full, strings.NewReader(body))
	req.Host = host
	return req
}

func sortedKeysOf(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	// insertion order does not matter; sort.Strings below
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad target %q: %v", raw, err)
	}
	return u
}

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

// runPresignedThroughRoot sends req through rootHandler and returns the
// recorder (status + body available to the caller).
func runPresignedThroughRoot(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	rootHandler(w, req)
	return w
}

func TestPresignedGET_HappyPath(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "presigned-body")

	req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})
	w := runPresignedThroughRoot(req)
	if w.Code != http.StatusOK {
		t.Fatalf("presigned GET: got status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "presigned-body" {
		t.Errorf("presigned GET body: got %q, want %q", got, "presigned-body")
	}
}

func TestPresignedGET_TamperedParam(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{
		extraQ: url.Values{"prefix": {"xyz"}},
	})
	w := runPresignedThroughRoot(req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tampered param: got status %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SignatureDoesNotMatch") {
		t.Errorf("tampered param: want SignatureDoesNotMatch in body, got: %s", w.Body.String())
	}
}

func TestPresignedGET_Expired(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{
		amzDate: time.Now().UTC().Add(-2 * time.Hour),
		expires: "1",
	})
	w := runPresignedThroughRoot(req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expired: got status %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "AccessDenied") || !strings.Contains(body, "expired") {
		t.Errorf("expired: want AccessDenied + expired message, got: %s", body)
	}
}

func TestPresignedGET_MissingParam(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{
		dropParam: "X-Amz-Expires",
	})
	w := runPresignedThroughRoot(req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing param: got status %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AuthorizationQueryParametersError") {
		t.Errorf("missing param: want AuthorizationQueryParametersError, got: %s", w.Body.String())
	}
}

func TestPresignedGET_WrongAccessKey(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{
		accessKey: "AKIAWRONGKEY",
	})
	w := runPresignedThroughRoot(req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong access key: got status %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidAccessKeyId") {
		t.Errorf("wrong access key: want InvalidAccessKeyId, got: %s", w.Body.String())
	}
}

func TestPresignedPUT_UnsignedPayload(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")

	req := buildPresignedRequest(t, "PUT", "/bkt/uploaded.bin", "put-payload", presignOpts{})
	w := runPresignedThroughRoot(req)
	if w.Code != http.StatusOK {
		t.Fatalf("presigned PUT: got status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(env.dataDir, "bkt", "uploaded.bin"))
	if err != nil {
		t.Fatalf("presigned PUT: object not stored: %v", err)
	}
	if string(data) != "put-payload" {
		t.Errorf("presigned PUT body: got %q, want %q", string(data), "put-payload")
	}
}

func TestHeaderAuthWins_OverPresignedParams(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	// Header-auth request that also carries a bogus X-Amz-Signature query
	// param. The header signature covers the param (header auth includes all
	// query params in the canonical query), so it only validates via header
	// auth — if presigned auth were chosen, the bogus signature would 403.
	target := "/bkt/hello.txt?X-Amz-Signature=deadbeef"
	now := time.Now().UTC()
	amzDate := now.Format(iso8601Format)
	dateStamp := now.Format(shortDateFormat)
	payloadHash := hashSHA256(nil)
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		"localhost:8443", payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		"GET",
		"/bkt/hello.txt",
		"X-Amz-Signature=deadbeef",
		canonicalHeaders,
		"host;x-amz-content-sha256;x-amz-date",
		payloadHash,
	}, "\n")
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, defaultRegion, serviceName)
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate,
		credentialScope,
		hashSHA256([]byte(canonicalRequest)),
	}, "\n")
	signingKey := getSigningKey(serverCredentials.SecretAccessKey, dateStamp, defaultRegion, serviceName)
	signature := hexEncode(hmacSHA256(signingKey, stringToSign))

	req := httptest.NewRequest("GET", target, nil)
	req.Host = "localhost:8443"
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s",
		serverCredentials.AccessKeyID, dateStamp, defaultRegion, serviceName, signature))

	w := runPresignedThroughRoot(req)
	if w.Code != http.StatusOK {
		t.Fatalf("header-auth-wins: got status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}
