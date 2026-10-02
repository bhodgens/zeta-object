package s3

import (
	"log"
	"net/http"
	"strings"
)

// region.go — SigV4 verification region accessor (region-config-2026-10
// leaf 01, Contract 1). The region comes from the server `region` config
// key, wired at startup by the config loader; tests set it via SetRegion.

// region returns the SigV4 region this server verifies against.
// Wires from the server config at startup; tests set it via
// SetRegion. Default "us-east-1".
func regionOf() string {
	if region == "" {
		return defaultRegion
	}
	return region
}

// SetRegion sets the verification region (config wiring entry;
// empty restores the default). Not safe for concurrent use with
// in-flight requests: call at startup only.
func SetRegion(r string) {
	// Belt and suspenders: config load already lowercases (SigV4
	// regions are lowercase), but direct callers get the same
	// normalization.
	region = strings.ToLower(r)
}

// regionExplicit reports whether an explicit verification region was
// configured (region-config-2026-10 leaf 02, Contract 2): true after any
// SetRegion with a non-empty value - including the default "us-east-1"
// itself - false in default mode (SetRegion never called, or called with
// ""). Explicit = strict region compare; default = permissive.
func regionExplicit() bool {
	return region != ""
}

// validRegionToken reports whether s is a well-formed SigV4 region token
// (Contract 2: [a-z0-9-]+). Empty is not valid.
func validRegionToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// checkRegionMatch decides whether a client's credential-scope region is
// acceptable (region-config-2026-10 leaf 02, Contract 2):
//
//   - Explicit mode (regionExplicit()): strict compare against regionOf().
//     Mismatch returns an error message naming the EXPECTED region.
//   - Default mode: permissive - the default "us-east-1" is accepted, and
//     any other well-formed region token [a-z0-9-]+ is accepted too (the
//     signing key below derives from the client's region, so the signature
//     still verifies cryptographically), with a one-line notice naming the
//     client's region. Malformed tokens are rejected.
//
// Called once per request at the region-check site of each verification
// path (header + presigned); never per-chunk or per-retry.
func checkRegionMatch(clientRegion string) *authFailureError {
	expected := regionOf()
	if clientRegion == expected {
		return nil
	}
	if !regionExplicit() {
		if validRegionToken(clientRegion) {
			log.Printf("Auth notice: permissive region mode (no explicit region configured); accepting client credential-scope region %q (expected %q)", clientRegion, expected)
			return nil
		}
	}
	if regionExplicit() {
		// Contract 2: an explicit-region mismatch fails with the
		// signature error, naming the expected region.
		log.Printf("Auth Error: Invalid region. Expected %s, got %s", expected, strconvQuote(clientRegion))
		return &authFailureError{"SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key, the credential scope region (expected '" + expected + "'), and the signing method.", http.StatusForbidden}
	}
	// Default mode, malformed token: keep the parse-error shape.
	log.Printf("Authentication Error: Invalid region. Expected %s, got %s", expected, strconvQuote(clientRegion))
	return &authFailureError{"AuthorizationHeaderMalformed",
		"Region in credential scope ('" + clientRegion + "') is incorrect; expected '" + expected + "'.", http.StatusBadRequest}
}

// region is the package-level verification region. Startup-only: written
// by SetRegion before request traffic, read-only afterwards.
var region = ""
