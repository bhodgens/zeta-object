package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sigv4.go — AWS Signature Version 4 request authentication

// decodeAWSChunked decodes aws-chunked Content-Encoding used by AWS CLI v2.
// Format: <hex-size>;chunk-signature=...\r\n<data>\r\n, ending with 0\r\n<trailers>\r\n\r\n
func decodeAWSChunked(body []byte) ([]byte, error) {
	var result bytes.Buffer
	reader := bufio.NewReader(bytes.NewReader(body))

	for {
		// Read the chunk header line
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("error reading chunk header: %w", err)
		}

		// Parse chunk size (format: "<hex>;chunk-signature=..." or just "<hex>")
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ";", 2)
		sizeStr := strings.TrimSpace(parts[0])

		if sizeStr == "" {
			continue // Skip empty lines
		}

		chunkSize, err := strconv.ParseInt(sizeStr, 16, 64)
		if err != nil {
			// Might be a trailer line, skip it
			continue
		}

		if chunkSize == 0 {
			// Final chunk - read remaining trailers
			break
		}

		// Read the chunk data
		chunkData := make([]byte, chunkSize)
		n, err := io.ReadFull(reader, chunkData)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("error reading chunk data: %w", err)
		}
		result.Write(chunkData[:n])

		// Read the trailing \r\n after chunk data
		reader.ReadString('\n')
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
	// Let's use r.URL.EscapedPath() if available and non-empty, otherwise r.URL.Path.
	escapedPath := r.URL.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"                          // Default for empty path
		if r.URL.Path != "" && r.URL.Path != "/" { // If path was not empty but RawPath was, re-escape (basic)
			escapedPath = (&url.URL{Path: r.URL.Path}).RequestURI() // This re-encodes based on Path
		}
	}
	return escapedPath
}

func getCanonicalQueryString(r *http.Request) string {
	queryParams := r.URL.Query()
	if len(queryParams) == 0 {
		return ""
	}

	var sortedKeys []string
	for k := range queryParams {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	var canonicalParams []string
	for _, k := range sortedKeys {
		values := queryParams[k]
		sort.Strings(values) // Sort values for the same key
		for _, v := range values {
			// S3 requires both key and value to be URI encoded.
			// r.URL.Query() gives decoded values. We need to re-encode them.
			canonicalParams = append(canonicalParams, url.QueryEscape(k)+"="+url.QueryEscape(v))
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
				processedValues = append(processedValues, strings.TrimSpace(v))
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
	// Handle various streaming payload types (AWS CLI v2 uses STREAMING-UNSIGNED-PAYLOAD-TRAILER)
	if strings.HasPrefix(xAmzContentSHA256, "STREAMING-") {
		// Streaming payloads are signed differently - treat as unsigned for basic implementation
		log.Printf("Note: Streaming payload type '%s' - accepting without body hash verification", xAmzContentSHA256)
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

func authenticateRequest(w http.ResponseWriter, r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	xAmzDate := r.Header.Get("x-amz-date")
	dateHeader := r.Header.Get("Date") // Fallback if x-amz-date is not present

	requestTimestamp := time.Time{}
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
		log.Printf("Authentication Error: Invalid date format. x-amz-date: '%s', Date: '%s'. Error: %v", xAmzDate, dateHeader, err)
		writeS3Error(w, "InvalidDate", "The date provided is invalid.", http.StatusBadRequest)
		return false
	}

	if time.Since(requestTimestamp).Abs() > 15*time.Minute {
		log.Printf("Authentication Error: Request timestamp %s is too skewed from server time %s.", requestTimestamp.Format(iso8601Format), time.Now().UTC().Format(iso8601Format))
		writeS3Error(w, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large.", http.StatusForbidden)
		return false
	}

	if authHeader == "" {
		// Enforce auth for all requests now. Remove temporary allowance for ListBuckets if any.
		log.Println("Authentication Error: Missing Authorization header.")
		writeS3Error(w, "AuthorizationHeaderMissing", "The authorization header is missing.", http.StatusForbidden)
		return false
	}

	matches := authHeaderRegex.FindStringSubmatch(authHeader)
	if len(matches) != 6 {
		log.Printf("Authentication Error: Invalid Authorization header format: %s", authHeader)
		writeS3Error(w, "AuthorizationHeaderMalformed", "The authorization header is malformed; it does not match the expected format.", http.StatusBadRequest)
		return false
	}

	accessKeyID := matches[1]
	dateStampFromCred := matches[2]
	regionFromCred := matches[3]
	signedHeadersFromAuth := strings.Split(matches[4], ";")
	clientSignature := matches[5]

	if accessKeyID != serverCredentials.AccessKeyID {
		log.Printf("Authentication Error: Unknown AccessKeyID: %s", accessKeyID)
		writeS3Error(w, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.", http.StatusForbidden)
		return false
	}

	// Validate dateStamp from credential scope matches the request date (short YYYYMMDD format)
	requestDateStamp := requestTimestamp.UTC().Format(shortDateFormat)
	if dateStampFromCred != requestDateStamp {
		log.Printf("Authentication Error: Date mismatch. Credential scope date: %s, Request date: %s", dateStampFromCred, requestDateStamp)
		writeS3Error(w, "SignatureDoesNotMatch", "Credential scope date mismatch.", http.StatusForbidden)
		return false
	}

	if regionFromCred != defaultRegion {
		log.Printf("Authentication Error: Invalid region. Expected %s, got %s", defaultRegion, regionFromCred)
		writeS3Error(w, "AuthorizationHeaderMalformed", "Region in credential scope ('"+regionFromCred+"') is incorrect; expected '"+defaultRegion+"'.", http.StatusForbidden)
		return false
	}

	// Step 1: Create a Canonical Request
	payloadHash, _, err := getPayloadHash(r) // bodyBytes might be needed if we re-calculate hash for some reason
	if err != nil {
		log.Printf("Authentication Error: Failed to get/verify payload hash: %v", err)
		writeS3Error(w, "SignatureDoesNotMatch", "Payload hash mismatch or error reading body.", http.StatusForbidden)
		return false
	}

	canonicalURI := getCanonicalURI(r)
	canonicalQueryString := getCanonicalQueryString(r)
	canonicalHeaders, signedHeadersString := getCanonicalHeaders(r, signedHeadersFromAuth)

	// Verify that the signedHeadersString from our calculation matches what client sent in Authorization header
	// The client's list of signed headers (matches[4]) should be used to build our canonicalHeaders string.
	// Then, our re-calculated signedHeadersString (from getCanonicalHeaders) should match matches[4].
	if signedHeadersString != matches[4] {
		log.Printf("Authentication Error: SignedHeaders mismatch. Client sent: '%s', Server calculated based on found headers: '%s'", matches[4], signedHeadersString)
		// This might happen if client claims to sign a header that's not present, or if our sorting/joining is different.
		// For robustness, ensure getCanonicalHeaders uses the client's list of signed headers strictly.
		// The current getCanonicalHeaders already does this by taking signedHeaderNames as input.
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

	// Step 5: Compare the Signatures
	if serverSignature != clientSignature {
		log.Printf("Authentication Error: Signature mismatch.\nServer Signature: %s\nClient Signature: %s\nString To Sign:\n%s\nCanonical Request:\n%s",
			serverSignature, clientSignature, stringToSign, canonicalRequest)
		writeS3Error(w, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
		return false
	}

	log.Println("Authentication Successful: SigV4 signature verified.")
	return true
}
