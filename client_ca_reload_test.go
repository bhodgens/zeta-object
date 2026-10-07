package main

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
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
	h3 "github.com/bhodgens/zeta-object/internal/frontend/h3"
)

// client_ca_reload_test.go — the MULTI-registrant client-CA reload registry
// (the wiring gap behind bf77bef: the h3 frontend shipped an atomic CA pool
// plus ReloadClientCA, but nothing registered it, so POST /auth/reload could
// not revoke a stale device certificate on the QUIC listener).
//
// The registry shape is the load-bearing part and every test here exists to
// pin it:
//   - one reload call reaches EVERY registered listener (the single
//     func() error slot this replaced silently dropped the earlier
//     registrant, which is why the QUIC path could never rotate);
//   - a THIRD registrant displaces nobody;
//   - one failing registrant does not stop the others, and its error is
//     surfaced honestly with the frontend's name;
//   - end-to-end through the real entry point, replacing the CA file and
//     calling POST /auth/reload's service revokes a stale h3 device
//     certificate and admits a new one.
//
// The registry is process-global, so every test resets it (t.Cleanup).

// caSerial numbers the generated certificates (serial numbers must be unique
// within a bundle; two CAs sharing one would still work, but uniqueness keeps
// the fixture honest).
var caSerial atomic.Int64

// testCA is a self-signed CA that can sign leaves: the same crypto/x509-only
// discipline as the h3 package's h3TestCA fixture (no openssl dependency) and
// the same shape as frontends_tls_test.go's writeTestCAFile.
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
		SerialNumber:          big.NewInt(caSerial.Add(1)),
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
	return &testCA{cert: cert, key: key,
		pemData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issueLeaf signs a client-auth leaf under the CA. The CN is the principal
// the h3 authenticator resolves, so both device names must exist in the
// registry for the assertion to be about TRUST, not the CN lookup.
func (ca *testCA) issueLeaf(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(caSerial.Add(1)),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// caTestPKI bundles two CAs, the server pair (the CA1 CA is itself used as
// the server certificate, and the loopback client skips verification) and the
// two device certificates the rotation needs.
type caTestPKI struct {
	dir      string
	caPath   string
	certPath string
	keyPath  string
	ca2PEM   []byte
	device1  tls.Certificate // signed by CA1: the certificate a rotation revokes
	device2  tls.Certificate // signed by CA2: admitted after the rotation
}

// installCAPKITestWiring generates the PKI, installs it into the process
// config the h3 factory reads (identity registry, cert pair, dataDir with a
// real bucket directory) and returns it. Everything is restored on cleanup.
func installCAPKITestWiring(t *testing.T) *caTestPKI {
	t.Helper()
	dir := t.TempDir()
	ca1 := newTestCA(t, "client-ca-reload-root-1")
	ca2 := newTestCA(t, "client-ca-reload-root-2")

	caPath := filepath.Join(dir, "client-ca.pem")
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server-key.pem")
	if err := os.WriteFile(caPath, ca1.pemData, 0o600); err != nil {
		t.Fatalf("write client CA bundle: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(ca1.key)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	if err := os.WriteFile(certPath, ca1.pemData, 0o600); err != nil {
		t.Fatalf("write server cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write server key: %v", err)
	}

	// A real dataDir with a real bucket: the h3 frontend's wrapped webdav
	// resolves paths through getBucketPath, so the GET below must reach a
	// backend that can answer 404 for a missing object.
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "photos"), 0o755); err != nil {
		t.Fatalf("create bucket directory: %v", err)
	}
	// Both device identities exist so the assertion is about the TRUST
	// decision, not the CN lookup (an unknown CN answers 401 and would mask a
	// handshake rejection).
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "device-1", AccessKey: "device-1", SecretKey: "sk"},
		{Name: "device-2", AccessKey: "device-2", SecretKey: "sk"},
	})
	if err != nil {
		t.Fatalf("build identity registry: %v", err)
	}

	prevReg := identityRegistry
	prevCfg := *serverConfig()
	prevPath := serverConfigPath
	prevStore := configStore
	prevCreds := serverCredentials
	identityRegistry = auth.NewReloadableRegistry(reg)
	setServerConfigField(func(c *ServerConfig) { c.CertFile = certPath; c.KeyFile = keyPath })
	setServerConfigField(func(c *ServerConfig) { c.DataDir = dataDir + "/" })
	serverCredentials.AccessKeyID, serverCredentials.SecretAccessKey = "env-ak", "env-sk"
	t.Cleanup(func() {
		identityRegistry = prevReg
		setServerConfig(prevCfg)
		serverConfigPath = prevPath
		configStore = prevStore
		serverCredentials = prevCreds
	})
	return &caTestPKI{
		dir: dir, caPath: caPath, certPath: certPath, keyPath: keyPath,
		ca2PEM:  ca2.pemData,
		device1: ca1.issueLeaf(t, "device-1"),
		device2: ca2.issueLeaf(t, "device-2"),
	}
}

// buildH3FrontendThroughFactory constructs the h3 frontend the way
// frontends.go's factory entry does — the real wiring path, so this test
// cannot pass against an isolated instance the production shape never uses.
func buildH3FrontendThroughFactory(t *testing.T, pki *caTestPKI) *h3.Frontend {
	t.Helper()
	// Isolation lives HERE, on the construction helper every caller inherits:
	// the factory registers this instance's clientCAFile (a path inside this
	// test's t.TempDir()) into the PROCESS-GLOBAL reloader registry, and a
	// registrant that outlives the test is a closure over a deleted path.
	//
	// SNAPSHOT-AND-RESTORE, not reset: a caller may have registered its own
	// live registrants BEFORE calling this helper (the both-listeners fan
	// test registers admin first), and a blind reset would silently drop
	// them. Restore the pre-call registry on cleanup so only THIS
	// construction's registrant is removed.
	prev := snapshotClientCAReloaderFuncs()
	t.Cleanup(func() { restoreClientCAReloaderFuncs(prev) })
	be, err := fsbackend.New(strings.TrimSuffix(serverConfig().DataDir, "/"))
	if err != nil {
		t.Fatalf("fsbackend: %v", err)
	}
	fe, err := frontendFactories["h3"](
		FrontendConfig{
			Type:       "h3",
			ListenAddr: "127.0.0.1:0",
			Bucket:     "photos",
			Options:    map[string]string{"clientCAFile": pki.caPath},
		}, be, stubCreds{})
	if err != nil {
		t.Fatalf("h3 factory: %v", err)
	}
	f, ok := fe.(*h3.Frontend)
	if !ok {
		t.Fatalf("factory returned %T, want *h3.Frontend", fe)
	}
	return f
}

// h3DeviceClient builds an HTTP/3 client presenting a SPECIFIC device
// certificate. Every call builds its own transport, so every request performs
// a FRESH handshake — a CA reload is only observable on a new connection, and
// a pooled one would mask the very thing this test measures.
func h3DeviceClient(t *testing.T, cert tls.Certificate) *http.Client {
	t.Helper()
	tr := &http3.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // loopback test: the server pair is self-signed
		ServerName:         "localhost",
		Certificates:       []tls.Certificate{cert},
	}}
	t.Cleanup(func() { tr.Close() })
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}

// startH3QUICServer opens the frontend's UDP listener and serves it,
// returning the live address.
func startH3QUICServer(t *testing.T, f *h3.Frontend) string {
	t.Helper()
	srv, err := f.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	return fmt.Sprintf("https://%s", srv.Addr().String())
}

// TestClientCAReloaderRegistry_OneReloadReachesEveryRegistrant is the test
// whose ABSENCE let the single-slot design exist: registering the admin
// frontend and then the h3 frontend must leave BOTH registered, and one
// reload call must invoke both. Under the old single func() error slot the
// second registration replaced the first, so one listener silently stopped
// rotating (the QUIC one, always, because h3 is constructed last).
func TestClientCAReloaderRegistry_OneReloadReachesEveryRegistrant(t *testing.T) {
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)

	var ran []string
	registerClientCAReloader("admin", func() error { ran = append(ran, "admin"); return nil })
	registerClientCAReloader("h3", func() error { ran = append(ran, "h3"); return nil })

	if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"admin", "h3"}) {
		t.Fatalf("registrants = %v, want both admin and h3 (registration order preserved)", got)
	}
	if err := reloadRegisteredClientCAs(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"admin", "h3"}) {
		t.Fatalf("reloaders invoked = %v, want both admin and h3", ran)
	}
}

// TestClientCAReloaderRegistry_ThirdRegistrantDisplacesNobody pins the
// registry SHAPE: adding a further listener (a future mTLS frontend) must
// not drop either existing registrant. This is the property a fixed-size
// single slot cannot have.
func TestClientCAReloaderRegistry_ThirdRegistrantDisplacesNobody(t *testing.T) {
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)

	var ran []string
	record := func(name string) func() error {
		return func() error { ran = append(ran, name); return nil }
	}
	registerClientCAReloader("admin", record("admin"))
	registerClientCAReloader("h3", record("h3"))
	registerClientCAReloader("webdav-mtls", record("webdav-mtls"))

	if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"admin", "h3", "webdav-mtls"}) {
		t.Fatalf("registrants = %v, want all three in registration order", got)
	}
	if err := reloadRegisteredClientCAs(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"admin", "h3", "webdav-mtls"}) {
		t.Fatalf("reloaders invoked = %v, want all three (a third registrant must drop neither of the first two)", ran)
	}
}

// TestClientCAReloaderRegistry_ReregisterReplacesSameName pins the one
// overwriting case that IS correct: re-registering a frontend NAME swaps
// that listener's entry (a re-constructed frontend is the live one, and the
// superseded instance must stop receiving reloads) without growing the
// registry or touching any other registrant.
func TestClientCAReloaderRegistry_ReregisterReplacesSameName(t *testing.T) {
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)

	var ran []string
	registerClientCAReloader("admin", func() error { ran = append(ran, "admin-stale"); return nil })
	registerClientCAReloader("h3", func() error { ran = append(ran, "h3"); return nil })
	registerClientCAReloader("admin", func() error { ran = append(ran, "admin-live"); return nil })

	if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"admin", "h3"}) {
		t.Fatalf("registrants = %v, want two entries (the admin entry replaced in place)", got)
	}
	if err := reloadRegisteredClientCAs(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"admin-live", "h3"}) {
		t.Fatalf("reloaders invoked = %v, want the live admin instance and h3", ran)
	}
}

// TestClientCAReloaderRegistry_FailureDoesNotStopTheOthers pins the failure
// policy. ORDER: every registrant runs in registration order and NO failure
// short-circuits the rest, because the failure mode this fan exists to
// prevent is precisely a silent partial rotation — a broken admin bundle
// must not leave the QUIC listener trusting its stale CA for the process
// lifetime. The failures are JOINED, each naming its frontend, so the 500
// body tells the operator which listener did not rotate.
func TestClientCAReloaderRegistry_FailureDoesNotStopTheOthers(t *testing.T) {
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)

	boom := errors.New("clientCAFile contains no usable PEM certificates")
	var ran []string
	registerClientCAReloader("admin", func() error { ran = append(ran, "admin"); return boom })
	registerClientCAReloader("h3", func() error { ran = append(ran, "h3"); return nil })
	registerClientCAReloader("late", func() error { ran = append(ran, "late"); return boom })

	err := reloadRegisteredClientCAs()
	if !reflect.DeepEqual(ran, []string{"admin", "h3", "late"}) {
		t.Fatalf("reloaders invoked = %v, want all three (a failing registrant must not short-circuit the fan)", ran)
	}
	if err == nil {
		t.Fatal("reload swallowed a failing registrant's error; POST /auth/reload must report it")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying cause wrapped (errors.Is must find it)", err)
	}
	// Every failure is reported, each naming its own frontend, so an operator
	// can tell WHICH listener did not rotate.
	for _, want := range []string{"admin client CA reload", "late client CA reload", boom.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err.Error(), want)
		}
	}
	// A successful registrant contributes no error text.
	if strings.Contains(err.Error(), "h3 client CA reload") {
		t.Fatalf("error %q blames a reloader that succeeded", err.Error())
	}
}

// TestClientCAReload_NoRegistrantsIsANoOp pins the server-with-no-mTLS-
// frontend case: a reload call must not fail just because nothing is
// registered.
func TestClientCAReload_NoRegistrantsIsANoOp(t *testing.T) {
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)

	if err := reloadRegisteredClientCAs(); err != nil {
		t.Fatalf("reload with no registrants = %v, want nil (a no-op, not a failure)", err)
	}
	if got := registeredClientCAReloaders(); len(got) != 0 {
		t.Fatalf("registrants = %v, want none", got)
	}
}

// TestClientCAReloaderRegistry_NilFuncRegistersNothing keeps the nil-safety
// of the old setter: a frontend that somehow yields a nil reload entry point
// must not poison the fan with a nil call.
func TestClientCAReloaderRegistry_NilFuncRegistersNothing(t *testing.T) {
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)

	registerClientCAReloader("h3", nil)
	if got := registeredClientCAReloaders(); len(got) != 0 {
		t.Fatalf("registrants = %v, want none after a nil registration", got)
	}
	if err := reloadRegisteredClientCAs(); err != nil {
		t.Fatalf("reload = %v, want nil", err)
	}
}

// TestFactory_AdminAndH3BothRegisterForOneReload pins the WIRING itself: a
// config carrying BOTH mTLS frontends must leave both registered through the
// factory, so the production shape (not just the registry helpers) fans out.
// Without the h3 entry in frontends.go, registrants = ["admin"] only and the
// QUIC listener can never rotate.
func TestFactory_AdminAndH3BothRegisterForOneReload(t *testing.T) {
	pki := installCAPKITestWiring(t)
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)
	installTestConfigStore(t, defaultServerConfig())

	_, _, err := buildFrontends([]FrontendConfig{
		{Type: "admin", ListenAddr: "127.0.0.1:0", Options: map[string]string{"clientCAFile": pki.caPath}},
		{Type: "h3", ListenAddr: "127.0.0.1:0", Bucket: "photos", Options: map[string]string{"clientCAFile": pki.caPath}},
	}, nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("buildFrontends: %v", err)
	}
	if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"admin", "h3"}) {
		t.Fatalf("registrants = %v, want both admin and h3 (the h3 factory entry must register its reload hook)", got)
	}
}

// TestH3ClientCAReload_EndToEndRevokesStaleDeviceCert is the wire-level
// proof, driven through the REAL entry point POST /auth/reload reaches
// (adminReloadAuthService). Before the wiring gap, replacing the CA file and
// calling the reload left the QUIC listener trusting the bundle it read in
// New: the stale device certificate kept working for the process lifetime,
// and a rotation was a silent no-op exactly where revocation matters most.
func TestH3ClientCAReload_EndToEndRevokesStaleDeviceCert(t *testing.T) {
	pki := installCAPKITestWiring(t)
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)

	// A real config file so the identity half of the reload runs the startup
	// sequence and succeeds (the CA half is what this test measures). It MUST
	// carry the device identities: the reload rebuilds the identity registry
	// from this file, so identities absent here would make device-2 an unknown
	// CN and answer 401 — which would mask the trust assertion.
	cfgPath := filepath.Join(pki.dir, "config.json")
	cfg := defaultServerConfig()
	cfg.DataDir = strings.TrimSuffix(serverConfig().DataDir, "/")
	cfg.Identities = []auth.IdentityConfig{
		{Name: "device-1", AccessKey: "device-1", SecretKey: "sk"},
		{Name: "device-2", AccessKey: "device-2", SecretKey: "sk"},
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, doc, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	serverConfigPath = cfgPath
	installTestConfigStore(t, defaultServerConfig())

	f := buildH3FrontendThroughFactory(t, pki)
	base := startH3QUICServer(t, f)

	// Pre-rotation: the CA1-signed device certificate is trusted and the
	// request reaches the handler (404 for a missing object).
	resp, err := h3DeviceClient(t, pki.device1).Get(base + "/probe.txt")
	if err != nil {
		t.Fatalf("pre-reload handshake failed: %v", err)
	}
	resp.Body.Close()

	// The operator replaces the trusted CA bundle and calls POST /auth/reload.
	if err := os.WriteFile(pki.caPath, pki.ca2PEM, 0o600); err != nil {
		t.Fatalf("replace client CA file: %v", err)
	}
	if err := adminReloadAuthService(context.Background()); err != nil {
		t.Fatalf("ReloadAuth: %v", err)
	}

	// The STALE device certificate is now rejected at the handshake: over h3
	// there is no pre-handshake 401, so the rejection is a CRYPTO_ERROR
	// transport error and never an HTTP status.
	resp2, err := h3DeviceClient(t, pki.device1).Get(base + "/probe.txt")
	if err == nil {
		resp2.Body.Close()
		t.Fatalf("stale-CA device certificate still reached the handler after /auth/reload (status %d); the QUIC listener never reloaded its CA", resp2.StatusCode)
	}
	var terr *quic.TransportError
	if !errors.As(err, &terr) || !terr.ErrorCode.IsCryptoError() {
		t.Fatalf("stale-client error = %T (%v), want a crypto TransportError (handshake rejection)", err, err)
	}

	// A certificate signed by the NEW CA is admitted: the handshake succeeds
	// and the request authenticates.
	resp3, err := h3DeviceClient(t, pki.device2).Get(base + "/probe.txt")
	if err != nil {
		t.Fatalf("new-CA device certificate rejected after rotation: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode == http.StatusUnauthorized || resp3.StatusCode == http.StatusForbidden {
		t.Fatalf("new-CA client status = %d, want the request authenticated (a 401/403 means the new CA was not trusted)", resp3.StatusCode)
	}
}

// TestH3ClientCAReload_AdminAndH3PoolsBothSwapOnOneReload pins the fan with
// REAL frontends on both sides: one reload call through the service must
// swap BOTH the admin listener's pool and the QUIC listener's pool. A pool
// that is silently left on the old bundle is the defect; a reloader that
// fails must be reported.
func TestH3ClientCAReload_AdminAndH3PoolsBothSwapOnOneReload(t *testing.T) {
	pki := installCAPKITestWiring(t)
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)
	installTestConfigStore(t, defaultServerConfig())

	// Both frontends built through their factory entries, so both register.
	adminCA := filepath.Join(pki.dir, "admin-ca.pem")
	if err := os.WriteFile(adminCA, newTestCA(t, "admin-ca-root-1").pemData, 0o600); err != nil {
		t.Fatalf("write admin CA: %v", err)
	}
	if _, err := frontendFactories["admin"](
		FrontendConfig{Type: "admin", ListenAddr: "127.0.0.1:0", Options: map[string]string{"clientCAFile": adminCA}},
		nilBackend{}, stubCreds{}); err != nil {
		t.Fatalf("admin factory: %v", err)
	}
	buildH3FrontendThroughFactory(t, pki)

	if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"admin", "h3"}) {
		t.Fatalf("registrants = %v, want both listeners registered", got)
	}
	// Replace BOTH bundles, then reload once.
	newAdminCA := newTestCA(t, "admin-ca-root-2")
	if err := os.WriteFile(adminCA, newAdminCA.pemData, 0o600); err != nil {
		t.Fatalf("replace admin CA: %v", err)
	}
	if err := os.WriteFile(pki.caPath, pki.ca2PEM, 0o600); err != nil {
		t.Fatalf("replace client CA: %v", err)
	}
	if err := reloadRegisteredClientCAs(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// The old bundles are now rejected by BOTH loaders: re-reading them
	// would succeed (the files exist and parse), so instead pin the fail-
	// closed property that proves the pool actually swapped — corrupting a
	// file AFTER a successful reload leaves the rotated pool serving, and a
	// reload of the corrupt file errors while the previous pool is intact.
	if err := os.WriteFile(pki.caPath, []byte("not a pem bundle"), 0o600); err != nil {
		t.Fatalf("corrupt client CA: %v", err)
	}
	if err := os.WriteFile(adminCA, []byte("not a pem bundle"), 0o600); err != nil {
		t.Fatalf("corrupt admin CA: %v", err)
	}
	err := reloadRegisteredClientCAs()
	if err == nil {
		t.Fatal("reload accepted a bundle with no certificates on both listeners")
	}
	for _, want := range []string{"admin client CA reload", "h3 client CA reload"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %q so the operator knows which listener did not rotate", err.Error(), want)
		}
	}
}

// TestReloadAuthService_IdentityFailureStillRotatesTheCA pins the order
// inside the service: the identity reload runs first, but its failure must
// not skip the CA fan. An operator who just replaced a CA file gets that
// rotation applied even while an unrelated config edit is invalid; the
// identity failure is still reported.
func TestReloadAuthService_IdentityFailureStillRotatesTheCA(t *testing.T) {
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)
	installTestConfigStore(t, defaultServerConfig())

	// A config file with an invalid value makes the identity half fail
	// (fail-closed: the previous registry keeps serving).
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"zfs_versioning":"nonsense"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	prevPath, prevReg, prevCfg := serverConfigPath, identityRegistry, *serverConfig()
	t.Cleanup(func() {
		serverConfigPath, identityRegistry = prevPath, prevReg
		setServerConfig(prevCfg)
	})
	serverConfigPath = cfgPath
	reg, err := auth.NewMultiRegistry(nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(reg)
	setServerConfig(defaultServerConfig())

	caCalled := false
	registerClientCAReloader("h3", func() error { caCalled = true; return nil })

	err = adminReloadAuthService(context.Background())
	if err == nil {
		t.Fatal("ReloadAuth reported success despite an invalid config (the identity reload must fail loudly)")
	}
	svcErr, ok := errors.AsType[*admin.ServiceError](err)
	if !ok || svcErr.Status != 500 || svcErr.Code != "ReloadFailed" {
		t.Fatalf("err = %v, want a 500 ReloadFailed ServiceError", err)
	}
	if !strings.Contains(svcErr.Message, "zfs_versioning") {
		t.Fatalf("message %q must carry the validator's named-offender error", svcErr.Message)
	}
	if !caCalled {
		t.Fatal("an identity-reload failure skipped the client-CA fan; a rotation the operator just performed must not be silently dropped")
	}
}
