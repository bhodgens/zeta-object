package s3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sigv4.go — AWS Signature Version 4 request authentication

// Regex for parsing the AWS V4 Authorization header (fix 10: tolerate optional
// whitespace after commas). Defined here rather than config.go; config.go's
// authHeaderRegex is superseded by this one (see leaf 2.2 cross-file note).
var authHeaderRegexTolerant = regexp.MustCompile(
	`^AWS4-HMAC-SHA256\s+Credential=([^/]+)/([^/]+)/([^/]+)/s3/aws4_request\s*,\s*SignedHeaders=([^,]+)\s*,\s*Signature=(\S+)\s*$`,
)

// canonicalWSRegex matches sequential whitespace inside header values (fix 3).
var canonicalWSRegex = regexp.MustCompile(`\s+`)

// Known STREAMING-* x-amz-content-sha256 constants we accept (fix 6).
var streamingPayloadAllowlist = map[string]bool{
	"STREAMING-UNSIGNED-PAYLOAD-TRAILER": true,
	"STREAMING-AWS4-HMAC-SHA256-PAYLOAD": true,
}

// Leaf 3.4: the x-amz-content-sha256 constant selecting signed chunked
// streaming (chunk-signature chain verified), and the chunk string-to-sign
// algorithm prefix from the AWS SigV4 streaming spec.
const (
	streamingSignedPayload = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	chunkSigAlgorithm      = "AWS4-HMAC-SHA256-PAYLOAD"
)

// Exported chunk-streaming constants.
const (
	StreamingSignedPayload = streamingSignedPayload
	ChunkSigAlgorithm      = chunkSigAlgorithm
	EmptyPayloadSHA256     = emptyPayloadSHA256
)

// decodedStreamingContextKey is the request-context key marking a body that
// authenticateRequest already decoded (and, for the signed variant,
// signature-verified) so handlers skip their own aws-chunked decode pass.
type decodedStreamingContextKey struct{}

// withDecodedStreaming marks a request context as carrying an already-decoded
// aws-chunked body.
func withDecodedStreaming(ctx context.Context) context.Context {
	return context.WithValue(ctx, decodedStreamingContextKey{}, true)
}

// isDecodedStreaming reports whether the request context was marked by
// withDecodedStreaming (body already decoded — and for
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD, signature-verified — during auth).
func isDecodedStreaming(ctx context.Context) bool {
	v, _ := ctx.Value(decodedStreamingContextKey{}).(bool)
	return v
}

// debugAuthEnabled reports whether verbose auth debugging is on (fix 9).
func debugAuthEnabled() bool {
	if os.Getenv("ZETAOBJECT_DEBUG_AUTH") == "1" {
		return true
	}
	// Deprecated-prefix fallback (bughunt H3).
	return os.Getenv("ZETAOBJECT_DEBUG_AUTH") == "1"
}

// VerifyDecodedLength checks a decoded body length against the
// x-amz-decoded-content-length header value (fix 5c). Empty header = not
// enforced. Call-site wiring in object/multipart handlers is leaf 2.4's job.
func VerifyDecodedLength(header string, got int) error {
	if header == "" {
		return nil
	}
	want, err := strconv.Atoi(header)
	if err != nil {
		return fmt.Errorf("invalid x-amz-decoded-content-length %q: %w", header, err)
	}
	if want != got {
		return fmt.Errorf("decoded content length mismatch: header says %d, got %d", want, got)
	}
	return nil
}

// decodeAWSChunked decodes aws-chunked Content-Encoding used by AWS CLI v2.
// Format: <hex-size>;chunk-signature=...\r\n<data>\r\n, ending with 0\r\n<trailers>\r\n\r\n
// Errors on: truncated streams (EOF before the 0-size final chunk), and
// corrupt chunk-size lines that are not valid hex (fix 5a/5b).
func decodeAWSChunked(body []byte) ([]byte, error) {
	var result bytes.Buffer
	reader := bufio.NewReader(bytes.NewReader(body))

	for {
		// Read the chunk header line
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// EOF before the final 0-size chunk: truncated stream (fix 5a)
				return nil, fmt.Errorf("truncated aws-chunked body: EOF before final zero-size chunk")
			}
			return nil, fmt.Errorf("error reading chunk header: %w", err)
		}

		// Parse chunk size (format: "<hex>;chunk-signature=..." or just "<hex>")
		line = strings.TrimSpace(line)
		if line == "" {
			continue // Skip empty lines
		}
		parts := strings.SplitN(line, ";", 2)
		sizeStr := strings.TrimSpace(parts[0])

		chunkSize, err := strconv.ParseInt(sizeStr, 16, 64)
		if err != nil {
			// Not valid hex: corrupt stream, not a skip-worthy trailer (fix 5b)
			return nil, fmt.Errorf("corrupt aws-chunked body: invalid chunk size %q", sizeStr)
		}

		if chunkSize == 0 {
			// Final chunk - trailers follow; we are done.
			break
		}

		// Read the chunk data
		chunkData := make([]byte, chunkSize)
		if _, err := io.ReadFull(reader, chunkData); err != nil {
			return nil, fmt.Errorf("truncated aws-chunked body: error reading chunk data: %w", err)
		}
		result.Write(chunkData)

		// Read the trailing \r\n after chunk data
		if _, err := reader.ReadString('\n'); err == io.EOF {
			return nil, fmt.Errorf("truncated aws-chunked body: EOF after chunk data")
		}
	}

	return result.Bytes(), nil
}

// emptyPayloadSHA256 is the well-known hex(sha256("")) used as the fourth
// line of every chunk string-to-sign per the AWS SigV4 streaming spec.
const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// decodeAndVerifyChunked decodes an aws-chunked body and verifies the
// SigV4 chunk-signature chain (leaf 3.4).
//
// Framing: `<hex-size>;chunk-signature=<64hex>\r\n<data>\r\n` repeated, ending
// with a final zero-size chunk `0;chunk-signature=<64hex>\r\n` (trailers may
// follow). seedSignature is the signature of the header request (the
// Authorization header signature); chunk signatures chain from it.
//
// Per-chunk string-to-sign (verified against the AWS "Transferring payload in
// multiple chunks" example vectors):
//
//	AWS4-HMAC-SHA256-PAYLOAD\n
//	<timestamp>\n
//	<scope>\n
//	<prev-sig>\n
//	<hex(sha256(""))>\n
//	<hex(sha256(chunk-data))>
//
// i.e. SIX newline-separated lines with NO empty line (the leaf plan sketched
// an empty line between prev-sig and the empty-hash; the AWS spec and its
// published golden vectors have none — AWS wins, deviation noted in the leaf
// report). Each chunk signature is compared constant-time via hmac.Equal on
// the raw HMAC bytes.
//
// Errors on: any chunk signature mismatch, a missing/malformed chunk
// signature field, a truncated stream (no final zero-size chunk), or corrupt
// framing. The caller checks the size sum against x-amz-decoded-content-length
// via VerifyDecodedLength.
func decodeAndVerifyChunked(body []byte, seedSignature string, signingKey []byte, timestamp string, scope string) ([]byte, error) {
	var result bytes.Buffer
	reader := bufio.NewReader(bytes.NewReader(body))

	prevSig := seedSignature

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("truncated aws-chunked body: EOF before final zero-size chunk")
			}
			return nil, fmt.Errorf("error reading chunk header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}

		// Parse "<hex-size>;chunk-signature=<64hex>".
		parts := strings.SplitN(line, ";", 2)
		sizeStr := strings.TrimSpace(parts[0])
		chunkSize, err := strconv.ParseInt(sizeStr, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("corrupt aws-chunked body: invalid chunk size %q", sizeStr)
		}
		if len(parts) != 2 {
			return nil, fmt.Errorf("corrupt aws-chunked body: missing chunk-signature field")
		}
		sigParts := strings.SplitN(strings.TrimSpace(parts[1]), "=", 2)
		if len(sigParts) != 2 || strings.TrimSpace(sigParts[0]) != "chunk-signature" {
			return nil, fmt.Errorf("corrupt aws-chunked body: malformed chunk-signature field %q", sizeStr) //nolint:gosec // G706: constant prefix + size string
		}
		chunkSig := strings.TrimSpace(sigParts[1])
		if !isLowercaseHex64(chunkSig) {
			return nil, fmt.Errorf("corrupt aws-chunked body: chunk signature is not 64 lowercase hex chars")
		}

		// Read the chunk data.
		chunkData := make([]byte, chunkSize)
		if _, err := io.ReadFull(reader, chunkData); err != nil {
			return nil, fmt.Errorf("truncated aws-chunked body: error reading chunk data: %w", err)
		}

		// Verify this chunk's signature BEFORE accepting its data.
		stringToSign := strings.Join([]string{
			chunkSigAlgorithm,
			timestamp,
			scope,
			prevSig,
			emptyPayloadSHA256,
			hashSHA256(chunkData),
		}, "\n")
		expected := hmacSHA256(signingKey, stringToSign)
		provided, err := hex.DecodeString(chunkSig)
		if err != nil || !hmac.Equal(expected, provided) {
			return nil, fmt.Errorf("chunk signature mismatch at offset %d", result.Len())
		}
		prevSig = chunkSig

		if chunkSize == 0 {
			// Final chunk verified; trailers may follow — we are done.
			break
		}

		result.Write(chunkData)

		// Consume the trailing \r\n after the chunk data.
		if _, err := reader.ReadString('\n'); err == io.EOF {
			return nil, fmt.Errorf("truncated aws-chunked body: EOF after chunk data")
		}
	}

	return result.Bytes(), nil
}

// SigV4 helper functions
func hashSHA256(data []byte) string {
	hasher := sha256.New()
	hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func getSigningKey(secretKey, dateStamp, region, serviceName string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, serviceName)
	kSigning := hmacSHA256(kService, "aws4_request")
	return kSigning
}

func getCanonicalURI(r *http.Request) string {
	// Normalize path according to S3 rules (e.g. remove multiple slashes, handle dot segments if necessary)
	// For simplicity, using r.URL.Path. Clients should send a pre-normalized path.
	// S3 requires that the path be URI-encoded.
	// If r.URL.RawPath is available and correctly encoded by client, it might be better.
	// Otherwise, ensure r.URL.Path is what's expected.
	// For a bucket operation (e.g. /mybucket/), path is /mybucket/
	// For root (ListBuckets), path is /
	if r.URL.Path == "" {
		return "/"
	}
	// S3 URI encoding: No normalization of /./ or /../ segments in the path itself for signature.
	// However, query parameters are handled separately.
	// Go's http.Request.URL.Path is already decoded. For SigV4, we need the URI-encoded path as sent by client.
	// If r.URL.RawPath is empty, it means the path was not escaped or was "/"
	// This part can be tricky. AWS SDKs handle this. For a minimal server, we might assume client sends correctly escaped path.
	// Use r.URL.EscapedPath() if available and non-empty, otherwise r.URL.Path.
	escapedPath := r.URL.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"                          // Default for empty path
		if r.URL.Path != "" && r.URL.Path != "/" { // If path was not empty but RawPath was, re-escape (basic)
			escapedPath = (&url.URL{Path: r.URL.Path}).RequestURI() // This re-encodes based on Path
		}
	}
	return escapedPath
}

// canonicalQueryEscape encodes a query key/value per SigV4 rules: spaces as
// %20 (not +), everything else via url.QueryEscape (fix 2).
func canonicalQueryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func getCanonicalQueryString(r *http.Request) string {
	return getCanonicalQueryStringExcluding(r, "")
}

// getCanonicalQueryStringExcluding builds the SigV4 canonical query string
// from all query params, sorted, with %20 escaping. Params whose lowercase
// name equals exclude (e.g. "x-amz-signature" for presigned auth) are
// omitted. Header auth (getCanonicalQueryString) behaves identically to
// before — the exclusion is only used by the presigned path.
func getCanonicalQueryStringExcluding(r *http.Request, exclude string) string {
	queryParams := r.URL.Query()
	if len(queryParams) == 0 {
		return ""
	}

	var sortedKeys []string
	for k := range queryParams {
		if exclude != "" && strings.EqualFold(k, exclude) {
			continue
		}
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	var canonicalParams []string
	for _, k := range sortedKeys {
		values := queryParams[k]
		sort.Strings(values) // Sort values for the same key
		for _, v := range values {
			// S3 requires both key and value to be URI encoded with %20 for spaces.
			canonicalParams = append(canonicalParams, canonicalQueryEscape(k)+"="+canonicalQueryEscape(v))
		}
	}
	return strings.Join(canonicalParams, "&")
}

func getCanonicalHeaders(r *http.Request, signedHeaderNames []string) (string, string) {
	var canonicalHeaders strings.Builder

	// Create a map for quick lookup of signed headers
	signedHeadersMap := make(map[string]bool)
	for _, h := range signedHeaderNames {
		signedHeadersMap[strings.ToLower(h)] = true
	}

	var actualSignedHeadersForOutput []string
	var headerPairs [][2]string

	// Handle the 'host' header specially - Go stores it in r.Host, not r.Header
	if signedHeadersMap["host"] && r.Host != "" {
		headerPairs = append(headerPairs, [2]string{"host", r.Host})
		actualSignedHeadersForOutput = append(actualSignedHeadersForOutput, "host")
	}

	for name, values := range r.Header {
		lowerName := strings.ToLower(name)
		// Skip 'host' since we handled it above
		if lowerName == "host" {
			continue
		}
		if signedHeadersMap[lowerName] {
			var processedValues []string
			for _, v := range values {
				// Fix 3: trim edges AND collapse sequential internal whitespace to a single space
				processedValues = append(processedValues, canonicalWSRegex.ReplaceAllString(strings.TrimSpace(v), " "))
			}
			headerPairs = append(headerPairs, [2]string{lowerName, strings.Join(processedValues, ",")})
			actualSignedHeadersForOutput = append(actualSignedHeadersForOutput, lowerName)
		}
	}

	// Sort headers by name
	sort.Slice(headerPairs, func(i, j int) bool {
		return headerPairs[i][0] < headerPairs[j][0]
	})

	for _, pair := range headerPairs {
		canonicalHeaders.WriteString(pair[0])
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(pair[1])
		canonicalHeaders.WriteString("\n")
	}

	// The SignedHeaders string is a semicolon-separated list of lowercase header names, sorted alphabetically.
	sort.Strings(actualSignedHeadersForOutput)
	return canonicalHeaders.String(), strings.Join(actualSignedHeadersForOutput, ";")
}

func getPayloadHash(r *http.Request) (string, []byte, error) {
	xAmzContentSHA256 := r.Header.Get("x-amz-content-sha256")
	if xAmzContentSHA256 == unsignedPayload {
		return unsignedPayload, nil, nil
	}
	// Fix 6: accept ONLY the known STREAMING-* constants; anything else
	// STREAMING-* is rejected.
	if strings.HasPrefix(xAmzContentSHA256, "STREAMING-") {
		if !streamingPayloadAllowlist[xAmzContentSHA256] {
			return "", nil, fmt.Errorf("unsupported streaming payload hash %q", xAmzContentSHA256)
		}
		log.Printf("Note: Streaming payload type '%s' - accepting without body hash verification", strconv.Quote(xAmzContentSHA256))
		return xAmzContentSHA256, nil, nil
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read request body: %w", err)
	}
	// Replace r.Body so it can be read again by handlers
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	payloadHash := hashSHA256(bodyBytes)

	// If x-amz-content-sha256 is provided and is not UNSIGNED_PAYLOAD, it MUST match the computed hash.
	if xAmzContentSHA256 != "" && xAmzContentSHA256 != payloadHash {
		return "", bodyBytes, fmt.Errorf("x-amz-content-sha256 mismatch. Provided: %s, Calculated: %s", xAmzContentSHA256, payloadHash)
	}
	return payloadHash, bodyBytes, nil // Return bodyBytes so it can be used if needed by caller
}

// isLowercaseHex64 reports whether s is exactly 64 lowercase hex chars.
func isLowercaseHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	if err != nil {
		return false
	}
	return s == strings.ToLower(s)
}

// isPresignedRequest reports whether the request carries SigV4 query-auth
// params (X-Amz-Algorithm=AWS4-HMAC-SHA256 + X-Amz-Signature) and no
// Authorization header. When Authorization is present, header auth wins
// (AWS behavior) and the query params are treated as ordinary signed data.
func isPresignedRequest(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return false
	}
	q := r.URL.Query()
	return q.Get("X-Amz-Algorithm") == awsAlgorithm && q.Get("X-Amz-Signature") != ""
}

// requiredPresignedParam describes one mandatory X-Amz-* query param.
type requiredPresignedParam struct {
	Name string
}

var requiredPresignedParams = []requiredPresignedParam{
	{Name: "X-Amz-Algorithm"},
	{Name: "X-Amz-Credential"},
	{Name: "X-Amz-Date"},
	{Name: "X-Amz-Expires"},
	{Name: "X-Amz-SignedHeaders"},
	{Name: "X-Amz-Signature"},
}
