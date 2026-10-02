// auth_adapter.go — the SigV4 adapter behind auth.Authenticator (leaf 02
// Task 1). authenticateRequest / authenticatePresigned become methods on
// the Frontend that resolve credentials through the injected
// auth.CredentialSource instead of a package-level credential global, and
// return typed auth failures (code, message, status) that the dispatch
// layer renders via writeS3Error — identical status/code/message bytes as
// the pre-move wire behavior.
package s3

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// authFailureError carries the S3 error triple an authentication rejection
// renders. writeAuthFailure writes it byte-identically to the pre-move
// writeS3Error calls.
type authFailureError struct {
	code    string
	message string
	status  int
}

// writeAuthFailure renders a on the wire.
func writeAuthFailure(w http.ResponseWriter, a authFailureError) {
	writeS3Error(w, a.code, a.message, a.status)
}

// Error satisfies the error interface so the Authenticator seam can
// return failures as values (the S3 rendering is dispatched elsewhere).
func (a *authFailureError) Error() string { return a.code + ": " + a.message }

// defaultCredentialSource is the nil-option fallback used only when a
// Frontend is built without WithCredentialSource. The wiring layer
// overrides it via installDefaultCredentialSource at startup; without an
// override every lookup misses (fail-closed). Guarded by hookMu (seam.go).
var defaultCredentialSourceImpl auth.CredentialSource

// installDefaultCredentialSource installs the process-wide credential
// source (package main's serverCredentials adapter). main() calls this
// before the listener opens.
func installDefaultCredentialSource(cs auth.CredentialSource) {
	hookMu.Lock()
	defer hookMu.Unlock()
	defaultCredentialSourceImpl = cs
}

// staticCredential is a fixed single-pair CredentialSource (tests).
type staticCredential struct {
	accessKey string
	secretKey string
}

func (s staticCredential) SecretKey(accessKeyID string) (string, bool) {
	if accessKeyID == s.accessKey {
		return s.secretKey, true
	}
	return "", false
}

// compile-time assertion: the adapter satisfies the auth seam.
var _ auth.Authenticator = (*sigv4Authenticator)(nil)

// sigv4Authenticator adapts SigV4 verification to auth.Authenticator.
type sigv4Authenticator struct {
	creds auth.CredentialSource
}

// Authenticate verifies the request's SigV4 signature (header form) and
// returns the identity on success. Failures return an *authFailureError the
// caller renders as an S3 error.
func (a *sigv4Authenticator) Authenticate(r *http.Request) (auth.Identity, error) {
	f := &Frontend{creds: a.creds, authz: a}
	if isPresignedRequest(r) {
		id, failure, ok := f.authenticatePresignedRequest(r)
		if !ok {
			return auth.Identity{}, failure
		}
		return id, nil
	}
	id, failure, ok := f.authenticateRequest(r)
	if !ok {
		return auth.Identity{}, failure
	}
	return id, nil
}

// authenticateRequest verifies header-form SigV4. It is the former
// package-main authenticateRequest restructured from
// (writes-response, bool) to (Identity, *authFailureError, bool) with the
// credential lookup routed through the injected source.
func (f *Frontend) authenticateRequest(r *http.Request) (auth.Identity, *authFailureError, bool) {
	authHeader := r.Header.Get("Authorization")
	xAmzDate := r.Header.Get("x-amz-date")
	dateHeader := r.Header.Get("Date") // Fallback if x-amz-date is not present

	var requestTimestamp timeTime
	var err error

	if xAmzDate != "" {
		requestTimestamp, err = timeParse(iso8601Format, xAmzDate)
	} else if dateHeader != "" {
		requestTimestamp, err = timeParse(httpTimeFormat, dateHeader)
	} else {
		log.Println("Authentication Error: Missing x-amz-date or Date header.")
		return auth.Identity{}, &authFailureError{"AccessDenied", "AWS authentication requires a valid Date or x-amz-date header", httpStatusForbidden}, false
	}
	if err != nil {
		log.Printf("Authentication Error: Invalid date format. x-amz-date: '%s', Date: '%s'. Error: %v", strconvQuote(xAmzDate), strconvQuote(dateHeader), err) //nolint:gosec // G706: values strconvQuote-sanitized; err is a parse error
		return auth.Identity{}, &authFailureError{"InvalidDate", "The date provided is invalid.", http.StatusBadRequest}, false
	}

	if timeSince(requestTimestamp).Abs() > fifteenMinutes {
		log.Printf("Authentication Error: Request timestamp %s is too skewed from server time %s.", strconvQuote(requestTimestamp.UTC().Format(iso8601Format)), strconvQuote(timeNowUTC().Format(iso8601Format))) //nolint:gosec // G706: strconvQuote-sanitized
		return auth.Identity{}, &authFailureError{"RequestTimeTooSkewed", "The difference between the request time and the current time is too large.", http.StatusForbidden}, false
	}

	if authHeader == "" {
		log.Println("Authentication Error: Missing Authorization header.")
		return auth.Identity{}, &authFailureError{"AuthorizationHeaderMissing", "The authorization header is missing.", http.StatusForbidden}, false
	}

	matches := authHeaderRegexTolerant.FindStringSubmatch(authHeader)
	if len(matches) != 6 {
		log.Printf("Authentication Error: Invalid Authorization header format: %s", strconvQuote(authHeader)) //nolint:gosec // G706: strconvQuote-sanitized
		return auth.Identity{}, &authFailureError{"AuthorizationHeaderMalformed", "The authorization header is malformed; it does not match the expected format.", http.StatusBadRequest}, false
	}

	accessKeyID := matches[1]
	dateStampFromCred := matches[2]
	regionFromCred := matches[3]
	signedHeadersFromAuth := strings.Split(matches[4], ";")
	clientSignature := matches[5]

	secretKey, identity, known := f.credentialSecret(accessKeyID)
	if !known {
		log.Printf("Authentication Error: Unknown AccessKeyID: %s", strconvQuote(accessKeyID)) //nolint:gosec // G706: strconvQuote-sanitized
		return auth.Identity{}, &authFailureError{"InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.", httpStatusForbidden}, false
	}

	// Fix 1 precondition: client signature must be 64 lowercase hex chars.
	if !isLowercaseHex64(clientSignature) {
		log.Printf("Authentication Error: Client signature is not 64 lowercase hex chars.")
		return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden}, false
	}

	// Fix 8: scope-date mismatch is an InvalidRequest/400, not a signature failure.
	requestDateStamp := requestTimestamp.UTC().Format(shortDateFormat)
	if dateStampFromCred != requestDateStamp {
		log.Printf("Authentication Error: Date mismatch. Credential scope date: %s, Request date: %s", strconvQuote(dateStampFromCred), strconvQuote(requestDateStamp)) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		return auth.Identity{}, &authFailureError{"InvalidRequest", "Date in credential scope does not match request date", http.StatusBadRequest}, false
	}

	// Fix 7 (updated, region-config-2026-10 leaf 02): region handling via
	// regionOf() with strict/permissive semantics (Contract 2). Explicit
	// mismatch is SignatureDoesNotMatch naming the expected region;
	// default mode is permissive with a notice (see checkRegionMatch).
	if failure := checkRegionMatch(regionFromCred); failure != nil {
		return auth.Identity{}, failure, false
	}

	// Step 1: Create a Canonical Request
	payloadHash, _, err := getPayloadHash(r)
	if err != nil {
		log.Printf("Authentication Error: Failed to get/verify payload hash: %v", err)
		return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch", "Payload hash mismatch or error reading body.", http.StatusForbidden}, false
	}

	canonicalURI := getCanonicalURI(r)
	canonicalQueryString := getCanonicalQueryString(r)
	canonicalHeaders, signedHeadersString := getCanonicalHeaders(r, signedHeadersFromAuth)

	// Fix 4: a SignedHeaders mismatch is now a hard reject. A client that
	// claims to sign headers it did not send (or vice versa) cannot have
	// produced a valid canonical request.
	if signedHeadersString != matches[4] {
		log.Printf("Authentication Error: SignedHeaders mismatch. Client sent: '%s', Server calculated: '%s'", strconvQuote(matches[4]), signedHeadersString) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden}, false
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
	signingKey := getSigningKey(secretKey, dateStampFromCred, regionFromCred, serviceName)

	// Step 4: Calculate the Signature
	serverSignature := hexEncode(hmacSHA256(signingKey, stringToSign))

	// Step 5: Compare the Signatures — timing-safe (fix 1)
	if !hmacEqual([]byte(serverSignature), []byte(clientSignature)) {
		// Fix 9: verbose diagnostics only when ZETAOBJECT_DEBUG_AUTH=1; one line always.
		if debugAuthEnabled() {
			log.Printf("Authentication Error: Signature mismatch.\nServer Signature: %s\nClient Signature: %s\nString To Sign:\n%s\nCanonical Request:\n%s", //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
				strconvQuote(serverSignature), strconvQuote(clientSignature), strconvQuote(stringToSign), strconvQuote(canonicalRequest)) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		} else {
			log.Println("Authentication Error: Signature mismatch.")
		}
		return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden}, false
	}

	// Leaf 3.4: for signed chunked streaming the header signature is the seed
	// of the chunk-signature chain — verify every chunk now, while the seed,
	// signing key, timestamp and scope are in scope. On success the request
	// body is replaced with the decoded bytes and the context is flagged so
	// handlers skip their own aws-chunked decode pass.
	if payloadHash == streamingSignedPayload {
		bodyBytes, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			log.Printf("Authentication Error: failed to read streaming body: %v", readErr)
			return auth.Identity{}, &authFailureError{"InvalidArgument", "Error reading request body.", http.StatusBadRequest}, false
		}
		decoded, verifyErr := decodeAndVerifyChunked(bodyBytes, serverSignature, signingKey,
			requestTimestamp.UTC().Format(iso8601Format), credentialScope)
		if verifyErr != nil {
			log.Printf("Authentication Error: chunk signature verification failed: %v", verifyErr)
			return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch", "Chunk signature verification failed.", http.StatusForbidden}, false
		}
		// Leaf 2.2 helper: decoded size must match x-amz-decoded-content-length.
		if lenErr := VerifyDecodedLength(r.Header.Get("x-amz-decoded-content-length"), len(decoded)); lenErr != nil {
			log.Printf("Authentication Error: %v", lenErr)
			return auth.Identity{}, &authFailureError{"InvalidArgument", "Decoded content length mismatch.", http.StatusBadRequest}, false
		}
		r.Body = io.NopCloser(bytes.NewBuffer(decoded))
		// authenticateRequest receives *http.Request by value; write the
		// context-flagged request back in place so serveHTTP's copy (and
		// therefore the object/multipart handlers) sees the flag too — same
		// in-place pattern as the r.Body replacement above.
		newReq := r.WithContext(withDecodedStreaming(r.Context()))
		*r = *newReq
	}

	log.Println("Authentication Successful: SigV4 signature verified.")
	return identity, nil, true
}

// credentialSecret resolves the signing secret for accessKeyID and returns
// the FULL identity (pluggable-authentication tree leaf 02). Resolution
// order: the installed IdentityRegistry (LookupByAccessKey — grants
// populated) wins; the legacy CredentialSource is the fallback (tests /
// transitional), synthesizing the wildcard identity so the pre-tree
// "single omnipotent pair" semantic is unchanged on that path.
func (f *Frontend) credentialSecret(accessKeyID string) (string, auth.Identity, bool) {
	if reg := identityRegistryFor(); reg != nil {
		if id, ok := reg.LookupByAccessKey(accessKeyID); ok {
			if st, ok2 := registrySecretSource(reg); ok2 {
				secret, found := st(accessKeyID)
				return secret, id, found
			}
		}
		return "", auth.Identity{}, false
	}
	if f.creds != nil {
		secret, ok := f.creds.SecretKey(accessKeyID)
		if !ok {
			return "", auth.Identity{}, false
		}
		return secret, auth.WildcardIdentity(accessKeyID), true
	}
	if src := credentialSourceFor(); src != nil {
		secret, ok := src.SecretKey(accessKeyID)
		if !ok {
			return "", auth.Identity{}, false
		}
		return secret, auth.WildcardIdentity(accessKeyID), true
	}
	return "", auth.Identity{}, false
}

// authenticatePresigned verifies query-form (presigned) SigV4. Former
// package-main authenticatePresigned restructured to the same failure
// triple; error codes/messages/statuses are byte-identical.
func (f *Frontend) authenticatePresigned(w http.ResponseWriter, r *http.Request) bool {
	_, failure, ok := f.authenticatePresignedRequest(r)
	if !ok {
		writeAuthFailure(w, *failure)
		return false
	}
	return true
}

// authenticatePresignedRequest is the verification core of the presigned
// path (separated so the Authenticator seam can grow query-auth support
// without the response-writing wrapper).
func (f *Frontend) authenticatePresignedRequest(r *http.Request) (auth.Identity, *authFailureError, bool) {
	q := r.URL.Query()

	// Presence check for every required param (missing → 400).
	for _, p := range requiredPresignedParams {
		if q.Get(p.Name) == "" {
			log.Printf("Presigned Auth Error: missing %s query parameter", p.Name)
			return auth.Identity{}, &authFailureError{"AuthorizationQueryParametersError",
				"Query-string authentication version 4 requires the X-Amz-Algorithm, X-Amz-Credential, X-Amz-Signature, X-Amz-Date, X-Amz-SignedHeaders, and X-Amz-Expires parameters.",
				http.StatusBadRequest}, false
		}
	}

	credential := q.Get("X-Amz-Credential")
	scopeParts := strings.Split(credential, "/")
	if len(scopeParts) != 5 || scopeParts[4] != "aws4_request" || scopeParts[3] != serviceName {
		log.Printf("Presigned Auth Error: malformed X-Amz-Credential %q", strconvQuote(credential)) //nolint:gosec // G706: strconv.Quote sanitizes
		return auth.Identity{}, &authFailureError{"AuthorizationQueryParametersError",
			"Error parsing the X-Amz-Credential parameter; the Credential is mal-formed; expecting \"<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request\".",
			http.StatusBadRequest}, false
	}
	accessKeyID, dateStampFromCred, regionFromCred := scopeParts[0], scopeParts[1], scopeParts[2]

	secretKey, identity, known := f.credentialSecret(accessKeyID)
	if !known {
		log.Printf("Presigned Auth Error: unknown AccessKeyID %q", strconvQuote(accessKeyID)) //nolint:gosec // G706: strconv.Quote sanitizes
		return auth.Identity{}, &authFailureError{"InvalidAccessKeyId",
			"The AWS Access Key Id you provided does not exist in our records.", http.StatusForbidden}, false
	}

	// region-config-2026-10 leaf 02: region handling via regionOf() with
	// strict/permissive semantics (Contract 2), same as the header path.
	if failure := checkRegionMatch(regionFromCred); failure != nil {
		return auth.Identity{}, failure, false
	}

	clientSignature := q.Get("X-Amz-Signature")
	if !isLowercaseHex64(clientSignature) {
		log.Printf("Presigned Auth Error: X-Amz-Signature is not 64 lowercase hex chars")
		return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided.", http.StatusForbidden}, false
	}

	// X-Amz-Date must parse; presigned requests skip the 15-min skew check —
	// X-Amz-Expires governs validity (AWS behavior).
	amzDate, err := timeParse(iso8601Format, q.Get("X-Amz-Date"))
	if err != nil {
		log.Printf("Presigned Auth Error: unparseable X-Amz-Date %q", strconvQuote(q.Get("X-Amz-Date"))) //nolint:gosec // G706: strconv.Quote sanitizes
		return auth.Identity{}, &authFailureError{"AuthorizationQueryParametersError",
			"X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\".", http.StatusBadRequest}, false
	}

	// Scope date must equal the X-Amz-Date date part.
	requestDateStamp := amzDate.UTC().Format(shortDateFormat)
	if dateStampFromCred != requestDateStamp {
		log.Printf("Presigned Auth Error: credential scope date %s != X-Amz-Date date %s", strconvQuote(dateStampFromCred), requestDateStamp) //nolint:gosec // G706: strconv.Quote sanitizes
		return auth.Identity{}, &authFailureError{"AuthorizationQueryParametersError",
			"Invalid credential date in X-Amz-Credential. This date must be the same as the X-Amz-Date parameter.",
			http.StatusBadRequest}, false
	}

	// X-Amz-Expires: integer seconds, 1..604800 (AWS limits).
	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires < 1 || expires > 604800 {
		log.Printf("Presigned Auth Error: invalid X-Amz-Expires %q", strconvQuote(q.Get("X-Amz-Expires"))) //nolint:gosec // G706: strconv.Quote sanitizes
		return auth.Identity{}, &authFailureError{"AuthorizationQueryParametersError",
			"X-Amz-Expires must be a number between 1 and 604800 seconds.", http.StatusBadRequest}, false
	}

	// Expiry window: [X-Amz-Date, X-Amz-Date + Expires]. Expired → AccessDenied.
	//nolint:durationcheck // expires is a bare int (seconds); this IS the int->Duration conversion
	expiresAt := amzDate.Add(timeDuration(expires) * timeSecond)
	if timeNow().After(expiresAt) {
		log.Printf("Presigned Auth Error: URL expired at %s", expiresAt.UTC().Format(iso8601Format)) //nolint:gosec // G706: time.Format output, no tainted input
		return auth.Identity{}, &authFailureError{"AccessDenied", "Request has expired", http.StatusForbidden}, false
	}

	signedHeaderNames := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")

	canonicalHeaders, signedHeadersString := getCanonicalHeaders(r, signedHeaderNames)
	if signedHeadersString != q.Get("X-Amz-SignedHeaders") {
		log.Printf("Presigned Auth Error: SignedHeaders mismatch. Client sent: %q, server calculated: %q", //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
			strconvQuote(q.Get("X-Amz-SignedHeaders")), strconvQuote(signedHeadersString)) //nolint:gosec // G706: strconv.Quote sanitizes
		return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided.", http.StatusForbidden}, false
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

	signingKey := getSigningKey(secretKey, dateStampFromCred, regionFromCred, serviceName)
	serverSignature := hexEncode(hmacSHA256(signingKey, stringToSign))

	if !hmacEqual([]byte(serverSignature), []byte(clientSignature)) {
		if debugAuthEnabled() {
			log.Printf("Presigned Auth Error: signature mismatch.\nServer Signature: %s\nClient Signature: %s\nString To Sign:\n%s\nCanonical Request:\n%s", //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
				strconvQuote(serverSignature), strconvQuote(clientSignature), strconvQuote(stringToSign), strconvQuote(canonicalRequest))
		} else {
			log.Println("Presigned Auth Error: Signature mismatch.")
		}
		return auth.Identity{}, &authFailureError{"SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided.", http.StatusForbidden}, false
	}

	log.Println("Authentication Successful: SigV4 presigned URL verified.")
	return identity, nil, true
}

// registrySecretSource adapts a MultiRegistry (which keeps the raw secret
// for SigV4 signing-key derivation) to a CredentialSource lookup without
// the s3 package importing internal/auth's concrete type — interface-level
// only, a no-op for registry implementations that do not expose secrets
// (option 2/3 backends), in which case lookup misses stay misses.
func registrySecretSource(reg auth.IdentityRegistry) (func(string) (string, bool), bool) {
	if src, ok := reg.(auth.CredentialSource); ok {
		return src.SecretKey, true
	}
	return nil, false
}
