package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
	_ "github.com/bhodgens/zeta-object/internal/frontend/h3"
)

// frontends_quic_test.go — the QUIC listener seam table (quic-h3-2026-10
// leaf 02 Task 1): mountFrontends rejects a QUICListenerFrontend without
// its own listenAddr (error naming the rule), classifies it as
// dedicated-listener (never shared-mux), and the listener spec carries the
// pre-built TLS config for main's UDP branch. No real sockets here — the
// real QUIC loopback lives in internal/frontend/h3/serve_test.go.

// fakeQUICFrontend is a minimal QUICListenerFrontend.
type fakeQUICFrontend struct {
	name    string
	addr    string
	tlsFail bool
}

func (f *fakeQUICFrontend) Name() string                      { return f.name }
func (f *fakeQUICFrontend) Handler() http.Handler             { return http.NewServeMux() }
func (f *fakeQUICFrontend) Authenticator() auth.Authenticator { return nil }
func (f *fakeQUICFrontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{}
}
func (f *fakeQUICFrontend) Addr() string         { return f.addr }
func (f *fakeQUICFrontend) IsQUICListener() bool { return true }
func (f *fakeQUICFrontend) TLSConfig() (*tls.Config, error) {
	if f.tlsFail {
		return nil, errors.New("no certificate pair configured")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13}, nil
}

var (
	_ frontend.Frontend             = (*fakeQUICFrontend)(nil)
	_ frontend.QUICListenerFrontend = (*fakeQUICFrontend)(nil)
)

// frontendNames renders a shared slice for failure messages.
func frontendNames(fs []frontend.Frontend) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name())
	}
	return out
}

// h3TestEnvForMain re-exports the h3 package's PKI fixture builder for the
// root-package factory tests (the builder lives in the h3 package's test
// files; this tiny adapter generates the same shapes locally).
type mainH3Env struct {
	registry auth.IdentityRegistry
	caPath   string
}

func h3TestEnvForMain(t *testing.T) *mainH3Env {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "main-test-root"},
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
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write ca.pem: %v", err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "device-1", AccessKey: "device-1", SecretKey: "sk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &mainH3Env{registry: reg, caPath: caPath}
}

// mountFrontends rejects a QUICListenerFrontend configured WITHOUT its own
// listenAddr: an empty address is NEVER a shared-mux fallback.
func TestMountFrontends_QUICWithoutListenAddrRejected(t *testing.T) {
	mux := http.NewServeMux()
	_, _, err := mountFrontends(mux, []frontendMount{
		{frontend: &fakeQUICFrontend{name: "h3", addr: ""}},
	})
	if err == nil {
		t.Fatal("mountFrontends accepted a QUIC frontend without listenAddr; want loud rejection")
	}
	for _, want := range []string{"h3", "requires its own listenAddr", "QUIC-listener frontend"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err.Error(), want)
		}
	}
}

// mountFrontends classifies a QUIC frontend as needing a dedicated listener
// even when an addr is present, and the spec carries the pre-built TLS
// config for main's UDP branch (never the TCP ListenAndServeTLS path).
func TestMountFrontends_QUICDedicatedListenerSpec(t *testing.T) {
	mux := http.NewServeMux()
	shared, extra, err := mountFrontends(mux, []frontendMount{
		{frontend: &fakeQUICFrontend{name: "h3", addr: "0.0.0.0:9443"}, listenAddr: "0.0.0.0:9443"},
	})
	if err != nil {
		t.Fatalf("mountFrontends: %v", err)
	}
	if len(shared) != 0 {
		t.Fatalf("shared = %v, want none (a QUIC frontend is never shared-mux)", frontendNames(shared))
	}
	if len(extra) != 1 {
		t.Fatalf("extra specs = %d, want 1", len(extra))
	}
	spec := extra[0]
	if spec.addr != "0.0.0.0:9443" {
		t.Fatalf("spec.addr = %q, want the frontend's Addr", spec.addr)
	}
	if spec.quicConfig == nil {
		t.Fatal("spec.quicConfig must carry the pre-built TLS config")
	}
	if spec.quicConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("spec.quicConfig.MinVersion = %x, want TLS 1.3", spec.quicConfig.MinVersion)
	}
	if spec.tlsConfig != nil {
		t.Fatal("spec.tlsConfig must stay nil for a QUIC frontend (it is not a TCP TLS listener)")
	}
}

// A TLSListenerFrontend that has NOT opted in (IsQUICListener false or
// absent marker semantics) is classified as a TCP TLS listener even
// though its Addr/TLSConfig methods structurally satisfy
// QUICListenerFrontend - the admin-frontend regression quic-h3-2026-10
// leaf 07's e2e run caught (admin was misrouted to UDP, its mTLS TCP
// listener never opened).
func TestMountFrontends_TLSFrontendWithoutQUICOptInStaysTCP(t *testing.T) {
	mux := http.NewServeMux()
	// fakeQUICFrontend with the marker FORCED OFF: structurally identical
	// to an admin-style TLS-listener frontend.
	noOptIn := &fakeQUICFrontend{name: "admin-shaped", addr: "127.0.0.1:9444"}
	shared, extra, err := mountFrontends(mux, []frontendMount{
		{frontend: &noOptInShim{fakeQUICFrontend: noOptIn}, listenAddr: "127.0.0.1:9444"},
	})
	if err != nil {
		t.Fatalf("mountFrontends: %v", err)
	}
	if len(shared) != 0 {
		t.Fatalf("shared = %v, want none (it still needs its own listener)", frontendNames(shared))
	}
	if len(extra) != 1 {
		t.Fatalf("extra specs = %d, want 1", len(extra))
	}
	if extra[0].quicConfig != nil {
		t.Fatal("a non-opted-in TLS-listener frontend must NOT be classified as QUIC (regression: admin misrouted to UDP)")
	}
	if extra[0].tlsConfig == nil {
		t.Fatal("a non-opted-in TLS-listener frontend must carry tlsConfig (TCP path)")
	}
}

// noOptInShim presents fakeQUICFrontend with IsQUICListener returning
// false - the shape every mere TLSListenerFrontend has.
type noOptInShim struct{ *fakeQUICFrontend }

func (s *noOptInShim) IsQUICListener() bool { return false }

// A QUICListenerFrontend whose TLSConfig fails aborts the mount loudly.
func TestMountFrontends_QUICTLSConfigErrorPropagates(t *testing.T) {
	mux := http.NewServeMux()
	_, _, err := mountFrontends(mux, []frontendMount{
		{frontend: &fakeQUICFrontend{name: "h3", addr: ":9443", tlsFail: true}, listenAddr: ":9443"},
	})
	if err == nil || !strings.Contains(err.Error(), "building QUIC listener config") {
		t.Fatalf("err = %v, want a wrapped QUIC-listener-config error", err)
	}
}

// startupPlan: a real h3 config entry (via the factory) produces a plan
// whose listener spec is the QUIC kind — the seam end-to-end without
// opening sockets. Requires a cert pair + CA file on disk and a registry.
func TestStartupPlan_H3FrontendConfig(t *testing.T) {
	env := h3TestEnvForMain(t)
	prevReg := identityRegistry
	identityRegistry = auth.NewReloadableRegistry(env.registry)
	defer func() { identityRegistry = prevReg }()

	plan, err := startupPlan([]FrontendConfig{
		{Type: "h3", ListenAddr: "127.0.0.1:9443", Bucket: "photos", Options: map[string]string{"clientCAFile": env.caPath}},
	}, nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("startupPlan with h3 entry: %v", err)
	}
	if len(plan.listeners) != 1 {
		t.Fatalf("listeners = %d, want 1", len(plan.listeners))
	}
	spec := plan.listeners[0]
	if spec.quicConfig == nil {
		t.Fatal("h3 listener spec must carry quicConfig")
	}
	if _, ok := spec.frontend.(frontend.QUICListenerFrontend); !ok {
		t.Fatalf("spec frontend %T does not implement QUICListenerFrontend", spec.frontend)
	}
}

// startupPlan: an h3 entry WITHOUT listenAddr is rejected by the factory
// (construction-time, naming the rule).
func TestStartupPlan_H3WithoutListenAddrRejected(t *testing.T) {
	env := h3TestEnvForMain(t)
	prevReg := identityRegistry
	identityRegistry = auth.NewReloadableRegistry(env.registry)
	defer func() { identityRegistry = prevReg }()

	_, err := startupPlan([]FrontendConfig{
		{Type: "h3", Bucket: "photos", Options: map[string]string{"clientCAFile": env.caPath}},
	}, nilBackend{}, stubCreds{})
	if err == nil {
		t.Fatal("startupPlan accepted an h3 entry without listenAddr")
	}
	if !strings.Contains(err.Error(), "listenAddr") {
		t.Fatalf("err = %q, want it to name listenAddr", err.Error())
	}
}

// startupPlan: an unknown option key on an h3 entry aborts naming the key
// and the known (empty except clientCAFile) set.
func TestStartupPlan_H3UnknownOptionRejected(t *testing.T) {
	env := h3TestEnvForMain(t)
	prevReg := identityRegistry
	identityRegistry = auth.NewReloadableRegistry(env.registry)
	defer func() { identityRegistry = prevReg }()

	_, err := startupPlan([]FrontendConfig{
		{Type: "h3", ListenAddr: "127.0.0.1:9443", Bucket: "photos", Options: map[string]string{"bogusKey": "x"}},
	}, nilBackend{}, stubCreds{})
	if err == nil || !strings.Contains(err.Error(), `unknown option key "bogusKey"`) {
		t.Fatalf("err = %v, want unknown-option-key rejection", err)
	}
	if !strings.Contains(err.Error(), "clientCAFile") {
		t.Fatalf("err = %q, want the known set to name clientCAFile", err.Error())
	}
}
