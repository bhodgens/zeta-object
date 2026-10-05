package h3

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

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
)

// frontend_test.go — construction table for the h3 frontend (leaf 02 Task
// 2): required listenAddr/bucket/clientCAFile, unknown option keys, the
// TLSConfig posture, Name/Capabilities delegation, Addr round-trip. The
// real QUIC round-trip lives in serve_test.go.

var h3Serial atomic.Int64

// h3TestCA mints a self-signed CA (crypto/x509 only — no openssl dep, same
// discipline as the admin package's testcerts).
type h3TestCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemData []byte
}

func newH3TestCA(t *testing.T, cn string) *h3TestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(h3Serial.Add(1)),
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
	return &h3TestCA{cert: cert, key: key, pemData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issueLeaf signs a leaf (client or server EKU) under the CA.
func (ca *h3TestCA) issueLeaf(t *testing.T, cn string, client bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(h3Serial.Add(1)),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if client {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
		tmpl.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeH3Temp(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// h3TestEnv bundles the generated PKI + registry and valid defaults.
type h3TestEnv struct {
	dir       string
	ca        *h3TestCA
	caPath    string
	certPath  string
	keyPath   string
	clientTLS tls.Certificate
	registry  auth.IdentityRegistry
}

func newH3TestEnv(t *testing.T) *h3TestEnv {
	t.Helper()
	dir := t.TempDir()
	ca := newH3TestCA(t, "h3-test-root")
	caPath := writeH3Temp(t, dir, "ca.pem", ca.pemData)
	_, serverPEM, serverKeyPEM := ca.issueLeaf(t, "h3-test-server", false)
	certPath := writeH3Temp(t, dir, "server.pem", serverPEM)
	keyPath := writeH3Temp(t, dir, "server-key.pem", serverKeyPEM)
	clientTLS, _, _ := ca.issueLeaf(t, "device-1", true)
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "device-1", AccessKey: "device-1", SecretKey: "sk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &h3TestEnv{
		dir:       dir,
		ca:        ca,
		caPath:    caPath,
		certPath:  certPath,
		keyPath:   keyPath,
		clientTLS: clientTLS,
		registry:  reg,
	}
}

func (e *h3TestEnv) validConfig() Config {
	return Config{
		ListenAddr:   "127.0.0.1:0",
		Bucket:       "photos",
		ClientCAFile: e.caPath,
		CertFile:     e.certPath,
		KeyFile:      e.keyPath,
	}
}

func TestNew_ConfigTable(t *testing.T) {
	env := newH3TestEnv(t)
	tests := []struct {
		name        string
		mutate      func(c *Config)
		errContains string
	}{
		{
			name: "valid config constructs",
		},
		{
			name:        "empty listenAddr rejected (never shared-mux fallback)",
			mutate:      func(c *Config) { c.ListenAddr = "" },
			errContains: "requires a non-empty listenAddr",
		},
		{
			name:        "empty bucket rejected (single-bucket re-root needs it)",
			mutate:      func(c *Config) { c.Bucket = "" },
			errContains: "requires a non-empty bucket",
		},
		{
			name:        "missing clientCAFile rejected naming the requirement",
			mutate:      func(c *Config) { c.ClientCAFile = "" },
			errContains: "clientCAFile is required",
		},
		{
			name:        "unreadable clientCAFile rejected naming the path",
			mutate:      func(c *Config) { c.ClientCAFile = filepath.Join(env.dir, "nope.pem") },
			errContains: "nope.pem",
		},
		{
			name:        "clientCAFile without usable PEM rejected naming the path",
			mutate:      func(c *Config) { c.ClientCAFile = writeH3Temp(t, env.dir, "junk.pem", []byte("not a cert")) },
			errContains: "junk.pem",
		},
		{
			name:        "nil registry rejected",
			mutate:      nil, // handled via the nilReg flag below
			errContains: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := env.validConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			f, err := New(nilBackendStub{}, cfg, env.registry, nil)
			if tt.name == "nil registry rejected" {
				// exercised separately below
				_ = f
				return
			}
			if tt.errContains == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("New succeeded, want error containing %q", tt.errContains)
			}
			if got := err.Error(); !contains(got, tt.errContains) {
				t.Fatalf("err = %q, want substring %q", got, tt.errContains)
			}
		})
	}
	if _, err := New(nilBackendStub{}, env.validConfig(), nil, nil); err == nil || !contains(err.Error(), "identity registry") {
		t.Fatalf("nil registry err = %v, want identity-registry error", err)
	}
}

// TestNew_NilBackendRejected pins the wrapped webdav's own rule surfacing.
func TestNew_NilBackendRejected(t *testing.T) {
	env := newH3TestEnv(t)
	_, err := New(nil, env.validConfig(), env.registry, nil)
	if err == nil || !contains(err.Error(), "backend is required") {
		t.Fatalf("nil backend err = %v, want webdav backend-required error", err)
	}
}

// TestFrontend_Seam pins Name, Addr round-trip, Capabilities delegation,
// and the compile-time QUICListenerFrontend assertion.
func TestFrontend_Seam(t *testing.T) {
	env := newH3TestEnv(t)
	f, err := New(nilBackendStub{}, env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if f.Name() != "h3" {
		t.Fatalf("Name = %q, want h3", f.Name())
	}
	if f.Addr() != "127.0.0.1:0" {
		t.Fatalf("Addr = %q, want config value round-trip", f.Addr())
	}
	caps := f.Capabilities()
	if caps.Buckets {
		t.Fatal("single-bucket mode must report Buckets false")
	}
	if !caps.ConditionalReads {
		t.Fatal("wrapped webdav reports ConditionalReads true")
	}
	if f.Handler() == nil {
		t.Fatal("Handler must return the wrapped webdav handler")
	}
	if f.Authenticator() == nil {
		t.Fatal("Authenticator must return the mTLS adapter")
	}
}

// TestTLSConfig_Posture pins the listener TLS configuration: process pair
// loaded, TLS 1.3 minimum, RequireAndVerifyClientCert, ClientCAs from the
// configured bundle.
func TestTLSConfig_Posture(t *testing.T) {
	env := newH3TestEnv(t)
	f, err := New(nilBackendStub{}, env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg, err := f.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates = %d, want 1 (process pair)", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("ClientCAs must be loaded from clientCAFile")
	}
	// A missing cert pair fails loudly naming the files.
	bad := env.validConfig()
	bad.CertFile = filepath.Join(env.dir, "missing.pem")
	bf, err := New(nilBackendStub{}, bad, env.registry, nil)
	if err != nil {
		t.Fatalf("New with missing cert file: %v", err)
	}
	if _, err := bf.TLSConfig(); err == nil || !contains(err.Error(), "missing.pem") {
		t.Fatalf("TLSConfig err = %v, want error naming missing.pem", err)
	}
}

// TestKnownOptionKeys pins the v1 option set: ONLY clientCAFile.
func TestKnownOptionKeys(t *testing.T) {
	if len(KnownOptionKeys) != 1 || !KnownOptionKeys["clientCAFile"] {
		t.Fatalf("KnownOptionKeys = %v, want exactly clientCAFile", KnownOptionKeys)
	}
}

// nilBackendStub embeds the backend interface; the webdav constructor only
// nil-checks the value, and these tests exercise construction/seam, not the
// data path (serve_test.go covers it with a real stub).
type nilBackendStub struct{ backend.Backend }

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || index(s, sub) >= 0)
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
