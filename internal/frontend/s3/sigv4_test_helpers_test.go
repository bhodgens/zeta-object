package s3_test

// sigv4_test_helpers_test.go — canonical-request construction for the
// s3 package's own tests (migrated from package main's sigv4 test
// helpers during the leaf-02 move).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testISO8601   = "20060102T150405Z"
	testShortDate = "20060102"
	testRegion    = "us-east-1"
	testService   = "s3"
	testAlgorithm = "AWS4-HMAC-SHA256"
)

func hashSHA256Hex(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func hmacSHA256Hex(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256Hex([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256Hex(kDate, region)
	kService := hmacSHA256Hex(kRegion, service)
	return hmacSHA256Hex(kService, "aws4_request")
}

// buildSignedRequestHelper constructs a header-signed SigV4 request.
// opts may override: amzDate, region, signature, payloadHash.
func buildSignedRequestHelper(t *testing.T, accessKey, secret string, opts map[string]string) *http.Request {
	t.Helper()
	body := "hello world"
	payloadHash := hashSHA256Hex([]byte(body))
	if v, ok := opts["payloadHash"]; ok {
		payloadHash = v
	}

	req := httptest.NewRequest("POST", "/bkt/obj", strings.NewReader(body))
	req.Host = "localhost:8443"

	now := time.Now().UTC()
	amzDate := now.Format(testISO8601)
	dateStamp := now.Format(testShortDate)
	if v, ok := opts["amzDate"]; ok {
		amzDate = v
		if ts, err := time.Parse(testISO8601, v); err == nil {
			dateStamp = ts.UTC().Format(testShortDate)
		}
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	region := testRegion
	if v, ok := opts["region"]; ok {
		region = v
	}

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, payloadHash, amzDate)

	canonicalRequest := strings.Join([]string{
		"POST",
		"/bkt/obj",
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scopeDate := dateStamp
	if v, ok := opts["scopeDate"]; ok {
		scopeDate = v
	}
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", scopeDate, region, testService)
	stringToSign := strings.Join([]string{
		testAlgorithm,
		amzDate,
		credentialScope,
		hashSHA256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := signingKey(secret, scopeDate, region, testService)
	signature := hex.EncodeToString(hmacSHA256Hex(key, stringToSign))
	if v, ok := opts["signature"]; ok {
		signature = v
	}

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		accessKey, scopeDate, region, testService, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
	return req
}

// buildPresignedRequestHelper constructs a presigned (query-auth) request.
// expired=true backdates X-Amz-Date beyond X-Amz-Expires.
func buildPresignedRequestHelper(t *testing.T, accessKey, secret string, expired bool) *http.Request {
	t.Helper()
	amzDate := time.Now().UTC()
	if expired {
		amzDate = amzDate.Add(-25 * time.Hour)
	}
	dateStamp := amzDate.Format(testShortDate)

	host := "localhost:8443"
	canonicalURI := "/bkt/obj.txt"
	signedHeaders := "host"
	canonicalHeaders := fmt.Sprintf("host:%s\n", host)
	canonicalRequest := strings.Join([]string{
		"GET",
		canonicalURI,
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + accessKey + "%2F" + dateStamp + "%2F" + testRegion + "%2Fs3%2Faws4_request&X-Amz-Date=" + amzDate.Format(testISO8601) + "&X-Amz-Expires=300&X-Amz-SignedHeaders=host",
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, testRegion, testService)
	stringToSign := strings.Join([]string{
		testAlgorithm,
		amzDate.Format(testISO8601),
		credentialScope,
		hashSHA256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := signingKey(secret, dateStamp, testRegion, testService)
	signature := hex.EncodeToString(hmacSHA256Hex(key, stringToSign))

	u := fmt.Sprintf("https://%s%s?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=%s%%2F%s%%2F%s%%2Fs3%%2Faws4_request&X-Amz-Date=%s&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=%s",
		host, canonicalURI, accessKey, dateStamp, testRegion, amzDate.Format(testISO8601), signature)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatalf("presigned request: %v", err)
	}
	req.Host = host
	return req
}

var _ = strconv.Itoa // keep strconv if helpers shrink
