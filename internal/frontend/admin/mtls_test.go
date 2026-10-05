package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMTLS_AuthenticateMatrix is the certificate-verification matrix: it
// drives the auth wrapper directly with a synthesized TLS connection state,
// which is how a bad issuer or an expired certificate (both rejected at the
// TLS handshake by RequireAndVerifyClientCert) is exercised without a
// handshake.
func TestMTLS_AuthenticateMatrix(t *testing.T) {
	env := newTestEnv(t, Options{})
	allowExclude := env.withPrincipals(t, []string{"alice"})
	allowInclude := env.withPrincipals(t, []string{"tester"})

	state := func(certs ...*x509.Certificate) *tls.ConnectionState {
		return &tls.ConnectionState{PeerCertificates: certs}
	}

	tests := []struct {
		name       string
		frontend   *adminFrontend
		tlsState   *tls.ConnectionState
		wantStatus int
		wantCN     string
		wantCode   string
	}{
		{"missing certificate", env.frontend, state(), 401, "", "Unauthorized"},
		{"nil TLS state", env.frontend, nil, 401, "", "Unauthorized"},
		{"valid certificate", env.frontend, state(env.client), 0, "tester", ""},
		{"wrong CA", env.frontend, state(env.wrongCert), 401, "", "Unauthorized"},
		{"expired certificate", env.frontend, state(env.expired), 401, "", "Unauthorized"},
		{"empty common name", env.frontend, state(env.emptyCN), 401, "", "Unauthorized"},
		{"allow-list excludes CN", allowExclude, state(env.client), 403, "", "Forbidden"},
		{"allow-list includes CN", allowInclude, state(env.client), 0, "tester", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			req.TLS = tt.tlsState
			cn, status, code := tt.frontend.authenticate(req)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (code %q)", status, tt.wantStatus, code)
			}
			if status == 0 {
				if cn != tt.wantCN {
					t.Fatalf("principal = %q, want %q", cn, tt.wantCN)
				}
				return
			}
			if code != tt.wantCode {
				t.Fatalf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// Certificate failures must be indistinguishable: a missing certificate, a
// wrong issuer and an expired certificate all render the SAME 401 body, which
// leaks nothing about why.
func TestMTLS_FailuresIndistinguishable(t *testing.T) {
	env := newTestEnv(t, Options{})
	bodies := map[string]string{}
	for name, certs := range map[string][]*x509.Certificate{
		"missing": nil,
		"wrongCA": {env.wrongCert},
		"expired": {env.expired},
	} {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		if certs != nil {
			req.TLS = &tls.ConnectionState{PeerCertificates: certs}
		}
		rr := httptest.NewRecorder()
		env.frontend.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, rr.Code)
		}
		bodies[name] = rr.Body.String()
	}
	if bodies["missing"] != bodies["wrongCA"] || bodies["missing"] != bodies["expired"] {
		t.Fatalf("401 bodies differ (detail leak): missing=%q wrongCA=%q expired=%q",
			bodies["missing"], bodies["wrongCA"], bodies["expired"])
	}
	for _, leak := range []string{"x509", "expired", "unknown authority", "handshake", "signed by", "pem"} {
		if strings.Contains(strings.ToLower(bodies["missing"]), leak) {
			t.Fatalf("401 body leaks %q: %s", leak, bodies["missing"])
		}
	}
}

// The 401 body is the standard JSON error envelope.
func TestMTLS_UnauthorizedEnvelope(t *testing.T) {
	env := newTestEnv(t, Options{})
	rr := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body errorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 401 body: %v (%s)", err, rr.Body.String())
	}
	if body.Error.Code != "Unauthorized" || body.Error.Message == "" {
		t.Fatalf("body = %+v, want a non-empty Unauthorized envelope", body.Error)
	}
}

// TestMTLS_TLSConfigSettings pins the configuration contract (leaf 01
// Contract 4): TLS 1.2 minimum, client certificates REQUIRED and verified,
// the CA pool from ClientCAFile and the process pair as the certificate.
func TestMTLS_TLSConfigSettings(t *testing.T) {
	env := newTestEnv(t, Options{})
	cfg, err := env.frontend.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("ClientCAs is nil, want the CA pool from ClientCAFile")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates = %d, want 1 (the process pair)", len(cfg.Certificates))
	}
}

// TestMTLS_TLSConfigHandshake proves the config produces a working mTLS
// listener: a client WITH a CA-signed certificate reaches the handler, while a
// client WITHOUT one fails the handshake (no route is served unauthenticated).
func TestMTLS_TLSConfigHandshake(t *testing.T) {
	env := newTestEnv(t, Options{Services: Services{
		Status: func(ctx context.Context) (StatusReport, error) {
			return StatusReport{Version: "mTLS-ok"}, nil
		},
	}})
	cfg, err := env.frontend.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}

	srv := httptest.NewUnstartedServer(env.frontend.Handler())
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()

	pool := env.caPool()

	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{env.clientTLS},
	}}}
	resp, err := withCert.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("request with a valid client certificate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	withoutCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool,
	}}}
	resp2, err := withoutCert.Get(srv.URL + "/status")
	if err == nil {
		resp2.Body.Close()
		t.Fatalf("client without a certificate reached the handler (status %d); want a handshake failure", resp2.StatusCode)
	}
}
