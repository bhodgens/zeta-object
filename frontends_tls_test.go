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
	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
)

// fakeTLSListenerFrontend implements frontend.TLSListenerFrontend to prove the
// main-package wiring of the listener seam (management-api-2026-10 Contract 1).
type fakeTLSListenerFrontend struct {
	name string
	addr string
	cfg  *tls.Config
	err  error
}

func (f *fakeTLSListenerFrontend) Name() string                      { return f.name }
func (f *fakeTLSListenerFrontend) Handler() http.Handler             { return http.NewServeMux() }
func (f *fakeTLSListenerFrontend) Authenticator() auth.Authenticator { return nil }
func (f *fakeTLSListenerFrontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{}
}
func (f *fakeTLSListenerFrontend) Addr() string                    { return f.addr }
func (f *fakeTLSListenerFrontend) TLSConfig() (*tls.Config, error) { return f.cfg, f.err }

var _ frontend.TLSListenerFrontend = (*fakeTLSListenerFrontend)(nil)

// A TLSListenerFrontend configured without its own listenAddr is rejected at
// startup with a message naming the rule.
func TestMountFrontends_TLSListenerRequiresOwnAddr(t *testing.T) {
	fe := &fakeTLSListenerFrontend{name: "fake-tls"}
	mux := http.NewServeMux()
	_, _, err := mountFrontends(mux, []frontendMount{{frontend: fe, listenAddr: ""}})
	if err == nil {
		t.Fatal("mountFrontends accepted a TLSListenerFrontend without a listenAddr")
	}
	for _, want := range []string{"fake-tls", "requires its own listenAddr"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
}

// A TLSListenerFrontend's pre-built config is carried into the listenerSpec.
func TestMountFrontends_TLSCarriesConfigIntoSpec(t *testing.T) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	fe := &fakeTLSListenerFrontend{name: "fake-tls", addr: "127.0.0.1:9443", cfg: cfg}
	mux := http.NewServeMux()
	shared, extra, err := mountFrontends(mux, []frontendMount{{frontend: fe, listenAddr: "127.0.0.1:9443"}})
	if err != nil {
		t.Fatalf("mountFrontends: %v", err)
	}
	if len(shared) != 0 {
		t.Fatalf("shared = %+v, want none (TLS-listener frontend is dedicated)", shared)
	}
	if len(extra) != 1 {
		t.Fatalf("extra = %+v, want one dedicated spec", extra)
	}
	if extra[0].tlsConfig != cfg {
		t.Fatalf("spec.tlsConfig = %p, want the frontend's TLSConfig %p", extra[0].tlsConfig, cfg)
	}
	if extra[0].addr != "127.0.0.1:9443" {
		t.Fatalf("spec.addr = %q, want 127.0.0.1:9443", extra[0].addr)
	}
	if _, ok := extra[0].frontend.(frontend.TLSListenerFrontend); !ok {
		t.Fatalf("dedicated spec must carry the TLSListenerFrontend for main's type-assert")
	}
}

// A TLSConfig() error aborts the mount loudly.
func TestMountFrontends_TLSConfigErrorPropagates(t *testing.T) {
	fe := &fakeTLSListenerFrontend{name: "fake-tls", addr: "127.0.0.1:9443", err: errors.New("boom")}
	mux := http.NewServeMux()
	_, _, err := mountFrontends(mux, []frontendMount{{frontend: fe, listenAddr: "127.0.0.1:9443"}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the wrapped TLSConfig error", err)
	}
}

// A plain dedicated frontend keeps the process-wide cert pair: its spec has no
// pre-built TLS config.
func TestMountFrontends_PlainDedicatedHasNoTLSConfig(t *testing.T) {
	plain := &stubFrontend{name: "plain"}
	mux := http.NewServeMux()
	_, extra, err := mountFrontends(mux, []frontendMount{{frontend: plain, listenAddr: ":8444"}})
	if err != nil {
		t.Fatalf("mountFrontends: %v", err)
	}
	if len(extra) != 1 || extra[0].tlsConfig != nil {
		t.Fatalf("extra = %+v, want one spec with a nil tlsConfig", extra)
	}
}

// An empty Addr is a construction error (admin.New), never a shared-mux
// fallback.
func TestTLSListener_EmptyAddrConstructionError(t *testing.T) {
	if _, err := admin.New(admin.Options{}); err == nil {
		t.Fatal("admin.New accepted an empty listenAddr")
	}
}

func TestAdminFactory_BuildsTLSListenerFrontend(t *testing.T) {
	caPath := writeTestCAFile(t)
	fe, err := frontendFactories["admin"](
		FrontendConfig{Type: "admin", ListenAddr: "127.0.0.1:0", Options: map[string]string{"clientCAFile": caPath}},
		nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("admin factory: %v", err)
	}
	if fe.Name() != "admin" {
		t.Fatalf("Name() = %q, want admin", fe.Name())
	}
	tl, ok := fe.(frontend.TLSListenerFrontend)
	if !ok {
		t.Fatalf("admin frontend = %T, want a TLSListenerFrontend", fe)
	}
	if tl.Addr() != "127.0.0.1:0" {
		t.Fatalf("Addr() = %q, want 127.0.0.1:0", tl.Addr())
	}
}

// An unknown option key aborts construction naming the key and the known set.
func TestAdminFactory_UnknownOptionKey(t *testing.T) {
	_, _, err := buildFrontends(
		[]FrontendConfig{{Type: "admin", ListenAddr: "127.0.0.1:0", Options: map[string]string{"bogus": "1"}}},
		nilBackend{}, stubCreds{})
	if err == nil {
		t.Fatal("admin factory accepted an unknown option key")
	}
	for _, want := range []string{"unknown option key", "bogus", "adminPrincipals", "allowNonLoopback", "clientCAFile"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
}

// A missing clientCAFile is a startup error naming the path.
func TestAdminFactory_MissingClientCAFileNamesPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-ca.pem")
	_, _, err := buildFrontends(
		[]FrontendConfig{{Type: "admin", ListenAddr: "127.0.0.1:0", Options: map[string]string{"clientCAFile": missing}}},
		nilBackend{}, stubCreds{})
	if err == nil {
		t.Fatal("admin factory accepted a missing clientCAFile")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q must name the path %q", err, missing)
	}
}

// A non-loopback listenAddr without allowNonLoopback aborts construction.
func TestAdminFactory_NonLoopbackRejected(t *testing.T) {
	caPath := writeTestCAFile(t)
	_, _, err := buildFrontends(
		[]FrontendConfig{{Type: "admin", ListenAddr: "0.0.0.0:9000", Options: map[string]string{"clientCAFile": caPath}}},
		nilBackend{}, stubCreds{})
	if err == nil {
		t.Fatal("admin factory accepted a non-loopback bind without allowNonLoopback")
	}
	if !strings.Contains(err.Error(), "0.0.0.0:9000") {
		t.Fatalf("error %q must name the address", err)
	}
}

// writeTestCAFile writes a freshly generated self-signed CA as a PEM bundle
// and returns its path (no openssl dependency).
func writeTestCAFile(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	return path
}
