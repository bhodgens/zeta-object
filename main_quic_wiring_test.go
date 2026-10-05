package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
	h3 "github.com/bhodgens/zeta-object/internal/frontend/h3"
)

// main_quic_wiring_test.go — the main-package QUIC wiring table
// (quic-h3-2026-10 leaf 02): buildDedicatedListeners routes a
// QUICListenerFrontend spec to a REAL UDP/QUIC server on 127.0.0.1:0, a
// foreign QUICListenerFrontend is a loud startup failure, and the
// start/drain goroutines work against the same graceful-stop fan the TCP
// listeners use.

// mainH3PKI bundles the cert pair + CA + registry the factory needs.
type mainH3PKI struct {
	registry auth.IdentityRegistry
	caPath   string
	certPath string
	keyPath  string
}

func newMainH3PKI(t *testing.T) *mainH3PKI {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "main-wiring-test-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server-key.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "device-1", AccessKey: "device-1", SecretKey: "sk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &mainH3PKI{registry: reg, caPath: caPath, certPath: certPath, keyPath: keyPath}
}

// installH3ProcessWiring swaps the process registry/cert pair for the test
// PKI and returns a restore func.
func installH3ProcessWiring(t *testing.T, pki *mainH3PKI) func() {
	t.Helper()
	prevReg := identityRegistry
	identityRegistry = auth.NewReloadableRegistry(pki.registry)
	prevCert, prevKey := serverConfig.CertFile, serverConfig.KeyFile
	serverConfig.CertFile, serverConfig.KeyFile = pki.certPath, pki.keyPath
	return func() {
		identityRegistry = prevReg
		serverConfig.CertFile, serverConfig.KeyFile = prevCert, prevKey
	}
}

// buildH3FrontendForTest constructs a real h3 frontend through the factory.
func buildH3FrontendForTest(t *testing.T, pki *mainH3PKI) *h3.Frontend {
	t.Helper()
	f, err := frontendFactories["h3"](
		FrontendConfig{Type: "h3", ListenAddr: "127.0.0.1:0", Bucket: "photos", Options: map[string]string{"clientCAFile": pki.caPath}},
		nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("h3 factory: %v", err)
	}
	return f.(*h3.Frontend)
}

// TestBuildDedicatedListeners_QUICSpec gets a real h3 listener bound: the
// spec lands in the quic slice (never among the TCP http.Servers), and the
// UDP socket is real.
func TestBuildDedicatedListeners_QUICSpec(t *testing.T) {
	pki := newMainH3PKI(t)
	defer installH3ProcessWiring(t, pki)()
	f := buildH3FrontendForTest(t, pki)

	extra, nonHTTP, quic := buildDedicatedListeners([]listenerSpec{{
		frontend: f,
		addr:     "127.0.0.1:0",
	}})
	if len(extra) != 0 || len(nonHTTP) != 0 {
		t.Fatalf("extra=%d nonHTTP=%d, want both empty (QUIC is never a TCP listener)", len(extra), len(nonHTTP))
	}
	if len(quic) != 1 {
		t.Fatalf("quic specs = %d, want 1", len(quic))
	}
	if quic[0].srv == nil {
		t.Fatal("quicServer.srv must carry the running h3.Server")
	}
	if _, ok := quic[0].srv.Addr().(*net.UDPAddr); !ok {
		t.Fatalf("Addr = %T, want *net.UDPAddr", quic[0].srv.Addr())
	}
	if err := quic[0].srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestBuildDedicatedListeners_ForeignQUICRejected pins the loud failure for
// a QUICListenerFrontend with no QUIC serving path (through the quitQUIC
// seam so the test binary survives the abort).
func TestBuildDedicatedListeners_ForeignQUICRejected(t *testing.T) {
	prevQuit := quitQUIC
	fatalMsg := ""
	quitQUIC = func(format string, args ...any) { fatalMsg = fmt.Sprintf(format, args...) }
	defer func() { quitQUIC = prevQuit }()

	buildDedicatedListeners([]listenerSpec{{
		frontend: &fakeQUICFrontend{name: "foreign", addr: "127.0.0.1:0"},
		addr:     "127.0.0.1:0",
	}})
	if !strings.Contains(fatalMsg, "foreign") || !strings.Contains(fatalMsg, "no QUIC serving path") {
		t.Fatalf("quitQUIC msg = %q, want it to name the frontend and the missing path", fatalMsg)
	}
}

// TestStartAndDrainQUICFrontend exercises the Serve goroutine + graceful
// drain: the fan completes without an unexpected error and without leaking.
func TestStartAndDrainQUICFrontend(t *testing.T) {
	pki := newMainH3PKI(t)
	defer installH3ProcessWiring(t, pki)()
	hf := buildH3FrontendForTest(t, pki)
	srv, err := hf.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	qs := quicServer{frontend: hf, srv: srv, addr: srv.Addr().String()}

	serverErr := make(chan error, 1)
	startQUICFrontend(qs, serverErr)

	time.Sleep(50 * time.Millisecond)
	var wg sync.WaitGroup
	drainQUICFrontends([]quicServer{qs}, &wg)
	wg.Wait()

	select {
	case err := <-serverErr:
		t.Fatalf("Serve surfaced an unexpected error: %v", err)
	default:
	}
	// Empty-slice drain calls stay safe (the fan calls them unconditionally).
	var wg2 sync.WaitGroup
	drainQUICFrontends(nil, &wg2)
	drainNonHTTPFrontends(nil, &wg2)
	wg2.Wait()
}

// TestStartQUICFrontend_CloseIdempotent pins that the drain path is safe
// when Close is called twice (test cleanup + drain fan) and that a drained
// server leaves no error in the funnel.
func TestStartQUICFrontend_CloseIdempotent(t *testing.T) {
	pki := newMainH3PKI(t)
	defer installH3ProcessWiring(t, pki)()
	hf := buildH3FrontendForTest(t, pki)
	srv, err := hf.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	qs := quicServer{frontend: hf, srv: srv, addr: srv.Addr().String()}
	serverErr := make(chan error, 1)
	startQUICFrontend(qs, serverErr)
	time.Sleep(50 * time.Millisecond)
	// Drain twice through the fan (double Close must not hang or panic).
	var wg sync.WaitGroup
	drainQUICFrontends([]quicServer{qs, qs}, &wg)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain hung on a double Close")
	}
	select {
	case err := <-serverErr:
		t.Fatalf("drain surfaced an unexpected error: %v", err)
	default:
	}
}

// Compile-time: the test file keeps frontend and tls imports honest.
var (
	_ frontend.Frontend = (*fakeQUICFrontend)(nil)
	_ *tls.Config       = nil
)
