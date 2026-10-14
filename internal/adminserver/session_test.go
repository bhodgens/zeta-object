package adminserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testOperatorToken = "operator-secret-token"

// stubGateway is an in-package stand-in for the mTLS gateway client. Each
// method records how it was called and returns a canned result, so the console
// routing and the proxy passthrough can be tested without a TLS server.
type stubGateway struct {
	gotMethod string
	gotPath   string
	gotName   string
	gotBody   json.RawMessage
	gotQuery  url.Values

	err error // transport or gateway failure when set

	body json.RawMessage

	// calls counts every method call.
	calls int
}

// newStubGateway returns a stub that answers every route with "{}" 200.
func newStubGateway() *stubGateway {
	return &stubGateway{body: json.RawMessage(`{}`)}
}

func (g *stubGateway) result(query url.Values) (json.RawMessage, error) {
	g.calls++
	g.gotQuery = query
	if g.err != nil {
		return nil, g.err
	}
	return g.body, nil
}

func (g *stubGateway) Status(_ context.Context, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath = http.MethodGet, "/status"
	return g.result(q)
}

func (g *stubGateway) GetConfig(_ context.Context, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath = http.MethodGet, "/config"
	return g.result(q)
}

func (g *stubGateway) PutConfig(_ context.Context, body json.RawMessage, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath, g.gotBody = http.MethodPut, "/config", body
	return g.result(q)
}

func (g *stubGateway) SaveConfig(_ context.Context, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath = http.MethodPost, "/config/save"
	return g.result(q)
}

func (g *stubGateway) ReloadAuth(_ context.Context, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath = http.MethodPost, "/auth/reload"
	return g.result(q)
}

func (g *stubGateway) ListBuckets(_ context.Context, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath = http.MethodGet, "/buckets"
	return g.result(q)
}

func (g *stubGateway) CreateBucket(_ context.Context, body json.RawMessage, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath, g.gotBody = http.MethodPost, "/buckets", body
	return g.result(q)
}

func (g *stubGateway) GetBucket(_ context.Context, name string, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath, g.gotName = http.MethodGet, "/buckets", name
	return g.result(q)
}

func (g *stubGateway) DeleteBucket(_ context.Context, name string, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath, g.gotName = http.MethodDelete, "/buckets", name
	return g.result(q)
}

func (g *stubGateway) PutBucketSettings(_ context.Context, name string, body json.RawMessage, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath, g.gotName, g.gotBody = http.MethodPut, "/buckets/settings", name, body
	return g.result(q)
}

func (g *stubGateway) Purge(_ context.Context, body json.RawMessage, q url.Values) (json.RawMessage, error) {
	g.gotMethod, g.gotPath, g.gotBody = http.MethodPost, "/purge", body
	return g.result(q)
}

// newTestServer builds a Server with a minimal valid config and a stub gateway.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithGateway(t, newStubGateway())
}

// newTestServerWithGateway builds a Server around the supplied gateway stub.
func newTestServerWithGateway(t *testing.T, gw Gateway) *Server {
	t.Helper()
	cfg := &Config{
		ListenAddr:    "127.0.0.1:0",
		GatewayURL:    "https://127.0.0.1:9708",
		CAFile:        "ca.pem",
		ClientCert:    "client.pem",
		ClientKey:     "client-key.pem",
		OperatorToken: testOperatorToken,
	}
	srv, err := NewServer(cfg, gw)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// doLogin posts a token to /login and returns the response, the session
// cookie (zero value if absent), and the CSRF token from the body.
func doLogin(t *testing.T, srv *Server, token string) (*http.Response, http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"token":`+strconv.Quote(token)+`}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	resp := rec.Result()
	var sess http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			sess = *c
		}
	}
	var payload struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil && resp.StatusCode == http.StatusOK {
		t.Fatalf("login body is not JSON: %v", err)
	}
	return resp, sess, payload.CSRF
}

func TestLoginMintsSessionCookie(t *testing.T) {
	srv := newTestServer(t)
	resp, sess, csrf := doLogin(t, srv, testOperatorToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	if sess.Name != SessionCookieName {
		t.Fatalf("session cookie name = %q, want %q", sess.Name, SessionCookieName)
	}
	if sess.Value == "" {
		t.Fatal("session cookie value is empty")
	}
	if !sess.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	// This request did not arrive over TLS, so the Secure attribute must be
	// omitted (Safari rejects a Secure cookie on plain http, even on loopback).
	if sess.Secure {
		t.Error("session cookie is Secure on an HTTP response")
	}
	if sess.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v, want Strict", sess.SameSite)
	}
	if sess.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", sess.Path)
	}
	if csrf == "" {
		t.Error("login did not return a CSRF token")
	}
}

func TestLoginWrongTokenRefused(t *testing.T) {
	srv := newTestServer(t)
	resp, sess, _ := doLogin(t, srv, "not-the-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token login status = %d, want 401", resp.StatusCode)
	}
	if sess.Name == SessionCookieName {
		t.Fatal("wrong token minted a session cookie")
	}
}

// newTestServerWithConfig builds a Server from an explicit config so tests
// can pin config-driven behavior (the cookie Secure policy).
func newTestServerWithConfig(t *testing.T, cfg *Config) *Server {
	t.Helper()
	srv, err := NewServer(cfg, newStubGateway())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// plainRequest is a request that did NOT arrive over TLS (r.TLS == nil): the
// shape every response has behind a TLS-terminating proxy.
func plainRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}

// tlsRequest is a request that DID arrive over TLS: the shape of a response
// when the console's own listener terminates TLS.
func tlsRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.TLS = &tls.ConnectionState{}
	return req
}

// cookiesByName collects the Set-Cookie headers of a response by name.
func cookiesByName(resp *http.Response) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range resp.Cookies() {
		out[c.Name] = c
	}
	return out
}

// TestCookieSecureFollowsTransportAndConfig pins the M16 fix (bughunt
// 2026-10-13): Secure is derived from the request transport OR the
// trustProxyTLS knob — never from r.TLS alone, which is nil behind a
// TLS-terminating proxy and left the session cookie shipping without Secure.
func TestCookieSecureFollowsTransportAndConfig(t *testing.T) {
	base := func() *Config {
		return &Config{
			ListenAddr:    "127.0.0.1:0",
			GatewayURL:    "https://127.0.0.1:9708",
			CAFile:        "ca.pem",
			ClientCert:    "client.pem",
			ClientKey:     "client-key.pem",
			OperatorToken: testOperatorToken,
		}
	}

	t.Run("plain request without the knob stays non-Secure", func(t *testing.T) {
		srv := newTestServerWithConfig(t, base())
		rec := httptest.NewRecorder()
		req := plainRequest(http.MethodPost, "/login")
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(`{"token":"` + testOperatorToken + `"}`))
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login status = %d, want 200", rec.Code)
		}
		cookies := cookiesByName(rec.Result())
		for _, name := range []string{SessionCookieName, CSRFCookieName} {
			if c := cookies[name]; c == nil {
				t.Fatalf("%s cookie not set", name)
			} else if c.Secure {
				t.Errorf("%s cookie is Secure on a plain response with trustProxyTLS unset", name)
			}
		}
	})

	t.Run("TLS request is Secure without the knob", func(t *testing.T) {
		srv := newTestServerWithConfig(t, base())
		rec := httptest.NewRecorder()
		req := tlsRequest(http.MethodPost, "/login")
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(`{"token":"` + testOperatorToken + `"}`))
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login status = %d, want 200", rec.Code)
		}
		cookies := cookiesByName(rec.Result())
		for _, name := range []string{SessionCookieName, CSRFCookieName} {
			if c := cookies[name]; c == nil || !c.Secure {
				t.Errorf("%s cookie missing Secure on a TLS response", name)
			}
		}
	})

	t.Run("trustProxyTLS forces Secure on a plain request", func(t *testing.T) {
		cfg := base()
		cfg.TrustProxyTLS = true
		srv := newTestServerWithConfig(t, cfg)
		rec := httptest.NewRecorder()
		req := plainRequest(http.MethodPost, "/login")
		// M16 residual: trustProxyTLS alone no longer forces Secure — the
		// proxy must also SAY the client-facing scheme was https via
		// X-Forwarded-Proto (a proxy may pass some vhosts plain). The
		// header-conditional matrix is pinned by
		// TestCookieSecureHonorsForwardedProtoOnlyUnderTrustProxy.
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(`{"token":"` + testOperatorToken + `"}`))
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login status = %d, want 200", rec.Code)
		}
		cookies := cookiesByName(rec.Result())
		for _, name := range []string{SessionCookieName, CSRFCookieName} {
			if c := cookies[name]; c == nil || !c.Secure {
				t.Errorf("%s cookie missing Secure with trustProxyTLS set on a plain request", name)
			}
		}
		// And the logout path's cookie expiry carries Secure too (it reads
		// the same derivation).
		sess := cookies[SessionCookieName]
		logout := plainRequest(http.MethodPost, "/logout")
		logout.Header.Set("X-Forwarded-Proto", "https")
		logout.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
		logout.Header.Set(CSRFHeaderName, cookies[CSRFCookieName].Value)
		rec2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec2, logout)
		if rec2.Code != http.StatusNoContent {
			t.Fatalf("logout status = %d, want 204", rec2.Code)
		}
		for _, c := range rec2.Result().Cookies() {
			if c.Name == SessionCookieName && !c.Secure {
				t.Error("logout cookie expiry is not Secure with trustProxyTLS set")
			}
		}
	})
}

// TestSecureCookiesNilConfigKeepsTransportOnly guards the nil-config fallback:
// a store built without a config derives Secure from the transport only.
func TestSecureCookiesNilConfigKeepsTransportOnly(t *testing.T) {
	cfg := (*Config)(nil)
	req := plainRequest(http.MethodGet, "/")
	if cfg.SecureCookies(req) {
		t.Error("nil config + plain request must not be Secure")
	}
	req.TLS = &tls.ConnectionState{}
	if !cfg.SecureCookies(req) {
		t.Error("TLS request must be Secure even with a nil config")
	}
}

// TestCookieSecureHonorsForwardedProtoOnlyUnderTrustProxy pins the M16
// residual: X-Forwarded-Proto: https flips the Secure bit ONLY when the
// config declares a TLS-terminating proxy (trustProxyTLS). By default the
// header is client-controlled and must never influence cookie policy —
// pre-fix, a plain-HTTP request forged with the header could not flip it
// (the header was ignored entirely), and a proxied vhost could not gain
// Secure either; post-fix both behaviors are pinned per-side.
func TestCookieSecureHonorsForwardedProtoOnlyUnderTrustProxy(t *testing.T) {
	login := func(t *testing.T, srv *Server, proto string) map[string]*http.Cookie {
		t.Helper()
		rec := httptest.NewRecorder()
		req := plainRequest(http.MethodPost, "/login")
		req.Header.Set("Content-Type", "application/json")
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		req.Body = io.NopCloser(strings.NewReader(`{"token":"` + testOperatorToken + `"}`))
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login status = %d, want 200", rec.Code)
		}
		return cookiesByName(rec.Result())
	}
	base := func() *Config {
		return &Config{
			ListenAddr:    "127.0.0.1:0",
			GatewayURL:    "https://127.0.0.1:9708",
			CAFile:        "ca.pem",
			ClientCert:    "client.pem",
			ClientKey:     "client-key.pem",
			OperatorToken: testOperatorToken,
		}
	}

	t.Run("forged header without trustProxyTLS is ignored", func(t *testing.T) {
		srv := newTestServerWithConfig(t, base())
		cookies := login(t, srv, "https")
		for _, name := range []string{SessionCookieName, CSRFCookieName} {
			if c := cookies[name]; c == nil || c.Secure {
				t.Errorf("%s cookie is Secure from a client-forged X-Forwarded-Proto without trustProxyTLS", name)
			}
		}
	})

	t.Run("http proto value with trustProxyTLS stays non-Secure", func(t *testing.T) {
		cfg := base()
		cfg.TrustProxyTLS = true
		srv := newTestServerWithConfig(t, cfg)
		cookies := login(t, srv, "http")
		for _, name := range []string{SessionCookieName, CSRFCookieName} {
			if c := cookies[name]; c == nil || c.Secure {
				t.Errorf("%s cookie is Secure with trustProxyTLS but a plain-HTTP forwarded proto", name)
			}
		}
	})

	t.Run("https proto with trustProxyTLS is Secure", func(t *testing.T) {
		cfg := base()
		cfg.TrustProxyTLS = true
		srv := newTestServerWithConfig(t, cfg)
		cookies := login(t, srv, "https")
		for _, name := range []string{SessionCookieName, CSRFCookieName} {
			if c := cookies[name]; c == nil || !c.Secure {
				t.Errorf("%s cookie missing Secure behind a declared proxy forwarding https", name)
			}
		}
	})

	t.Run("header match is case-insensitive on the scheme", func(t *testing.T) {
		cfg := base()
		cfg.TrustProxyTLS = true
		srv := newTestServerWithConfig(t, cfg)
		cookies := login(t, srv, "HTTPS")
		if c := cookies[SessionCookieName]; c == nil || !c.Secure {
			t.Error("HTTPS (uppercase) forwarded proto did not yield Secure behind a declared proxy")
		}
	})
}

// TestWarnInsecureCookiePolicy pins the M16 residual's startup warning:
// it fires exactly when the console serves plain HTTP with neither its own
// TLS pair nor trustProxyTLS, and stays silent in both secure modes.
func TestWarnInsecureCookiePolicy(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   bool
	}{
		{"plain HTTP warns", func(c *Config) {}, true},
		{"TLS pair silences", func(c *Config) { c.CertFile, c.KeyFile = "cert.pem", "key.pem" }, false},
		{"trustProxyTLS silences", func(c *Config) { c.TrustProxyTLS = true }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				ListenAddr:    "127.0.0.1:0",
				GatewayURL:    "https://127.0.0.1:9708",
				CAFile:        "ca.pem",
				ClientCert:    "client.pem",
				ClientKey:     "client-key.pem",
				OperatorToken: testOperatorToken,
			}
			tc.mutate(cfg)
			var buf bytes.Buffer
			log.SetOutput(&buf)
			defer log.SetOutput(os.Stderr)
			cfg.WarnInsecureCookiePolicy()
			got := buf.String()
			if tc.want && !strings.Contains(got, "Secure") {
				t.Errorf("plain-HTTP console produced no Secure-cookie warning, log = %q", got)
			}
			if !tc.want && got != "" {
				t.Errorf("secure mode produced a spurious warning, log = %q", got)
			}
		})
	}
}

// TestTokenEqual proves the operator-token path is correct and uses the
// constant-time primitive. The source assertion is timing-insensitive: it
// checks the code path, not wall-clock behaviour.
func TestTokenEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"", "x", false},
		{"x", "", false},
		{"", "", true},
		{"a-longer-token", "a-longer-tokem", false},
	}
	for _, tc := range cases {
		if got := tokenEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("tokenEqual(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTokenEqualUsesSubtle(t *testing.T) {
	src, err := os.ReadFile("session.go")
	if err != nil {
		t.Fatalf("reading session.go: %v", err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare") {
		t.Fatal("operator-token comparison does not use crypto/subtle constant-time compare")
	}
}

func TestTamperedCookieRejected(t *testing.T) {
	srv := newTestServer(t)
	_, sess, _ := doLogin(t, srv, testOperatorToken)
	if sess.Value == "" {
		t.Fatal("no session cookie to tamper with")
	}
	// Tamper a SIGNIFICANT byte. The cookie is
	// base64url(payload) + "." + base64url(hmac), and a 32-byte HMAC
	// encodes to 43 RawURL characters: the LAST character carries only 2
	// significant bits, so the rest are discarded on decode and flipping
	// 'a' to 'b' there yields the IDENTICAL MAC - the server then
	// correctly accepts what is still the same cookie. Replace the FIRST
	// signature character instead, whose 6 bits all survive decoding, and
	// assert the decoded signature really changed so this trap cannot
	// come back.
	dot := strings.LastIndex(sess.Value, ".")
	if dot <= 0 || dot == len(sess.Value)-1 {
		t.Fatalf("unexpected cookie shape %q", sess.Value)
	}
	sig := sess.Value[dot+1:]
	origSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	repl := byte('A')
	if sig[0] == repl {
		repl = 'B'
	}
	tampered := sess.Value[:dot+1] + string(repl) + sig[1:]
	if tampered == sess.Value {
		t.Fatal("tamper did not change the cookie value")
	}
	tamSig, err := base64.RawURLEncoding.DecodeString(tampered[dot+1:])
	if err != nil {
		t.Fatalf("decoding tampered signature: %v", err)
	}
	if string(tamSig) == string(origSig) {
		t.Fatalf("tamper is not significant: the decoded signature is identical")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tampered})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie status = %d, want 401", rec.Code)
	}
}

func TestExpiredCookieRejected(t *testing.T) {
	srv := newTestServer(t)
	_, sess, _ := doLogin(t, srv, testOperatorToken)
	srv.sessions.now = func() time.Time { return time.Now().Add(SessionLifetime + time.Minute) }

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired cookie status = %d, want 401", rec.Code)
	}
}

func TestIdleExpiredCookieRejected(t *testing.T) {
	srv := newTestServer(t)
	_, sess, _ := doLogin(t, srv, testOperatorToken)
	// Past the idle window but well inside the absolute lifetime.
	srv.sessions.now = func() time.Time { return time.Now().Add(SessionIdleExpiry + time.Minute) }

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("idle-expired cookie status = %d, want 401", rec.Code)
	}
}

func TestRestartInvalidatesSession(t *testing.T) {
	srv1 := newTestServer(t)
	_, sess, _ := doLogin(t, srv1, testOperatorToken)

	// A restart is a fresh process: new HMAC key, empty session map.
	srv2 := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
	rec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after restart status = %d, want 401", rec.Code)
	}
}

func TestCSRFRequiredOnMutatingRequests(t *testing.T) {
	srv := newTestServer(t)
	srv.HandleAPI(http.MethodPost, "/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_, sess, csrf := doLogin(t, srv, testOperatorToken)
	if csrf == "" {
		t.Fatal("no CSRF token from login")
	}

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusForbidden},
		{"wrong", "deadbeef", http.StatusForbidden},
		{"correct", csrf, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/config", nil)
			req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
			if tc.header != "" {
				req.Header.Set(CSRFHeaderName, tc.header)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("CSRF %s: status = %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

func TestSafeMethodNeedsNoCSRF(t *testing.T) {
	srv := newTestServer(t)
	srv.HandleAPI(http.MethodGet, "/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_, sess, _ := doLogin(t, srv, testOperatorToken)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("safe method status = %d, want 200", rec.Code)
	}
}
