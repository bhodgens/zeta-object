package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// In-test PKI: every certificate is generated with crypto/x509 so this
// package's tests carry no openssl dependency. The CA pool, the server pair
// and each client pair are all minted here.

var testSerial atomic.Int64

func nextSerial() *big.Int { return big.NewInt(testSerial.Add(1)) }

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	pool *x509.CertPool
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
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pool: pool,
	}
}

// issue signs a leaf certificate with the CA. clientAuth selects the client
// EKU; the server form carries loopback SANs so a real handshake against
// 127.0.0.1 verifies.
func (ca *testCA) issue(t *testing.T, cn string, clientAuth bool) (certPEM, keyPEM []byte, cert tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if clientAuth {
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
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("assemble leaf pair: %v", err)
	}
	return certPEM, keyPEM, pair
}

func writeTemp(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// startMutualTLSServer starts an httptest TLS server that REQUIRES and
// VERIFIES a client certificate against ca.
func startMutualTLSServer(t *testing.T, ca *testCA, serverPair tls.Certificate, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.pool,
		Certificates: []tls.Certificate{serverPair},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// clientFiles writes a client pair plus the CA bundle into dir and returns
// the three paths for a Config.
func clientFiles(t *testing.T, dir string, ca *testCA, clientPair []byte, keyPEM []byte) (caFile, certFile, keyFile string) {
	t.Helper()
	caFile = writeTemp(t, dir, "ca.pem", ca.pem)
	certFile = writeTemp(t, dir, "client.pem", clientPair)
	keyFile = writeTemp(t, dir, "client-key.pem", keyPEM)
	return
}

// cnHandler answers with the presented client certificate's common name.
func cnHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"cn": cn})
	})
}

// --- Task 1: mTLS transport and error mapping -----------------------------

func TestMutualTLSHandshakeSucceeds(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	srv := startMutualTLSServer(t, ca, serverPair, cnHandler())

	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body, err := c.Status(context.Background(), nil)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if got["cn"] != "console" {
		t.Fatalf("server saw client CN %q, want %q", got["cn"], "console")
	}
}

func TestClientWithoutCertificateIsRefused(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	caFile := writeTemp(t, dir, "ca.pem", ca.pem)

	srv := startMutualTLSServer(t, ca, serverPair, cnHandler())

	// No client pair configured: the transport MUST NOT present one, so a
	// server that requires a client certificate rejects the handshake.
	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Status(context.Background(), nil); err == nil {
		t.Fatal("expected a transport failure for a client without a certificate, got nil")
	} else if _, ok := errors.AsType[*TransportError](err); !ok {
		t.Fatalf("error is %T (%v), want *TransportError", err, err)
	}
}

func TestErrorEnvelopeRoundTrips(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	const envelope = `{"error":{"code":"DatasetBucketNotDeletable","message":"bucket is backed by a dataset and cannot be deleted"}}`
	srv := startMutualTLSServer(t, ca, serverPair, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, envelope)
	}))

	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.DeleteBucket(context.Background(), "b", nil)
	he, ok := errors.AsType[*HTTPError](err)
	if !ok {
		t.Fatalf("error is %T (%v), want *HTTPError", err, err)
	}
	if he.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", he.Status)
	}
	if he.Code != "DatasetBucketNotDeletable" {
		t.Errorf("code = %q, want DatasetBucketNotDeletable", he.Code)
	}
	if he.Message != "bucket is backed by a dataset and cannot be deleted" {
		t.Errorf("message = %q, want the gateway's message verbatim", he.Message)
	}
}

func TestUnathorizedAndServerErrorMapToTypedError(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"code":"Unauthorized","message":"client certificate authentication required"}}`},
		{"internal", http.StatusInternalServerError, `{"error":{"code":"InternalError","message":"status failed"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startMutualTLSServer(t, ca, serverPair, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Status(context.Background(), nil)
			he, ok := errors.AsType[*HTTPError](err)
			if !ok {
				t.Fatalf("error is %T (%v), want *HTTPError", err, err)
			}
			if he.Status != tc.status {
				t.Errorf("status = %d, want %d", he.Status, tc.status)
			}
		})
	}
}

func TestTimeoutIsTransportErrorNamingURL(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	srv := startMutualTLSServer(t, ca, serverPair, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	saved := requestTimeout
	requestTimeout = 40 * time.Millisecond
	t.Cleanup(func() { requestTimeout = saved })

	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Status(context.Background(), nil)
	te, ok := errors.AsType[*TransportError](err)
	if !ok {
		t.Fatalf("error is %T (%v), want *TransportError", err, err)
	}
	if te.URL != srv.URL+"/status" {
		t.Errorf("transport error URL = %q, want %q", te.URL, srv.URL+"/status")
	}
	if !strings.Contains(te.Reason, "timeout") {
		t.Errorf("transport reason = %q, want it to name a timeout", te.Reason)
	}
}

// --- Task 2: the route methods --------------------------------------------

type recordedRequest struct {
	method string
	path   string
	query  string
	body   string
}

// recordingServer answers every request with fixtureBody at status, recording
// what the client actually sent.
func recordingServer(t *testing.T, ca *testCA, serverPair tls.Certificate, rec *recordedRequest, status int, fixtureBody string) *httptest.Server {
	t.Helper()
	return startMutualTLSServer(t, ca, serverPair, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, fixtureBody)
	}))
}

func TestRouteMethodsSendMethodPathQueryBody(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	const fixture = `{"ok":true,"shape":"gateway"}`

	cases := []struct {
		name       string
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   string
		invoke     func(c *Client, q url.Values) (json.RawMessage, error)
	}{
		{"status", "GET", "/status", "", "", func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.Status(context.Background(), q)
		}},
		{"getConfig", "GET", "/config", "", "", func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.GetConfig(context.Background(), q)
		}},
		{"putConfig", "PUT", "/config", "", `{"logLevel":"debug"}`, func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.PutConfig(context.Background(), json.RawMessage(`{"logLevel":"debug"}`), q)
		}},
		{"saveConfig", "POST", "/config/save", "", "", func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.SaveConfig(context.Background(), q)
		}},
		{"reloadAuth", "POST", "/auth/reload", "", "", func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.ReloadAuth(context.Background(), q)
		}},
		{"listBuckets", "GET", "/buckets", "a=b&c=d", "", func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.ListBuckets(context.Background(), q)
		}},
		{"createBucket", "POST", "/buckets", "", `{"name":"my-bucket"}`, func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.CreateBucket(context.Background(), json.RawMessage(`{"name":"my-bucket"}`), q)
		}},
		{"getBucket", "GET", "/buckets/my-bucket", "", "", func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.GetBucket(context.Background(), "my-bucket", q)
		}},
		{"deleteBucket", "DELETE", "/buckets/my-bucket", "", "", func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.DeleteBucket(context.Background(), "my-bucket", q)
		}},
		{"putBucketSettings", "PUT", "/buckets/my-bucket/settings", "", `{"auditReads":true}`, func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.PutBucketSettings(context.Background(), "my-bucket", json.RawMessage(`{"auditReads":true}`), q)
		}},
		{"purge", "POST", "/purge", "", `{"dataset":"pool/ds"}`, func(c *Client, q url.Values) (json.RawMessage, error) {
			return c.Purge(context.Background(), json.RawMessage(`{"dataset":"pool/ds"}`), q)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec recordedRequest
			srv := recordingServer(t, ca, serverPair, &rec, http.StatusOK, fixture)
			c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			var q url.Values
			if tc.wantQuery != "" {
				q, err = url.ParseQuery(tc.wantQuery)
				if err != nil {
					t.Fatalf("ParseQuery: %v", err)
				}
			}
			body, err := tc.invoke(c, q)
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if rec.method != tc.wantMethod {
				t.Errorf("method = %q, want %q", rec.method, tc.wantMethod)
			}
			if rec.path != tc.wantPath {
				t.Errorf("path = %q, want %q", rec.path, tc.wantPath)
			}
			if rec.query != tc.wantQuery {
				t.Errorf("query = %q, want %q", rec.query, tc.wantQuery)
			}
			if rec.body != tc.wantBody {
				t.Errorf("body = %q, want %q", rec.body, tc.wantBody)
			}
			if string(body) != fixture {
				t.Errorf("decoded body = %q, want %q", body, fixture)
			}
		})
	}
}

func TestPassThroughErrorCodesSurvive(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	cases := []struct {
		name    string
		status  int
		code    string
		message string
		invoke  func(c *Client) (json.RawMessage, error)
	}{
		{
			name:    "bucket delete refusal",
			status:  http.StatusConflict,
			code:    "DatasetBucketNotDeletable",
			message: "bucket is backed by a dataset and cannot be deleted",
			invoke: func(c *Client) (json.RawMessage, error) {
				return c.DeleteBucket(context.Background(), "dsb", nil)
			},
		},
		{
			name:    "config validation",
			status:  http.StatusBadRequest,
			code:    "InvalidArgument",
			message: "tlsMinVersion: unknown value",
			invoke: func(c *Client) (json.RawMessage, error) {
				return c.PutConfig(context.Background(), json.RawMessage(`{"tlsMinVersion":"x"}`), nil)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": tc.code, "message": tc.message}})
			var rec recordedRequest
			srv := recordingServer(t, ca, serverPair, &rec, tc.status, string(body))
			c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = tc.invoke(c)
			he, ok := errors.AsType[*HTTPError](err)
			if !ok {
				t.Fatalf("error is %T (%v), want *HTTPError", err, err)
			}
			if he.Code != tc.code || he.Message != tc.message {
				t.Errorf("(code,message) = (%q,%q), want (%q,%q)", he.Code, he.Message, tc.code, tc.message)
			}
		})
	}
}

// --- Task 3: certificate reload -------------------------------------------

func TestReloadUsesNewCertificate(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	alphaCert, alphaKey, _ := ca.issue(t, "alpha", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, alphaCert, alphaKey)

	srv := startMutualTLSServer(t, ca, serverPair, cnHandler())
	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	assertCN := func(want string) {
		t.Helper()
		body, err := c.Status(context.Background(), nil)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		var got map[string]string
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got["cn"] != want {
			t.Fatalf("server saw CN %q, want %q", got["cn"], want)
		}
	}

	assertCN("alpha")

	// Replace the files on disk with a new pair signed by the same CA.
	betaCert, betaKey, _ := ca.issue(t, "beta", true)
	if err := os.WriteFile(certFile, betaCert, 0o600); err != nil {
		t.Fatalf("overwrite cert: %v", err)
	}
	if err := os.WriteFile(keyFile, betaKey, 0o600); err != nil {
		t.Fatalf("overwrite key: %v", err)
	}
	if err := c.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertCN("beta")
}

func TestReloadUnreadableKeepsPreviousCertificate(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	alphaCert, alphaKey, _ := ca.issue(t, "alpha", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, alphaCert, alphaKey)

	srv := startMutualTLSServer(t, ca, serverPair, cnHandler())
	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Corrupt the certificate file: the reload must fail AND the previous
	// certificate must keep working.
	if err := os.WriteFile(certFile, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	if err := c.Reload(); err == nil {
		t.Fatal("Reload with an unreadable certificate: expected an error, got nil")
	}

	body, err := c.Status(context.Background(), nil)
	if err != nil {
		t.Fatalf("Status after failed reload: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["cn"] != "alpha" {
		t.Fatalf("server saw CN %q, want alpha (previous certificate must keep working)", got["cn"])
	}
}
