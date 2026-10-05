// Package gateway is a typed mTLS JSON client over the gateway's management
// API (docs/plans/management-api-2026-10 master.md Contract 5). It builds the
// transport from the console's config, calls each management route one-to-one,
// and surfaces the gateway's own error envelope unchanged so the console UI can
// show the gateway's message verbatim.
//
// It deliberately holds NO business logic: no retries, no caching, no
// reshaping of the gateway's JSON. The UI is written against the gateway's
// shapes.
package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// requestTimeout bounds every exchange with the gateway. A management request
// carries a small JSON body and a small response; a hung listener must not hang
// the console indefinitely. It is a package variable only so tests can lower
// it; production never changes it.
var requestTimeout = 15 * time.Second

// maxResponseBytes bounds a management response body read into memory.
const maxResponseBytes = 8 << 20 // 8 MiB

// Config configures a Client (master.md Contract 1 supplies these from the
// console config).
type Config struct {
	// BaseURL is the gateway admin listener's origin, e.g.
	// "https://127.0.0.1:9708".
	BaseURL string
	// CAFile is the PEM bundle of CAs that sign the gateway's server
	// certificate.
	CAFile string
	// ClientCert and ClientKey are the console's client certificate pair.
	// They are presented to the gateway, which uses the certificate's common
	// name as the audit principal. Both empty means "present no certificate"
	// (a gateway that requires one will then refuse the handshake).
	ClientCert string
	ClientKey  string
}

// Client talks to one gateway admin listener. It is safe for concurrent use.
//
// The client certificate is held in an atomic pointer, not written into the
// tls.Config: a tls.Config must not be mutated after first use, so a reload is
// surfaced through GetClientCertificate, which the transport consults on every
// handshake (the same reason the gateway's own admin code uses
// GetConfigForClient). Reload swaps the stored certificate atomically.
type Client struct {
	baseURL  *url.URL
	http     *http.Client
	roots    *x509.CertPool
	certFile string
	keyFile  string
	cert     atomic.Pointer[tls.Certificate]
}

// New builds a Client. It is fail-loud: a malformed or non-HTTPS base URL, a
// missing/unreadable CA bundle, or one half of the client pair without the
// other is a construction error.
func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing base URL: %w", err)
	}
	if base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("gateway: base URL must be an https origin")
	}
	roots, err := loadCAs(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	certFile := strings.TrimSpace(cfg.ClientCert)
	keyFile := strings.TrimSpace(cfg.ClientKey)
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("gateway: clientCert and clientKey must be set together")
	}

	c := &Client{baseURL: base, roots: roots, certFile: certFile, keyFile: keyFile}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}
	if certFile != "" {
		if err := c.loadCert(); err != nil {
			return nil, err
		}
		// Surface the current certificate per handshake so a reload takes
		// effect without ever mutating this config.
		tlsCfg.GetClientCertificate = c.getClientCertificate
	}

	c.http = &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   requestTimeout,
	}
	return c, nil
}

// loadCAs reads a PEM bundle into a certificate pool. A missing or unreadable
// file, or a bundle with no usable certificate, is a loud error.
func loadCAs(path string) (*x509.CertPool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("gateway: caFile is required")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gateway: reading caFile: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("gateway: caFile contains no usable PEM certificates")
	}
	return pool, nil
}

// loadCert reads the client pair from disk and stores it atomically. On any
// failure the previously stored certificate is left untouched.
func (c *Client) loadCert() error {
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		// LoadX509KeyPair errors name the parse failure, never key material.
		return fmt.Errorf("gateway: loading client certificate pair: %w", err)
	}
	c.cert.Store(&pair)
	return nil
}

// getClientCertificate supplies the handshake with the currently loaded client
// certificate. It never returns the material to a caller.
func (c *Client) getClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	cert := c.cert.Load()
	if cert == nil {
		// Unreachable when a pair was configured at construction, but the
		// handshake must never proceed without a certificate.
		return nil, fmt.Errorf("gateway: no client certificate loaded")
	}
	return cert, nil
}

// Reload re-reads the client certificate pair from disk and swaps it in. After
// the files are replaced and Reload succeeds, the next request presents the
// new certificate; if the files are unreadable Reload returns the error and
// the previous certificate keeps working.
func (c *Client) Reload() error {
	if c.certFile == "" || c.keyFile == "" {
		return fmt.Errorf("gateway: no client certificate configured to reload")
	}
	if err := c.loadCert(); err != nil {
		return err
	}
	// Drop idle connections so the next request performs a fresh handshake
	// and therefore presents the new certificate.
	c.http.CloseIdleConnections()
	return nil
}

// HTTPError is a non-2xx gateway response. It carries the HTTP status and the
// gateway's own error envelope (code and message strings) so a caller can
// re-emit the envelope unchanged. The gateway's error envelope is
// {"error":{"code":"...","message":"..."}}.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("gateway: %d %s: %s", e.Status, e.Code, e.Message)
}

// TransportError is a failure to reach or complete the exchange with the
// gateway: an unreachable listener, a rejected certificate, or a timeout. It
// carries a human-readable reason and never panics.
type TransportError struct {
	// URL is the full request URL the client tried.
	URL string
	// Reason is a human-readable description of the failure.
	Reason string
	// Err is the underlying error.
	Err error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("gateway: transport to %s failed: %s", e.URL, e.Reason)
}

func (e *TransportError) Unwrap() error { return e.Err }

// errorEnvelope mirrors the gateway's error shape; only the fields the console
// re-emits are decoded.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// gatewayHTTPError maps a non-2xx response onto an *HTTPError. When the body
// carries the gateway's error envelope its code and message round-trip
// verbatim; otherwise the raw body (or the status text) becomes the message so
// nothing is silently dropped.
func gatewayHTTPError(status int, raw []byte) *HTTPError {
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && (env.Error.Code != "" || env.Error.Message != "") {
		return &HTTPError{Status: status, Code: env.Error.Code, Message: env.Error.Message}
	}
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &HTTPError{Status: status, Code: "", Message: msg}
}

// transportError classifies an http.Client error into a *TransportError.
func transportError(target string, err error) *TransportError {
	reason := err.Error()
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		reason = "timeout: " + err.Error()
	} else if errors.Is(err, context.DeadlineExceeded) {
		reason = "timeout: " + err.Error()
	}
	return &TransportError{URL: target, Reason: reason, Err: err}
}

// do performs one management request and returns the decoded response body. A
// non-2xx response is an *HTTPError; a failure to complete the exchange is a
// *TransportError. It adds no retries, caching, or reshaping.
func (c *Client) do(ctx context.Context, method, path string, body json.RawMessage, query url.Values) (json.RawMessage, error) {
	u := *c.baseURL
	u.Path = strings.TrimSuffix(c.baseURL.Path, "/") + path
	u.RawQuery = query.Encode()

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, &TransportError{URL: u.String(), Reason: "building request: " + err.Error(), Err: err}
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(u.String(), err)
	}
	defer func() {
		// The body has been fully read (or the read failed above); a
		// Close failure is logged rather than swallowed, matching the
		// repo's no-ignored-error rule.
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("gateway: closing response body from %s: %v", u.String(), cerr)
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &TransportError{URL: u.String(), Reason: "reading gateway response: " + err.Error(), Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, gatewayHTTPError(resp.StatusCode, raw)
	}
	var decoded json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("gateway: decoding %s %s response: %w", method, path, err)
	}
	return decoded, nil
}

// --- Management routes (docs/plans/management-api-2026-10 Contract 5). ------
//
// Each method performs exactly one gateway route and returns the decoded body.
// query is forwarded verbatim (nil for no query); body is forwarded verbatim.

// Status calls GET /status.
func (c *Client) Status(ctx context.Context, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/status", nil, query)
}

// GetConfig calls GET /config.
func (c *Client) GetConfig(ctx context.Context, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/config", nil, query)
}

// PutConfig calls PUT /config with the patch body forwarded verbatim.
func (c *Client) PutConfig(ctx context.Context, body json.RawMessage, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPut, "/config", body, query)
}

// SaveConfig calls POST /config/save.
func (c *Client) SaveConfig(ctx context.Context, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/config/save", nil, query)
}

// ReloadAuth calls POST /auth/reload.
func (c *Client) ReloadAuth(ctx context.Context, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/auth/reload", nil, query)
}

// ListBuckets calls GET /buckets.
func (c *Client) ListBuckets(ctx context.Context, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/buckets", nil, query)
}

// CreateBucket calls POST /buckets with the body forwarded verbatim.
func (c *Client) CreateBucket(ctx context.Context, body json.RawMessage, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/buckets", body, query)
}

// GetBucket calls GET /buckets/{name}.
func (c *Client) GetBucket(ctx context.Context, name string, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/buckets/"+url.PathEscape(name), nil, query)
}

// DeleteBucket calls DELETE /buckets/{name}.
func (c *Client) DeleteBucket(ctx context.Context, name string, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodDelete, "/buckets/"+url.PathEscape(name), nil, query)
}

// PutBucketSettings calls PUT /buckets/{name}/settings with the body forwarded
// verbatim.
func (c *Client) PutBucketSettings(ctx context.Context, name string, body json.RawMessage, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPut, "/buckets/"+url.PathEscape(name)+"/settings", body, query)
}

// Purge calls POST /purge with the body forwarded verbatim.
func (c *Client) Purge(ctx context.Context, body json.RawMessage, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/purge", body, query)
}
