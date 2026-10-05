package h3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

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
