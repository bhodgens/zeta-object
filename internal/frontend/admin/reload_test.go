package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// mtlsClient builds an http client that trusts rootPool for the server and
// presents the given client certificates. Keep-alives are disabled so each
// request performs a fresh handshake (a reload must be observable on the next
// connection, not masked by a pooled one).
func mtlsClient(rootPool *x509.CertPool, certs ...tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: rootPool, Certificates: certs},
		DisableKeepAlives: true,
	}}
}

func get(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url+"/status", nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// TestReloadClientCARevokesOldCertificate pins leaf 04 Task 4: after the CA
// file is replaced with a DIFFERENT CA and ReloadClientCA runs, a certificate
// from the OLD CA is rejected at the handshake while one from the NEW CA is
// accepted — no restart. It also pins that the frontend satisfies the
// ClientCAReloader interface package main injects.
func TestReloadClientCARevokesOldCertificate(t *testing.T) {
	env := newTestEnv(t, Options{Services: Services{
		Status: func(ctx context.Context) (StatusReport, error) { return StatusReport{Version: "ok"}, nil },
	}})
	var _ ClientCAReloader = env.frontend

	cfg, err := env.frontend.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(env.frontend.Handler())
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()

	rootPool := env.caPool() // trusts CA1 (the server certificate's issuer)
	oldClient := mtlsClient(rootPool, env.clientTLS)

	// Before the reload the CA1-signed client is admitted.
	resp, err := get(oldClient, srv.URL)
	if err != nil {
		t.Fatalf("pre-reload CA1 client: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-reload status = %d, want 200", resp.StatusCode)
	}

	// Operator replaces the trusted CA file with CA2 and reloads.
	ca2 := newTestCA(t, "test-root-2")
	if err := os.WriteFile(env.caPath, ca2.pemData, 0o600); err != nil {
		t.Fatalf("rewrite CA file: %v", err)
	}
	if err := env.frontend.ReloadClientCA(); err != nil {
		t.Fatalf("ReloadClientCA: %v", err)
	}

	// The OLD-CA client is now rejected at the handshake.
	if resp, err := get(oldClient, srv.URL); err == nil {
		resp.Body.Close()
		t.Fatalf("old-CA client reached the handler after reload (status %d); want a handshake failure", resp.StatusCode)
	}

	// A NEW-CA client is admitted.
	newPEM, newKeyPEM, _ := ca2.issue(t, "tester-2", issueOptions{clientAuth: true})
	newPair, err := tls.X509KeyPair(newPEM, newKeyPEM)
	if err != nil {
		t.Fatalf("client key pair: %v", err)
	}
	resp2, err := get(mtlsClient(rootPool, newPair), srv.URL)
	if err != nil {
		t.Fatalf("post-reload CA2 client: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("post-reload new-CA status = %d, want 200", resp2.StatusCode)
	}
}

// TestReloadClientCAFailClosed pins that a bad CA file leaves the previous
// pool in place: the reload errors and the old CA client keeps working.
func TestReloadClientCAFailClosed(t *testing.T) {
	env := newTestEnv(t, Options{})

	if err := os.WriteFile(env.caPath, []byte("not a pem bundle"), 0o600); err != nil {
		t.Fatalf("corrupt CA file: %v", err)
	}
	if err := env.frontend.ReloadClientCA(); err == nil {
		t.Fatal("ReloadClientCA accepted a bundle with no certificates")
	}
	// The old pool is intact: the CA1-signed fixture still verifies against
	// the atomic pool.
	if env.frontend.currentPool() == nil {
		t.Fatal("CA pool was cleared on a failed reload")
	}
	if _, _, code := env.frontend.authenticate(env.authedRequest(http.MethodGet, "/status")); code != "" {
		t.Fatalf("old CA client rejected after a failed reload: code = %q", code)
	}
}
