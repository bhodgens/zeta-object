// oidc_test.go — the console OIDC flow against a full local IdP (leaf 03
// Task 2): discovery, JWKS and token endpoints emulated with httptest; ID
// tokens signed RS256 with locally generated keys (leaf 01's test-token
// discipline, re-implemented locally in this package). Covers the whole
// matrix: login redirect shape, state validation, single-use replay
// rejection, code exchange, nonce binding, session mint, logout clearing,
// mTLS precedence, and the disabled-mode 404s.
package admin

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// --- local OIDC IdP stand-in ---------------------------------------------

// testIdP emulates Dex: discovery, JWKS, and a token endpoint that returns
// one RS256 ID token per issued code.
type testIdP struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	codes    map[string]map[string]any // code -> ID-token claims
	lastForm url.Values                // the last token-endpoint form (assertions)
	failNext int                       // >0: next N token calls answer 500
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	idp := &testIdP{t: t, key: key, kid: "test-1", codes: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 idp.issuer,
			"jwks_uri":               idp.issuer + "/jwks",
			"authorization_endpoint": idp.issuer + "/auth",
			"token_endpoint":         idp.issuer + "/token",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub, err := x509.MarshalPKIXPublicKey(&idp.key.PublicKey)
		if err != nil {
			t.Fatalf("marshal pkix: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": idp.kid,
				"alg": "RS256",
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(modulusBytes(t, &idp.key.PublicKey)),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(idp.key.PublicKey.E)).Bytes()),
			}},
		})
		_ = pub
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.lastForm = r.PostForm
		if idp.failNext > 0 {
			idp.failNext--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		code := r.PostForm.Get("code")
		claims, ok := idp.codes[code]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		delete(idp.codes, code) // single use
		_, _ = w.Write([]byte(`{"id_token":"` + idp.signIDToken(claims) + `"}`))
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	idp.issuer = idp.srv.URL
	return idp
}

func modulusBytes(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()
	return pub.N.Bytes()
}

// signIDToken produces one RS256 compact JWS signed with the test key.
func (idp *testIdP) signIDToken(claims map[string]any) string {
	idp.t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": idp.kid})
	payload, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, sum[:])
	if err != nil {
		idp.t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// issueCode registers a code the token endpoint will honor once.
func (idp *testIdP) issueCode(nonce string, exp time.Time) string {
	code := "code-" + idp.issuer + "-" + time.Now().Format("150405.000000000")
	idp.codes[code] = map[string]any{
		"iss":                idp.issuer,
		"aud":                "zeta-console",
		"exp":                exp.Unix(),
		"preferred_username": "alice",
		"nonce":              nonce,
	}
	return code
}

// registry builds a FederationRegistry pointed at the test IdP (leaf 01's
// constructor does the real discovery fetch + issuer match).
func (idp *testIdP) registry(t *testing.T) *auth.FederationRegistry {
	t.Helper()
	reg, err := auth.NewFederationRegistry(auth.FederationConfig{
		Mode:      "oidc",
		IssuerURL: idp.issuer,
		ClientID:  "zeta-console",
		Audience:  "zeta-console",
	})
	if err != nil {
		t.Fatalf("NewFederationRegistry: %v", err)
	}
	return reg
}

// --- flow harness ---------------------------------------------------------

// fedEnv is one admin frontend with console federation enabled against a
// test IdP, plus the fixed secret the cookie key derives from.
type fedEnv struct {
	*testEnv
	idp *testIdP
	fed *fedSessions
}

func newFedEnv(t *testing.T) *fedEnv {
	t.Helper()
	idp := newTestIdP(t)
	fed, err := newAdminFederation(federationDeps{
		Cfg: ConsoleOIDCConfig{
			Enabled:          true,
			ClientID:         "zeta-console",
			ClientSecretFile: "/dev/null-not-read-in-tests",
			SessionTTL:       0,
		},
		Registry: idp.registry(t),
		Discovery: &oidcDiscovery{
			AuthorizationEndpoint: idp.issuer + "/auth",
			TokenEndpoint:         idp.issuer + "/token",
			Issuer:                idp.issuer,
		},
		Secret: "test-client-secret",
	})
	if err != nil {
		t.Fatalf("newAdminFederation: %v", err)
	}
	env := newTestEnv(t, Options{})
	fed.mtls = env.frontend
	env.frontend.fed = fed
	return &fedEnv{testEnv: env, idp: idp, fed: fed}
}

// plainRequest builds a request with NO client certificate.
func plainRequest(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

// cookieValue extracts one cookie from a recorder.
func cookieValue(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q cookie in response", name)
	return nil
}

// doLogin drives login → callback end to end, returning the callback
// recorder with the handler injected so error branches can be staged.
func (e *fedEnv) doLogin(t *testing.T, stage func(beforeCallback *testIdP)) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(login, plainRequest(http.MethodGet, "/auth/login"))
	if login.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", login.Code)
	}
	stateCookie := cookieValue(t, login, fedStateCookieName)
	st, err := verifyStateCookieValue(e.fed.key[:], stateCookie.Value, time.Now())
	if err != nil {
		t.Fatalf("state cookie did not verify: %v", err)
	}
	code := e.idp.issueCode(st.Nonce, time.Now().Add(time.Hour))
	if stage != nil {
		stage(e.idp)
	}
	cb := plainRequest(http.MethodGet, "/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(st.State))
	cb.AddCookie(&http.Cookie{Name: fedStateCookieName, Value: stateCookie.Value})
	rec := httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(rec, cb)
	return rec
}

// --- the table ------------------------------------------------------------

func TestOIDC_FlowMatrix(t *testing.T) {
	t.Run("login redirects to discovery authorization endpoint with state+nonce", func(t *testing.T) {
		e := newFedEnv(t)
		login := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(login, plainRequest(http.MethodGet, "/auth/login"))
		if login.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", login.Code)
		}
		loc, err := url.Parse(login.Header().Get("Location"))
		if err != nil {
			t.Fatalf("Location: %v", err)
		}
		if got := loc.Scheme + "://" + loc.Host + loc.Path; got != e.idp.issuer+"/auth" {
			t.Fatalf("redirect = %q, want the discovery authorization_endpoint", got)
		}
		q := loc.Query()
		if q.Get("response_type") != "code" || q.Get("client_id") != "zeta-console" ||
			q.Get("scope") != "openid" || q.Get("state") == "" || q.Get("nonce") == "" {
			t.Fatalf("authorization query = %v", q)
		}
		if q.Get("redirect_uri") != "http://"+plainRequest("GET", "/").Host+"/auth/callback" {
			t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
		}
		if host := plainRequest("GET", "/").Host; host != "example.com" {
			t.Fatalf("prereq: httptest default host = %q (this assertion pins it)", host)
		}
		// State cookie: signed, scoped, HttpOnly.
		sc := cookieValue(t, login, fedStateCookieName)
		if !sc.HttpOnly || sc.Path != "/auth/callback" {
			t.Fatalf("state cookie = %+v", sc)
		}
		if _, err := verifyStateCookieValue(e.fed.key[:], sc.Value, time.Now()); err != nil {
			t.Fatalf("state cookie does not verify: %v", err)
		}
	})

	t.Run("full flow mints a valid session and redirects to the FIXED root", func(t *testing.T) {
		e := newFedEnv(t)
		rec := e.doLogin(t, nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback status = %d body=%s, want 302", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("callback redirect = %q, want fixed \"/\" (no open redirect)", loc)
		}
		sess := cookieValue(t, rec, fedSessionCookieName)
		principal, err := verifySessionCookieValue(e.fed.key[:], sess.Value, time.Now())
		if err != nil {
			t.Fatalf("session cookie does not verify: %v", err)
		}
		// hostOf strips the port from the issuer URL (leaf 01 principal
		// format fed:<issuer-host>/<username> — host WITHOUT port).
		want := "fed:127.0.0.1/alice"
		if principal != want {
			t.Fatalf("principal = %q, want %q", principal, want)
		}
		if !sess.HttpOnly || !sess.Secure {
			t.Fatalf("session cookie flags = %+v", sess)
		}
		// The state cookie is consumed.
		for _, c := range rec.Result().Cookies() {
			if c.Name == fedStateCookieName && c.MaxAge >= 0 {
				t.Fatalf("state cookie not cleared: %+v", c)
			}
		}
		// The token endpoint saw the full server-side exchange form.
		if got := e.idp.lastForm.Get("grant_type"); got != "authorization_code" {
			t.Fatalf("grant_type = %q", got)
		}
		if got := e.idp.lastForm.Get("client_secret"); got != "test-client-secret" {
			t.Fatalf("client_secret not sent server-side")
		}
	})

	t.Run("login without federation-ready callback works over httptest host", func(t *testing.T) {
		e := newFedEnv(t)
		req := plainRequest(http.MethodGet, "/auth/login")
		req.Host = "admin.example:9443"
		req.TLS = &tls.ConnectionState{} // production is always TLS
		login := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(login, req)
		loc, _ := url.Parse(login.Header().Get("Location"))
		if got := loc.Query().Get("redirect_uri"); got != "https://admin.example:9443/auth/callback" {
			t.Fatalf("redirect_uri = %q (explicit Host honored over TLS scheme)", got)
		}
	})

	t.Run("callback with mismatched state is rejected", func(t *testing.T) {
		e := newFedEnv(t)
		login := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(login, plainRequest(http.MethodGet, "/auth/login"))
		sc := cookieValue(t, login, fedStateCookieName)
		code := e.idp.issueCode("nonce", time.Now().Add(time.Hour))
		cb := plainRequest(http.MethodGet, "/auth/callback?code="+code+"&state=WRONG")
		cb.AddCookie(&http.Cookie{Name: fedStateCookieName, Value: sc.Value})
		rec := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(rec, cb)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("replayed state cookie is rejected (single use)", func(t *testing.T) {
		e := newFedEnv(t)
		rec := e.doLogin(t, nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("first callback = %d", rec.Code)
		}
		// Replay the SAME code+state+cookie: the code is consumed AND the
		// cookie was cleared; both independently fail.
		login := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(login, plainRequest(http.MethodGet, "/auth/login"))
		sc := cookieValue(t, login, fedStateCookieName)
		st, err := verifyStateCookieValue(e.fed.key[:], sc.Value, time.Now())
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		code := e.idp.issueCode(st.Nonce, time.Now().Add(time.Hour))
		replay := plainRequest(http.MethodGet, "/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(st.State))
		replay.AddCookie(&http.Cookie{Name: fedStateCookieName, Value: sc.Value})
		first := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(first, replay)
		if first.Code != http.StatusFound {
			t.Fatalf("first use = %d", first.Code)
		}
		second := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(second, replay)
		if second.Code != http.StatusUnauthorized {
			t.Fatalf("replayed callback = %d, want 401", second.Code)
		}
	})

	t.Run("expired state cookie is rejected", func(t *testing.T) {
		e := newFedEnv(t)
		e.fed.now = func() time.Time { return testNow }
		login := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(login, plainRequest(http.MethodGet, "/auth/login"))
		sc := cookieValue(t, login, fedStateCookieName)
		e.fed.now = func() time.Time { return testNow.Add(fedStateTTL + time.Minute) }
		cb := plainRequest(http.MethodGet, "/auth/callback?code=c&state=s")
		cb.AddCookie(&http.Cookie{Name: fedStateCookieName, Value: sc.Value})
		rec := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(rec, cb)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("callback with no state cookie is rejected", func(t *testing.T) {
		e := newFedEnv(t)
		rec := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(rec, plainRequest(http.MethodGet, "/auth/callback?code=c&state=s"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("IdP error response maps to 401 LoginFailed", func(t *testing.T) {
		e := newFedEnv(t)
		rec := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(rec, plainRequest(http.MethodGet, "/auth/callback?error=access_denied&error_description=nope"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "LoginFailed") {
			t.Fatalf("body = %s, want LoginFailed code", rec.Body.String())
		}
	})

	t.Run("token endpoint failure never leaks IdP detail", func(t *testing.T) {
		e := newFedEnv(t)
		rec := e.doLogin(t, func(idp *testIdP) { idp.failNext = 1 })
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "500") || strings.Contains(body, "Internal") {
			t.Fatalf("body leaks IdP detail: %s", body)
		}
	})

	t.Run("token with wrong nonce is rejected", func(t *testing.T) {
		e := newFedEnv(t)
		login := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(login, plainRequest(http.MethodGet, "/auth/login"))
		sc := cookieValue(t, login, fedStateCookieName)
		st, err := verifyStateCookieValue(e.fed.key[:], sc.Value, time.Now())
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		code := e.idp.issueCode("DIFFERENT-NONCE", time.Now().Add(time.Hour))
		cb := plainRequest(http.MethodGet, "/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(st.State))
		cb.AddCookie(&http.Cookie{Name: fedStateCookieName, Value: sc.Value})
		rec := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(rec, cb)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("logout clears the session and audits", func(t *testing.T) {
		e := newFedEnv(t)
		rec := e.doLogin(t, nil)
		sess := cookieValue(t, rec, fedSessionCookieName)
		var calls []auditCall
		e.frontend.opts.Audit = func(principal, method, bucket, key, op string, status int, denied bool) {
			calls = append(calls, auditCall{principal, method, bucket, key, op, status, denied})
		}
		lo := plainRequest(http.MethodPost, "/auth/logout")
		lo.AddCookie(&http.Cookie{Name: fedSessionCookieName, Value: sess.Value})
		out := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(out, lo)
		if out.Code != http.StatusOK {
			t.Fatalf("logout status = %d, want 200", out.Code)
		}
		cleared := cookieValue(t, out, fedSessionCookieName)
		if cleared.MaxAge >= 0 {
			t.Fatalf("session cookie MaxAge = %d, want < 0 (drop)", cleared.MaxAge)
		}
		if len(calls) != 1 || calls[0].op != "admin" || calls[0].principal == "" {
			t.Fatalf("logout audit = %+v, want one op=admin record with the fed principal", calls)
		}
		// Stateless-cookie tradeoff (documented in session.go): the server
		// cannot revoke a presented value before its expiry — logout
		// clears the BROWSER's cookie via MaxAge=-1 (asserted above).
		// There is deliberately no session store (charter: no persistent
		// server-owned state). A short session TTL bounds exposure.
	})

	t.Run("logout without any credential is 401", func(t *testing.T) {
		e := newFedEnv(t)
		rec := httptest.NewRecorder()
		e.frontend.Handler().ServeHTTP(rec, plainRequest(http.MethodPost, "/auth/logout"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong-method on fed routes is a 405", func(t *testing.T) {
		e := newFedEnv(t)
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/auth/login"},
			{http.MethodPost, "/auth/callback"},
			{http.MethodGet, "/auth/logout"},
		} {
			rec := httptest.NewRecorder()
			e.frontend.Handler().ServeHTTP(rec, plainRequest(tc.method, tc.path))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
			}
		}
	})
}

// TestOIDC_SessionAuthenticatesCapabilities pins the fed_session →
// /capabilities path and mTLS PRECEDENCE when both credentials are present.
func TestOIDC_SessionAuthenticatesCapabilities(t *testing.T) {
	e := newFedEnv(t)
	e.frontend.capabilities = &CapabilityDeps{
		ListDatasets: func(context.Context) ([]string, error) { return []string{"tank/photos"}, nil },
	}
	rec := e.doLogin(t, nil)
	sess := cookieValue(t, rec, fedSessionCookieName)

	cap := plainRequest(http.MethodGet, "/capabilities")
	cap.AddCookie(&http.Cookie{Name: fedSessionCookieName, Value: sess.Value})
	capRec := httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(capRec, cap)
	if capRec.Code != http.StatusOK {
		t.Fatalf("fed-session capabilities = %d body=%s, want 200", capRec.Code, capRec.Body.String())
	}

	// Precedence: a valid mTLS cert PLUS the session authenticates as the
	// CERT CN (mTLS wins), never as the fed principal.
	both := e.authedRequest(http.MethodGet, "/capabilities")
	both.AddCookie(&http.Cookie{Name: fedSessionCookieName, Value: sess.Value})
	var seen string
	e.frontend.opts.Audit = func(principal, _, _, _, _ string, _ int, _ bool) { seen = principal }
	bothRec := httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(bothRec, both)
	if bothRec.Code != http.StatusOK {
		t.Fatalf("both-credentials capabilities = %d", bothRec.Code)
	}
	if seen != "tester" {
		t.Fatalf("mTLS precedence violated: audited principal = %q, want cert CN tester", seen)
	}

	// A TAMPERED session cookie authenticates nothing even with mTLS
	// absent.
	tampered := plainRequest(http.MethodGet, "/capabilities")
	tampered.AddCookie(&http.Cookie{Name: fedSessionCookieName, Value: sess.Value[:len(sess.Value)-4] + "AAAA"})
	tRec := httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(tRec, tampered)
	if tRec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered session = %d, want 401", tRec.Code)
	}
}

// TestOIDC_DisabledMode pin the byte-identical disabled behavior: /auth/*
// is 404 again, no cookie authenticates anything, and construction with an
// enabled config but broken wiring fails LOUD.
func TestOIDC_DisabledMode(t *testing.T) {
	env := newTestEnv(t, Options{})
	for _, path := range []string{"/auth/login", "/auth/callback"} {
		rec := httptest.NewRecorder()
		env.frontend.Handler().ServeHTTP(rec, plainRequest(http.MethodGet, path))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("disabled %s = %d, want 404", path, rec.Code)
		}
	}
	// A fed_session cookie from ANY source never authenticates when
	// console OIDC is disabled.
	req := plainRequest(http.MethodGet, "/capabilities")
	req.AddCookie(&http.Cookie{Name: fedSessionCookieName, Value: "whatever"})
	rec := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled-mode fed cookie = %d, want 401", rec.Code)
	}

	// Construction failures are loud and specific.
	idp := newTestIdP(t)
	for name, deps := range map[string]federationDeps{
		"nil registry":    {Cfg: ConsoleOIDCConfig{Enabled: true}},
		"empty client id": {Cfg: ConsoleOIDCConfig{Enabled: true, ClientID: ""}, Registry: idp.registry(t), Discovery: &oidcDiscovery{AuthorizationEndpoint: "a", TokenEndpoint: "t"}, Secret: "s"},
		"no discovery":    {Cfg: ConsoleOIDCConfig{Enabled: true, ClientID: "c"}, Registry: idp.registry(t), Secret: "s"},
		"no secret":       {Cfg: ConsoleOIDCConfig{Enabled: true, ClientID: "c"}, Registry: idp.registry(t), Discovery: &oidcDiscovery{AuthorizationEndpoint: "a", TokenEndpoint: "t"}},
	} {
		if _, err := newAdminFederation(deps); err == nil {
			t.Fatalf("%s: construction accepted", name)
		}
	}
	// Disabled config is (nil, nil).
	if f, err := newAdminFederation(federationDeps{}); f != nil || err != nil {
		t.Fatalf("disabled construction = (%v, %v), want (nil, nil)", f, err)
	}
}

// TestOIDC_AuditRecords pins that a successful login writes exactly one
// standard 8-key audit record (op=admin) attributed to the fed principal.
func TestOIDC_AuditRecords(t *testing.T) {
	e := newFedEnv(t)
	var calls []auditCall
	e.frontend.opts.Audit = func(principal, method, bucket, key, op string, status int, denied bool) {
		calls = append(calls, auditCall{principal, method, bucket, key, op, status, denied})
	}
	rec := e.doLogin(t, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d", rec.Code)
	}
	want := "fed:127.0.0.1/alice"
	if len(calls) != 1 {
		t.Fatalf("login wrote %d audit records, want 1 (%+v)", len(calls), calls)
	}
	c := calls[0]
	if c.principal != want || c.op != "admin" || c.method != http.MethodPost || c.status != http.StatusOK || c.denied {
		t.Fatalf("login audit = %+v, want one op=admin 200 record for %q", c, want)
	}
}
