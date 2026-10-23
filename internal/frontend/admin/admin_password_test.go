package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adminPasswordPath is the wire path the appliance firstboot wizard POSTs
// (Contract G4 option a). Every test here pins that exact literal so a
// route rename breaks the build, not the appliance.
const adminPasswordPath = "/api/admin/password"

// throwawayPassword is a test-only value; production passwords never
// appear in this repo.
const throwawayPassword = "s3cr3t-throwaway-VALUE"

// TestAdminPassword_SuccessChangesCredential pins the happy path: the
// service receives the new_password body field verbatim, the response is
// 200 {"changed":true}, and the response body never echoes the password.
func TestAdminPassword_SuccessChangesCredential(t *testing.T) {
	var got string
	env := newRouteEnv(t, Services{SetAdminPassword: func(_ context.Context, newPassword string) error {
		got = newPassword
		return nil
	}}, nil)
	rr := serve(t, env, http.MethodPost, adminPasswordPath,
		`{"new_password":"`+throwawayPassword+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got != throwawayPassword {
		t.Fatalf("service got %q, want the body value", got)
	}
	if strings.Contains(rr.Body.String(), throwawayPassword) {
		t.Fatalf("response body echoed the password: %s", rr.Body.String())
	}
	body := mustJSON[map[string]bool](t, rr)
	if !body["changed"] {
		t.Fatalf("body = %v, want {changed:true}", body)
	}
}

// TestAdminPassword_RequiresCertificate pins the mTLS gate: no certificate,
// a wrong-CA certificate and an expired certificate all answer the SAME
// generic 401 (the password never reaches the service).
func TestAdminPassword_RequiresCertificate(t *testing.T) {
	called := false
	env := newRouteEnv(t, Services{SetAdminPassword: func(_ context.Context, _ string) error {
		called = true
		return nil
	}}, nil)
	cases := map[string]*tls.ConnectionState{
		"missing": nil,
		"wrongCA": {PeerCertificates: []*x509.Certificate{env.wrongCert}},
		"expired": {PeerCertificates: []*x509.Certificate{env.expired}},
	}
	for name, state := range cases {
		req := httptest.NewRequest(http.MethodPost, adminPasswordPath,
			strings.NewReader(`{"new_password":"`+throwawayPassword+`"}`))
		req.TLS = state
		rr := httptest.NewRecorder()
		env.frontend.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, rr.Code)
		}
		if strings.Contains(rr.Body.String(), throwawayPassword) {
			t.Fatalf("%s: 401 body leaked the password: %s", name, rr.Body.String())
		}
	}
	if called {
		t.Fatal("service was reached without a verified client certificate")
	}
}

// TestAdminPassword_PrincipalAllowList pins that a certificate outside
// adminPrincipals is a 403 and the service is not reached.
func TestAdminPassword_PrincipalAllowList(t *testing.T) {
	called := false
	env := newRouteEnv(t, Services{SetAdminPassword: func(_ context.Context, _ string) error {
		called = true
		return nil
	}}, nil)
	allowAlice := env.withPrincipals(t, []string{"alice"})
	req := env.authedJSON(http.MethodPost, adminPasswordPath,
		`{"new_password":"`+throwawayPassword+`"}`)
	rr := httptest.NewRecorder()
	allowAlice.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if called {
		t.Fatal("service was reached for a non-allow-listed principal")
	}
}

// TestAdminPassword_BadPayload400 pins the 400 class: a non-JSON body, a
// missing new_password field and an empty/whitespace value all answer 400
// InvalidArgument and never reach the service.
func TestAdminPassword_BadPayload400(t *testing.T) {
	called := false
	env := newRouteEnv(t, Services{SetAdminPassword: func(_ context.Context, _ string) error {
		called = true
		return nil
	}}, nil)
	for _, body := range []string{
		`not json`,
		`{}`,
		`{"new_password":""}`,
		`{"new_password":"   "}`,
		`{"password":"x"}`,
	} {
		rr := serve(t, env, http.MethodPost, adminPasswordPath, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, rr.Code)
		}
		if e := mustJSON[errorEnvelope](t, rr); e.Error.Code != "InvalidArgument" {
			t.Fatalf("body %s: code = %q, want InvalidArgument", body, e.Error.Code)
		}
	}
	if called {
		t.Fatal("service was reached with an invalid payload")
	}
}

// TestAdminPassword_Method405 pins that only POST is routed.
func TestAdminPassword_Method405(t *testing.T) {
	env := newRouteEnv(t, Services{}, nil)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rr := serve(t, env, m, adminPasswordPath, "")
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", m, rr.Code)
		}
	}
}

// TestAdminPassword_NilService503 pins the unwired-service answer.
func TestAdminPassword_NilService503(t *testing.T) {
	env := newRouteEnv(t, Services{}, nil)
	rr := serve(t, env, http.MethodPost, adminPasswordPath,
		`{"new_password":"x"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// TestAdminPassword_ServiceErrorPassthrough pins that a *ServiceError from
// the wiring keeps its honest status and code.
func TestAdminPassword_ServiceErrorPassthrough(t *testing.T) {
	env := newRouteEnv(t, Services{SetAdminPassword: func(_ context.Context, _ string) error {
		return &ServiceError{Status: 500, Code: "ReloadFailed", Message: "activation failed"}
	}}, nil)
	rr := serve(t, env, http.MethodPost, adminPasswordPath,
		`{"new_password":"x"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if e := mustJSON[errorEnvelope](t, rr); e.Error.Code != "ReloadFailed" {
		t.Fatalf("code = %q, want ReloadFailed", e.Error.Code)
	}
}

// TestAdminPassword_AuditNeverCarriesCredential pins the audit contract:
// exactly one record per request, op admin, and NEITHER audit coordinate
// ever carries the password (it is not an object key).
func TestAdminPassword_AuditNeverCarriesCredential(t *testing.T) {
	var calls []auditCall
	env := newRouteEnv(t, Services{SetAdminPassword: func(_ context.Context, _ string) error {
		return nil
	}}, &calls)
	rr := serve(t, env, http.MethodPost, adminPasswordPath,
		`{"new_password":"`+throwawayPassword+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(calls) != 1 {
		t.Fatalf("audit calls = %d, want exactly 1", len(calls))
	}
	c := calls[0]
	if c.op != "admin" || c.principal != "tester" || c.method != http.MethodPost {
		t.Fatalf("audit record = %+v", c)
	}
	if strings.Contains(c.bucket, throwawayPassword) || strings.Contains(c.key, throwawayPassword) {
		t.Fatalf("audit coordinates leaked the password: %+v", c)
	}
	if c.bucket != "" || c.key != "" {
		t.Fatalf("audit coordinates = %q/%q, want empty", c.bucket, c.key)
	}
}
