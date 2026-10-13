package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- error types -------------------------------------------------------------

func TestHTTPErrorMessageFormatting(t *testing.T) {
	e := &HTTPError{Status: 409, Code: "DatasetBucketNotDeletable", Message: "nope"}
	want := "gateway: 409 DatasetBucketNotDeletable: nope"
	if got := e.Error(); got != want {
		t.Errorf("HTTPError.Error() = %q, want %q", got, want)
	}
}

func TestTransportErrorMessageAndUnwrap(t *testing.T) {
	underlying := errors.New("dial refused")
	e := &TransportError{URL: "https://127.0.0.1:9708/status", Reason: "timeout: too slow", Err: underlying}
	want := "gateway: transport to https://127.0.0.1:9708/status failed: timeout: too slow"
	if got := e.Error(); got != want {
		t.Errorf("TransportError.Error() = %q, want %q", got, want)
	}
	if got := e.Unwrap(); !errors.Is(got, underlying) {
		t.Errorf("Unwrap() = %v, want the underlying error", got)
	}
	if !errors.Is(e, underlying) {
		t.Errorf("errors.Is(TransportError, underlying) = false, want true")
	}
}

// gatewayHTTPError maps a non-2xx body onto an *HTTPError. The envelope form
// round-trips the gateway's code and message verbatim; a plain-text body (or an
// empty body) becomes the message so nothing is silently dropped.
func TestGatewayHTTPErrorBodyMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantMsg    string
		wantString string
	}{
		{
			name:       "envelope round-trips",
			status:     409,
			body:       `{"error":{"code":"Nope","message":"bucket is locked"}}`,
			wantCode:   "Nope",
			wantMsg:    "bucket is locked",
			wantString: "gateway: 409 Nope: bucket is locked",
		},
		{
			name:       "message-only envelope is honored",
			status:     502,
			body:       `{"error":{"message":"upstream exploded"}}`,
			wantCode:   "",
			wantMsg:    "upstream exploded",
			wantString: "gateway: 502 : upstream exploded",
		},
		{
			name:       "plain text body becomes the message",
			status:     500,
			body:       "  plain refusal text  ",
			wantCode:   "",
			wantMsg:    "plain refusal text",
			wantString: "gateway: 500 : plain refusal text",
		},
		{
			name:       "empty body falls back to the status text",
			status:     503,
			body:       "",
			wantCode:   "",
			wantMsg:    "Service Unavailable",
			wantString: "gateway: 503 : Service Unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			he := gatewayHTTPError(tc.status, []byte(tc.body))
			if he.Status != tc.status {
				t.Errorf("status = %d, want %d", he.Status, tc.status)
			}
			if he.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", he.Code, tc.wantCode)
			}
			if he.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", he.Message, tc.wantMsg)
			}
			if got := he.Error(); got != tc.wantString {
				t.Errorf("Error() = %q, want %q", got, tc.wantString)
			}
		})
	}
}

// --- transport failure classification ---------------------------------------

// A listener that is not up must surface as a *TransportError naming the
// target URL, not as a raw net error.
func TestUnreachableListenerIsTransportError(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	caFile := writeTemp(t, dir, "ca.pem", ca.pem)

	// Grab a real free port, then close its listener so nothing is serving.
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Start()
	addr := srv.Listener.Addr().String()
	srv.Close()

	c, err := New(Config{BaseURL: "https://" + addr, CAFile: caFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Status(context.Background(), nil)
	if err == nil {
		t.Fatal("expected a transport failure against a closed listener, got nil")
	}
	te, ok := errors.AsType[*TransportError](err)
	if !ok {
		t.Fatalf("error is %T (%v), want *TransportError", err, err)
	}
	if te.URL != "https://"+addr+"/status" {
		t.Errorf("URL = %q, want %q", te.URL, "https://"+addr+"/status")
	}
	if te.Reason == "" {
		t.Error("Reason is empty, want a human-readable failure reason")
	}
	if te.Err == nil {
		t.Error("Err is nil, want the underlying error")
	}
}

// A response body that is not JSON (after a 2xx status) must be a decoding
// error, since the client never reshapes or silently drops a body.
func TestNonJSONSuccessBodyIsDecodeError(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	srv := startMutualTLSServer(t, ca, serverPair, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "this is not json")
	}))

	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Status(context.Background(), nil); err == nil {
		t.Fatal("expected a decode error for a non-JSON 2xx body, got nil")
	} else if !strings.Contains(err.Error(), "decoding") {
		t.Errorf("error = %v, want it to name the decode failure", err)
	}
}

// --- New() construction failures --------------------------------------------

func TestNewRejectsMalformedBaseURL(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	caFile := writeTemp(t, dir, "ca.pem", ca.pem)

	cases := []struct {
		name    string
		baseURL string
		wantSub string
	}{
		{"http is not https", "http://127.0.0.1:9708", "https origin"},
		{"missing host", "https://", "https origin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{BaseURL: tc.baseURL, CAFile: caFile})
			if err == nil {
				t.Fatalf("New(%q): expected an error, got nil", tc.baseURL)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}

	if _, err := New(Config{BaseURL: "https://127.0.0.1:9708/%zz", CAFile: caFile}); err == nil {
		t.Error("New with an unparseable base URL: expected an error, got nil")
	} else if !strings.Contains(err.Error(), "parsing base URL") {
		t.Errorf("error = %v, want it to name the URL parse failure", err)
	}
}

func TestNewRejectsBadCABundle(t *testing.T) {
	cases := []struct {
		name    string
		caFile  string
		wantSub string
	}{
		{"empty path is required", "", "caFile is required"},
		{"missing file", filepath.Join(t.TempDir(), "absent.pem"), "reading caFile"},
		{"no usable pem", writeTemp(t, t.TempDir(), "junk.pem", []byte("not a certificate")), "no usable PEM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{BaseURL: "https://127.0.0.1:9708", CAFile: tc.caFile})
			if err == nil {
				t.Fatalf("New: expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestNewRejectsHalfClientPair(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile := writeTemp(t, dir, "ca.pem", ca.pem)
	certFile := writeTemp(t, dir, "client.pem", clientCert)
	keyFile := writeTemp(t, dir, "client-key.pem", clientKey)

	cases := []struct {
		name       string
		clientCert string
		clientKey  string
	}{
		{"cert without key", certFile, ""},
		{"key without cert", "", keyFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{BaseURL: "https://127.0.0.1:9708", CAFile: caFile, ClientCert: tc.clientCert, ClientKey: tc.clientKey})
			if err == nil {
				t.Fatal("New: expected a paired-certificates error, got nil")
			}
			if !strings.Contains(err.Error(), "must be set together") {
				t.Errorf("error = %v, want it to name the pairing rule", err)
			}
		})
	}
}

func TestNewRejectsUnreadableClientPair(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	caFile := writeTemp(t, dir, "ca.pem", ca.pem)
	certFile := writeTemp(t, dir, "client.pem", []byte("not a pem"))
	keyFile := writeTemp(t, dir, "client-key.pem", []byte("not a key"))

	if _, err := New(Config{BaseURL: "https://127.0.0.1:9708", CAFile: caFile, ClientCert: certFile, ClientKey: keyFile}); err == nil {
		t.Fatal("New with an unparseable client pair: expected an error, got nil")
	} else if !strings.Contains(err.Error(), "loading client certificate pair") {
		t.Errorf("error = %v, want it to name the certificate load failure", err)
	}
}

// Reload without a configured pair is a loud error, not a silent no-op.
func TestReloadWithoutConfiguredPairFails(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	caFile := writeTemp(t, dir, "ca.pem", ca.pem)

	c, err := New(Config{BaseURL: "https://127.0.0.1:9708", CAFile: caFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Reload(); err == nil {
		t.Fatal("Reload with no client pair configured: expected an error, got nil")
	} else if !strings.Contains(err.Error(), "no client certificate configured") {
		t.Errorf("error = %v, want it to name the missing configuration", err)
	}
}

// --- query forwarding and verbatim bodies ------------------------------------

// A query on a bucket route must be forwarded verbatim, and a nil query must
// produce no query string at all.
func TestQueryForwardingOnBucketRoutes(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	cases := []struct {
		name      string
		query     url.Values
		wantQuery string
	}{
		{"nil query produces no query string", nil, ""},
		{"query forwarded verbatim", url.Values{"detail": []string{"full"}}, "detail=full"},
		{"multi-value query encoded", url.Values{"tag": []string{"a", "b"}}, "tag=a&tag=b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec recordedRequest
			srv := recordingServer(t, ca, serverPair, &rec, http.StatusOK, `{"ok":true}`)
			c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.GetBucket(context.Background(), "my-bucket", tc.query); err != nil {
				t.Fatalf("GetBucket: %v", err)
			}
			if rec.query != tc.wantQuery {
				t.Errorf("query = %q, want %q", rec.query, tc.wantQuery)
			}
			if rec.path != "/buckets/my-bucket" {
				t.Errorf("path = %q, want /buckets/my-bucket", rec.path)
			}
		})
	}
}

// The body must reach the gateway byte-for-byte, with the JSON content type
// set; a bodyless request must carry no content type and no body.
func TestBodyForwardedVerbatimWithContentType(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	const body = `{"name":"b","dataset":"pool/ds"}`
	var rec recordedRequest
	var gotCT string
	srv := startMutualTLSServer(t, ca, serverPair, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.body = string(raw)
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":true}`)
	}))
	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.CreateBucket(context.Background(), json.RawMessage(body), nil); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if rec.body != body {
		t.Errorf("body = %q, want %q byte-for-byte", rec.body, body)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}

	// Bodyless route: no Content-Type, no body.
	var rec2 recordedRequest
	srv2 := recordingServer(t, ca, serverPair, &rec2, http.StatusOK, `{"ok":true}`)
	c2, err := New(Config{BaseURL: srv2.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c2.SaveConfig(context.Background(), nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if rec2.body != "" {
		t.Errorf("body = %q, want empty for a bodyless route", rec2.body)
	}
}

// A route path containing characters that need escaping must reach the
// gateway path-escaped (Contract 5: url.PathEscape on the name).
func TestBucketNameIsPathEscaped(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	var rec recordedRequest
	srv := recordingServer(t, ca, serverPair, &rec, http.StatusOK, `{"ok":true}`)
	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	name := "my bucket+1"
	if _, err := c.GetBucket(context.Background(), name, nil); err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if rec.path != "/buckets/"+url.PathEscape(name) {
		t.Errorf("path = %q, want %q", rec.path, "/buckets/"+url.PathEscape(name))
	}
}

// --- misc ---------------------------------------------------------------------

// The response read is bounded by maxResponseBytes via io.LimitReader; a body
// at exactly the limit still decodes (this pins the constant's intent: the
// bound must be well above any real management payload).
func TestMaxResponseBytesConstant(t *testing.T) {
	if maxResponseBytes != 8<<20 {
		t.Errorf("maxResponseBytes = %d, want 8 MiB", maxResponseBytes)
	}
}

// A request cancelled by the caller surfaces as a *TransportError, not a bare
// context error.
func TestCallerCancellationIsTransportError(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "test-ca")
	_, _, serverPair := ca.issue(t, "gateway", false)
	clientCert, clientKey, _ := ca.issue(t, "console", true)
	caFile, certFile, keyFile := clientFiles(t, dir, ca, clientCert, clientKey)

	srv := startMutualTLSServer(t, ca, serverPair, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	c, err := New(Config{BaseURL: srv.URL, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Status(ctx, nil)
	if err == nil {
		t.Fatal("expected a transport failure for a cancelled context, got nil")
	}
	if _, ok := errors.AsType[*TransportError](err); !ok {
		t.Fatalf("error is %T (%v), want *TransportError", err, err)
	}
}

// Reload with a corrupt key file fails and the previous certificate keeps
// working (exercises the loadCert failure branch through Reload, complementing
// TestReloadUnreadableKeepsPreviousCertificate's corrupt-cert case).
func TestReloadCorruptKeyFailsLoudly(t *testing.T) {
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

	if err := os.WriteFile(keyFile, []byte("corrupt key"), 0o600); err != nil {
		t.Fatalf("corrupt key: %v", err)
	}
	if err := c.Reload(); err == nil {
		t.Fatal("Reload with a corrupt key: expected an error, got nil")
	}

	if _, err := c.Status(context.Background(), nil); err != nil {
		t.Fatalf("Status after failed reload: %v", err)
	}
}
