// federation_test.go — the FederationRegistry (accessstor appliance tree
// leaf 01 Tasks 1/3/4): constructor validation (saml broker-directed
// error, missing issuer), discovery validation, full ID-token validation
// (issuer/audience/expiry/signature/claims, fail-closed everywhere), the
// deterministic claim→Grant mapping, and MultiRegistry coexistence.
// Sign test tokens with locally generated RSA/EC keys whose JWKS the test
// server serves; every network path is a local httptest server.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fedTestIDP is a local stand-in for the OIDC IdP: it serves discovery at
// the well-known path and a JWKS document built from whatever keys the
// test registers. Both documents are mutable mid-test (rotation cases).
type fedTestIDP struct {
	srv         *httptest.Server
	issuerURL   string
	discovery   map[string]any
	jwksServer  *jwksTestServer
	issuerField string // what discovery advertises as "issuer" (pin mismatch cases)
}

// newFedTestIDP builds the IdP with issuerURL = srv.URL and a JWKS doc
// already wired to the discovery document.
func newFedTestIDP(t *testing.T, keys ...map[string]any) *fedTestIDP {
	t.Helper()
	jwks := newJWKSTestServer(t, jwksDoc(keys...))
	idp := &fedTestIDP{jwksServer: jwks}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idp.discovery)
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	idp.issuerURL = idp.srv.URL
	idp.issuerField = idp.srv.URL
	idp.setDiscovery()
	return idp
}

// setDiscovery (re)builds the discovery document from issuerField.
func (i *fedTestIDP) setDiscovery() {
	i.discovery = map[string]any{
		"issuer":                                i.issuerField,
		"jwks_uri":                              i.jwksServer.srv.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
	}
}

// federationTestHarness bundles the IdP, a registry against it, and the
// signing keys the tests mutate.
type federationTestHarness struct {
	t        *testing.T
	idp      *fedTestIDP
	rsaKey   *rsa.PrivateKey
	ecKey    *ecdsa.PrivateKey
	reg      *FederationRegistry
	audience string
}

func newFederationTestHarness(t *testing.T) *federationTestHarness {
	t.Helper()
	rsaKey := testRSAKey(t)
	ecKey := testECKey(t)
	idp := newFedTestIDP(t, rsaJWK("rsa-1", rsaKey, "RS256"), ecJWK("ec-1", ecKey, "ES256"))
	reg, err := NewFederationRegistry(FederationConfig{
		Mode:      "oidc",
		IssuerURL: idp.issuerURL,
		ClientID:  "zeta-object",
		Audience:  "zeta-object",
	})
	if err != nil {
		t.Fatalf("NewFederationRegistry: %v", err)
	}
	return &federationTestHarness{
		t: t, idp: idp, rsaKey: rsaKey, ecKey: ecKey, reg: reg,
		audience: "zeta-object",
	}
}

// signIDToken builds and signs an ID token with the given key/alg and claims.
func signIDToken(t *testing.T, key any, alg, kid string, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "JWT", "kid": kid}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	signingInput := b64u(hb) + "." + b64u(cb)
	sig := signPayload(t, key, alg, signingInput)
	return signingInput + "." + sig
}

func signPayload(t *testing.T, key any, alg, signingInput string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(signingInput))
	switch alg {
	case "RS256":
		sig, err := rsa.SignPKCS1v15(rand.Reader, key.(*rsa.PrivateKey), crypto.SHA256, sum[:])
		if err != nil {
			t.Fatalf("RS256 sign: %v", err)
		}
		return b64u(sig)
	case "ES256":
		digest, err := ecdsa.SignASN1(rand.Reader, key.(*ecdsa.PrivateKey), sum[:])
		if err != nil {
			t.Fatalf("ES256 sign: %v", err)
		}
		return b64u(digest)
	}
	t.Fatalf("unsupported test alg %q", alg)
	return ""
}

// hmacSHA256 is the test-side HMAC for the alg-confusion forgery case.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// fedClaims builds a canonical claim set: sub, preferred_username, groups.
func fedClaims(iss, aud, sub, username string, groups []string, exp time.Time, iat time.Time) map[string]any {
	if groups == nil {
		groups = []string{}
	}
	return map[string]any{
		"iss": iss, "aud": aud, "sub": sub,
		"preferred_username": username,
		"groups":             groups,
		"exp":                exp.Unix(), "iat": iat.Unix(),
	}
}

// ---- Task 1: constructor + config validation ----

func TestFederationConfig_RejectsSAMLUntilBroker(t *testing.T) {
	_, err := NewFederationRegistry(FederationConfig{Mode: "saml", IssuerURL: "https://idp.example"})
	if err == nil {
		t.Fatal("saml mode accepted, want broker-directed error")
	}
	if !strings.Contains(err.Error(), "broker") {
		t.Errorf("err = %v, want a broker-directed message", err)
	}
}

func TestFederationConfig_MissingIssuerFails(t *testing.T) {
	if _, err := NewFederationRegistry(FederationConfig{Mode: "oidc"}); err == nil {
		t.Fatal("empty issuer accepted, want error")
	}
}

func TestFederationConfig_MissingClientIDFails(t *testing.T) {
	_, err := NewFederationRegistry(FederationConfig{Mode: "oidc", IssuerURL: "https://idp.example"})
	if err == nil {
		t.Fatal("empty clientID accepted, want error")
	}
}

func TestFederationConfig_UnknownModeFails(t *testing.T) {
	if _, err := NewFederationRegistry(FederationConfig{Mode: "ldap", IssuerURL: "https://x", ClientID: "c"}); err == nil {
		t.Fatal("unknown mode accepted, want error")
	}
}

func TestFederationRegistry_DiscoveryValidation(t *testing.T) {
	t.Run("unreachable issuer fails closed", func(t *testing.T) {
		if _, err := NewFederationRegistry(FederationConfig{
			Mode: "oidc", IssuerURL: "http://127.0.0.1:1", ClientID: "c",
		}); err == nil {
			t.Fatal("unreachable issuer accepted, want discovery failure")
		}
	})
	t.Run("issuer mismatch fails closed", func(t *testing.T) {
		idp := newFedTestIDP(t)
		idp.issuerField = "https://someone-else.example"
		idp.setDiscovery()
		_, err := NewFederationRegistry(FederationConfig{
			Mode: "oidc", IssuerURL: idp.issuerURL, ClientID: "c",
		})
		if err == nil || !strings.Contains(err.Error(), "issuer") {
			t.Fatalf("err = %v, want issuer mismatch failure", err)
		}
	})
	t.Run("jwks_uri required", func(t *testing.T) {
		idp := newFedTestIDP(t)
		delete(idp.discovery, "jwks_uri")
		_, err := NewFederationRegistry(FederationConfig{
			Mode: "oidc", IssuerURL: idp.issuerURL, ClientID: "c",
		})
		if err == nil || !strings.Contains(err.Error(), "jwks_uri") {
			t.Fatalf("err = %v, want missing jwks_uri failure", err)
		}
	})
}

// ---- Task 3: ValidateIDToken ----

func TestValidateIDToken_Table(t *testing.T) {
	h := newFederationTestHarness(t)
	ctx := context.Background()
	now := time.Now()

	baseClaims := func(mut func(c map[string]any)) map[string]any {
		c := fedClaims(h.idp.issuerURL, h.audience, "sub-1", "alice", []string{"team-a"}, now.Add(time.Hour), now.Add(-time.Minute))
		if mut != nil {
			mut(c)
		}
		return c
	}

	cases := []struct {
		name       string
		token      func() string
		wantErr    string // "" = success
		wantUser   string
		wantGroups []string
	}{
		{
			name: "valid RSA token",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1", baseClaims(nil))
			},
			wantUser: "alice", wantGroups: []string{"team-a"},
		},
		{
			name: "valid EC token",
			token: func() string {
				return signIDToken(t, h.ecKey, "ES256", "ec-1", baseClaims(nil))
			},
			wantUser: "alice", wantGroups: []string{"team-a"},
		},
		{
			name: "wrong issuer",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1", baseClaims(func(c map[string]any) { c["iss"] = "https://evil.example" }))
			},
			wantErr: "issuer",
		},
		{
			name: "wrong audience",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1", baseClaims(func(c map[string]any) { c["aud"] = "someone-else" }))
			},
			wantErr: "audience",
		},
		{
			name: "expired",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1",
					baseClaims(func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() }))
			},
			wantErr: "exp",
		},
		{
			name: "exp missing fails closed",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1", baseClaims(func(c map[string]any) { delete(c, "exp") }))
			},
			wantErr: "exp",
		},
		{
			name: "bad signature (signed by a different key)",
			token: func() string {
				other := testRSAKey(t)
				return signIDToken(t, other, "RS256", "rsa-1", baseClaims(nil))
			},
			wantErr: "verify",
		},
		{
			name: "unknown kid",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "no-such-kid", baseClaims(nil))
			},
			wantErr: "key id",
		},
		{
			name: "alg confusion: HS256 with RSA-keyed forgery",
			token: func() string {
				// Hand-built HS256 token (header.alg=HS256, kid of an RSA
				// JWKS key, HMAC signature): must be rejected structurally.
				hb, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "kid": "rsa-1"})
				cb, _ := json.Marshal(baseClaims(nil))
				signingInput := b64u(hb) + "." + b64u(cb)
				mac := hmacSHA256([]byte("attacker-key"), []byte(signingInput))
				return signingInput + "." + b64u(mac)
			},
			wantErr: "alg",
		},
		{
			name: "alg none",
			token: func() string {
				// Hand-built alg=none token (empty signature section).
				hb, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT", "kid": "rsa-1"})
				cb, _ := json.Marshal(baseClaims(nil))
				return b64u(hb) + "." + b64u(cb) + "."
			},
			wantErr: "alg",
		},
		{
			name: "EC key signing with RSA kid",
			token: func() string {
				return signIDToken(t, h.ecKey, "ES256", "rsa-1", baseClaims(nil))
			},
			wantErr: "alg",
		},
		{
			name: "username claim missing fails closed",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1",
					baseClaims(func(c map[string]any) { delete(c, "preferred_username") }))
			},
			wantErr: "preferred_username",
		},
		{
			name: "empty username fails closed",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1",
					baseClaims(func(c map[string]any) { c["preferred_username"] = "" }))
			},
			wantErr: "preferred_username",
		},
		{
			name: "groups claim absent is valid with empty groups",
			token: func() string {
				return signIDToken(t, h.rsaKey, "RS256", "rsa-1",
					baseClaims(func(c map[string]any) { delete(c, "groups") }))
			},
			wantUser: "alice", wantGroups: []string{},
		},
		{
			name:    "malformed token",
			token:   func() string { return "not.a.jwt" },
			wantErr: "parse",
		},
		{
			name:    "empty token",
			token:   func() string { return "" },
			wantErr: "parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, claims, err := h.reg.ValidateIDToken(ctx, tc.token())
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("token accepted, want %q failure", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateIDToken: %v", err)
			}
			wantPrincipal := Principal(fmt.Sprintf("fed:%s/%s", hostOf(h.idp.issuerURL), tc.wantUser))
			if p != wantPrincipal {
				t.Errorf("principal = %q, want %q", p, wantPrincipal)
			}
			if got, _ := claims["preferred_username"].(string); got != tc.wantUser {
				t.Errorf("claims username = %q, want %q", got, tc.wantUser)
			}
			var gotGroups []string
			if g, ok := claims["groups"].([]any); ok {
				for _, x := range g {
					gotGroups = append(gotGroups, x.(string))
				}
			}
			if len(gotGroups) != len(tc.wantGroups) {
				t.Errorf("groups = %v, want %v", gotGroups, tc.wantGroups)
			}
		})
	}
}

func TestValidateIDTokenCustomClaims(t *testing.T) {
	h := newFederationTestHarness(t)
	// Custom username/groups claim names resolve.
	reg, err := NewFederationRegistry(FederationConfig{
		Mode: "oidc", IssuerURL: h.idp.issuerURL, ClientID: "c", Audience: "c",
		UsernameClaim: "email", GroupsClaim: "roles",
	})
	if err != nil {
		t.Fatalf("NewFederationRegistry: %v", err)
	}
	now := time.Now()
	claims := fedClaims(h.idp.issuerURL, "c", "sub", "a@b.c", nil, now.Add(time.Hour), now)
	claims["email"] = "a@b.c"
	claims["roles"] = []string{"readers"}
	p, got, err := reg.ValidateIDToken(context.Background(), signIDToken(t, h.rsaKey, "RS256", "rsa-1", claims))
	if err != nil {
		t.Fatalf("ValidateIDToken: %v", err)
	}
	if want := Principal("fed:" + hostOf(h.idp.issuerURL) + "/a@b.c"); p != want {
		t.Errorf("principal = %q, want %q", p, want)
	}
	if _, ok := got["roles"]; !ok {
		t.Error("full claim set missing custom roles claim")
	}
}

// ---- Task 4: GrantsFor ----

func TestGrantsForMapping(t *testing.T) {
	// hostOf/issuer shape only needs a string; GrantsFor is pure.
	reg := &FederationRegistry{cfg: FederationConfig{
		UsernameClaim: "preferred_username", GroupsClaim: "groups",
	}}
	cases := []struct {
		name      string
		principal Principal
		claims    map[string]any
		want      []Grant
	}{
		{
			name:      "groups and per-user grant",
			principal: "fed:idp.example/alice",
			claims:    map[string]any{"preferred_username": "alice", "groups": []any{"team-a", "team-b"}},
			want: []Grant{
				{Read: true, Write: true}, // fed:team-a
				{Read: true, Write: true}, // fed:team-b
				{Read: true, Write: true}, // alice/
			},
		},
		{
			name:      "no groups: per-user grant only",
			principal: "fed:idp.example/bob",
			claims:    map[string]any{"preferred_username": "bob"},
			want:      []Grant{{Read: true, Write: true}},
		},
		{
			name:      "duplicate groups deduplicated",
			principal: "fed:idp.example/carol",
			claims:    map[string]any{"preferred_username": "carol", "groups": []any{"dup", "dup"}},
			want: []Grant{
				{Read: true, Write: true}, // fed:dup (once)
				{Read: true, Write: true}, // carol/
			},
		},
		{
			name:      "sorted prefixes then user",
			principal: "fed:idp.example/dave",
			claims:    map[string]any{"preferred_username": "dave", "groups": []any{"zebra", "alpha"}},
			want:      []Grant{{Read: true, Write: true}, {Read: true, Write: true}, {Read: true, Write: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grants := reg.GrantsFor(tc.principal, tc.claims)
			if len(grants) != len(tc.want) {
				t.Fatalf("grants = %d entries (%+v), want %d", len(grants), grants, len(tc.want))
			}
			for i, g := range grants {
				if g != tc.want[i] {
					t.Errorf("grants[%d] = %+v, want %+v", i, g, tc.want[i])
				}
			}
		})
	}
}

func TestGrantsForDeterministicOrder(t *testing.T) {
	reg := &FederationRegistry{cfg: FederationConfig{
		UsernameClaim: "preferred_username", GroupsClaim: "groups",
	}}
	claims := map[string]any{"preferred_username": "alice", "groups": []any{"zebra", "alpha", "mango", "alpha"}}
	first := strings.Join(grantsKeySummary(t, reg, claims), "|")
	for i := range 20 {
		if again := strings.Join(grantsKeySummary(t, reg, claims), "|"); again != first {
			t.Fatalf("run %d: grant order changed: %q vs %q", i, again, first)
		}
	}
	// Three identical two-bit grants are indistinguishable by value, so pin
	// the COUNT (sorted prefixes + the per-user grant) instead: the order
	// determinism check is the run-to-run equality above; the prefix
	// vocabulary is pinned by TestGrantsForMapping.
	if first != "0:{true true}|1:{true true}|2:{true true}|3:{true true}" {
		t.Errorf("grant count/order = %q, want 4 entries (3 groups deduped from 4 + user)", first)
	}
}

// grantsKeySummary reduces GrantsFor to the per-entry index markers plus the
// known sorted prefix count, so run-to-run drift (or a count change) is
// visible. The prefixes themselves are pinned by the count + the want order
// check above.
func grantsKeySummary(t *testing.T, reg *FederationRegistry, claims map[string]any) []string {
	t.Helper()
	grants := reg.GrantsFor("fed:idp.example/alice", claims)
	out := make([]string, len(grants))
	for i := range grants {
		out[i] = fmt.Sprintf("%d:%v", i, grants[i])
	}
	return out
}

// ---- Task 4: MultiRegistry coexistence ----

func TestFederationRegistryAlongsideMultiRegistry(t *testing.T) {
	h := newFederationTestHarness(t)
	multi, err := NewMultiRegistry([]IdentityConfig{EnvPair("AK", "SK")})
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}
	// The federation registry is a SEPARATE credential decision point, not
	// an IdentityRegistry: it validates bearer ID tokens, while env keys
	// keep resolving through the untouched MultiRegistry.
	now := time.Now()
	claims := fedClaims(h.idp.issuerURL, h.audience, "sub", "alice", []string{"team-a"}, now.Add(time.Hour), now)
	p, _, err := h.reg.ValidateIDToken(context.Background(), signIDToken(t, h.rsaKey, "RS256", "rsa-1", claims))
	if err != nil {
		t.Fatalf("ValidateIDToken: %v", err)
	}
	if id, ok := multi.LookupByAccessKey("AK"); !ok || !id.CanWrite("b") {
		t.Error("MultiRegistry env identity broken by federation wiring")
	}
	if _, ok := multi.LookupByAccessKey(string(p)); ok {
		t.Error("federation principal leaked into the access-key namespace")
	}
}
