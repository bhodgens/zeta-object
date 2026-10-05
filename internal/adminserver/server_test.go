package adminserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/adminserver/gateway"
)

// decodeEnvelope parses the JSON error envelope from a response body.
func decodeEnvelope(t *testing.T, body string) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("response is not the JSON error envelope: %v (body %q)", err, body)
	}
	return env
}

// authedSession carries a live session cookie and its CSRF token.
type authedSession struct {
	cookie string
	csrf   string
}

// loginSession performs a JSON login and returns the session material.
func loginSession(t *testing.T, srv *Server) authedSession {
	t.Helper()
	_, sess, csrf := doLogin(t, srv, testOperatorToken)
	if sess.Value == "" || csrf == "" {
		t.Fatal("login did not yield a session and CSRF token")
	}
	return authedSession{cookie: sess.Value, csrf: csrf}
}

// request issues an authenticated request, setting the CSRF header on mutating
// methods.
func (a authedSession) request(srv *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: a.cookie})
	if isMutating(method) {
		req.Header.Set(CSRFHeaderName, a.csrf)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestUnauthenticatedAPIEnvelope(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api status = %d, want 401", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.String())
	if env.Error.Code == "" || env.Error.Message == "" {
		t.Fatalf("envelope missing code/message: %+v", env)
	}
}

func TestEveryAPIPathIsGated(t *testing.T) {
	srv := newTestServer(t)
	paths := []string{
		"/api/status",
		"/api/config",
		"/api/config/save",
		"/api/auth/reload",
		"/api/buckets",
		"/api/buckets/photos",
		"/api/purge",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET %s status = %d, want 401 without a session", p, rec.Code)
			}
		})
	}
}

func TestLogoutClearsSession(t *testing.T) {
	srv := newTestServer(t)
	_, sess, _ := doLogin(t, srv, testOperatorToken)
	if sess.Value == "" {
		t.Fatal("no session to log out")
	}

	logout := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logout.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, logout)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", rec.Code)
	}

	// The old cookie is now dead.
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.Value})
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout /api status = %d, want 401", rec2.Code)
	}
}

func TestLoginRejectsNonJSONBody(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed login body status = %d, want 400", rec.Code)
	}
}

// TestLoginAcceptsFormToken pins the form fallback: the shell posts JSON but
// offers a form, so a form-encoded "token" field must also work.
func TestLoginAcceptsFormToken(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader("token="+testOperatorToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("form login status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("form login did not set a session cookie")
	}
}

// TestProxyRoutesCallCorrectGatewayMethod is the Contract 2 row-for-row check:
// every console route calls exactly the matching gateway method and path.
func TestProxyRoutesCallCorrectGatewayMethod(t *testing.T) {
	cases := []struct {
		method, path       string
		wantMethod, wantGW string
		wantName           string
	}{
		{http.MethodGet, "/api/status", http.MethodGet, "/status", ""},
		{http.MethodGet, "/api/config", http.MethodGet, "/config", ""},
		{http.MethodPut, "/api/config", http.MethodPut, "/config", ""},
		{http.MethodPost, "/api/config/save", http.MethodPost, "/config/save", ""},
		{http.MethodPost, "/api/auth/reload", http.MethodPost, "/auth/reload", ""},
		{http.MethodGet, "/api/buckets", http.MethodGet, "/buckets", ""},
		{http.MethodPost, "/api/buckets", http.MethodPost, "/buckets", ""},
		{http.MethodGet, "/api/buckets/photos", http.MethodGet, "/buckets", "photos"},
		{http.MethodDelete, "/api/buckets/photos", http.MethodDelete, "/buckets", "photos"},
		{http.MethodPut, "/api/buckets/photos/settings", http.MethodPut, "/buckets/settings", "photos"},
		{http.MethodPost, "/api/purge", http.MethodPost, "/purge", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			gw := newStubGateway()
			srv := newTestServerWithGateway(t, gw)
			sess := loginSession(t, srv)
			rec := sess.request(srv, tc.method, tc.path, strings.NewReader("{}"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if gw.calls != 1 {
				t.Fatalf("gateway calls = %d, want 1", gw.calls)
			}
			if gw.gotMethod != tc.wantMethod || gw.gotPath != tc.wantGW {
				t.Fatalf("gateway call = %s %s, want %s %s", gw.gotMethod, gw.gotPath, tc.wantMethod, tc.wantGW)
			}
			if gw.gotName != tc.wantName {
				t.Fatalf("gateway name = %q, want %q", gw.gotName, tc.wantName)
			}
		})
	}
}

// TestProxyForwardsBodyVerbatim proves the request body reaches the gateway
// unchanged (no reshaping, no re-encoding).
func TestProxyForwardsBodyVerbatim(t *testing.T) {
	gw := newStubGateway()
	srv := newTestServerWithGateway(t, gw)
	sess := loginSession(t, srv)
	const body = `{"name":"photos","retention": 7,  "x":[1,2]}`
	rec := sess.request(srv, http.MethodPut, "/api/config", strings.NewReader(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if string(gw.gotBody) != body {
		t.Fatalf("forwarded body = %q, want %q", gw.gotBody, body)
	}
}

// TestProxyForwardsQuery proves the query string survives.
func TestProxyForwardsQuery(t *testing.T) {
	gw := newStubGateway()
	srv := newTestServerWithGateway(t, gw)
	sess := loginSession(t, srv)
	rec := sess.request(srv, http.MethodGet, "/api/buckets?limit=5&prefix=a%2Fb", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := gw.gotQuery.Get("limit"); got != "5" {
		t.Errorf("forwarded limit = %q, want 5", got)
	}
	if got := gw.gotQuery.Get("prefix"); got != "a/b" {
		t.Errorf("forwarded prefix = %q, want a/b", got)
	}
}

// TestProxyReturnsGatewayBodyAndContentType proves a success body is returned
// byte-identically and typed as JSON.
func TestProxyReturnsGatewayBodyAndContentType(t *testing.T) {
	gw := newStubGateway()
	gw.body = json.RawMessage(`{"version":"1.2","buckets":["a","b"]}`)
	srv := newTestServerWithGateway(t, gw)
	sess := loginSession(t, srv)
	rec := sess.request(srv, http.MethodGet, "/api/status", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != string(gw.body) {
		t.Fatalf("body = %q, want byte-identical %q", got, gw.body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// TestProxyMapsGatewayHTTPError proves a non-2xx gateway response keeps its
// status and its own code/message.
func TestProxyMapsGatewayHTTPError(t *testing.T) {
	cases := []struct {
		name       string
		he         *gateway.HTTPError
		wantStatus int
	}{
		{
			name:       "dataset bucket not deletable",
			he:         &gateway.HTTPError{Status: http.StatusConflict, Code: "DatasetBucketNotDeletable", Message: "bucket is dataset-backed"},
			wantStatus: http.StatusConflict,
		},
		{
			name:       "validator message",
			he:         &gateway.HTTPError{Status: http.StatusBadRequest, Code: "InvalidArgument", Message: "retention must be > 0"},
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newStubGateway()
			gw.err = tc.he
			srv := newTestServerWithGateway(t, gw)
			sess := loginSession(t, srv)
			rec := sess.request(srv, http.MethodDelete, "/api/buckets/data", nil)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			env := decodeEnvelope(t, rec.Body.String())
			if env.Error.Code != tc.he.Code || env.Error.Message != tc.he.Message {
				t.Fatalf("envelope = %+v, want code=%q message=%q", env, tc.he.Code, tc.he.Message)
			}
		})
	}
}

// TestProxyTransportFailureIs502 proves a failure to reach the gateway is a 502
// carrying the reason, never a fabricated success.
func TestProxyTransportFailureIs502(t *testing.T) {
	gw := newStubGateway()
	gw.err = &gateway.TransportError{URL: "https://127.0.0.1:9708/status", Reason: "connection refused"}
	srv := newTestServerWithGateway(t, gw)
	sess := loginSession(t, srv)
	rec := sess.request(srv, http.MethodGet, "/api/status", nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.String())
	if env.Error.Code == "" || !strings.Contains(env.Error.Message, "connection refused") {
		t.Fatalf("envelope = %+v, want the transport reason", env)
	}
}

// TestUnknownAPIPathIs404 proves an unknown /api path is the JSON envelope 404.
func TestUnknownAPIPathIs404(t *testing.T) {
	srv := newTestServer(t)
	sess := loginSession(t, srv)
	rec := sess.request(srv, http.MethodGet, "/api/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown /api path status = %d, want 404", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.String())
	if env.Error.Code == "" {
		t.Fatalf("404 is not the JSON envelope: %q", rec.Body.String())
	}
}

// TestWrongMethodOnKnownPathIs405 proves a known path with a wrong method is a
// 405 with the envelope and an Allow header.
func TestWrongMethodOnKnownPathIs405(t *testing.T) {
	srv := newTestServer(t)
	sess := loginSession(t, srv)
	rec := sess.request(srv, http.MethodDelete, "/api/status", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q, want it to list GET", allow)
	}
	env := decodeEnvelope(t, rec.Body.String())
	if env.Error.Code == "" {
		t.Fatalf("405 is not the JSON envelope: %q", rec.Body.String())
	}
}

// TestStaticTraversalViaHandler proves the static surface never escapes the
// embedded FS, including encoded forms, when reached through the root handler.
func TestStaticTraversalViaHandler(t *testing.T) {
	srv := newTestServer(t)
	for _, p := range []string{
		"/assets/../index.html",
		"/assets/..%2findex.html",
		"/assets/%2e%2e/index.html",
		"/assets/../../etc/passwd",
	} {
		t.Run(p, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want 404", p, rec.Code)
			}
		})
	}
}

// TestServerTLSStampsSecureCookies proves that over HTTPS every cookie carries
// Secure.
func TestServerTLSStampsSecureCookies(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewTLSServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/login", "application/json",
		strings.NewReader(`{"token":"`+testOperatorToken+`"}`))
	if err != nil {
		t.Fatalf("TLS login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS login status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("response did not arrive over TLS")
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName || c.Name == CSRFCookieName {
			if !c.Secure {
				t.Errorf("cookie %s over TLS is not Secure", c.Name)
			}
		}
	}
}

// TestServerPlainHTTPOmitsSecureCookies proves that on plain HTTP the cookie is
// not Secure (Safari would reject it otherwise, even on loopback).
func TestServerPlainHTTPOmitsSecureCookies(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/login", "application/json",
		strings.NewReader(`{"token":"`+testOperatorToken+`"}`))
	if err != nil {
		t.Fatalf("plain login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sawSession bool
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			sawSession = true
			if c.Secure {
				t.Error("session cookie is Secure on an HTTP response")
			}
		}
	}
	if !sawSession {
		t.Fatal("plain login did not set a session cookie")
	}
}

// TestAllowNonLoopbackWithCertificateServesTLS proves the console starts and
// serves HTTPS when allowNonLoopback is set and a listener certificate pair is
// configured.
func TestAllowNonLoopbackWithCertificateServesTLS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	cfg := &Config{
		ListenAddr:       "127.0.0.1:0",
		GatewayURL:       "https://127.0.0.1:9708",
		CAFile:           "ca.pem",
		ClientCert:       "client.pem",
		ClientKey:        "client-key.pem",
		OperatorToken:    testOperatorToken,
		CertFile:         certFile,
		KeyFile:          keyFile,
		AllowNonLoopback: true,
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	srv, err := NewServer(cfg, newStubGateway())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler(), TLSConfig: cfg.TLSConfig()}
	go func() { _ = hs.ServeTLS(ln, certFile, keyFile) }()
	defer func() { _ = hs.Close() }()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // test-only self-signed cert
	}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET https: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS GET / status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("response did not arrive over TLS")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("negotiated %#x, want at least TLS 1.2", resp.TLS.Version)
	}
}

// generateSelfSignedCert writes a self-signed server certificate and key to a
// temp dir and returns their paths.
func generateSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "console.pem")
	keyFile = filepath.Join(dir, "console-key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("writing cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	return certFile, keyFile
}
