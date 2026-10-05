package adminserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
