// federation.go — the FederationRegistry (accessstor appliance tree leaf
// 01): validates OIDC ID tokens against the IdP's discovery document and
// JWKS, then maps claims onto the existing Grant model. This registry is a
// SEPARATE credential decision point beside the IdentityRegistry seam — it
// authenticates bearer ID tokens, not access keys — so MultiRegistry, the
// frozen Identity shapes, and the rich-grant bridge are untouched.
//
// Security posture (fail-closed everywhere):
//   - Discovery is fetched and validated at construction: issuer must
//     equal the configured IssuerURL, jwks_uri must be present.
//   - Token signature verification uses ONLY RSA (RS/PS) or ECDSA (ES)
//     with the structural alg check from jwks.go — no HMAC path exists
//     here at all, so the classic HMAC-with-public-key confusion and
//     "none" are rejected structurally, before any crypto runs.
//   - iss, aud, and exp are all verified against the configuration; a
//     missing exp is an error, never an unlimited token.
//   - A missing/empty username claim fails closed: no principal is
//     synthesized from sub or any other claim.
//
// Principal format: fed:<issuer-host>/<username>. SAML is NOT parsed
// here: the appliance's Dex broker converts SAML to OIDC upstream, so
// mode "saml" is accepted at parse time and rejected with a broker-directed
// error (config forward-compat; leaf 02's SAML path depends on it).
//
// Charter: the JWKS cache is memory-only transient state; nothing here is
// persisted server-side.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// FederationConfig is the zeta_federation config block (leaf 01 Task 1).
// All fields come from the config file's "zeta_federation" object; see
// federationConfigFromServerConfig in package main for the JSON key names.
type FederationConfig struct {
	Mode          string // "oidc" only in this leaf; "saml" parses and fails with a broker-directed error
	IssuerURL     string
	ClientID      string
	Audience      string // empty defaults to ClientID
	TrustedCAs    string // path to a PEM bundle; empty = system roots
	GroupsClaim   string // default "groups"
	UsernameClaim string // default "preferred_username"

	// httpclient is test-injected (nil = a bounded default client). It is
	// deliberately unexported: the config surface is JSON-only.
	httpclient *http.Client
}

// defaults for the optional claim-name fields (leaf Contract 1).
const (
	defaultGroupsClaim   = "groups"
	defaultUsernameClaim = "preferred_username"
)

// FederationModeOIDC is the only mode this leaf implements.
const FederationModeOIDC = "oidc"

// FederationModeSAML is accepted at parse time and rejected with a
// broker-directed error: the appliance's Dex broker converts SAML to OIDC
// upstream, so this registry only ever sees OIDC-shaped tokens.
const FederationModeSAML = "saml"

// Principal names a federated identity: fed:<issuer-host>/<username>.
// It is distinct from the access-key namespace (Identity.AccessKeyID) on
// purpose: a federated principal never collides with a configured or env
// access key, and leaf 02's STS exchange is what mints S3 credentials for
// it (never this registry).
type Principal string

// FederationRegistry validates OIDC ID tokens and maps claims to grants.
type FederationRegistry struct {
	cfg       FederationConfig
	issuer    *url.URL
	discovery map[string]any
	jwks      *jwksCache
}

// NewFederationRegistry validates cfg, fetches the discovery document
// (<IssuerURL>/.well-known/openid-configuration), and wires the JWKS
// cache. Discovery is fetched EAGERLY at construction (fail-loud startup
// semantics, same posture as the rest of the config surface) and the JWKS
// document itself is fetched lazily on the first validation.
func NewFederationRegistry(cfg FederationConfig) (*FederationRegistry, error) {
	switch cfg.Mode {
	case FederationModeSAML:
		// Parse-and-fail with a broker-directed error (leaf Notes): Dex
		// fronts SAML upstreams and speaks OIDC to this registry; direct
		// SAML assertion consumption is not implemented in this leaf.
		return nil, fmt.Errorf("federation mode %q is not served directly: configure the SAML upstream in the identity broker (Dex) and use mode %q — the broker converts SAML assertions to OIDC ID tokens this registry can validate", FederationModeSAML, FederationModeOIDC)
	case FederationModeOIDC:
	default:
		return nil, fmt.Errorf("invalid federation mode %q (want %q, or %q via the identity broker)", cfg.Mode, FederationModeOIDC, FederationModeSAML)
	}
	if cfg.IssuerURL == "" {
		return nil, fmt.Errorf("federation: issuerURL is required")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("federation: clientID is required")
	}
	if cfg.Audience == "" {
		cfg.Audience = cfg.ClientID
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = defaultGroupsClaim
	}
	if cfg.UsernameClaim == "" {
		cfg.UsernameClaim = defaultUsernameClaim
	}
	issuer, err := url.Parse(cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("federation: issuerURL %q is not a valid URL: %w", cfg.IssuerURL, err)
	}
	if issuer.Scheme != "https" && issuer.Scheme != "http" {
		return nil, fmt.Errorf("federation: issuerURL %q must be http(s)", cfg.IssuerURL)
	}
	if issuer.Host == "" {
		return nil, fmt.Errorf("federation: issuerURL %q has no host", cfg.IssuerURL)
	}
	client := cfg.httpclient
	if client == nil {
		client = &http.Client{Timeout: jwksHTTPTimeout}
	}
	doc, err := fetchDiscovery(context.Background(), client, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	if advertised, _ := doc["issuer"].(string); advertised != cfg.IssuerURL {
		return nil, fmt.Errorf("federation: discovery issuer %q does not match configured issuerURL %q", advertised, cfg.IssuerURL)
	}
	jwksURI, _ := doc["jwks_uri"].(string)
	if jwksURI == "" {
		return nil, fmt.Errorf("federation: discovery document for %q has no jwks_uri", cfg.IssuerURL)
	}
	return &FederationRegistry{
		cfg:       cfg,
		issuer:    issuer,
		discovery: doc,
		jwks:      newJWKSCache(client, jwksURI),
	}, nil
}

// fetchDiscovery GETs <issuer>/.well-known/openid-configuration and decodes
// the JSON document. Exactly two HTTP GETs exist in this leaf: this one and
// the JWKS fetch — no OIDC SDK.
func fetchDiscovery(ctx context.Context, client *http.Client, issuerURL string) (map[string]any, error) {
	wellKnown := strings.TrimSuffix(issuerURL, "/") + "/.well-known/openid-configuration"
	ctx, cancel := context.WithTimeout(ctx, jwksHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return nil, fmt.Errorf("federation: discovery request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("federation: discovery fetch for %s failed: %w", wellKnown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("federation: discovery fetch for %s: unexpected status %s", wellKnown, resp.Status)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("federation: discovery document %s is not valid JSON: %w", wellKnown, err)
	}
	return doc, nil
}

// ValidateIDToken verifies one OIDC ID token end-to-end and returns the
// principal plus the FULL claim set. Fail-closed on every error path.
func (r *FederationRegistry) ValidateIDToken(ctx context.Context, idToken string) (Principal, map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", nil, fmt.Errorf("federation: token parse: not a JWS compact serialization")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", nil, fmt.Errorf("federation: token parse: header: %w", err)
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return "", nil, fmt.Errorf("federation: token parse: header: %w", err)
	}
	// Structural alg gate BEFORE any key lookup: none/HMAC names never
	// reach the JWKS cache at all.
	if err := algAllowedFor(jwksEntry{kid: header.Kid}, header.Alg); err != nil {
		// The kty is empty here so family checks would reject; run the
		// explicit accepted-alg check for the precise error instead.
		if !acceptedTokenAlg(header.Alg) {
			return "", nil, fmt.Errorf("federation: token alg %q is not an accepted signature algorithm", header.Alg)
		}
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", nil, fmt.Errorf("federation: token parse: payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return "", nil, fmt.Errorf("federation: token parse: payload: %w", err)
	}
	if header.Kid == "" {
		return "", nil, fmt.Errorf("federation: token header carries no key id")
	}
	entry, err := r.jwks.get(ctx, header.Kid)
	if err != nil {
		return "", nil, fmt.Errorf("federation: %w", err)
	}
	// Family check against the ACTUAL key (the structural half of the
	// alg-confusion defense; the token-level gate above passed already).
	if err := algAllowedFor(entry, header.Alg); err != nil {
		return "", nil, fmt.Errorf("federation: %w", err)
	}
	if err := r.verifySignature(entry, header.Alg, parts[0]+"."+parts[1], parts[2]); err != nil {
		return "", nil, fmt.Errorf("federation: signature verify: %w", err)
	}
	if err := r.verifyClaims(claims); err != nil {
		return "", nil, err
	}
	username, err := r.usernameFrom(claims)
	if err != nil {
		return "", nil, err
	}
	return Principal(fmt.Sprintf("fed:%s/%s", hostOf(r.cfg.IssuerURL), username)), claims, nil
}

// acceptedTokenAlg reports whether alg is in the accepted RS/PS/ES set.
func acceptedTokenAlg(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512",
		"ES256", "ES384", "ES512":
		return true
	}
	return false
}

// verifySignature runs the crypto for the accepted algorithm families.
// Only family-consistent (key, alg) pairs ever reach here (algAllowedFor
// ran against the resolved key), so each branch can trust its key type.
func (r *FederationRegistry) verifySignature(entry jwksEntry, alg string, signingInput, signatureB64 string) error {
	sig, err := base64.RawURLEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("signature encoding: %w", err)
	}
	digest, hash, err := signatureHash(alg, signingInput)
	if err != nil {
		return err
	}
	pub, err := signatureKey(entry, alg)
	if err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(alg, "RS"):
		return rsa.VerifyPKCS1v15(pub.rsa, hash, digest, sig)
	case strings.HasPrefix(alg, "PS"):
		opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: hash}
		return rsa.VerifyPSS(pub.rsa, hash, digest, sig, opts)
	case strings.HasPrefix(alg, "ES"):
		if !ecdsa.VerifyASN1(pub.ec, digest, sig) {
			return fmt.Errorf("ECDSA signature invalid")
		}
		return nil
	default:
		// Unreachable: the accepted-alg gate rejects anything else.
		return fmt.Errorf("algorithm %q rejected", alg)
	}
}

// signatureKeys bundles the concrete public-key types the verify branches
// need; exactly one is non-nil after signatureKey resolves it.
type signatureKeys struct {
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}

// signatureKey narrows the JWKS entry's key to the concrete type the alg
// family demands (the kty/alg family match was already enforced; this is
// the type-assertion backstop that fails closed rather than panics).
func signatureKey(entry jwksEntry, alg string) (signatureKeys, error) {
	if strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS") {
		k, ok := entry.key.(*rsa.PublicKey)
		if !ok {
			return signatureKeys{}, fmt.Errorf("alg %s requires an RSA key", alg)
		}
		return signatureKeys{rsa: k}, nil
	}
	k, ok := entry.key.(*ecdsa.PublicKey)
	if !ok {
		return signatureKeys{}, fmt.Errorf("alg %s requires an EC key", alg)
	}
	return signatureKeys{ec: k}, nil
}

// signatureHash hashes signingInput with the hash the alg demands.
func signatureHash(alg, signingInput string) ([]byte, crypto.Hash, error) {
	switch alg {
	case "RS256", "PS256", "ES256":
		d := sha256.Sum256([]byte(signingInput))
		return d[:], crypto.SHA256, nil
	case "RS384", "PS384", "ES384":
		d := sha512.Sum384([]byte(signingInput))
		return d[:], crypto.SHA384, nil
	case "RS512", "PS512", "ES512":
		d := sha512.Sum512([]byte(signingInput))
		return d[:], crypto.SHA512, nil
	}
	return nil, 0, fmt.Errorf("unsupported signature algorithm %q", alg)
}

// verifyClaims enforces iss / aud / exp against the configuration.
func (r *FederationRegistry) verifyClaims(claims map[string]any) error {
	if iss, _ := claims["iss"].(string); iss != r.cfg.IssuerURL {
		return fmt.Errorf("token issuer %q does not match configured issuer %q", iss, r.cfg.IssuerURL)
	}
	if !audienceContains(claims["aud"], r.cfg.Audience) {
		return fmt.Errorf("token audience does not include configured audience %q", r.cfg.Audience)
	}
	expRaw, ok := claims["exp"]
	if !ok {
		return fmt.Errorf("token carries no exp claim (fail-closed: expiry is mandatory)")
	}
	exp, err := numericDate(expRaw)
	if err != nil {
		return fmt.Errorf("token exp: %w", err)
	}
	if !time.Now().Before(exp) {
		return fmt.Errorf("token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	return nil
}

// audienceContains accepts both the string form and the array-of-strings
// form of "aud".
func audienceContains(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// numericDate decodes a JWT NumericDate (number, or a JSON number that
// arrived as float64 through encoding/json).
func numericDate(v any) (time.Time, error) {
	switch n := v.(type) {
	case float64:
		sec := int64(n)
		return time.Unix(sec, 0), nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return time.Time{}, fmt.Errorf("not a number: %w", err)
		}
		return time.Unix(int64(f), 0), nil
	default:
		return time.Time{}, fmt.Errorf("not a numeric date (%T)", v)
	}
}

// usernameFrom extracts the configured username claim; missing or empty
// fails closed (no principal is ever synthesized from another claim).
func (r *FederationRegistry) usernameFrom(claims map[string]any) (string, error) {
	u, _ := claims[r.cfg.UsernameClaim].(string)
	if strings.TrimSpace(u) == "" {
		return "", fmt.Errorf("token has no usable %q claim (fail-closed: the username claim is mandatory)", r.cfg.UsernameClaim)
	}
	return u, nil
}

// GrantsFor maps claims to the frozen two-bit Grant model (leaf Contract
// 1): each group g (from cfg.GroupsClaim) contributes a grant bucketed
// "fed:"+g with read-write ops, then the per-user grant "<username>/"
// read-write. The result is sorted (prefix order) and deduplicated;
// time-scoping is deliberately NOT applied here (the STS TTL governs, leaf
// 02). Groups that are absent or not a string array simply contribute
// nothing (the token's own validity is ValidateIDToken's job). The grants
// are bucket NAMES for the STS exchange to apply against the frozen
// map[string]Grant shape — one Grant per sorted prefix, in order.
func (r *FederationRegistry) GrantsFor(p Principal, claims map[string]any) []Grant {
	prefixes := make([]string, 0, 8)
	seen := map[string]bool{}
	add := func(prefix string) {
		if prefix == "" || seen[prefix] {
			return
		}
		seen[prefix] = true
		prefixes = append(prefixes, prefix)
	}
	if groups, ok := claims[r.cfg.GroupsClaim].([]any); ok {
		for _, g := range groups {
			if s, ok := g.(string); ok && s != "" {
				add("fed:" + s)
			}
		}
	}
	if u, ok := claims[r.cfg.UsernameClaim].(string); ok && u != "" {
		add(u + "/")
	}
	sort.Strings(prefixes)
	grants := make([]Grant, 0, len(prefixes))
	for range prefixes {
		grants = append(grants, Grant{Read: true, Write: true})
	}
	return grants
}

// hostOf extracts the host (no port) from a URL string; on any parse
// trouble the whole string is the host so the principal stays stable.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if h, _, err := net.SplitHostPort(u.Host); err == nil {
		return h
	}
	return u.Host
}
