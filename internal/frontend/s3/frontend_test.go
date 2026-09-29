package s3_test

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"mini-s3/internal/frontend"
	"mini-s3/internal/frontend/s3"
)

// TestS3Frontend_ImplementsFrontend pins the frontend contract surface.
func TestS3Frontend_ImplementsFrontend(t *testing.T) {
	f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	var asFrontend frontend.Frontend = f

	if asFrontend.Name() != "s3" {
		t.Fatalf("Name() = %q, want %q", asFrontend.Name(), "s3")
	}
	if asFrontend.Handler() == nil {
		t.Fatal("Handler() = nil, want non-nil")
	}
	if asFrontend.Authenticator() == nil {
		t.Fatal("Authenticator() = nil, want non-nil")
	}
	caps := asFrontend.Capabilities()
	if !caps.Buckets || !caps.Multipart || !caps.PresignedURLs || !caps.ConditionalReads {
		t.Fatalf("Capabilities() = %+v, want Buckets/Multipart/PresignedURLs/ConditionalReads true", caps)
	}
	if caps.Versioning {
		t.Fatal("Capabilities().Versioning = true, want false (not implemented)")
	}
}

// TestS3Frontend_Conformance runs the reusable conformance suite against
// the s3 frontend (leaf-02 Task 4).
func TestS3Frontend_Conformance(t *testing.T) {
	f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}

// signedRequest builds a fully-signed header-auth request against the
// test server (same canonical rules as the production verifier).
func signedRequest(t *testing.T, method, target, body, accessKey, secret string) *http.Request {
	t.Helper()
	payloadHash := hashSHA256Hex([]byte(body))
	req, err := http.NewRequest(method, "https://localhost:8443"+target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "localhost:8443"
	now := time.Now().UTC()
	amzDate := now.Format(testISO8601)
	dateStamp := now.Format(testShortDate)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalURI := req.URL.EscapedPath()
	canonicalQuery := req.URL.RawQuery
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash}, "\n")
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, testRegion, testService)
	stringToSign := strings.Join([]string{testAlgorithm, amzDate, credentialScope, hashSHA256Hex([]byte(canonicalRequest))}, "\n")
	kDate := hmacSHA256Hex([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256Hex(kDate, testRegion)
	kService := hmacSHA256Hex(kRegion, testService)
	key := hmacSHA256Hex(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256Hex(key, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		accessKey, dateStamp, testRegion, testService, signedHeaders, signature))
	return req
}
