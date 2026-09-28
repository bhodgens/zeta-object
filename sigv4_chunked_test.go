package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sigv4_chunked_test.go — leaf 3.4: aws-chunked chunk-signature chain
// verification (decodeAndVerifyChunked) + authenticateRequest/handler wiring.

// signStreamingChunks builds a fully-signed aws-chunked body using the same
// helpers as the server. Returns the body bytes and the per-chunk signatures
// (including the final zero-size chunk).
func signStreamingChunks(t *testing.T, seedSig, timestamp, scope string, signingKey []byte, chunkData [][]byte) ([]byte, []string) {
	t.Helper()
	emptyHash := hashSHA256(nil)
	prev := seedSig
	var buf bytes.Buffer
	var sigs []string
	for _, data := range chunkData {
		sts := strings.Join([]string{
			chunkSigAlgorithm,
			timestamp,
			scope,
			prev,
			emptyHash,
			hashSHA256(data),
		}, "\n")
		sig := hex.EncodeToString(hmacSHA256(signingKey, sts))
		fmt.Fprintf(&buf, "%x;chunk-signature=%s\r\n", len(data), sig)
		buf.Write(data)
		if len(data) > 0 {
			buf.WriteString("\r\n")
		}
		sigs = append(sigs, sig)
		prev = sig
	}
	return buf.Bytes(), sigs
}

// streamingTestContext derives the timestamp/scope/signing-key/seed tuple for
// a "now" request against the server's real credentials.
func streamingTestContext() (timestamp, scope string, signingKey []byte, seed string) {
	now := time.Now().UTC()
	timestamp = now.Format(iso8601Format)
	dateStamp := now.Format(shortDateFormat)
	scope = fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, defaultRegion, serviceName)
	signingKey = getSigningKey(serverCredentials.SecretAccessKey, dateStamp, defaultRegion, serviceName)
	seed = hex.EncodeToString(hmacSHA256(signingKey, "seed-string-to-sign"))
	return timestamp, scope, signingKey, seed
}

// Valid 2-chunk signed body decodes and passes.
func TestDecodeAndVerifyChunked_TwoChunksValid(t *testing.T) {
	timestamp, scope, key, seed := streamingTestContext()
	body, _ := signStreamingChunks(t, seed, timestamp, scope, key,
		[][]byte{[]byte("hello "), []byte("world"), {}})

	decoded, err := decodeAndVerifyChunked(body, seed, key, timestamp, scope)
	if err != nil {
		t.Fatalf("decodeAndVerifyChunked error: %v", err)
	}
	if string(decoded) != "hello world" {
		t.Errorf("decoded = %q, want %q", string(decoded), "hello world")
	}
}

// Interoperability: the official AWS SigV4 streaming example (PUT Object,
// 20130524T000000Z, example credentials) must verify bit-for-bit.
func TestDecodeAndVerifyChunked_AWSGoldenVectors(t *testing.T) {
	const (
		secret    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		timestamp = "20130524T000000Z"
		scope     = "20130524/us-east-1/s3/aws4_request"
		seed      = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	)
	key := getSigningKey(secret, "20130524", "us-east-1", "s3")

	chunk1 := bytes.Repeat([]byte("a"), 65536)
	chunk2 := bytes.Repeat([]byte("a"), 1024)
	body, sigs := signStreamingChunks(t, seed, timestamp, scope, key,
		[][]byte{chunk1, chunk2, {}})

	want := []string{
		"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
	}
	for i, w := range want {
		if sigs[i] != w {
			t.Errorf("chunk %d signature = %s, want %s (AWS doc golden vector)", i+1, sigs[i], w)
		}
	}

	decoded, err := decodeAndVerifyChunked(body, seed, key, timestamp, scope)
	if err != nil {
		t.Fatalf("decodeAndVerifyChunked error: %v", err)
	}
	if len(decoded) != 66560 {
		t.Errorf("decoded length = %d, want 66560", len(decoded))
	}
}

// Corrupted chunk data (flip a byte, recompute nothing) must error.
func TestDecodeAndVerifyChunked_CorruptedChunkData(t *testing.T) {
	timestamp, scope, key, seed := streamingTestContext()
	body, _ := signStreamingChunks(t, seed, timestamp, scope, key,
		[][]byte{[]byte("hello "), []byte("world"), {}})

	// Flip one byte inside chunk 1's data. Chunk 1 header line is
	// "6;chunk-signature=<64>\r\n" then 6 data bytes; corrupt the last one.
	i := bytes.Index(body, []byte("hello "))
	corrupted := append([]byte{}, body...)
	corrupted[i+4] ^= 0x01 // "hello" → "helln" (bit flip)

	if _, err := decodeAndVerifyChunked(corrupted, seed, key, timestamp, scope); err == nil {
		t.Fatal("decodeAndVerifyChunked succeeded on corrupted chunk data, want error")
	}
}

// Wrong signature on chunk 2 (chunk 1 valid) must error.
func TestDecodeAndVerifyChunked_WrongSigOnChunk2(t *testing.T) {
	timestamp, scope, key, seed := streamingTestContext()
	body, sigs := signStreamingChunks(t, seed, timestamp, scope, key,
		[][]byte{[]byte("hello "), []byte("world"), {}})

	// Replace chunk 2's signature with the (valid) chunk 1 signature — a
	// wrong-but-well-formed value that breaks the chain at chunk 2.
	badSig := strings.Replace(string(body), sigs[1], sigs[0], 1)
	if badSig == string(body) {
		t.Fatal("test setup: failed to corrupt chunk 2 signature")
	}

	if _, err := decodeAndVerifyChunked([]byte(badSig), seed, key, timestamp, scope); err == nil {
		t.Fatal("decodeAndVerifyChunked succeeded with wrong signature on chunk 2, want error")
	}
}

// Truncated stream (final zero-size chunk missing) must error.
func TestDecodeAndVerifyChunked_MissingFinalChunk(t *testing.T) {
	timestamp, scope, key, seed := streamingTestContext()
	body, _ := signStreamingChunks(t, seed, timestamp, scope, key,
		[][]byte{[]byte("hello "), []byte("world"), {}})

	// Cut everything from the final chunk header line onwards.
	i := strings.LastIndex(string(body), "0;chunk-signature=")
	if i < 0 {
		t.Fatal("test setup: final chunk marker not found")
	}
	truncated := body[:i]

	if _, err := decodeAndVerifyChunked(truncated, seed, key, timestamp, scope); err == nil {
		t.Fatal("decodeAndVerifyChunked succeeded on truncated body, want error")
	}
}

// buildStreamingAuthRequest builds a fully-signed request whose body is a
// valid signed aws-chunked stream. The chunk signatures chain from the REAL
// header signature (extracted from the Authorization header that
// buildSignedRequest produces) — exactly what the server does. mangle, when
// non-nil, corrupts the encoded body after signing (expected result 403).
func buildStreamingAuthRequest(t *testing.T, chunkData [][]byte, mangle func([]byte) []byte, decodedLenHeader string) (*http.Request, []byte) {
	t.Helper()
	want := "200"
	if mangle != nil {
		want = "403"
	}
	req, _ := buildSignedRequest(t, map[string]string{
		"body":        "placeholder", // body is not part of the signed-streaming canonical request
		"payloadHash": streamingSignedPayload,
		"wantCode":    want,
	})
	req.Header.Set("Content-Encoding", "aws-chunked")
	if decodedLenHeader != "" {
		req.Header.Set("x-amz-decoded-content-length", decodedLenHeader)
	}

	// Seed = the signature of the header request.
	auth := req.Header.Get("Authorization")
	_, seed, ok := strings.Cut(auth, "Signature=")
	if !ok {
		t.Fatal("test setup: no Signature= in Authorization header")
	}

	// Scope/signing key are re-derived from the request's actual x-amz-date
	// (buildSignedRequest stamps "now").
	amzDate := req.Header.Get("x-amz-date")
	ts, err := time.Parse(iso8601Format, amzDate)
	if err != nil {
		t.Fatalf("test setup: bad x-amz-date: %v", err)
	}
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", ts.UTC().Format(shortDateFormat), defaultRegion, serviceName)
	key := getSigningKey(serverCredentials.SecretAccessKey, ts.UTC().Format(shortDateFormat), defaultRegion, serviceName)
	timestamp := ts.UTC().Format(iso8601Format)

	body, _ := signStreamingChunks(t, seed, timestamp, scope, key, chunkData)
	if mangle != nil {
		body = mangle(body)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return req, body
}

func TestAuthenticateRequest_StreamingSignedPayload(t *testing.T) {
	req, _ := buildStreamingAuthRequest(t, [][]byte{[]byte("hello "), []byte("world"), {}}, nil, "11")

	w := httptest.NewRecorder()
	if !authenticateRequest(w, req) {
		t.Fatalf("authenticateRequest failed: %d %s", w.Code, w.Body.String())
	}

	decoded, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reading replaced body: %v", err)
	}
	if string(decoded) != "hello world" {
		t.Errorf("decoded body = %q, want %q", string(decoded), "hello world")
	}
	if !isDecodedStreaming(req.Context()) {
		t.Error("isDecodedStreaming(context) = false, want true after streaming auth")
	}
}

// Corrupted stream (byte flip) is rejected with 403 SignatureDoesNotMatch.
func TestAuthenticateRequest_StreamingCorruptedRejected(t *testing.T) {
	req, body := buildStreamingAuthRequest(t, [][]byte{[]byte("hello "), []byte("world"), {}},
		func(b []byte) []byte {
			i := bytes.Index(b, []byte("hello "))
			b[i+4] ^= 0x01
			return b
		}, "11")

	w := httptest.NewRecorder()
	if authenticateRequest(w, req) {
		t.Fatal("authenticateRequest accepted corrupted streaming body")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body was %s)", w.Code, body[:0])
	}
}

// Decoded length mismatch vs x-amz-decoded-content-length is rejected (400).
func TestAuthenticateRequest_StreamingDecodedLengthMismatch(t *testing.T) {
	req, _ := buildStreamingAuthRequest(t, [][]byte{[]byte("hello "), []byte("world"), {}}, nil, "99")

	w := httptest.NewRecorder()
	if authenticateRequest(w, req) {
		t.Fatal("authenticateRequest accepted decoded-length mismatch")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// reSignStreamingPut recomputes the Authorization signature for a request
// originally built by buildSignedRequest (POST) after switching it to PUT,
// signing the raw (encoded) chunked body as the payload hash — the same
// canonical shape the real client uses for streaming PUTs.
func reSignStreamingPut(t *testing.T, req *http.Request) {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reSignStreamingPut: reading body: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))

	amzDate := req.Header.Get("x-amz-date")
	ts, err := time.Parse(iso8601Format, amzDate)
	if err != nil {
		t.Fatalf("reSignStreamingPut: bad x-amz-date: %v", err)
	}
	dateStamp := ts.UTC().Format(shortDateFormat)
	// Streaming canonical requests sign the streaming constant as the payload
	// hash (not the body hash) — mirrors the real AWS CLI wire format.
	payloadHash := streamingSignedPayload
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, streamingSignedPayload, amzDate)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.Path,
		"",
		canonicalHeaders,
		signedHeaders,
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
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization",
		fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
			serverCredentials.AccessKeyID, credentialScope, signedHeaders, signature))

	// The seed signature for the chunk chain is now the new header signature.
	seed := signature
	timestamp := ts.UTC().Format(iso8601Format)
	key := signingKey
	scope := credentialScope
	// Rebuild the chunked body chained from the new seed.
	body, _ := signStreamingChunks(t, seed, timestamp, scope, key,
		[][]byte{[]byte("hello "), []byte("world"), {}})
	req.Body = io.NopCloser(bytes.NewReader(body))
}

// Handler wiring integration: PUT with signed streaming payload through
// rootHandler stores the DECODED bytes, and the handler does not re-decode.
func TestRootHandler_StreamingPutIntegration(t *testing.T) {
	env := setupTestEnv(t)
	// buildSignedRequest (main_test.go, unmodifiable) signs for /bkt/obj, so
	// the integration request must target that exact bucket/key.
	env.setupBucket(t, "bkt")

	req, _ := buildStreamingAuthRequest(t, [][]byte{[]byte("hello "), []byte("world"), {}}, nil, "11")
	// Re-sign as PUT: buildSignedRequest hardcodes POST, so recompute the
	// canonical request + Authorization signature with the PUT method and a
	// PUT-sized payload hash (the raw chunked body hash).
	req.Method = http.MethodPut
	reSignStreamingPut(t, req)

	w := httptest.NewRecorder()
	rootHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rootHandler status = %d, want 200: %s", w.Code, w.Body.String())
	}

	path := filepath.Join(env.dataDir, "bkt", "obj")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stored object missing: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("stored bytes = %q, want decoded %q", string(got), "hello world")
	}
}

// The unsigned-trailer streaming constant still decodes through the strict
// unsigned path (no signature math) via the handler.
func TestPutObjectHandler_UnsignedTrailerStillDecodes(t *testing.T) {
	env := setupTestEnv(t)
	env.setupBucket(t, "unsigned-bucket")

	raw := "5;chunk-signature=abc\r\nhello\r\n5;chunk-signature=def\r\n worl\r\n0\r\n\r\n"
	req := httptest.NewRequest("PUT", "/unsigned-bucket/u.txt", strings.NewReader(raw))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "10")

	w := httptest.NewRecorder()
	putObjectHandler(w, req, "unsigned-bucket", "u.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("putObjectHandler status = %d, want 200: %s", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(env.dataDir, "unsigned-bucket", "u.txt"))
	if err != nil {
		t.Fatalf("stored object missing: %v", err)
	}
	if string(got) != "hello worl" {
		t.Errorf("stored bytes = %q, want %q", string(got), "hello worl")
	}
}
