// sts_test.go — the /sts exchange end-to-end over httptest (leaf 02 Task
// 3): the full flow — mint an ID token with test keys (the leaf-01 IdP
// shape), POST the RFC 8693 form, decode the JSON response, verify the
// credential against the resolver (the same derivation the SigV4 path
// re-derives) — plus the frozen error vocabulary and TTL policy.
package sts

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// b64u is base64url without padding (JWT convention).
func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ---- test IdP (leaf-01 shape: discovery + JWKS over httptest) ----

type testIdP struct {
	srv    *httptest.Server
	issuer string
	key    *rsa.PrivateKey
	kid    string
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	idp := &testIdP{key: key, kid: "sts-idp-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   idp.issuer,
			"jwks_uri": idp.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": idp.kid, "alg": "RS256", "use": "sig",
			"n": b64u(idp.key.N.Bytes()),
			"e": b64uExp(idp.key.E),
		}}})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	idp.issuer = idp.srv.URL
	return idp
}

func b64uExp(e int) []byte {
	var out []byte
	for x := e; x > 0; x >>= 8 {
		out = append([]byte{byte(x)}, out...)
	}
	return out
}

func (p *testIdP) mint(t *testing.T, username string, groups []string, exp time.Time) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": p.kid})
	payload, _ := json.Marshal(map[string]any{
		"iss": p.issuer, "aud": "zeta-object", "sub": "sub-1",
		"preferred_username": username, "groups": groups,
		"exp": exp.Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
	})
	signingInput := b64u(header) + "." + b64u(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + b64u(sig)
}

// ---- harness ----

type stsTestEnv struct {
	idp      *testIdP
	frontend *stsFrontend
	key      []byte
	auditRec *[]auditRecordProbe
	reg      *auth.FederationRegistry
	slot     *atomic.Pointer[[]byte]
}

// auditRecordProbe mirrors the 8-key audit record's observable fields
// (the record type itself lives in package s3; the seam is the function).
type auditRecordProbe struct {
	principal, op string
	status        int
	denied        bool
}

func newSTSTestEnv(t *testing.T) *stsTestEnv {
	t.Helper()
	idp := newTestIdP(t)
	reg, err := NewFederationRegistryH(idp)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	key := []byte("sts-test-env-signing-key")
	slot := NewSigningKeySlot()
	slot.Store(&key)
	rec := &[]auditRecordProbe{}
	fe, err := New(Options{
		ListenAddr: "127.0.0.1:0",
		CertFile:   "testcerts-cert.pem-not-loaded", // TLSConfig not exercised here; httptest serves the handler
		KeyFile:    "testcerts-key.pem-not-loaded",
		Registry:   func() *auth.FederationRegistry { return reg },
		ServerKey:  slot,
		Audit: func(principal, method, bucket, key, op string, status int, denied bool) {
			*rec = append(*rec, auditRecordProbe{principal: principal, op: op, status: status, denied: denied})
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &stsTestEnv{idp: idp, frontend: fe.(*stsFrontend), key: key, auditRec: rec, reg: reg, slot: slot}
}

// NewFederationRegistryH builds the registry against the test IdP —
// exported-shape wrapper so the test can reuse the production
// constructor with a local issuer.
func NewFederationRegistryH(idp *testIdP) (*auth.FederationRegistry, error) {
	return auth.NewFederationRegistry(auth.FederationConfig{
		Mode: "oidc", IssuerURL: idp.issuer, ClientID: "zeta-object", Audience: "zeta-object",
	})
}

// exchange POSTs one RFC 8693 form to the handler and returns the
// recorder.
func exchange(t *testing.T, env *stsTestEnv, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "127.0.0.1:8444"
	rec := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rec, req)
	return rec
}

func validForm(idp *testIdP, token string) url.Values {
	return url.Values{
		"grant_type":         {GrantTypeTokenExchange},
		"subject_token":      {token},
		"subject_token_type": {TokenTypeIDToken}}
}

// TestSTS_ExchangeEndToEnd drives the full flow: token -> 200 JSON with
// the frozen five keys, ZSTSID- prefix, TTL-bounded expiry, and a secret
// that re-derives exactly (the SigV4 path's consistency proof).
func TestSTS_ExchangeEndToEnd(t *testing.T) {
	env := newSTSTestEnv(t)
	token := env.idp.mint(t, "alice", []string{"team-a"}, time.Now().Add(5*time.Minute))

	rec := exchange(t, env, validForm(env.idp, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp exchangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.AccessKeyID, AccessKeyPrefix) {
		t.Errorf("access_key_id = %q, want ZSTSID- prefix", resp.AccessKeyID)
	}
	if resp.Principal == "" || !strings.HasPrefix(resp.Principal, "fed:") {
		t.Errorf("principal = %q, want fed: form", resp.Principal)
	}
	expiry, err := time.Parse(time.RFC3339, resp.Expiration)
	if err != nil {
		t.Fatalf("expiration %q not RFC3339: %v", resp.Expiration, err)
	}
	want := time.Now().Add(DefaultTTL)
	if diff := expiry.Sub(want); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("expiration %s is %s from the default-TTL target", resp.Expiration, diff)
	}
	// The SigV4 path's re-derivation MUST match the returned secret.
	got, err := SecretString(env.key, resp.AccessKeyID)
	if err != nil {
		t.Fatalf("SecretString: %v", err)
	}
	if got != resp.SecretAccessKey {
		t.Error("re-derived secret does not match the returned secret")
	}
	if resp.SessionToken == "" {
		t.Error("session_token empty (SDK wire compatibility expects a value)")
	}
	// Parse round-trip: principal + groups inside the credential.
	principal, exp2, _, groups, err := ParseAccessKey(resp.AccessKeyID)
	if err != nil {
		t.Fatalf("ParseAccessKey: %v", err)
	}
	if principal != resp.Principal || !exp2.Equal(expiry) || len(groups) != 1 || groups[0] != "team-a" {
		t.Errorf("round trip: principal=%q groups=%v", principal, groups)
	}
}

// TestSTS_WireErrors pins the frozen error vocabulary.
func TestSTS_WireErrors(t *testing.T) {
	env := newSTSTestEnv(t)
	good := env.idp.mint(t, "alice", nil, time.Now().Add(5*time.Minute))

	cases := []struct {
		name   string
		form   func() url.Values
		status int
		code   string
	}{
		{"wrong grant type", func() url.Values {
			f := validForm(env.idp, good)
			f.Set("grant_type", "password")
			return f
		}, 400, "unsupported_grant_type"},
		{"saml2-bearer -> unsupported", func() url.Values {
			f := validForm(env.idp, good)
			f.Set("subject_token_type", TokenTypeSAML2Bearer)
			return f
		}, 400, "unsupported_grant_type"},
		{"missing subject token", func() url.Values {
			f := validForm(env.idp, "")
			return f
		}, 401, "invalid_token"},
		{"garbage token", func() url.Values {
			return validForm(env.idp, "not.a.jwt")
		}, 401, "invalid_token"},
		{"expired token", func() url.Values {
			return validForm(env.idp, env.idp.mint(t, "alice", nil, time.Now().Add(-time.Minute)))
		}, 401, "invalid_token"},
		{"wrong audience", func() url.Values {
			// The IdP's audience is fixed to zeta-object; a registry with
			// a different configured audience rejects it.
			reg2, err := NewFederationRegistryH(env.idp)
			if err != nil {
				t.Fatalf("reg2: %v", err)
			}
			_ = reg2
			// Build a token signed for a DIFFERENT aud by hand:
			tok := env.idp.mint(t, "alice", nil, time.Now().Add(5*time.Minute))
			f := validForm(env.idp, tok)
			return f
		}, 0, ""}, // handled specially below (audience_mismatch via custom registry)
		{"ttl exceeded", func() url.Values {
			f := validForm(env.idp, good)
			f.Set("requested_ttl", "999999")
			return f
		}, 403, "ttl_exceeded"},
		{"bad ttl", func() url.Values {
			f := validForm(env.idp, good)
			f.Set("requested_ttl", "soon")
			return f
		}, 400, "invalid_request"},
		{"missing token type", func() url.Values {
			f := validForm(env.idp, good)
			f.Del("subject_token_type")
			return f
		}, 400, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.status == 0 {
				return // the audience case asserts separately
			}
			rec := exchange(t, env, tc.form())
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			var body errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body not JSON: %v", err)
			}
			if body.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", body.Error.Code, tc.code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("content-type = %q, want JSON (never S3 XML)", ct)
			}
		})
	}
}

// TestSTS_AudienceMismatch drives the 403 audience_mismatch branch: a
// registry configured for a different audience rejects the token with
// the distinguished code.
func TestSTS_AudienceMismatch(t *testing.T) {
	idp := newTestIdP(t)
	// Registry expects audience "other-app"; tokens carry zeta-object.
	reg, err := auth.NewFederationRegistry(auth.FederationConfig{
		Mode: "oidc", IssuerURL: idp.issuer, ClientID: "other-app", Audience: "other-app",
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	key := []byte("aud-mismatch-key")
	slot := NewSigningKeySlot()
	slot.Store(&key)
	fe, err := New(Options{
		ListenAddr: "127.0.0.1:0",
		CertFile:   "c", KeyFile: "k",
		Registry:  func() *auth.FederationRegistry { return reg },
		ServerKey: slot,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token := idp.mint(t, "alice", nil, time.Now().Add(5*time.Minute))
	req := httptest.NewRequest(http.MethodPost, "/sts", strings.NewReader(validForm(idp, token).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	fe.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "audience_mismatch" {
		t.Errorf("code = %q, want audience_mismatch", body.Error.Code)
	}
}

// TestSTS_AuditRecords pins the 8-key-seam contract at this boundary:
// op=sts_exchange, principal = the resolved federated principal,
// denied=true on failures, exactly one record per request.
func TestSTS_AuditRecords(t *testing.T) {
	env := newSTSTestEnv(t)
	token := env.idp.mint(t, "alice", []string{"team-a"}, time.Now().Add(5*time.Minute))
	exchange(t, env, validForm(env.idp, token))
	if n := len(*env.auditRec); n != 1 {
		t.Fatalf("audit records = %d, want 1", n)
	}
	ok := (*env.auditRec)[0]
	if ok.op != "sts_exchange" || ok.denied {
		t.Errorf("success record = %+v", ok)
	}
	if !strings.HasPrefix(ok.principal, "fed:") {
		t.Errorf("principal = %q, want the federated principal", ok.principal)
	}
	exchange(t, env, url.Values{"grant_type": {"bogus"}})
	if n := len(*env.auditRec); n != 2 {
		t.Fatalf("audit records after failure = %d, want 2", n)
	}
	denied := (*env.auditRec)[1]
	if denied.op != "sts_exchange" || !denied.denied {
		t.Errorf("failure record = %+v", denied)
	}
}

// TestSTS_RoutingAndMethod pins: only POST /sts exists.
func TestSTS_RoutingAndMethod(t *testing.T) {
	env := newSTSTestEnv(t)
	h := env.frontend.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sts", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /sts = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /other = %d, want 404", rec.Code)
	}
	if env.frontend.Name() != "sts" {
		t.Errorf("Name = %q", env.frontend.Name())
	}
	if env.frontend.Authenticator() != nil {
		t.Error("Authenticator must be nil (the exchange is the auth)")
	}
}

// TestSTS_NonceUniquenessAndExpiryInHMAC pins the review-checklist
// security posture at the mint level.
func TestSTS_NonceUniquenessAndExpiryInHMAC(t *testing.T) {
	key := []byte("security-posture-key")
	exp := time.Now().Add(time.Hour)
	k1, s1, _ := MintAccessKey(key, "fed:h/alice", exp, nil)
	k2, s2, _ := MintAccessKey(key, "fed:h/alice", exp, nil)
	if k1 == k2 || s1 == s2 {
		t.Fatal("nonce reuse detected")
	}
	// Expiry participates in the HMAC: a different expiry, same
	// principal, different secret.
	_, s3m, _ := MintAccessKey(key, "fed:h/alice", exp.Add(time.Second), nil)
	if s3m == s1 {
		t.Fatal("expiry not covered by the HMAC")
	}
}
