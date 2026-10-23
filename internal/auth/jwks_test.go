// jwks_test.go — the JWKS fetch/cache/refresh seam (accessstor appliance
// tree leaf 01 Task 2). Table-driven where it counts: fresh fetch, cache
// hit, unknown-kid single refresh, refresh failure fails closed with a
// 30s negative cache, unsupported key types skipped, malformed keys fail
// the refresh. Every test drives a local httptest server — no network.
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// b64u is the base64url (no padding) encoding JWKS uses.
func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// testRSAKey / testECKey are generated once per call; 2048-bit RSA keeps
// signing fast enough while staying realistic.
func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

func testECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	return k
}

// rsaJWK renders one RSA JWK entry. alg empty = omit the field (family
// inference path); set it to pin the advertised-alg path.
func rsaJWK(kid string, priv *rsa.PrivateKey, alg string) map[string]any {
	m := map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"n":   b64u(priv.N.Bytes()),
		"e":   b64u(big.NewInt(int64(priv.E)).Bytes()),
	}
	if alg != "" {
		m["alg"] = alg
	}
	return m
}

// ecJWK renders one EC JWK entry (P-256 keys only in these tests).
func ecJWK(kid string, priv *ecdsa.PrivateKey, alg string) map[string]any {
	size := (priv.Curve.Params().BitSize + 7) / 8
	m := map[string]any{
		"kty": "EC",
		"kid": kid,
		"use": "sig",
		"crv": priv.Curve.Params().Name,
		"x":   b64u(priv.X.FillBytes(make([]byte, size))),
		"y":   b64u(priv.Y.FillBytes(make([]byte, size))),
	}
	if alg != "" {
		m["alg"] = alg
	}
	return m
}

func jwksDoc(keys ...map[string]any) []byte {
	doc, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		panic(err) // map[string]any with JSON-safe values never fails
	}
	return doc
}

// jwksTestServer serves a mutable JWKS document at /jwks and counts hits.
type jwksTestServer struct {
	srv  *httptest.Server
	hits atomic.Int32
	doc  atomic.Value // []byte
	fail atomic.Bool  // when true, every request answers 500
}

func newJWKSTestServer(t *testing.T, initial []byte) *jwksTestServer {
	t.Helper()
	s := &jwksTestServer{}
	s.doc.Store(initial)
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		if s.fail.Load() {
			http.Error(w, "jwks unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.doc.Load().([]byte))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func TestJWKSCacheFreshFetchThenCacheHit(t *testing.T) {
	priv := testRSAKey(t)
	ts := newJWKSTestServer(t, jwksDoc(rsaJWK("k1", priv, "")))
	c := newJWKSCache(ts.srv.Client(), ts.srv.URL+"/jwks")

	entry, err := c.get(context.Background(), "k1")
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if _, ok := entry.key.(*rsa.PublicKey); !ok {
		t.Errorf("entry.key is %T, want *rsa.PublicKey", entry.key)
	}
	if entry.kty != "RSA" {
		t.Errorf("entry.kty = %q, want RSA", entry.kty)
	}
	if hits := ts.hits.Load(); hits != 1 {
		t.Fatalf("hits after first get = %d, want 1 (fresh fetch)", hits)
	}

	// Second get: cache hit, no HTTP traffic.
	if _, err := c.get(context.Background(), "k1"); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if hits := ts.hits.Load(); hits != 1 {
		t.Errorf("hits after cache hit = %d, want 1 (no refetch)", hits)
	}
}

func TestJWKSCacheUnknownKidRefreshesExactlyOnce(t *testing.T) {
	priv := testRSAKey(t)
	ts := newJWKSTestServer(t, jwksDoc(rsaJWK("old", priv, "")))
	c := newJWKSCache(ts.srv.Client(), ts.srv.URL+"/jwks")

	if _, err := c.get(context.Background(), "old"); err != nil {
		t.Fatalf("warm-up get: %v", err)
	}
	if hits := ts.hits.Load(); hits != 1 {
		t.Fatalf("warm-up hits = %d, want 1", hits)
	}

	// A NEW key appears at the IdP (rotation): the very next get for its
	// kid must refresh exactly once and then serve from cache.
	priv2 := testRSAKey(t)
	ts.doc.Store(jwksDoc(rsaJWK("old", priv, ""), rsaJWK("new", priv2, "")))
	if _, err := c.get(context.Background(), "new"); err != nil {
		t.Fatalf("get after rotation: %v", err)
	}
	if hits := ts.hits.Load(); hits != 2 {
		t.Errorf("hits after rotation get = %d, want 2 (exactly one refresh)", hits)
	}
	if _, err := c.get(context.Background(), "new"); err != nil {
		t.Fatalf("second get of rotated key: %v", err)
	}
	if hits := ts.hits.Load(); hits != 2 {
		t.Errorf("hits after second rotated get = %d, want 2 (cache hit)", hits)
	}
}

func TestJWKSCacheRefreshFailureFailsClosedWithNegativeCache(t *testing.T) {
	priv := testRSAKey(t)
	ts := newJWKSTestServer(t, jwksDoc(rsaJWK("k1", priv, "")))
	c := newJWKSCache(ts.srv.Client(), ts.srv.URL+"/jwks")
	c.now = time.Now // pinned here so the negative-window test can move it

	if _, err := c.get(context.Background(), "k1"); err != nil {
		t.Fatalf("warm-up get: %v", err)
	}
	ts.fail.Store(true)

	// Unknown kid + failing JWKS endpoint: fail closed, and the failure is
	// negative-cached so the retry does NOT hit the server again.
	if _, err := c.get(context.Background(), "missing"); err == nil {
		t.Fatal("get with failing JWKS endpoint succeeded, want error")
	}
	if hits := ts.hits.Load(); hits != 2 {
		t.Fatalf("hits after failed refresh = %d, want 2", hits)
	}
	if _, err := c.get(context.Background(), "missing"); err == nil {
		t.Fatal("get inside negative-cache window succeeded, want error")
	}
	if hits := ts.hits.Load(); hits != 2 {
		t.Errorf("hits inside negative-cache window = %d, want 2 (no fetch storm)", hits)
	}

	// Past the window: exactly one more fetch is attempted.
	c.now = func() time.Time { return time.Now().Add(jwksNegativeCacheTTL + time.Second) }
	if _, err := c.get(context.Background(), "missing"); err == nil {
		t.Fatal("get after negative-cache expiry succeeded, want error")
	}
	if hits := ts.hits.Load(); hits != 3 {
		t.Errorf("hits after window expiry = %d, want 3 (one retry)", hits)
	}
}

func TestJWKSCacheUnknownKidAfterSuccessfulRefreshIsNegativeCached(t *testing.T) {
	priv := testRSAKey(t)
	ts := newJWKSTestServer(t, jwksDoc(rsaJWK("k1", priv, "")))
	c := newJWKSCache(ts.srv.Client(), ts.srv.URL+"/jwks")

	// The refresh SUCCEEDS but the kid is simply not there: fail closed and
	// negative-cache so a flood of forged kids cannot fetch-storm the IdP.
	if _, err := c.get(context.Background(), "forged"); err == nil {
		t.Fatal("get of unknown kid succeeded, want error")
	}
	if hits := ts.hits.Load(); hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
	if _, err := c.get(context.Background(), "forged"); err == nil {
		t.Fatal("second get of unknown kid succeeded, want negative-cached error")
	}
	if hits := ts.hits.Load(); hits != 1 {
		t.Errorf("hits = %d, want 1 (negative cache held)", hits)
	}
}

func TestJWKSCacheSkipsSymmetricKeysAndRejectsMalformed(t *testing.T) {
	priv := testRSAKey(t)
	t.Run("oct keys are skipped, never trusted", func(t *testing.T) {
		ts := newJWKSTestServer(t, jwksDoc(map[string]any{
			"kty": "oct", "kid": "sym", "alg": "HS256", "k": b64u([]byte("secret")),
		}, rsaJWK("k1", priv, "")))
		c := newJWKSCache(ts.srv.Client(), ts.srv.URL+"/jwks")
		if _, err := c.get(context.Background(), "sym"); err == nil {
			t.Fatal("symmetric JWKS key resolved, want skip (kid miss)")
		}
		// The RSA key beside it still resolves.
		if _, err := c.get(context.Background(), "k1"); err != nil {
			t.Fatalf("RSA key beside oct key: %v", err)
		}
	})
	t.Run("malformed RSA key fails the refresh", func(t *testing.T) {
		ts := newJWKSTestServer(t, jwksDoc(map[string]any{
			"kty": "RSA", "kid": "broken", "n": "!!!not-base64url!!!", "e": "AQAB",
		}))
		c := newJWKSCache(ts.srv.Client(), ts.srv.URL+"/jwks")
		if _, err := c.get(context.Background(), "broken"); err == nil {
			t.Fatal("malformed RSA JWKS key accepted, want refresh failure")
		}
	})
	t.Run("non-200 fails the refresh", func(t *testing.T) {
		ts := newJWKSTestServer(t, jwksDoc())
		ts.fail.Store(true)
		c := newJWKSCache(ts.srv.Client(), ts.srv.URL+"/jwks")
		if _, err := c.get(context.Background(), "k1"); err == nil {
			t.Fatal("JWKS 500 accepted, want error")
		}
	})
}

func TestAlgAllowedForRejectsConfusion(t *testing.T) {
	priv := testRSAKey(t)
	ec := testECKey(t)
	cases := []struct {
		name     string
		entry    jwksEntry
		tokenAlg string
		wantOK   bool
	}{
		{"RSA family, no advertised alg", jwksEntry{kid: "k", kty: "RSA", key: priv.Public}, "RS256", true},
		{"PS within RSA family", jwksEntry{kid: "k", kty: "RSA", key: priv.Public}, "PS256", true},
		{"HMAC vs RSA key", jwksEntry{kid: "k", kty: "RSA", key: priv.Public}, "HS256", false},
		{"advertised alg exact match", jwksEntry{kid: "k", kty: "RSA", alg: "RS256", key: priv.Public}, "RS256", true},
		{"advertised alg mismatch (RS384 vs RS256)", jwksEntry{kid: "k", kty: "RSA", alg: "RS256", key: priv.Public}, "RS384", false},
		{"EC curve match", jwksEntry{kid: "k", kty: "EC", crv: "P-256", key: ec.Public}, "ES256", true},
		{"EC curve mismatch", jwksEntry{kid: "k", kty: "EC", crv: "P-256", key: ec.Public}, "ES512", false},
		{"EC vs HMAC", jwksEntry{kid: "k", kty: "EC", crv: "P-256", key: ec.Public}, "HS256", false},
		{"none is never allowed", jwksEntry{kid: "k", kty: "RSA", key: priv.Public}, "none", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := algAllowedFor(tc.entry, tc.tokenAlg)
			if tc.wantOK && err != nil {
				t.Errorf("algAllowedFor(%s) = %v, want nil", tc.tokenAlg, err)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("algAllowedFor(%s) = nil, want alg-confusion rejection", tc.tokenAlg)
			}
		})
	}
}
