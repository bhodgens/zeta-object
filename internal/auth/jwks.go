// jwks.go — the JWKS fetch/cache/refresh seam (accessstor appliance tree
// leaf 01 Task 2). Fetches the IdP's JSON Web Key Set, caches parsed keys
// by key id, refreshes EXACTLY ONCE per token validation when a token
// carries an unknown kid (rotation path), and negative-caches failures for
// 30s so a flood of forged kids or an IdP outage cannot turn every
// validation into a fetch storm.
//
// Security posture (leaf Notes):
//   - ONLY RSA and EC public keys are parsed. Symmetric ("oct") JWKS
//     entries are skipped structurally, which kills the classic
//     HMAC-with-public-key confusion at the trust-store level: no HMAC
//     verification key can ever be derived from a JWKS document here.
//   - algAllowedFor is the structural alg check: a key's kty (and, when
//     the JWK advertises an explicit "alg", that exact value) must match
//     the token's alg family, so RS/PS/ES tokens can never verify against
//     the wrong key class and "none" is never a legal alg.
//
// Charter: the cache is memory-only transient state, never persisted.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// jwksNegativeCacheTTL is how long a failed (or kid-missing) refresh stays
// negative-cached: repeated validations inside the window reuse the failure
// instead of hammering the IdP.
const jwksNegativeCacheTTL = 30 * time.Second

// jwksHTTPTimeout bounds a single JWKS fetch so a hung IdP cannot stall
// every validation path indefinitely.
const jwksHTTPTimeout = 10 * time.Second

// jwksEntry is one cached JWKS key. alg is the JWK's explicit "alg" when
// advertised ("" = family inference from kty); crv is the EC curve.
type jwksEntry struct {
	kid string
	kty string
	crv string
	alg string
	key crypto.PublicKey // *rsa.PublicKey or *ecdsa.PublicKey
}

// algAllowedFor enforces the structural alg check: the token's alg must
// belong to the family the key type advertises, and when the JWK carries an
// explicit "alg" it must match exactly. Fail-closed on everything else.
func algAllowedFor(entry jwksEntry, tokenAlg string) error {
	switch tokenAlg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
		if entry.kty != "RSA" {
			return fmt.Errorf("token alg %s requires an RSA key, JWKS key %q is %s", tokenAlg, entry.kid, entry.kty)
		}
	case "ES256", "ES384", "ES512":
		if entry.kty != "EC" {
			return fmt.Errorf("token alg %s requires an EC key, JWKS key %q is %s", tokenAlg, entry.kid, entry.kty)
		}
		if entry.crv != curveForAlg(tokenAlg) {
			return fmt.Errorf("token alg %s requires curve %s, JWKS key %q advertises %s", tokenAlg, curveForAlg(tokenAlg), entry.kid, entry.crv)
		}
	default:
		// "none", HMAC names, anything else: reject structurally.
		return fmt.Errorf("token alg %q is not an accepted signature algorithm", tokenAlg)
	}
	if entry.alg != "" && entry.alg != tokenAlg {
		return fmt.Errorf("JWKS key %q advertises alg %s, token used %s", entry.kid, entry.alg, tokenAlg)
	}
	return nil
}

// curveForAlg maps an ES* alg to the curve its size demands.
func curveForAlg(alg string) string {
	switch alg {
	case "ES256":
		return elliptic.P256().Params().Name
	case "ES384":
		return elliptic.P384().Params().Name
	default:
		return elliptic.P521().Params().Name
	}
}

// jwksDocument is the raw JWKS response shape.
type jwksDocument struct {
	Keys []map[string]json.RawMessage `json:"keys"`
}

// jwksCache caches parsed JWKS keys by kid under a mutex. A failed or
// kid-missing refresh is negative-cached (negativeUntil) so the retry cost
// is one map lookup, not one HTTP fetch, until the TTL passes.
type jwksCache struct {
	client *http.Client
	url    string

	mu      sync.Mutex
	entries map[string]jwksEntry
	// negativeUntil marks a kid whose refresh recently failed (endpoint
	// error, malformed key, or kid-absent-after-successful-fetch): gets
	// inside the window fail closed without refetching.
	negativeUntil map[string]time.Time
	// lastRefreshAt is the global one-refresh-per-validation throttle:
	// validations arriving within the window after a failed refresh reuse
	// the failure rather than refetching. (Per-kid negativeUntil is the
	// precise mechanism; this guards the cold path where nothing is known
	// about the kid yet.)
	lastRefreshFailure time.Time

	// now is the clock seam tests move (negative-cache TTL expiry).
	now func() time.Time
}

// newJWKSCache builds a cache fetching url with client (tests inject an
// httptest-compatible client; nil falls back to a bounded default).
func newJWKSCache(client *http.Client, url string) *jwksCache {
	if client == nil {
		client = &http.Client{Timeout: jwksHTTPTimeout}
	}
	return &jwksCache{
		client:        client,
		url:           url,
		entries:       map[string]jwksEntry{},
		negativeUntil: map[string]time.Time{},
		now:           time.Now,
	}
}

// get resolves kid. Cached keys answer immediately; an unknown kid
// triggers AT MOST ONE refresh (the caller's max-one-per-validation rule
// holds because get is called once per validation path); a failed or
// kid-absent refresh is negative-cached for jwksNegativeCacheTTL.
func (c *jwksCache) get(ctx context.Context, kid string) (jwksEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if entry, ok := c.entries[kid]; ok {
		return entry, nil
	}
	if until, neg := c.negativeUntil[kid]; neg && now.Before(until) {
		return jwksEntry{}, fmt.Errorf("JWKS refresh for key id %q recently failed; refusing to refetch inside the %s negative-cache window", kid, jwksNegativeCacheTTL)
	}
	if !c.lastRefreshFailure.IsZero() && now.Sub(c.lastRefreshFailure) < jwksNegativeCacheTTL {
		return jwksEntry{}, fmt.Errorf("a JWKS refresh recently failed; refusing to refetch inside the %s negative-cache window", jwksNegativeCacheTTL)
	}
	entry, found, err := c.refreshLocked(ctx, kid)
	if err != nil {
		c.negativeUntil[kid] = now.Add(jwksNegativeCacheTTL)
		c.lastRefreshFailure = now
		return jwksEntry{}, err
	}
	// A successful fetch that does not contain the kid is still a miss:
	// fail closed and negative-cache the kid (forged-kid fetch-storm guard).
	if !found {
		c.negativeUntil[kid] = now.Add(jwksNegativeCacheTTL)
		return jwksEntry{}, fmt.Errorf("key id %q not present in the JWKS document", kid)
	}
	delete(c.negativeUntil, kid)
	c.entries[kid] = entry
	return entry, nil
}

// refreshLocked fetches and parses the JWKS document, returning the entry
// for kid and whether it was present (a present=false result with nil error
// is a successful fetch of a document that lacks the kid). Caller holds c.mu.
func (c *jwksCache) refreshLocked(ctx context.Context, kid string) (jwksEntry, bool, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, jwksHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, c.url, nil)
	if err != nil {
		return jwksEntry{}, false, fmt.Errorf("JWKS request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return jwksEntry{}, false, fmt.Errorf("JWKS fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jwksEntry{}, false, fmt.Errorf("JWKS fetch: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return jwksEntry{}, false, fmt.Errorf("JWKS read: %w", err)
	}
	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return jwksEntry{}, false, fmt.Errorf("JWKS parse: %w", err)
	}
	for i, raw := range doc.Keys {
		if ktyJSON, ok := raw["kty"]; !ok || string(ktyJSON) != `"RSA"` && string(ktyJSON) != `"EC"` {
			continue // oct/unknown key types are skipped, never trusted
		}
		entry, err := parseJWK(raw)
		if err != nil {
			return jwksEntry{}, false, fmt.Errorf("JWKS key %d: %w", i, err)
		}
		if entry.kid == kid {
			return entry, true, nil
		}
	}
	return jwksEntry{}, false, nil
}

// parseJWK parses ONE RSA or EC JWK entry. Malformed values are a hard
// error (fail the refresh) — a trust store must never guess.
func parseJWK(raw map[string]json.RawMessage) (jwksEntry, error) {
	entry := jwksEntry{}
	if err := json.Unmarshal(raw["kty"], &entry.kty); err != nil {
		return entry, fmt.Errorf("kty: %w", err)
	}
	if err := json.Unmarshal(raw["kid"], &entry.kid); err != nil {
		return entry, fmt.Errorf("kid: %w", err)
	}
	if entry.kid == "" {
		return entry, fmt.Errorf("kid must not be empty")
	}
	// "alg" and "crv" are optional / EC-only.
	if alg, ok := raw["alg"]; ok {
		if err := json.Unmarshal(alg, &entry.alg); err != nil {
			return entry, fmt.Errorf("alg: %w", err)
		}
	}
	if crv, ok := raw["crv"]; ok {
		if err := json.Unmarshal(crv, &entry.crv); err != nil {
			return entry, fmt.Errorf("crv: %w", err)
		}
	}
	switch entry.kty {
	case "RSA":
		n, err := b64uBigInt(raw["n"])
		if err != nil {
			return entry, fmt.Errorf("n: %w", err)
		}
		e, err := b64uBigInt(raw["e"])
		if err != nil {
			return entry, fmt.Errorf("e: %w", err)
		}
		if e.BitLen() > 32 || !e.IsInt64() || e.Int64() <= 0 || e.Int64() > (1<<31) {
			return entry, fmt.Errorf("implausible exponent")
		}
		entry.key = &rsa.PublicKey{N: n, E: int(e.Int64())}
	case "EC":
		curve, ok := curveByName(entry.crv)
		if !ok {
			return entry, fmt.Errorf("unsupported curve %q", entry.crv)
		}
		x, err := b64uBytes(raw["x"])
		if err != nil {
			return entry, fmt.Errorf("x: %w", err)
		}
		y, err := b64uBytes(raw["y"])
		if err != nil {
			return entry, fmt.Errorf("y: %w", err)
		}
		// Uncompressed SEC 1 point (0x04 || X || Y), validated on-curve by
		// ParseUncompressedPublicKey — a malformed point fails the refresh.
		size := (curve.Params().BitSize + 7) / 8
		if len(x) != size || len(y) != size {
			return entry, fmt.Errorf("coordinate length mismatch: want %d bytes each, got %d/%d", size, len(x), len(y))
		}
		point := make([]byte, 1+2*size)
		point[0] = 0x04
		copy(point[1:], x)
		copy(point[1+size:], y)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return entry, fmt.Errorf("invalid EC point: %w", err)
		}
		entry.key = pub
	default:
		return entry, fmt.Errorf("unsupported key type %q", entry.kty)
	}
	return entry, nil
}

// b64uBigInt decodes a base64url unsigned integer field.
func b64uBigInt(raw json.RawMessage) (*big.Int, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// b64uBytes decodes a base64url field preserving any leading zero bytes
// (EC coordinates are fixed-width; big.Int would strip them).
func b64uBytes(raw json.RawMessage) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// curveByName resolves the supported EC curves.
func curveByName(name string) (elliptic.Curve, bool) {
	switch name {
	case elliptic.P256().Params().Name:
		return elliptic.P256(), true
	case elliptic.P384().Params().Name:
		return elliptic.P384(), true
	case elliptic.P521().Params().Name:
		return elliptic.P521(), true
	}
	return nil, false
}
