package main

import (
	"bufio"
	"bytes"
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
	"time"
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

// debugAuthEnabled reports whether verbose auth debugging is on (fix 9).
func debugAuthEnabled() bool {
	return os.Getenv("MINIS3_DEBUG_AUTH") == "1"
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
	// STREAMING-* is rejected. (Full chunk-signature verification is leaf 3.4.)
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

func authenticateRequest(w http.ResponseWriter, r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	xAmzDate := r.Header.Get("x-amz-date")
	dateHeader := r.Header.Get("Date") // Fallback if x-amz-date is not present

	var requestTimestamp time.Time
	var err error

	if xAmzDate != "" {
		requestTimestamp, err = time.Parse(iso8601Format, xAmzDate)
	} else if dateHeader != "" {
		requestTimestamp, err = time.Parse(http.TimeFormat, dateHeader)
	} else {
		log.Println("Authentication Error: Missing x-amz-date or Date header.")
		writeS3Error(w, "AccessDenied", "AWS authentication requires a valid Date or x-amz-date header", http.StatusForbidden)
		return false
	}
	if err != nil {
		log.Printf("Authentication Error: Invalid date format. x-amz-date: '%s', Date: '%s'. Error: %v", strconv.Quote(xAmzDate), strconv.Quote(dateHeader), err)
		writeS3Error(w, "InvalidDate", "The date provided is invalid.", http.StatusBadRequest)
		return false
	}

	if time.Since(requestTimestamp).Abs() > 15*time.Minute {
		log.Printf("Authentication Error: Request timestamp %s is too skewed from server time %s.", strconv.Quote(requestTimestamp.Format(iso8601Format)), time.Now().UTC().Format(iso8601Format))
		writeS3Error(w, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large.", http.StatusForbidden)
		return false
	}

	if authHeader == "" {
		log.Println("Authentication Error: Missing Authorization header.")
		writeS3Error(w, "AuthorizationHeaderMissing", "The authorization header is missing.", http.StatusForbidden)
		return false
	}

	matches := authHeaderRegexTolerant.FindStringSubmatch(authHeader)
	if len(matches) != 6 {
		log.Printf("Authentication Error: Invalid Authorization header format: %s", strconv.Quote(authHeader))
		writeS3Error(w, "AuthorizationHeaderMalformed", "The authorization header is malformed; it does not match the expected format.", http.StatusBadRequest)
		return false
	}

	accessKeyID := matches[1]
	dateStampFromCred := matches[2]
	regionFromCred := matches[3]
	signedHeadersFromAuth := strings.Split(matches[4], ";")
	clientSignature := matches[5]

	if accessKeyID != serverCredentials.AccessKeyID {
		log.Printf("Authentication Error: Unknown AccessKeyID: %s", strconv.Quote(accessKeyID))
		writeS3Error(w, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.", http.StatusForbidden)
		return false
	}

	// Fix 1 precondition: client signature must be 64 lowercase hex chars.
	if !isLowercaseHex64(clientSignature) {
		log.Printf("Authentication Error: Client signature is not 64 lowercase hex chars.")
		writeS3Error(w, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
		return false
	}

	// Fix 8: scope-date mismatch is an InvalidRequest/400, not a signature failure.
	requestDateStamp := requestTimestamp.UTC().Format(shortDateFormat)
	if dateStampFromCred != requestDateStamp {
		log.Printf("Authentication Error: Date mismatch. Credential scope date: %s, Request date: %s", strconv.Quote(dateStampFromCred), strconv.Quote(requestDateStamp))
		writeS3Error(w, "InvalidRequest", "Date in credential scope does not match request date", http.StatusBadRequest)
		return false
	}

	// Fix 7: region mismatch is AuthorizationHeaderMalformed/400 (AWS behavior).
	if regionFromCred != defaultRegion {
		log.Printf("Authentication Error: Invalid region. Expected %s, got %s", defaultRegion, strconv.Quote(regionFromCred))
		writeS3Error(w, "AuthorizationHeaderMalformed", "Region in credential scope ('"+regionFromCred+"') is incorrect; expected '"+defaultRegion+"'.", http.StatusBadRequest)
		return false
	}

	// Step 1: Create a Canonical Request
	payloadHash, _, err := getPayloadHash(r)
	if err != nil {
		log.Printf("Authentication Error: Failed to get/verify payload hash: %v", err)
		writeS3Error(w, "SignatureDoesNotMatch", "Payload hash mismatch or error reading body.", http.StatusForbidden)
		return false
	}

	canonicalURI := getCanonicalURI(r)
	canonicalQueryString := getCanonicalQueryString(r)
	canonicalHeaders, signedHeadersString := getCanonicalHeaders(r, signedHeadersFromAuth)

	// Fix 4: a SignedHeaders mismatch is now a hard reject. A client that
	// claims to sign headers it did not send (or vice versa) cannot have
	// produced a valid canonical request.
	if signedHeadersString != matches[4] {
		log.Printf("Authentication Error: SignedHeaders mismatch. Client sent: '%s', Server calculated: '%s'", strconv.Quote(matches[4]), signedHeadersString)
		writeS3Error(w, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
		return false
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders, // Already ends with a newline
		signedHeadersString,
		payloadHash,
	}, "\n")

	// Step 2: Create the String to Sign
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStampFromCred, regionFromCred, serviceName)
	hashedCanonicalRequest := hashSHA256([]byte(canonicalRequest))

	stringToSign := strings.Join([]string{
		awsAlgorithm,
		requestTimestamp.UTC().Format(iso8601Format),
		credentialScope,
		hashedCanonicalRequest,
	}, "\n")

	// Step 3: Calculate the Signing Key
	signingKey := getSigningKey(serverCredentials.SecretAccessKey, dateStampFromCred, regionFromCred, serviceName)

	// Step 4: Calculate the Signature
	serverSignature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	// Step 5: Compare the Signatures — timing-safe (fix 1)
	if !hmac.Equal([]byte(serverSignature), []byte(clientSignature)) {
		// Fix 9: verbose diagnostics only when MINIS3_DEBUG_AUTH=1; one line always.
		if debugAuthEnabled() {
			log.Printf("Authentication Error: Signature mismatch.\nServer Signature: %s\nClient Signature: %s\nString To Sign:\n%s\nCanonical Request:\n%s",
				strconv.Quote(serverSignature), strconv.Quote(clientSignature), strconv.Quote(stringToSign), strconv.Quote(canonicalRequest))
		} else {
			log.Println("Authentication Error: Signature mismatch.")
		}
		writeS3Error(w, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
		return false
	}

	log.Println("Authentication Successful: SigV4 signature verified.")
	return true
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
	name string
}

var requiredPresignedParams = []requiredPresignedParam{
	{name: "X-Amz-Algorithm"},
	{name: "X-Amz-Credential"},
	{name: "X-Amz-Date"},
	{name: "X-Amz-Expires"},
	{name: "X-Amz-SignedHeaders"},
	{name: "X-Amz-Signature"},
}

// authenticatePresigned validates a SigV4 presigned (query-string) request
// and writes the S3 error response itself on failure. AWS error conventions:
// AuthorizationQueryParametersError/400 for malformed params,
// InvalidAccessKeyId/403 for unknown access keys, AccessDenied/403 for
// expired URLs, SignatureDoesNotMatch/403 for signature failures.
func authenticatePresigned(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()

	// Presence check for every required param (missing → 400).
	for _, p := range requiredPresignedParams {
		if q.Get(p.name) == "" {
			log.Printf("Presigned Auth Error: missing %s query parameter", p.name)
			writeS3Error(w, "AuthorizationQueryParametersError",
				"Query-string authentication version 4 requires the X-Amz-Algorithm, X-Amz-Credential, X-Amz-Signature, X-Amz-Date, X-Amz-SignedHeaders, and X-Amz-Expires parameters.",
				http.StatusBadRequest)
			return false
		}
	}

	credential := q.Get("X-Amz-Credential")
	scopeParts := strings.Split(credential, "/")
	if len(scopeParts) != 5 || scopeParts[4] != "aws4_request" || scopeParts[3] != serviceName {
		log.Printf("Presigned Auth Error: malformed X-Amz-Credential %q", strconv.Quote(credential)) //nolint:gosec // G706: strconv.Quote sanitizes
		writeS3Error(w, "AuthorizationQueryParametersError",
			"Error parsing the X-Amz-Credential parameter; the Credential is mal-formed; expecting \"<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request\".",
			http.StatusBadRequest)
		return false
	}
	accessKeyID, dateStampFromCred, regionFromCred := scopeParts[0], scopeParts[1], scopeParts[2]

	if accessKeyID != serverCredentials.AccessKeyID {
		log.Printf("Presigned Auth Error: unknown AccessKeyID %q", strconv.Quote(accessKeyID)) //nolint:gosec // G706: strconv.Quote sanitizes
		writeS3Error(w, "InvalidAccessKeyId",
			"The AWS Access Key Id you provided does not exist in our records.", http.StatusForbidden)
		return false
	}

	if regionFromCred != defaultRegion {
		log.Printf("Presigned Auth Error: region %q incorrect; expected %q", strconv.Quote(regionFromCred), defaultRegion) //nolint:gosec // G706: strconv.Quote sanitizes
		writeS3Error(w, "AuthorizationQueryParametersError",
			"Error parsing the X-Amz-Credential parameter; the region is incorrect; expected '"+defaultRegion+"'.",
			http.StatusBadRequest)
		return false
	}

	clientSignature := q.Get("X-Amz-Signature")
	if !isLowercaseHex64(clientSignature) {
		log.Printf("Presigned Auth Error: X-Amz-Signature is not 64 lowercase hex chars")
		writeS3Error(w, "SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
		return false
	}

	// X-Amz-Date must parse; presigned requests skip the 15-min skew check —
	// X-Amz-Expires governs validity (AWS behavior).
	amzDate, err := time.Parse(iso8601Format, q.Get("X-Amz-Date"))
	if err != nil {
		log.Printf("Presigned Auth Error: unparseable X-Amz-Date %q", strconv.Quote(q.Get("X-Amz-Date"))) //nolint:gosec // G706: strconv.Quote sanitizes
		writeS3Error(w, "AuthorizationQueryParametersError",
			"X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\".", http.StatusBadRequest)
		return false
	}

	// Scope date must equal the X-Amz-Date date part.
	requestDateStamp := amzDate.UTC().Format(shortDateFormat)
	if dateStampFromCred != requestDateStamp {
		log.Printf("Presigned Auth Error: credential scope date %s != X-Amz-Date date %s", strconv.Quote(dateStampFromCred), requestDateStamp) //nolint:gosec // G706: strconv.Quote sanitizes
		writeS3Error(w, "AuthorizationQueryParametersError",
			"Invalid credential date in X-Amz-Credential. This date must be the same as the X-Amz-Date parameter.",
			http.StatusBadRequest)
		return false
	}

	// X-Amz-Expires: integer seconds, 1..604800 (AWS limits).
	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires < 1 || expires > 604800 {
		log.Printf("Presigned Auth Error: invalid X-Amz-Expires %q", strconv.Quote(q.Get("X-Amz-Expires"))) //nolint:gosec // G706: strconv.Quote sanitizes
		writeS3Error(w, "AuthorizationQueryParametersError",
			"X-Amz-Expires must be a number between 1 and 604800 seconds.", http.StatusBadRequest)
		return false
	}

	// Expiry window: [X-Amz-Date, X-Amz-Date + Expires]. Expired → AccessDenied.
	expiresAt := amzDate.Add(time.Duration(expires) * time.Second)
	if time.Now().After(expiresAt) {
		log.Printf("Presigned Auth Error: URL expired at %s", expiresAt.UTC().Format(iso8601Format)) //nolint:gosec // G706: time.Format output, no tainted input
		writeS3Error(w, "AccessDenied", "Request has expired", http.StatusForbidden)
		return false
	}

	signedHeaderNames := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")

	canonicalHeaders, signedHeadersString := getCanonicalHeaders(r, signedHeaderNames)
	if signedHeadersString != q.Get("X-Amz-SignedHeaders") {
		log.Printf("Presigned Auth Error: SignedHeaders mismatch. Client sent: %q, server calculated: %q",
			strconv.Quote(q.Get("X-Amz-SignedHeaders")), strconv.Quote(signedHeadersString)) //nolint:gosec // G706: strconv.Quote sanitizes
		writeS3Error(w, "SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
		return false
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		getCanonicalURI(r),
		// All query params EXCEPT X-Amz-Signature.
		getCanonicalQueryStringExcluding(r, "X-Amz-Signature"),
		canonicalHeaders,
		signedHeadersString,
		// Presigned URLs never sign the body (UNSIGNED-PAYLOAD).
		unsignedPayload,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStampFromCred, regionFromCred, serviceName)
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate.UTC().Format(iso8601Format),
		credentialScope,
		hashSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := getSigningKey(serverCredentials.SecretAccessKey, dateStampFromCred, regionFromCred, serviceName)
	serverSignature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	if !hmac.Equal([]byte(serverSignature), []byte(clientSignature)) {
		if debugAuthEnabled() {
			log.Printf("Presigned Auth Error: signature mismatch.\nServer Signature: %s\nClient Signature: %s\nString To Sign:\n%s\nCanonical Request:\n%s",
				strconv.Quote(serverSignature), strconv.Quote(clientSignature), strconv.Quote(stringToSign), strconv.Quote(canonicalRequest))
		} else {
			log.Println("Presigned Auth Error: Signature mismatch.")
		}
		writeS3Error(w, "SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
		return false
	}

	log.Println("Authentication Successful: SigV4 presigned URL verified.")
	return true
}
