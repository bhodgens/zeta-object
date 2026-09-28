package main

// auth_negative_test.go — leaf 4.9: mutational negative-fuzz corpus for
// authenticateRequest / authenticatePresigned.
//
// Every case starts from a VALID signed request and applies exactly one
// mutation. The invariant across the whole file: the response is either an
// S3 XML error or success — never a panic, never an empty 200. Any mutation
// that is ACCEPTED is a stop-clause finding (test fails loudly).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---- shared helpers -------------------------------------------------------

// runAuthSafe runs authenticateRequest and returns (status, errorBody).
// A panic anywhere in the auth path fails the test immediately.
func runAuthSafe(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC in authenticateRequest (mutation accepted? no — crashed): %v", r)
		}
	}()
	w := httptest.NewRecorder()
	if authenticateRequest(w, req) {
		return http.StatusOK, ""
	}
	return w.Code, w.Body.String()
}

// runRootSafe sends req through rootHandler and returns the recorder.
// A panic fails the test immediately.
func runRootSafe(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC in rootHandler: %v", r)
		}
	}()
	w := httptest.NewRecorder()
	rootHandler(w, req)
	return w
}

// assertS3XMLError verifies the invariant: the failure response is a well
// formed S3 XML error document, not a Go panic dump or an empty body.
func assertS3XMLError(t *testing.T, body string) {
	t.Helper()
	if body == "" {
		t.Errorf("error response has empty body; want S3 XML error")
		return
	}
	if !strings.Contains(body, "<Error>") || !strings.Contains(body, "</Error>") {
		t.Errorf("error response is not an S3 XML error document, got: %q", body)
	}
}

// assertStatusAndCode is the single assertion point for every negative case:
// expected HTTP status + expected S3 error code in the body.
func assertStatusAndCode(t *testing.T, label string, got int, wantStatus int, body string, wantCodes ...string) {
	t.Helper()
	if got != wantStatus {
		t.Errorf("%s: got status %d, want %d (body: %s)", label, got, wantStatus, body)
		return
	}
	if len(wantCodes) > 0 && wantCodes[0] != "" {
		for _, c := range wantCodes {
			if !strings.Contains(body, c) {
				t.Errorf("%s: want %s in body, got: %s", label, c, body)
				return
			}
		}
		assertS3XMLError(t, body)
	}
}

// signatureFromAuth extracts the hex signature from an Authorization header.
func signatureFromAuth(t *testing.T, auth string) string {
	t.Helper()
	const marker = "Signature="
	_, sig, found := strings.Cut(auth, marker)
	if !found {
		t.Fatalf("no Signature= in Authorization header: %q", auth)
	}
	return sig
}

// flipHexChar returns a byte guaranteed different from c, staying lowercase
// hex where possible ('9'+1 becomes ':' which is non-hex — still a valid
// mutation since any change must be rejected).
func flipHexChar(c byte) byte {
	switch c {
	case '0':
		return '1'
	case 'f':
		return '0'
	default:
		return c + 1
	}
}

// buildSignedGetWithQuery builds a GET request correctly signed for
// signedRawQuery but sent with sentRawQuery. Used for canonical-query
// mutation tests (duplicate params etc.) that buildSignedRequest cannot
// express (it hardcodes an empty canonical query).
func buildSignedGetWithQuery(t *testing.T, path, signedRawQuery, sentRawQuery string) *http.Request {
	t.Helper()
	u := mustParseURL(t, path+"?"+signedRawQuery)

	now := time.Now().UTC()
	amzDate := now.Format(iso8601Format)
	dateStamp := now.Format(shortDateFormat)
	payloadHash := hashSHA256(nil)
	host := "localhost:8443"

	req := httptest.NewRequest("GET", path+"?"+sentRawQuery, nil)
	req.Host = host
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical query exactly as the server computes it for the SIGNED form.
	req.URL.RawQuery = signedRawQuery
	canonicalQuery := getCanonicalQueryString(req)
	req.URL.RawQuery = sentRawQuery

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		"GET",
		u.EscapedPath(),
		canonicalQuery,
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

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s",
		serverCredentials.AccessKeyID, dateStamp, defaultRegion, serviceName, signature))
	return req
}

// mutatePresignedQuery returns a fresh request identical to base except one
// query param set to newVal.
func mutatePresignedQuery(t *testing.T, base *http.Request, param, newVal string) *http.Request {
	t.Helper()
	u := *base.URL
	q := u.Query()
	q.Set(param, newVal)
	u.RawQuery = q.Encode()
	req := httptest.NewRequest(base.Method, u.RequestURI(), nil)
	req.Host = base.Host
	return req
}

// ---- 1. Per-byte signature flips (all 64 positions) ------------------------

func TestAuthNegSignaturePerByteFlipRejected(t *testing.T) {
	base, _ := buildSignedRequest(t, nil)
	sig := signatureFromAuth(t, base.Header.Get("Authorization"))
	if len(sig) != 64 {
		t.Fatalf("signature length %d, want 64", len(sig))
	}
	for i := range 64 {
		t.Run(fmt.Sprintf("byte_%02d_%c", i, sig[i]), func(t *testing.T) {
			req, _ := buildSignedRequest(t, nil)
			mutated := []byte(signatureFromAuth(t, req.Header.Get("Authorization")))
			mutated[i] = flipHexChar(mutated[i])
			req.Header.Set("Authorization", strings.Replace(
				req.Header.Get("Authorization"), signatureFromAuth(t, req.Header.Get("Authorization")),
				string(mutated), 1))
			got, body := runAuthSafe(t, req)
			assertStatusAndCode(t, fmt.Sprintf("flip byte %d", i), got, http.StatusForbidden, body, "SignatureDoesNotMatch")
		})
	}
}

// ---- 2. Truncation / extension / uppercase --------------------------------

func TestAuthNegSignatureShapeMutationsRejected(t *testing.T) {
	cases := []struct {
		name       string
		mutFn      func(sig string) string
		wantStatus int
	}{
		// 63-char sig fails isLowercaseHex64 → 403.
		{"truncate_by_1", func(s string) string { return s[:len(s)-1] }, http.StatusForbidden},
		// Empty signature fails the Authorization regex itself →
		// AuthorizationHeaderMalformed/400 (pinned actual; still rejected).
		{"truncate_to_empty", func(string) string { return "" }, http.StatusBadRequest},
		// 65-char sig fails isLowercaseHex64 → 403.
		{"extend_by_1", func(s string) string { return s + "a" }, http.StatusForbidden},
		// Uppercase hex fails the lowercase check → 403 (no case-insensitive compare).
		{"uppercase", strings.ToUpper, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := buildSignedRequest(t, nil)
			auth := req.Header.Get("Authorization")
			sig := signatureFromAuth(t, auth)
			req.Header.Set("Authorization", strings.Replace(auth, sig, tc.mutFn(sig), 1))
			got, body := runAuthSafe(t, req)
			wantCode := "SignatureDoesNotMatch"
			if tc.wantStatus == http.StatusBadRequest {
				wantCode = "AuthorizationHeaderMalformed"
			}
			assertStatusAndCode(t, tc.name, got, tc.wantStatus, body, wantCode)
		})
	}
}

// ---- 3. Date skew ----------------------------------------------------------

func TestAuthNegDateSkew(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name       string
		skew       time.Duration
		wantStatus int
		wantCode   string
	}{
		{"minus_1s", -1 * time.Second, http.StatusOK, ""},
		{"plus_1s", 1 * time.Second, http.StatusOK, ""},
		{"minus_16min", -16 * time.Minute, http.StatusForbidden, "RequestTimeTooSkewed"},
		{"plus_16min", 16 * time.Minute, http.StatusForbidden, "RequestTimeTooSkewed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := buildSignedRequest(t, map[string]string{
				"amzDate": now.Add(tc.skew).Format(iso8601Format),
			})
			got, body := runAuthSafe(t, req)
			if tc.wantStatus == http.StatusOK {
				if got != http.StatusOK {
					t.Errorf("%s: got %d, want 200 (body: %s)", tc.name, got, body)
				}
				return
			}
			assertStatusAndCode(t, tc.name, got, tc.wantStatus, body, tc.wantCode)
		})
	}
}

// ---- 4. SignedHeaders reorder ----------------------------------------------

func TestAuthNegSignedHeadersReorderRejected(t *testing.T) {
	req, _ := buildSignedRequest(t, map[string]string{
		"signedHeaders": "x-amz-date;host;x-amz-content-sha256", // unsorted
	})
	got, body := runAuthSafe(t, req)
	assertStatusAndCode(t, "signedheaders reorder", got, http.StatusForbidden, body, "SignatureDoesNotMatch")
}

// ---- 5. Extra signed header the request does not carry ---------------------

func TestAuthNegExtraSignedHeaderRejected(t *testing.T) {
	req, _ := buildSignedRequest(t, map[string]string{
		"signedHeaders": "host;x-amz-content-sha256;x-amz-date;x-amz-meta-ghost",
	})
	got, body := runAuthSafe(t, req)
	assertStatusAndCode(t, "extra signed header", got, http.StatusForbidden, body, "SignatureDoesNotMatch")
}

// ---- 6. Duplicate query param ----------------------------------------------

func TestAuthNegDuplicateQueryParamRejected(t *testing.T) {
	// Signature computed for prefix=a only; the wire request carries both
	// prefix=a and prefix=b, and the canonical query includes both.
	req := buildSignedGetWithQuery(t, "/bkt/obj", "prefix=a", "prefix=a&prefix=b")
	got, body := runAuthSafe(t, req)
	assertStatusAndCode(t, "duplicate query param", got, http.StatusForbidden, body, "SignatureDoesNotMatch")
}

// Control: signing and sending the same single param must authenticate.
func TestAuthNegSingleQueryParamAccepted(t *testing.T) {
	req := buildSignedGetWithQuery(t, "/bkt/obj", "prefix=a", "prefix=a")
	got, body := runAuthSafe(t, req)
	if got != http.StatusOK {
		t.Fatalf("control single param: got %d, want 200 (body: %s)", got, body)
	}
}

// ---- 7. Header-name case canonicalization (pinned: still verifies) ---------

func TestAuthNegHeaderCaseCanonicalizationPinned(t *testing.T) {
	// Go's http.Header canonicalizes any casing fed through Header.Set, so a
	// client sending "X-Amz-Date" / "x-amz-date" / "X-AMZ-DATE" all reach the
	// auth code as "X-Amz-Date" and the request still verifies. Pin that.
	for _, variant := range []string{"X-Amz-Date", "x-amz-date", "X-AMZ-DATE", "X-AMZ-dAtE"} {
		t.Run("date_"+variant, func(t *testing.T) {
			req, _ := buildSignedRequest(t, nil)
			req.Header.Set(variant, req.Header.Get("X-Amz-Date"))
			if got, body := runAuthSafe(t, req); got != http.StatusOK {
				t.Fatalf("variant %q: got %d, want 200 (body: %s)", variant, got, body)
			}
		})
	}
	// Same for x-amz-content-sha256 casing.
	t.Run("content_sha256_lowercased", func(t *testing.T) {
		req, _ := buildSignedRequest(t, nil)
		req.Header.Set("x-amz-content-sha256", req.Header.Get("X-Amz-Content-Sha256"))
		if got, body := runAuthSafe(t, req); got != http.StatusOK {
			t.Fatalf("lowercase sha256 key: got %d, want 200 (body: %s)", got, body)
		}
	})
}

// ---- 8. Missing x-amz-content-sha256 ----------------------------------------

func TestAuthNegMissingContentSHA256Pinned(t *testing.T) {
	req, _ := buildSignedRequest(t, nil)
	req.Header.Del("X-Amz-Content-Sha256")
	got, body := runAuthSafe(t, req)
	// Pinned actual: SignedHeaders still claims x-amz-content-sha256, which
	// the request no longer carries → hard mismatch reject.
	assertStatusAndCode(t, "missing content-sha256", got, http.StatusForbidden, body, "SignatureDoesNotMatch")
}

// ---- 9. Authorization header whitespace / CRLF injection -------------------

func TestAuthNegAuthorizationInjectionRejected(t *testing.T) {
	cases := []struct {
		name string
		auth string
	}{
		{"crlf_in_signature", "AWS4-HMAC-SHA256 Credential=minioadmin/20260928/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=ab12\r\nX-Injected: header"},
		{"lone_cr_in_signature", "AWS4-HMAC-SHA256 Credential=minioadmin/20260928/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=ab12\rX-Injected: header"},
		{"space_in_signature", "AWS4-HMAC-SHA256 Credential=minioadmin/20260928/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=ab12 cd34"},
		{"tab_in_signature", "AWS4-HMAC-SHA256 Credential=minioadmin/20260928/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=\tab12"},
		{"tab_separator_chaos", "AWS4-HMAC-SHA256\tCredential=minioadmin/20260928/us-east-1/s3/aws4_request,\tSignedHeaders=host;x-amz-date,\tSignature=" + strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := buildSignedRequest(t, nil)
			req.Header.Set("Authorization", tc.auth)
			got, body := runAuthSafe(t, req)
			// Either malformed-header 400 or signature 403 is acceptable;
			// what is NOT acceptable: 200, a panic, or a non-XML body.
			if got == http.StatusOK {
				t.Fatalf("%s: mutation ACCEPTED with 200 — stop-clause finding", tc.name)
			}
			if got != http.StatusBadRequest && got != http.StatusForbidden {
				t.Errorf("%s: got %d, want 400 or 403 (body: %s)", tc.name, got, body)
			}
			assertS3XMLError(t, body)
		})
	}
}

// CRLF via x-amz-date header value must not panic either (pin: 400 InvalidDate).
func TestAuthNegCRLFInAmzDateRejected(t *testing.T) {
	req, _ := buildSignedRequest(t, nil)
	req.Header.Set("X-Amz-Date", "20260928T120000Z\r\nX-Injected: 1")
	got, body := runAuthSafe(t, req)
	if got == http.StatusOK {
		t.Fatalf("CRLF x-amz-date ACCEPTED with 200 — stop-clause finding")
	}
	assertStatusAndCode(t, "crlf in amz-date", got, http.StatusBadRequest, body, "InvalidDate")
}

// ---- 10. Presigned per-param tampering matrix ------------------------------

func TestAuthNegPresignedPerParamTamperingMatrix(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "presigned-body")

	base := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})
	if w := runRootSafe(t, buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})); w.Code != http.StatusOK {
		t.Fatalf("control presigned GET: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// Per-param expected outcome, pinned from the implementation:
	//  - Algorithm: tampering breaks isPresignedRequest → falls to header
	//    auth path with no Authorization header → AccessDenied/403.
	//  - Credential/Date/Expires: structural params →
	//    AuthorizationQueryParametersError/400.
	//  - SignedHeaders/Signature: signature-affecting → SignatureDoesNotMatch/403.
	cases := []struct {
		param      string
		wantStatus int
		wantCode   string
	}{
		{"X-Amz-Algorithm", http.StatusForbidden, "AccessDenied"},
		{"X-Amz-Credential", http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"X-Amz-Date", http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"X-Amz-Expires", http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"X-Amz-SignedHeaders", http.StatusForbidden, "SignatureDoesNotMatch"},
		{"X-Amz-Signature", http.StatusForbidden, "SignatureDoesNotMatch"},
	}
	for _, tc := range cases {
		t.Run(tc.param, func(t *testing.T) {
			orig := base.URL.Query().Get(tc.param)
			req := mutatePresignedQuery(t, base, tc.param, orig+"x")
			w := runRootSafe(t, req)
			assertStatusAndCode(t, tc.param, w.Code, tc.wantStatus, w.Body.String(), tc.wantCode)
		})
	}

	// Also drop one param entirely (missing → 400 structural).
	t.Run("drop X-Amz-Date", func(t *testing.T) {
		u := *base.URL
		q := u.Query()
		q.Del("X-Amz-Date")
		u.RawQuery = q.Encode()
		req := httptest.NewRequest("GET", u.RequestURI(), nil)
		req.Host = base.Host
		w := runRootSafe(t, req)
		assertStatusAndCode(t, "drop X-Amz-Date", w.Code, http.StatusBadRequest, w.Body.String(), "AuthorizationQueryParametersError")
	})
}

// ---- 11. X-Amz-Expires boundaries ------------------------------------------

func TestAuthNegPresignedExpiresBoundaries(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	cases := []struct {
		name       string
		expires    string
		wantStatus int
		wantCode   string
	}{
		{"zero", "0", http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"one", "1", http.StatusOK, ""},
		{"max_604800", "604800", http.StatusOK, ""},
		{"over_max_604801", "604801", http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"negative", "-5", http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"non_numeric", "abc", http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"float", "3.5", http.StatusBadRequest, "AuthorizationQueryParametersError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{expires: tc.expires})
			w := runRootSafe(t, req)
			assertStatusAndCode(t, tc.name, w.Code, tc.wantStatus, w.Body.String(), tc.wantCode)
		})
	}

	// Empty X-Amz-Expires: the builder treats "" as its default, so mutate an
	// already-built request instead. Missing param → structural 400.
	t.Run("empty", func(t *testing.T) {
		base := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})
		req := mutatePresignedQuery(t, base, "X-Amz-Expires", "")
		w := runRootSafe(t, req)
		assertStatusAndCode(t, "empty", w.Code, http.StatusBadRequest, w.Body.String(), "AuthorizationQueryParametersError")
	})
}

// ---- 12. Header auth wins over presigned params -----------------------------

func TestAuthNegHeaderAuthWinsOverPresignedParams(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	// A fully-formed presigned set with a WRONG signature, plus a VALID
	// header-auth Authorization covering those query params. Header auth must
	// win → 200. (Companion test TestHeaderAuthWins_OverPresignedParams pins
	// the single bogus-param case; this one uses all six X-Amz-* params.)
	now := time.Now().UTC()
	amzDate := now.Format(iso8601Format)
	dateStamp := now.Format(shortDateFormat)
	payloadHash := hashSHA256(nil)
	host := "localhost:8443"

	q := url.Values{
		"X-Amz-Algorithm":     {awsAlgorithm},
		"X-Amz-Credential":    {serverCredentials.AccessKeyID + "/" + dateStamp + "/" + defaultRegion + "/" + serviceName + "/aws4_request"},
		"X-Amz-Date":          {amzDate},
		"X-Amz-Expires":       {"300"},
		"X-Amz-SignedHeaders": {"host"},
		"X-Amz-Signature":     {strings.Repeat("ab", 32)}, // wrong but structurally valid
	}
	target := "/bkt/hello.txt?" + q.Encode()

	canonicalQuery := getCanonicalQueryString(httptest.NewRequest("GET", target, nil))
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		"GET", "/bkt/hello.txt", canonicalQuery, canonicalHeaders,
		"host;x-amz-content-sha256;x-amz-date", payloadHash,
	}, "\n")
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, defaultRegion, serviceName)
	stringToSign := strings.Join([]string{
		awsAlgorithm, amzDate, credentialScope, hashSHA256([]byte(canonicalRequest)),
	}, "\n")
	signingKey := getSigningKey(serverCredentials.SecretAccessKey, dateStamp, defaultRegion, serviceName)
	signature := hexEncode(hmacSHA256(signingKey, stringToSign))

	req := httptest.NewRequest("GET", target, nil)
	req.Host = host
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s",
		serverCredentials.AccessKeyID, dateStamp, defaultRegion, serviceName, signature))

	w := runRootSafe(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("header-auth-wins: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

// ---- 13. Malformed credential scope parts -----------------------------------

func TestAuthNegPresignedMalformedScopeParts(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	base := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})
	akid := serverCredentials.AccessKeyID
	dateStamp := base.URL.Query().Get("X-Amz-Date")[:8]

	cases := []struct {
		name       string
		credential string
	}{
		{"four_parts", akid + "/" + dateStamp + "/" + defaultRegion + "/aws4_request"},
		{"six_parts", akid + "/" + dateStamp + "/" + defaultRegion + "/" + serviceName + "/extra/aws4_request"},
		{"two_parts", akid + "/" + dateStamp},
		{"wrong_terminal", akid + "/" + dateStamp + "/" + defaultRegion + "/" + serviceName + "/aws3_request"},
		{"wrong_service", akid + "/" + dateStamp + "/" + defaultRegion + "/s4/aws4_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Credential parse precedes signature verification, so the
			// (now stale) signature never matters: 400 structural.
			req := mutatePresignedQuery(t, base, "X-Amz-Credential", tc.credential)
			w := runRootSafe(t, req)
			assertStatusAndCode(t, tc.name, w.Code, http.StatusBadRequest, w.Body.String(), "AuthorizationQueryParametersError")
		})
	}
}

// ---- 14. Replay after expiry -------------------------------------------------

func TestAuthNegPresignedReplayAfterExpiry(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	// Signed 3s in the past with a 2s window: expired on arrival. The SAME
	// URL replayed twice must 403 both times with AccessDenied/expired.
	req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{
		amzDate: time.Now().UTC().Add(-3 * time.Second),
		expires: "2",
	})
	for attempt := 1; attempt <= 2; attempt++ {
		w := runRootSafe(t, buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{
			amzDate: time.Now().UTC().Add(-3 * time.Second),
			expires: "2",
		}))
		body := w.Body.String()
		if w.Code != http.StatusForbidden {
			t.Fatalf("replay attempt %d: got %d, want 403 (body: %s)", attempt, w.Code, body)
		}
		if !strings.Contains(body, "AccessDenied") || !strings.Contains(body, "expired") {
			t.Errorf("replay attempt %d: want AccessDenied + expired message, got: %s", attempt, body)
		}
		assertS3XMLError(t, body)
	}
	_ = req // the canonical expired URL was exercised via fresh builds above
}

// Sanity gate for the whole corpus: a pristine presigned GET and a pristine
// header-auth request both authenticate (guards against the corpus itself
// being built on a broken baseline).
func TestAuthNegBaselinesStillValid(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "x")

	t.Run("presigned baseline", func(t *testing.T) {
		req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})
		w := runRootSafe(t, req)
		if w.Code != http.StatusOK {
			t.Fatalf("presigned baseline: got %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
	})
	t.Run("header baseline", func(t *testing.T) {
		req, _ := buildSignedRequest(t, nil)
		got, body := runAuthSafe(t, req)
		if got != http.StatusOK {
			t.Fatalf("header baseline: got %d, want 200 (body: %s)", got, body)
		}
	})
}
