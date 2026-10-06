package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// cert_test.go — the mTLS certificate authenticator table (quic-h3-2026-10
// leaf 02 Task 2). The unit table pins the CN→identity decision: known CN
// resolves, unknown/empty CN fail closed, no peer certificate fails closed.
// A certificate signed by a WRONG CA never reaches this adapter in production
// (the TLS handshake rejects it under RequireAndVerifyClientCert) — that
// property is pinned by the h3 package's real-loopback serve test.

var certSerial atomic.Int64

// issueTestCert mints a certificate with the given CN, signed by ca (nil ⇒
// self-signed). clientAuth controls the EKU.
func issueTestCert(t *testing.T, cn string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, clientAuth bool) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(certSerial.Add(1)),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  caCert == nil,
	}
	if clientAuth {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	parent, pk := tmpl, any(key)
	if caCert != nil {
		parent, pk = caCert, caKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, pk)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, parsed
}

// stubCNRegistry resolves exactly one CN.
type stubCNRegistry struct {
	known string
}

func (s stubCNRegistry) LookupByAccessKey(accessKeyID string) (Identity, bool) {
	if accessKeyID == s.known {
		return Identity{AccessKeyID: accessKeyID, BucketGrants: map[string]Grant{"b": {Read: true, Write: true}}}, true
	}
	return Identity{}, false
}

func TestCertAuthenticator(t *testing.T) {
	reg := stubCNRegistry{known: "device-1"}
	a := NewCertAuthenticator(reg)

	knownCert, knownParsed := issueTestCert(t, "device-1", nil, nil, true)
	_, unknownParsed := issueTestCert(t, "device-x", nil, nil, true)
	_, emptyParsed := issueTestCert(t, "", nil, nil, true)

	tests := []struct {
		name    string
		tls     *tls.ConnectionState
		want    string
		wantErr error
	}{
		{
			name: "known CN resolves to the registry identity",
			tls:  &tls.ConnectionState{PeerCertificates: []*x509.Certificate{knownParsed}},
			want: "device-1",
		},
		{
			name:    "unknown CN fails closed",
			tls:     &tls.ConnectionState{PeerCertificates: []*x509.Certificate{unknownParsed}},
			wantErr: ErrCertUnknownCN,
		},
		{
			name:    "empty CN fails closed",
			tls:     &tls.ConnectionState{PeerCertificates: []*x509.Certificate{emptyParsed}},
			wantErr: ErrCertUnknownCN,
		},
		{
			name:    "no peer certificate fails closed",
			tls:     &tls.ConnectionState{},
			wantErr: ErrCertMissing,
		},
		{
			name:    "nil TLS state fails closed",
			tls:     nil,
			wantErr: ErrCertMissing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := a.Authenticate(&http.Request{TLS: tt.tls})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Authenticate err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if id.AccessKeyID != tt.want {
				t.Fatalf("AccessKeyID = %q, want %q", id.AccessKeyID, tt.want)
			}
			if g, ok := id.BucketGrants["b"]; !ok || !g.Read || !g.Write {
				t.Fatalf("grants = %+v, want readwrite on b", id.BucketGrants)
			}
		})
	}
	_ = knownCert // the wire form is exercised by the h3 serve test
}

// TestNewCertAuthenticator_NilRegistryPans pins the wiring-bug posture
// (same as NewBasicAuthenticator).
func TestNewCertAuthenticator_NilRegistryPans(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewCertAuthenticator(nil) must panic")
		}
	}()
	NewCertAuthenticator(nil)
}

// TestIdentityFromCertificate pins the pure mapping helper's parity with the
// request-path adapter.
func TestIdentityFromCertificate(t *testing.T) {
	reg := stubCNRegistry{known: "device-1"}
	_, parsed := issueTestCert(t, "device-1", nil, nil, true)
	id, err := IdentityFromCertificate(reg, parsed)
	if err != nil || id.AccessKeyID != "device-1" {
		t.Fatalf("IdentityFromCertificate = %+v, %v", id, err)
	}
	if _, err := IdentityFromCertificate(reg, nil); !errors.Is(err, ErrCertMissing) {
		t.Fatalf("nil cert err = %v, want ErrCertMissing", err)
	}
}

// TestCertAuthenticator_NilRegistryFailsClosed pins that a registry-less
// adapter returns ErrCertRegistryNil rather than dereferencing nil. Before
// this guard the zero-value adapter PANICKED inside the request handler, so a
// wiring slip surfaced as a crash instead of a status. ErrCertRegistryNil is
// a typed rejection: frontends render it as a client error (401), not a 500.
func TestCertAuthenticator_NilRegistryFailsClosed(t *testing.T) {
	a := &CertAuthenticator{}
	_, knownParsed := issueTestCert(t, "device-1", nil, nil, true)

	if _, err := a.Authenticate(&http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{knownParsed}}}); !errors.Is(err, ErrCertRegistryNil) {
		t.Fatalf("nil registry err = %v, want ErrCertRegistryNil (must not deref nil)", err)
	}
	// No peer certificate still short-circuits first (the same rejection a
	// wired adapter gives, not a registry one).
	if _, err := a.Authenticate(&http.Request{}); !errors.Is(err, ErrCertMissing) {
		t.Fatalf("nil registry + no cert err = %v, want ErrCertMissing", err)
	}
	// The pure helper behaves identically.
	if _, err := IdentityFromCertificate(nil, knownParsed); !errors.Is(err, ErrCertRegistryNil) {
		t.Fatalf("IdentityFromCertificate(nil reg) err = %v, want ErrCertRegistryNil", err)
	}
}

// TestCertificateCN pins the diagnostic helper.
func TestCertificateCN(t *testing.T) {
	_, parsed := issueTestCert(t, "cn-1", nil, nil, true)
	if got := CertificateCN(&http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{parsed}}}); got != "cn-1" {
		t.Fatalf("CertificateCN = %q, want cn-1", got)
	}
	if got := CertificateCN(&http.Request{}); got != "" {
		t.Fatalf("CertificateCN(no tls) = %q, want empty", got)
	}
}
