package admin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// In-test PKI fixtures: unit tests generate every certificate with
// crypto/x509 so the package has no openssl dependency (leaf 01 Task 2).
var serialCounter atomic.Int64

func nextSerial() *big.Int {
	return big.NewInt(serialCounter.Add(1))
}

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemData []byte
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{
		cert:    cert,
		key:     key,
		pemData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

type issueOptions struct {
	clientAuth bool
	notBefore  time.Time
	notAfter   time.Time
}

// issue signs a leaf certificate. clientAuth selects the client EKU; the
// server form carries loopback SANs so a real handshake against 127.0.0.1
// verifies.
func (ca *testCA) issue(t *testing.T, cn string, opts issueOptions) (certPEM, keyPEM []byte, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	notBefore := opts.notBefore
	if notBefore.IsZero() {
		notBefore = time.Now().Add(-time.Hour)
	}
	notAfter := opts.notAfter
	if notAfter.IsZero() {
		notAfter = time.Now().Add(24 * time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if opts.clientAuth {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
		tmpl.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert (cn=%q): %v", cn, err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, cert
}

func writeTemp(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// testEnv bundles a generated PKI and an admin frontend built from it.
type testEnv struct {
	frontend  *adminFrontend
	ca        *testCA
	caPath    string
	client    *x509.Certificate
	clientTLS tls.Certificate
	emptyCN   *x509.Certificate
	wrongCert *x509.Certificate
	expired   *x509.Certificate
}

// newTestEnv generates a CA, a server pair and client certificates, then
// builds the frontend from opts (filling in the generated paths when the
// caller leaves them unset).
func newTestEnv(t *testing.T, opts Options) *testEnv {
	t.Helper()
	dir := t.TempDir()
	ca := newTestCA(t, "test-root")
	caPath := writeTemp(t, dir, "ca.pem", ca.pemData)

	serverPEM, serverKeyPEM, _ := ca.issue(t, "admin-server", issueOptions{})
	serverCertPath := writeTemp(t, dir, "server.pem", serverPEM)
	serverKeyPath := writeTemp(t, dir, "server-key.pem", serverKeyPEM)

	clientPEM, clientKeyPEM, clientCert := ca.issue(t, "tester", issueOptions{clientAuth: true})
	clientPair, err := tls.X509KeyPair(clientPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("client key pair: %v", err)
	}

	_, _, emptyCN := ca.issue(t, "", issueOptions{clientAuth: true})
	if emptyCN == nil {
		t.Fatal("empty-CN certificate fixture was not produced")
	}

	wrongCA := newTestCA(t, "other-root")
	_, _, wrongCert := wrongCA.issue(t, "tester", issueOptions{clientAuth: true})

	_, _, expired := ca.issue(t, "tester", issueOptions{
		clientAuth: true,
		notBefore:  time.Now().Add(-48 * time.Hour),
		notAfter:   time.Now().Add(-24 * time.Hour),
	})

	if opts.ClientCAFile == "" {
		opts.ClientCAFile = caPath
	}
	if opts.ListenAddr == "" {
		opts.ListenAddr = "127.0.0.1:0"
	}
	if opts.CertFile == "" {
		opts.CertFile = serverCertPath
	}
	if opts.KeyFile == "" {
		opts.KeyFile = serverKeyPath
	}
	built, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	af, ok := built.(*adminFrontend)
	if !ok {
		t.Fatalf("New returned %T, want *adminFrontend", built)
	}
	return &testEnv{
		frontend:  af,
		ca:        ca,
		caPath:    caPath,
		client:    clientCert,
		clientTLS: clientPair,
		emptyCN:   emptyCN,
		wrongCert: wrongCert,
		expired:   expired,
	}
}

// caPool exposes the fixture CA for client-side verification in handshake
// tests.
func (e *testEnv) caPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(e.ca.cert)
	return pool
}

// withPrincipals builds a second frontend over the SAME PKI with a different
// principal allow-list.
func (e *testEnv) withPrincipals(t *testing.T, principals []string) *adminFrontend {
	t.Helper()
	built, err := New(Options{
		ListenAddr:      e.frontend.opts.ListenAddr,
		ClientCAFile:    e.caPath,
		AdminPrincipals: principals,
		CertFile:        e.frontend.opts.CertFile,
		KeyFile:         e.frontend.opts.KeyFile,
	})
	if err != nil {
		t.Fatalf("New (principals %v): %v", principals, err)
	}
	af, ok := built.(*adminFrontend)
	if !ok {
		t.Fatalf("New returned %T, want *adminFrontend", built)
	}
	return af
}
