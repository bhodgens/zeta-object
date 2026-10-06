// certauth_predicate_test.go — the owncloud OCS surface must classify
// credential rejections with the SAME predicate the webdav data plane uses.
//
// authenticateOCS carried its own inline copy of the four Basic sentinels, so
// every CertAuthenticator rejection (ErrCertMissing / ErrCertUnknownCN /
// ErrCertRegistryNil) fell to the "authenticator-internal failure" branch and
// rendered as OCS statuscode 500 "Internal Server Error" — the same
// stale-sentinel bug webdav's dispatch.go fixed in b0679d4, one package over,
// and it becomes live the moment anything wires a certificate authenticator
// into the owncloud frontend (the OCS negotiation surface is authenticated by
// the same adapter the data plane is).
//
// These tests drive the OCS dispatch with a REAL auth.CertAuthenticator so
// the sentinels are the ones the wire produces, and assert the whole set of
// auth rejection sentinels resolves identically to the wrapped webdav
// frontend's own classification.
package owncloud

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/webdav"
)

// certKnownCN is the only CN the OCS cert registry resolves.
const certKnownCN = "device-1"

// certRegistryStub is the CertAuthenticator's registry seam (CN == AccessKey).
type certRegistryStub struct{ known string }

func (s certRegistryStub) LookupByAccessKey(accessKeyID string) (auth.Identity, bool) {
	if accessKeyID != s.known {
		return auth.Identity{}, false
	}
	return auth.WildcardIdentity(accessKeyID), true
}

// newCertAuthOwncloud wraps a REAL webdav frontend whose authenticator is the
// REAL CertAuthenticator — the composition a certificate-authenticated
// owncloud frontend has (today's factory always wires Basic, which is why
// this predicate was latent rather than live).
func newCertAuthOwncloud(t *testing.T) *Frontend {
	t.Helper()
	wd, err := webdav.New(nilBackend{}, webdav.Config{Bucket: "oc-bkt"},
		webdav.WithAuthenticator(auth.NewCertAuthenticator(certRegistryStub{known: certKnownCN})))
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(wd)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// certReq builds a request carrying a peer certificate with the given CN; an
// empty cn means no TLS state at all (the no-peer-certificate rejection).
func certReq(method, target, cn string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if cn != "" {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		}}
	}
	return req
}

// ocsStatusCodeOf extracts the OCS <statuscode> from a response body.
func ocsStatusCodeOf(t *testing.T, body string) string {
	t.Helper()
	const open, close = "<ocs><meta><status>failure</status><statuscode>", "</statuscode>"
	_, rest, ok := strings.Cut(body, open)
	if !ok {
		t.Fatalf("body is not a failure envelope: %s", body)
	}
	statusCode, _, ok := strings.Cut(rest, close)
	if !ok {
		t.Fatalf("body has no statuscode element: %s", body)
	}
	return statusCode
}

// TestOCS_CertRejectionsRender997 pins every CertAuthenticator rejection
// shape on the OCS negotiation surface: the 997 "Unauthorised" envelope
// (HTTP 401 on v2, HTTP 200 with the statuscode in the envelope on v1), which
// is what the Basic rejections already get. A 500 statuscode here means the
// sentinel list went stale — the exact regression this file exists for.
func TestOCS_CertRejectionsRender997(t *testing.T) {
	f := newCertAuthOwncloud(t)
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
			for _, version := range []int{1, 2} {
				rec := httptest.NewRecorder()
				f.Handler().ServeHTTP(rec, certReq("GET", fmt.Sprintf("/ocs/v%d.php/cloud/user", version), tc.cn))
				wantHTTP := http.StatusUnauthorized
				if version == 1 {
					wantHTTP = http.StatusOK // v1 carries the status in the envelope
				}
				if rec.Code != wantHTTP {
					t.Fatalf("v%d HTTP status = %d, want %d", version, rec.Code, wantHTTP)
				}
				if got := ocsStatusCodeOf(t, rec.Body.String()); got != "997" {
					t.Fatalf("v%d OCS statuscode = %s, want 997 (a certificate rejection is a client error, not a server fault): %s",
						version, got, rec.Body.String())
				}
			}
		})
	}
}

// TestOCS_CertRegistryNilRenders997 pins the fail-closed third sentinel: a
// CertAuthenticator with no registry is a credential rejection, so 997.
func TestOCS_CertRegistryNilRenders997(t *testing.T) {
	wd, err := webdav.New(nilBackend{}, webdav.Config{Bucket: "oc-bkt"},
		webdav.WithAuthenticator(&auth.CertAuthenticator{}))
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(wd)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, certReq("GET", "/ocs/v2.php/cloud/user", certKnownCN))
	if got := ocsStatusCodeOf(t, rec.Body.String()); got != "997" {
		t.Fatalf("registry-less cert authenticator: OCS statuscode = %s, want 997 (fail closed, never 500)", got)
	}
}

// TestOCS_AllAuthSentinelsClassifyAsRejections is the anti-staleness pin:
// EVERY rejection sentinel internal/auth declares must be classified the same
// way by the OCS surface. A sentinel added in internal/auth without updating
// this frontend's classification re-creates the bug silently — and this table
// fails instead. Wrapped sentinels included (errors.Is, not ==).
func TestOCS_AllAuthSentinelsClassifyAsRejections(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{name: "basic missing", err: auth.ErrBasicMissing},
		{name: "basic malformed", err: auth.ErrBasicMalformed},
		{name: "bad credentials", err: auth.ErrBadCredentials},
		{name: "basic unsupported", err: auth.ErrBasicUnsupported},
		{name: "cert missing", err: auth.ErrCertMissing},
		{name: "cert unknown CN", err: auth.ErrCertUnknownCN},
		{name: "cert registry nil", err: auth.ErrCertRegistryNil},
		{name: "wrapped basic missing", err: fmt.Errorf("parse header: %w", auth.ErrBasicMissing)},
		{name: "wrapped cert unknown CN", err: fmt.Errorf("resolve cn %q: %w", "device-x", auth.ErrCertUnknownCN)},
	}
	for _, s := range sentinels {
		t.Run(s.name, func(t *testing.T) {
			wd, err := webdav.New(nilBackend{}, webdav.Config{Bucket: "oc-bkt"},
				webdav.WithAuthenticator(fixedErrAuth{err: s.err}))
			if err != nil {
				t.Fatal(err)
			}
			f, err := New(wd)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ocs/v2.php/cloud/user", nil))
			if got := ocsStatusCodeOf(t, rec.Body.String()); got != "997" {
				t.Fatalf("%s: OCS statuscode = %s, want 997 (every auth rejection sentinel is a client error)", s.name, got)
			}
			// The DAV data plane, delegated to the wrapped webdav frontend,
			// must agree on the SAME request.
			davRec := httptest.NewRecorder()
			f.Handler().ServeHTTP(davRec, httptest.NewRequest("PROPFIND", "/remote.php/webdav/x", nil))
			if davRec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: delegated webdav status = %d, want 401 (the OCS surface must classify identically)", s.name, davRec.Code)
			}
		})
	}
}

// fixedErrAuth returns a fixed error — the seam for driving the predicate
// with sentinels no single adapter produces by itself.
type fixedErrAuth struct{ err error }

func (a fixedErrAuth) Authenticate(_ *http.Request) (auth.Identity, error) {
	return auth.Identity{}, a.err
}

// TestOCS_NonSentinelAuthFailureStill500 is the other half of the split: a
// genuine authenticator fault (a wiring/lookup crash, not a credential
// decision) must stay 500. The shared predicate must not widen into "any
// error".
func TestOCS_NonSentinelAuthFailureStill500(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bare internal error", err: errors.New("registry: connection reset")},
		{name: "wrapped internal error", err: fmt.Errorf("lookup identity: %w", errors.New("boom"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wd, err := webdav.New(nilBackend{}, webdav.Config{Bucket: "oc-bkt"},
				webdav.WithAuthenticator(fixedErrAuth{err: tc.err}))
			if err != nil {
				t.Fatal(err)
			}
			f, err := New(wd)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ocs/v2.php/cloud/user", nil))
			if got := ocsStatusCodeOf(t, rec.Body.String()); got != "500" {
				t.Fatalf("internal authenticator failure: OCS statuscode = %s, want 500 (a real fault is not a credential rejection)", got)
			}
		})
	}
}

// TestOCS_KnownCNAuthenticates is the positive control: sharing the predicate
// must not turn the 997 branch into a blanket denial — a known CN resolves to
// its identity and the OCS endpoint answers its document.
func TestOCS_KnownCNAuthenticates(t *testing.T) {
	f := newCertAuthOwncloud(t)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, certReq("GET", "/ocs/v2.php/cloud/user", certKnownCN))
	if rec.Code != http.StatusOK {
		t.Fatalf("known CN: HTTP status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<status>ok</status>") {
		t.Fatalf("known CN did not reach the endpoint: %s", body)
	}
	if strings.Contains(body, "997") {
		t.Fatalf("authenticated request rendered the rejection envelope: %s", body)
	}
}
