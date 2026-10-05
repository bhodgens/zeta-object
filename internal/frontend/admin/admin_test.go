package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// authedRequest builds a request whose TLS connection state carries a valid
// client certificate (the handler-level equivalent of a completed mTLS
// handshake).
func (e *testEnv) authedRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{e.client}}
	return req
}

func TestAdmin_NameAddrAndCapabilities(t *testing.T) {
	env := newTestEnv(t, Options{ListenAddr: "127.0.0.1:9000"})
	if got := env.frontend.Name(); got != "admin" {
		t.Fatalf("Name() = %q, want admin", got)
	}
	if got := env.frontend.Addr(); got != "127.0.0.1:9000" {
		t.Fatalf("Addr() = %q, want 127.0.0.1:9000", got)
	}
	caps := env.frontend.Capabilities()
	if !caps.Buckets {
		t.Fatal("Capabilities().Buckets = false, want true")
	}
	if caps.Versioning || caps.ConditionalReads || caps.Multipart || caps.PresignedURLs {
		t.Fatalf("Capabilities() = %+v, want Buckets only", caps)
	}
}

// An empty Addr is a construction error, never a shared-mux fallback
// (Contract 1).
func TestAdmin_EmptyListenAddrRejected(t *testing.T) {
	_, err := New(Options{})
	if err == nil {
		t.Fatal("New accepted an empty listenAddr")
	}
	if !strings.Contains(err.Error(), "listenAddr") {
		t.Fatalf("error %q must name the listenAddr rule", err)
	}
}

// A missing clientCAFile is a loud startup error naming the path.
func TestAdmin_MissingClientCAFileNamesPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")
	_, err := New(Options{ListenAddr: "127.0.0.1:0", ClientCAFile: missing})
	if err == nil {
		t.Fatal("New accepted a missing clientCAFile")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q must name the path %q", err, missing)
	}
}

// A nil Status service answers 503 with the NotImplemented envelope.
func TestAdmin_StatusNilService503(t *testing.T) {
	env := newTestEnv(t, Options{})
	rr := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/status"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	body := decodeError(t, rr)
	if body.Error.Code != "NotImplemented" {
		t.Fatalf("error code = %q, want NotImplemented", body.Error.Code)
	}
}

// A configured Status service is invoked and its report returned as JSON.
func TestAdmin_StatusService200(t *testing.T) {
	env := newTestEnv(t, Options{Services: Services{
		Status: func(ctx context.Context) (StatusReport, error) {
			return StatusReport{Version: "1.2.3", Frontends: []string{"admin"}}, nil
		},
	}})
	rr := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/status"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var report StatusReport
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Version != "1.2.3" {
		t.Fatalf("report = %+v, want version 1.2.3", report)
	}
}

// Every unrouted path is a JSON 404; a wrong method on a KNOWN route is a JSON
// 405 (leaf 04). Unknown-path requests still authenticate first.
func TestAdmin_UnknownRoute404(t *testing.T) {
	env := newTestEnv(t, Options{})
	// Genuinely unknown paths (never a route).
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/nope"},
		{http.MethodGet, "/buckets/x/y/z"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/"},
	} {
		rr := httptest.NewRecorder()
		env.frontend.Handler().ServeHTTP(rr, env.authedRequest(tc.method, tc.path))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status = %d, want 404", tc.method, tc.path, rr.Code)
		}
		if body := decodeError(t, rr); body.Error.Code != "NotFound" {
			t.Fatalf("%s %s: error code = %q, want NotFound", tc.method, tc.path, body.Error.Code)
		}
	}
	// Known route, wrong method: 405.
	rr := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodPost, "/status"))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /status: status = %d, want 405", rr.Code)
	}
	if body := decodeError(t, rr); body.Error.Code != "MethodNotAllowed" {
		t.Fatalf("POST /status: error code = %q, want MethodNotAllowed", body.Error.Code)
	}
}

// No route — not even a health check — is reachable without a verified client
// certificate.
func TestAdmin_NoCertificate401OnEveryRoute(t *testing.T) {
	env := newTestEnv(t, Options{})
	for _, path := range []string{"/status", "/config", "/buckets", "/purge", "/healthz", "/"} {
		rr := httptest.NewRecorder()
		env.frontend.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without a certificate: status = %d, want 401", path, rr.Code)
		}
	}
}

func decodeError(t *testing.T, rr *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var body errorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rr.Body.String(), err)
	}
	return body
}
