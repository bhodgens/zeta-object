// dispatch_certauth_test.go — the mTLS certificate rejections must render as
// a 401 challenge through the dispatch pipeline, not a 500 (bughunt H5:
// dispatch.go's 401 predicate knew only the four Basic sentinels, so every
// CertAuthenticator rejection fell through to the authenticator-internal
// branch and answered as a server fault — which zeta-cache and every
// retry/error-budget path then treats as retryable). Also pins the other half
// of the split: a genuine non-sentinel authenticator failure stays 500.
package webdav

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// certKnownCN is the one CN the stub registry resolves.
const certKnownCN = "device-1"

// certStubRegistry is the CertAuthenticator registry stub (the frozen v1
// seam is CN == AccessKey, so one principal namespace).
type certStubRegistry struct{ known string }

func (s certStubRegistry) LookupByAccessKey(accessKeyID string) (auth.Identity, bool) {
	if accessKeyID != s.known {
		return auth.Identity{}, false
	}
	return auth.WildcardIdentity(accessKeyID), true
}

// certAuthFrontend builds a frontend whose authenticator is the REAL
// CertAuthenticator over certStubRegistry.
func certAuthFrontend(t *testing.T, cfg Config) *Frontend {
	t.Helper()
	f, err := New(newStubBackend(), cfg,
		WithAuthenticator(auth.NewCertAuthenticator(certStubRegistry{known: certKnownCN})))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// certRequest builds a request carrying a peer certificate with the given CN.
// cn == "" means no TLS state at all (what a plain test request looks like)
// — the no-peer-certificate rejection.
func certRequest(method, target, cn string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if cn != "" {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		}}
	}
	return req
}

// TestAuth_CertRejectionsRender401 pins every CertAuthenticator rejection
// shape the wire can produce: no peer certificate (ErrCertMissing) and a
// verified certificate whose CN is not in the registry (ErrCertUnknownCN).
// Both are 401 + WWW-Authenticate with an empty body — the same rendering the
// four Basic sentinels get. A 500 here means the rejection is on the
// authenticator-internal branch, i.e. the bug this file exists for.
func TestAuth_CertRejectionsRender401(t *testing.T) {
	f := certAuthFrontend(t, Config{})
	cases := []struct {
		name string
		cn   string
	}{
		{name: "no peer certificate presented", cn: ""},
		{name: "verified cert, CN not in the registry", cn: "device-x"},
		{name: "verified cert, empty CN", cn: "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, certRequest("GET", "/photos/a.txt", tc.cn))
			if rec.Code != 401 {
				t.Fatalf("status = %d, want 401 (a cert rejection is a client error, not a server fault)", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="zeta-object"` {
				t.Fatalf("WWW-Authenticate = %q, want the realm challenge", got)
			}
			if got := rec.Header().Get("Content-Length"); got != "0" {
				t.Fatalf("Content-Length = %q, want 0 (empty body)", got)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("body = %q, want empty (the challenge carries no detail)", rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != "" {
				t.Fatalf("401 must NOT carry Allow, got %q", got)
			}
		})
	}
}

// TestAuth_CertRejectionsEveryMethod pins that the challenge precedes method
// dispatch for the certificate path too (leaf 04's rule, cert adapter
// included): every method 401s rather than 405/207/200.
func TestAuth_CertRejectionsEveryMethod(t *testing.T) {
	f := certAuthFrontend(t, Config{})
	methods := []string{"OPTIONS", "PROPFIND", "GET", "HEAD", "PUT", "DELETE", "MKCOL", "COPY", "MOVE", "LOCK"}
	for _, m := range methods {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, certRequest(m, "/photos/a.txt", "device-x"))
		if rec.Code != 401 {
			t.Fatalf("%s with an unknown CN: status = %d, want 401", m, rec.Code)
		}
	}
}

// TestAuth_CertKnownCNAuthenticates is the positive control: the fix must not
// turn the 401 branch into a blanket denial — a known CN resolves to its
// identity and the request proceeds to dispatch.
func TestAuth_CertKnownCNAuthenticates(t *testing.T) {
	f := certAuthFrontend(t, Config{})
	f.be.(*stubBackend).seed("photos", "a.txt", []byte("x"))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, certRequest("GET", "/photos/a.txt", certKnownCN))
	if rec.Code != 200 {
		t.Fatalf("known CN: status = %d, want 200 (auth passed, then dispatch)", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("authenticated response carries a challenge: %q", got)
	}
}

// TestAuth_CertRegistryNilRenders401 pins the fail-closed split's third
// sentinel: a CertAuthenticator that cannot answer because it has no registry
// (the zero-value adapter — a wiring bug that must not panic and must not
// read as a server fault) is still a credential rejection, so 401.
func TestAuth_CertRegistryNilRenders401(t *testing.T) {
	f, err := New(newStubBackend(), Config{}, WithAuthenticator(&auth.CertAuthenticator{}))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, certRequest("GET", "/photos/a.txt", certKnownCN))
	if rec.Code != 401 {
		t.Fatalf("registry-less cert authenticator: status = %d, want 401 (fail closed, never 500/panic)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

// TestAuth_CertRejection_WrappedStill401 pins that the predicate matches
// wrapped sentinels (errors.Is, not ==): an adapter that annotates a
// rejection still renders 401.
func TestAuth_CertRejection_WrappedStill401(t *testing.T) {
	f, err := New(newStubBackend(), Config{}, WithAuthenticator(erroringAuthenticator{
		err: fmt.Errorf("resolve cn %q: %w", "device-x", auth.ErrCertUnknownCN),
	}))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/photos/a.txt", nil))
	if rec.Code != 401 {
		t.Fatalf("wrapped ErrCertUnknownCN: status = %d, want 401", rec.Code)
	}
}

// TestAuth_CertPipelineInternalFaultStill500 is the other half of the split:
// a non-sentinel error from an authenticator on the certificate path is a
// genuine internal fault (a wiring/lookup crash, not a credential decision)
// and must stay 500. The 401 predicate must not widen into "any error".
func TestAuth_CertPipelineInternalFaultStill500(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bare internal error", err: errStubInternal},
		{name: "wrapped internal error", err: fmt.Errorf("lookup backend: %w", errStubInternal)},
		{name: "registry lookup exploded", err: errors.New("registry: connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := New(newStubBackend(), Config{}, WithAuthenticator(erroringAuthenticator{err: tc.err}))
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, certRequest("GET", "/photos/a.txt", certKnownCN))
			if rec.Code != 500 {
				t.Fatalf("internal authenticator failure: status = %d, want 500 (a real fault is not a credential rejection)", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("500 must not carry a challenge")
			}
		})
	}
}

// erroringAuthenticator returns a fixed error — the seam for driving the
// predicate with errors the real adapters do not produce themselves (wrapped
// sentinels, internal faults) without touching production code.
type erroringAuthenticator struct{ err error }

func (a erroringAuthenticator) Authenticate(_ *http.Request) (auth.Identity, error) {
	return auth.Identity{}, a.err
}
