package s3_test

// region_sigv4_test.go — the strict/permissive region matrix for the SigV4
// header and presigned verification paths (region-config-2026-10 leaf 02,
// Contract 2). Four cells per path:
//
//	explicit eu-west-1 + client eu-west-1  -> OK
//	explicit eu-west-1 + client us-east-1  -> FAIL naming eu-west-1 (expected)
//	default mode   + client us-east-1      -> OK (today's behavior)
//	default mode   + client eu-west-1      -> OK with a notice naming the
//	                                          client's region
//
// plus a sanity cell: a non-well-formed client region token is rejected in
// default mode (the permissive escape hatch requires [a-z0-9-]+).

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// buildPresignedRequestRegionHelper constructs a presigned (query-auth)
// request signed for an explicit client region (region-config-2026-10
// leaf 02 matrix).
func buildPresignedRequestRegionHelper(t *testing.T, accessKey, secret, region string) *http.Request {
	t.Helper()
	amzDate := time.Now().UTC()
	dateStamp := amzDate.Format(testShortDate)

	host := "localhost:8443"
	canonicalURI := "/bkt/obj.txt"
	signedHeaders := "host"
	canonicalHeaders := fmt.Sprintf("host:%s\n", host)
	canonicalQueryString := "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + accessKey + "%2F" + dateStamp + "%2F" + region + "%2Fs3%2Faws4_request&X-Amz-Date=" + amzDate.Format(testISO8601) + "&X-Amz-Expires=300&X-Amz-SignedHeaders=host"
	canonicalRequest := strings.Join([]string{
		"GET",
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, testService)
	stringToSign := strings.Join([]string{
		testAlgorithm,
		amzDate.Format(testISO8601),
		credentialScope,
		hashSHA256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256Hex([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256Hex(kDate, region)
	kService := hmacSHA256Hex(kRegion, testService)
	key := hmacSHA256Hex(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256Hex(key, stringToSign))

	u := fmt.Sprintf("https://%s%s?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=%s%%2F%s%%2F%s%%2Fs3%%2Faws4_request&X-Amz-Date=%s&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=%s",
		host, canonicalURI, accessKey, dateStamp, region, amzDate.Format(testISO8601), signature)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatalf("presigned request: %v", err)
	}
	req.Host = host
	return req
}

// captureLog swaps the standard logger's output for a buffer and returns a
// restore func plus a pointer to the buffer.
func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	return &buf, func() { log.SetOutput(os.Stderr) }
}

// regionMatrix runs the four Contract 2 cells (plus the malformed-token
// sanity cell) against makeReq(region) through the given authenticator.
func regionMatrix(t *testing.T, makeReq func(t *testing.T, clientRegion string) *http.Request) {
	t.Helper()
	cells := []struct {
		name         string
		serverRegion string // SetRegion value; "" = default (permissive) mode
		clientRegion string
		wantOK       bool
		wantNotice   bool // one-line notice naming the client's region
	}{
		{name: "explicit eu-west-1 + client eu-west-1 -> OK", serverRegion: "eu-west-1", clientRegion: "eu-west-1", wantOK: true},
		{name: "explicit eu-west-1 + client us-east-1 -> FAIL naming eu-west-1", serverRegion: "eu-west-1", clientRegion: "us-east-1", wantOK: false},
		{name: "default mode + client us-east-1 -> OK", serverRegion: "", clientRegion: "us-east-1", wantOK: true},
		{name: "default mode + client eu-west-1 -> OK with notice", serverRegion: "", clientRegion: "eu-west-1", wantOK: true, wantNotice: true},
		{name: "default mode + malformed client region token -> FAIL", serverRegion: "", clientRegion: "EU.West.1", wantOK: false},
	}
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			s3.SetRegion(c.serverRegion)
			defer s3.SetRegion("")

			f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
			a := f.Authenticator()

			buf, restoreLog := captureLog(t)
			defer restoreLog()

			id, err := a.Authenticate(makeReq(t, c.clientRegion))
			if c.wantOK {
				if err != nil {
					t.Fatalf("Authenticate() err = %v, want OK (server region %q, client region %q)", err, c.serverRegion, c.clientRegion)
				}
				if id.AccessKeyID != "" && id.AccessKeyID != "minioadmin" {
					t.Errorf("identity AccessKeyID = %q, want minioadmin", id.AccessKeyID)
				}
			} else {
				if err == nil {
					t.Fatalf("Authenticate() = OK, want failure (server region %q, client region %q)", c.serverRegion, c.clientRegion)
				}
				if !strings.Contains(err.Error(), "eu-west-1") && c.serverRegion == "eu-west-1" {
					t.Errorf("failure message %q does not name the expected region eu-west-1", err.Error())
				}
			}

			if c.wantNotice {
				if !strings.Contains(buf.String(), c.clientRegion) {
					t.Errorf("permissive-mode notice not logged naming client region %q; log:\n%s", c.clientRegion, buf.String())
				}
			} else if c.clientRegion != c.serverRegion && c.serverRegion != "" {
				// Strict-mode mismatch must not fire the permissive notice.
				if strings.Contains(buf.String(), "permissive") {
					t.Errorf("permissive notice fired in strict mode; log:\n%s", buf.String())
				}
			}
		})
	}
}

// TestRegionMatrix_Header pins Contract 2 on the header-form (Authorization
// header) SigV4 path.
func TestRegionMatrix_Header(t *testing.T) {
	regionMatrix(t, func(t *testing.T, clientRegion string) *http.Request {
		return buildSignedRequestHelper(t, "minioadmin", "minioadmin", map[string]string{"region": clientRegion})
	})
}

// TestRegionMatrix_Presigned pins Contract 2 on the presigned (query-auth)
// SigV4 path (authenticatePresignedRequest).
func TestRegionMatrix_Presigned(t *testing.T) {
	regionMatrix(t, func(t *testing.T, clientRegion string) *http.Request {
		return buildPresignedRequestRegionHelper(t, "minioadmin", "minioadmin", clientRegion)
	})
}

// TestRegionMatrix_StrictMismatchCode pins the failure shape for the strict
// mismatch cell: SignatureDoesNotMatch with a message naming the expected
// region (parent master.md Contract 2 / leaf Goal).
func TestRegionMatrix_StrictMismatchCode(t *testing.T) {
	s3.SetRegion("eu-west-1")
	defer s3.SetRegion("")

	f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	a := f.Authenticator()

	_, err := a.Authenticate(buildSignedRequestHelper(t, "minioadmin", "minioadmin", map[string]string{"region": "us-east-1"}))
	if err == nil {
		t.Fatal("Authenticate() = OK, want SignatureDoesNotMatch")
	}
	if !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("error = %q, want code SignatureDoesNotMatch", err.Error())
	}
	if !strings.Contains(err.Error(), "eu-west-1") {
		t.Errorf("error = %q, want message naming the expected region eu-west-1", err.Error())
	}
}
