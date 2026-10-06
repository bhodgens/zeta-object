package h3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// serve_test.go — the REAL loopback integration (leaf 02 Task 4): start the
// h3 frontend on 127.0.0.1:0 with an in-memory stub backend, speak HTTP/3
// with the quic-go CLIENT (http3.Transport), and pin:
//   - a PUT then GET round-trips the bytes over QUIC;
//   - a client with NO certificate fails the HANDSHAKE (a connection error,
//     never an HTTP status — the h3 wire has no pre-handshake 401);
//   - the handshake-failure error shape for leaf 03/04 probes: the client's
//     RoundTrip returns an error wrapping *quic.TransportError whose
//     ErrorCode.IsCryptoError() is true (the server's "certificate required"
//     TLS alert, 0x174 = CRYPTO_ERROR + tls.AlertBadCertificate family).
//   - a Range GET returns 206 + Content-Range (leaf 01's Range semantics
//     are in the wrapped webdav's get.go; inherited by wrapping).
//
// NOTE FOR THE ORCHESTRATOR: the Range-GET expectation below pins 206
// because leaf 01's webdav Range handling IS present in this worktree
// (internal/frontend/webdav/get.go carries the leaf 01 Range code).
//
// h3Client builds an http.Client speaking HTTP/3 to addr, optionally
// presenting a client certificate.
func h3Client(t *testing.T, env *h3TestEnv, addr string, withCert bool) *http.Client {
	t.Helper()
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // loopback test: server cert is self-signed
		ServerName:         "localhost",
	}
	if withCert {
		tlsCfg.Certificates = []tls.Certificate{env.clientTLS}
	}
	tr := &http3.Transport{TLSClientConfig: tlsCfg}
	t.Cleanup(func() { tr.Close() })
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}

func TestServe_PutGetRoundTrip(t *testing.T) {
	env := newH3TestEnv(t)
	be := newH3MemBackend()
	f, err := New(be, env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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

	client := h3Client(t, env, srv.Addr().String(), true)
	base := fmt.Sprintf("https://%s", srv.Addr().String())

	// PUT then GET round-trip over HTTP/3.
	body := []byte("h3 round-trip payload 0123456789")
	putReq, err := http.NewRequest(http.MethodPut, base+"/h3-putget.txt", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	putReq.ContentLength = int64(len(body))
	putResp, err := client.Do(putReq)
	if err != nil {
		t.Fatalf("PUT over h3: %v", err)
	}
	io.Copy(io.Discard, putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusCreated && putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 201/200", putResp.StatusCode)
	}

	getResp, err := client.Get(base + "/h3-putget.txt")
	if err != nil {
		t.Fatalf("GET over h3: %v", err)
	}
	defer getResp.Body.Close()
	got, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("read GET body: %v", err)
	}
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip bytes = %q, want %q", got, body)
	}

	// Range GET: leaf 01's Range semantics HAVE landed in the tree (the
	// wrapped webdav's get.go carries the leaf 01 Range handling and
	// answers a satisfiable single-span request with 206). Pin 206; the
	// Content-Range grammar is leaf 01's own test's property.
	rangeReq, err := http.NewRequest(http.MethodGet, base+"/h3-putget.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	rangeReq.Header.Set("Range", "bytes=0-3")
	rangeResp, err := client.Do(rangeReq)
	if err != nil {
		t.Fatalf("Range GET over h3: %v", err)
	}
	rangeBody, err := io.ReadAll(rangeResp.Body)
	if err != nil {
		t.Fatalf("read Range GET body: %v", err)
	}
	rangeResp.Body.Close()
	// Leaf 01's semantics land in the tree: expect 206 + Content-Range.
	if rangeResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range GET status = %d, want 206 (leaf 01 Range semantics in webdav/get.go)", rangeResp.StatusCode)
	}
	if cr := rangeResp.Header.Get("Content-Range"); cr == "" {
		t.Fatal("Range GET must carry Content-Range (leaf 01 semantics)")
	}
	if !bytes.Equal(rangeBody, body[:4]) {
		t.Fatalf("Range GET body = %q, want %q", rangeBody, body[:4])
	}
}

// TestServe_NoClientCertFailsHandshake pins the mTLS posture end-to-end: a
// client presenting NO certificate is rejected at the TLS/QUIC handshake —
// the failure is a connection error, not an HTTP status.
func TestServe_NoClientCertFailsHandshake(t *testing.T) {
	env := newH3TestEnv(t)
	be := newH3MemBackend()
	f, err := New(be, env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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

	client := h3Client(t, env, srv.Addr().String(), false)
	resp, err := client.Get(fmt.Sprintf("https://%s/nope.txt", srv.Addr().String()))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("no-cert GET succeeded with status %d; the handshake must fail instead", resp.StatusCode)
	}
	// Error shape for leaf 03/04 probes: the RoundTrip error wraps a
	// *quic.TransportError with a CRYPTO_ERROR code (the server's TLS
	// alert). A wrong-CA certificate fails the same way (tls alert from
	// the handshake), so this pins the whole reject-at-handshake class.
	var terr *quic.TransportError
	if !errors.As(err, &terr) {
		t.Fatalf("handshake error type = %T (%v), want *quic.TransportError", err, err)
	}
	if !terr.ErrorCode.IsCryptoError() {
		t.Fatalf("error code = %v, want a CRYPTO_ERROR (TLS alert)", terr.ErrorCode)
	}
}

// TestServe_WrongCACertFailsHandshake pins that a certificate signed by an
// untrusted CA is rejected by the handshake (never reaching the CN lookup).
func TestServe_WrongCACertFailsHandshake(t *testing.T) {
	env := newH3TestEnv(t)
	rogue := newH3TestCA(t, "rogue-root")
	rogueTLS, _, _ := rogue.issueLeaf(t, "device-1", true)

	be := newH3MemBackend()
	f, err := New(be, env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{rogueTLS},
	}
	tr := &http3.Transport{TLSClientConfig: tlsCfg}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://%s/nope.txt", srv.Addr().String()))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("wrong-CA cert accepted with status %d; the handshake must fail", resp.StatusCode)
	}
	var terr *quic.TransportError
	if !errors.As(err, &terr) || !terr.ErrorCode.IsCryptoError() {
		t.Fatalf("handshake error = %T (%v), want crypto TransportError", err, err)
	}
}

// TestListen_BadAddrFailsLoud pins a bind failure surfacing named. A
// syntactically invalid host:port fails at ListenUDP; a well-formed but
// unroutable address fails at bind too.
func TestListen_BadAddrFailsLoud(t *testing.T) {
	env := newH3TestEnv(t)
	for _, bad := range []string{"this is not an addr", "256.256.256.256:1"} {
		cfg := env.validConfig()
		cfg.ListenAddr = bad
		f, err := New(newH3MemBackend(), cfg, env.registry, nil)
		if err != nil {
			t.Fatalf("New (%s): %v", bad, err)
		}
		if _, err := f.Listen(); err == nil {
			t.Fatalf("Listen on %q must fail loudly", bad)
		}
	}
}

// h3ClientWithCert builds an http.Client speaking HTTP/3 to addr with a
// SPECIFIC client certificate. Each call builds its own http3.Transport, so
// every client performs a fresh handshake (a CA reload is only observable on
// a new connection — a pooled one would mask it).
func h3ClientWithCert(t *testing.T, cert tls.Certificate) *http.Client {
	t.Helper()
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // loopback test: server cert is self-signed
		ServerName:         "localhost",
		Certificates:       []tls.Certificate{cert},
	}
	tr := &http3.Transport{TLSClientConfig: tlsCfg}
	t.Cleanup(func() { tr.Close() })
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}

// TestReloadClientCA_RevokesOldCertificate pins the h3 client-CA reload: a
// certificate signed by the OLD CA is rejected at the handshake after the CA
// file is replaced and ReloadClientCA runs, while one signed by the NEW CA is
// admitted — no restart. Without the atomic pool + GetConfigForClient hook
// the listener keeps trusting the bundle it read in New, so an operator who
// replaces clientCAFile to revoke a stolen device certificate gets a silent
// no-op (and POST /auth/reload cannot revoke anything over QUIC).
func TestReloadClientCA_RevokesOldCertificate(t *testing.T) {
	env := newH3TestEnv(t)
	// Both principals exist so the assertion is about the TRUST decision,
	// not about the CN lookup (an unknown CN answers 401, which would mask
	// a handshake rejection).
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "device-1", AccessKey: "device-1", SecretKey: "sk"},
		{Name: "device-2", AccessKey: "device-2", SecretKey: "sk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(newH3MemBackend(), env.validConfig(), reg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var _ interface{ ReloadClientCA() error } = f
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
	base := fmt.Sprintf("https://%s", srv.Addr().String())

	// Pre-reload: the CA1-signed device certificate is admitted (the
	// handshake succeeds; the object simply does not exist yet).
	resp, err := h3ClientWithCert(t, env.clientTLS).Get(base + "/probe.txt")
	if err != nil {
		t.Fatalf("pre-reload CA1 client handshake failed: %v", err)
	}
	resp.Body.Close()

	// Operator replaces the trusted CA file with a DIFFERENT CA and reloads.
	ca2 := newH3TestCA(t, "h3-test-root-2")
	if err := os.WriteFile(env.caPath, ca2.pemData, 0o600); err != nil {
		t.Fatalf("replace CA file: %v", err)
	}
	if err := f.ReloadClientCA(); err != nil {
		t.Fatalf("ReloadClientCA: %v", err)
	}

	// The OLD-CA client is now rejected at the handshake.
	resp2, err := h3ClientWithCert(t, env.clientTLS).Get(base + "/probe.txt")
	if err == nil {
		resp2.Body.Close()
		t.Fatalf("old-CA client reached the handler after reload (status %d); the handshake must fail", resp2.StatusCode)
	}
	var terr *quic.TransportError
	if !errors.As(err, &terr) || !terr.ErrorCode.IsCryptoError() {
		t.Fatalf("post-reload old-CA client error = %T (%v), want a crypto TransportError (handshake rejection)", err, err)
	}

	// A NEW-CA client is admitted: the handshake succeeds and the request
	// reaches the handler (404 for a missing object, never a crypto error).
	newClientTLS, _, _ := ca2.issueLeaf(t, "device-2", true)
	resp3, err := h3ClientWithCert(t, newClientTLS).Get(base + "/probe.txt")
	if err != nil {
		t.Fatalf("post-reload new-CA client handshake failed: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode == http.StatusUnauthorized || resp3.StatusCode == http.StatusForbidden {
		t.Fatalf("post-reload new-CA client status = %d, want the request authenticated (a 401/403 means the new CA was not trusted)", resp3.StatusCode)
	}
}

// TestReloadClientCA_FailClosed pins that a corrupt CA bundle leaves the
// previous pool serving: the reload errors and the previous CA still
// authenticates (never a pool cleared to "trust nothing").
func TestReloadClientCA_FailClosed(t *testing.T) {
	env := newH3TestEnv(t)
	f, err := New(newH3MemBackend(), env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := os.WriteFile(env.caPath, []byte("not a pem bundle"), 0o600); err != nil {
		t.Fatalf("corrupt CA file: %v", err)
	}
	if err := f.ReloadClientCA(); err == nil {
		t.Fatal("ReloadClientCA accepted a bundle with no certificates")
	}
	// The old pool is intact: the CA1-signed fixture still verifies.
	if f.currentPool() == nil {
		t.Fatal("CA pool was cleared on a failed reload")
	}
	srv, err := f.Listen()
	if err != nil {
		t.Fatalf("Listen after a failed reload: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	base := fmt.Sprintf("https://%s", srv.Addr().String())
	resp, err := h3ClientWithCert(t, env.clientTLS).Get(base + "/probe.txt")
	if err != nil {
		t.Fatalf("old-CA client rejected after a FAILED reload: %v (the previous pool must keep serving)", err)
	}
	resp.Body.Close()
}

// TestShutdown_DrainsInflightResponse pins the drain ORDER (L7): the UDP
// socket and QUIC listener must stay open while in-flight requests finish, so
// a large streaming GET arrives WHOLE even though shutdown was requested
// mid-body. Tearing the socket down first (the pre-fix shape) destroys the
// QUIC transport underneath the live stream and the client sees a truncated
// body — an s3 write that had already drained cleanly is lost.
func TestShutdown_DrainsInflightResponse(t *testing.T) {
	env := newH3TestEnv(t)
	be := newH3MemBackend()
	f, err := New(be, env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Seed a body far larger than one QUIC flow-control window so the
	// response is genuinely mid-flight when the drain starts.
	const size = 2 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if _, err := be.Put(context.Background(), "photos", "big.bin", bytes.NewReader(payload), size, objectmodel.PutOptions{}); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	srv, err := f.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	t.Cleanup(func() {
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})

	client := h3Client(t, env, srv.Addr().String(), true)
	resp, err := client.Get(fmt.Sprintf("https://%s/big.bin", srv.Addr().String()))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}

	// Read SLOWLY in the background: the server blocks mid-body on this
	// reader, which is exactly the in-flight request the drain must respect.
	// The chunk/sleep cadence keeps the body in flight for seconds, so the
	// drain genuinely has to wait rather than finding an already-finished
	// response.
	const chunk = 16 << 10
	const perChunk = 25 * time.Millisecond
	// got is written ONLY by the reader goroutine and read only after
	// readDone, so it needs no lock; the main goroutine polls gotN (an
	// atomic length mirror) to know when the response is in flight.
	var got bytes.Buffer
	var gotN atomic.Int64
	readDone := make(chan error, 1)
	var eofReached atomic.Bool
	go func() {
		buf := make([]byte, chunk)
		for {
			n, err := io.ReadFull(resp.Body, buf)
			got.Write(buf[:n])
			read := gotN.Add(int64(n))
			switch {
			case err == nil:
				time.Sleep(perChunk)
			case errors.Is(err, io.EOF) && read == size:
				eofReached.Store(true)
				readDone <- nil
				return
			default:
				// A short read against a Content-Length body (or any
				// stream error) IS the truncation this test catches.
				readDone <- fmt.Errorf("after %d/%d bytes: %w", read, size, err)
				return
			}
		}
	}()

	// Wait for the response to be in flight (headers + first bytes landed).
	deadline := time.Now().Add(10 * time.Second)
	for gotN.Load() < chunk {
		if time.Now().After(deadline) {
			t.Fatal("the streaming GET never delivered its first chunk")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if eofReached.Load() {
		t.Fatal("the client finished reading before shutdown: the fixture no longer exercises an in-flight drain")
	}

	// Drain with a real budget, in the background so the test can observe
	// that the client read completes INSIDE the drain.
	shutdownErr := make(chan error, 1)
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	go func() { shutdownErr <- srv.Shutdown(drainCtx) }()

	select {
	case err := <-shutdownErr:
		if err != nil {
			t.Fatalf("Shutdown within a live drain budget = %v, want nil (an in-flight response must finish, not be cut)", err)
		}
		t.Logf("drain finished in %s", time.Since(start).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("Shutdown blocked forever; it must respect the caller's drain context")
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("streaming GET body read failed during the drain: %v (the response was truncated)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the in-flight response never completed after the drain")
	}
	if got.Len() != size {
		t.Fatalf("body bytes = %d, want %d (the response must arrive whole)", got.Len(), size)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatal("body content differs from the seeded object")
	}
	// The UDP socket and the QUIC listener are closed once the drain is over.
	if err := srv.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Logf("post-drain Close: %v", err)
	}
}

// TestShutdown_RespectsCancelledDrainContext pins the drain BUDGET (L7): a
// caller whose budget is already spent must get its error back immediately
// rather than waiting on a peer that never stops sending. The pre-fix shape
// passed context.Background() into the shutdown and then waited on an
// unbounded channel receive, so one busy client kept the whole
// listener-drain fan (and main's wg.Wait) alive until systemd SIGKILLed the
// process.
func TestShutdown_RespectsCancelledDrainContext(t *testing.T) {
	env := newH3TestEnv(t)
	be := newH3MemBackend()
	f, err := New(be, env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const size = 2 << 20
	payload := make([]byte, size)
	if _, err := be.Put(context.Background(), "photos", "big.bin", bytes.NewReader(payload), size, objectmodel.PutOptions{}); err != nil {
		t.Fatalf("seed object: %v", err)
	}
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

	// A client that opens the response and then stops reading entirely: the
	// request stays in flight no matter how long we wait.
	resp, err := h3Client(t, env, srv.Addr().String(), true).Get(fmt.Sprintf("https://%s/big.bin", srv.Addr().String()))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}

	// The caller's budget is ALREADY spent.
	drainCtx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- srv.Shutdown(drainCtx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown with a spent budget = %v, want a context error (deadline exceeded / canceled)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown ignored the cancelled drain context and blocked forever")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s with an already-cancelled budget; it must return immediately", elapsed)
	}
}

// TestClose_IdempotentEnough pins Close twice not panicking (the drain fan
// and a test cleanup may both call it).
func TestClose_SecondCloseErrOK(t *testing.T) {
	env := newH3TestEnv(t)
	f, err := New(newH3MemBackend(), env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv, err := f.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// A second Close reports an error (listener already closed) but must
	// not panic or hang.
	done := make(chan error, 1)
	go func() { done <- srv.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second Close hung")
	}
	_ = context.Background()
}
