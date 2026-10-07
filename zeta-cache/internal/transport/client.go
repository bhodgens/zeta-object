package transport

// client.go - the REAL webdav HTTP client behind the Transport interface
// (leaf 06). Wire contract per the plan tree's locked server facts:
//   - ETag = MD5 of the stored body, quoted on the wire.
//   - PUT requires Content-Length (the gateway rejects chunked with 400)
//     and honors If-Match / If-None-Match (412 on failure).
//   - MOVE honors If-Match on the DESTINATION; Overwrite: T/F.
//   - PROPFIND Depth 1 answers 207 with prefixed d:/oc: elements and
//     xmlns declarations on the ROOT element (e2e 19/25's ownCloud-compat
//     form). Parsing accepts the default-namespace form too.
//   - LOCK/UNLOCK per RFC 4918 §9.10/9.11 as the gateway pins them
//     (exclusive write, Timeout: Second-N, If: (<token>) enforcement).
//   - BatchDelete is POST ?batch with the internal/batchops JSON manifest
//     (flat ops, per-item results, no server-side expansion).
//
// Retry safety (the no-double-apply rule) lives in roundTrip: an h3
// upgrade attempt that fails BEFORE the request was sent (UDP dial or
// TLS/QUIC handshake) may fall back to TCP within the same call; a
// failure after send is returned, never retried here. The leaf-07
// scheduler owns all other retry policy; the ONE in-transport retry is
// the idempotent-read reconnect on connection reset.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// Client is the real Transport: webdav over TCP/HTTPS with Basic auth,
// optionally upgraded to HTTP/3 + mTLS when the gateway advertises
// alt-svc AND a client certificate is configured. Construct with
// NewClient; safe for concurrent use.
type Client struct {
	base    *url.URL // serverUrl (scheme https / http for tests)
	bucket  string
	user    string
	pass    string
	hc      *http.Client // TCP transport
	h3t     *http3.Transport
	cert    *tls.Certificate
	logger  *logSink
	nowFn   func() time.Time
	maxBody int64 // PROPFIND/batch body read bound

	altsvc altsvcState
}

// logSink is the minimal logger seam (avoids importing log for tests).
type logSink struct{ f func(string, ...any) }

func (l *logSink) printf(format string, args ...any) {
	if l != nil && l.f != nil {
		l.f(format, args...)
	}
}

// Options configures NewClient.
type Options struct {
	// ServerURL is the gateway base ("https://host:8443"). Required.
	ServerURL string
	// Bucket is the synced bucket; every key re-roots under /<bucket>/.
	Bucket string
	// BasicAuth: accessKey/secretKey (either may be empty for mTLS-only).
	BasicAuthUser, BasicAuthPass string
	// Store is the cert storage (leaf's CertStorage seam). May be nil.
	Store CertStorage
	// InsecureSkipVerify disables server-cert verification. Every true
	// value comes from an explicit config key the CALLER warns about;
	// kept here so the transport owns exactly one tls.Config.
	InsecureSkipVerify bool
	// Logger receives the transport's WARNING/INFO lines; nil = discard.
	Logf func(format string, args ...any)
	// Now overrides the clock (tests).
	Now func() time.Time
}

// NewClient builds the Client. Errors name the problem (bad URL,
// unreadable cert material).
func NewClient(o Options) (*Client, error) {
	if o.ServerURL == "" {
		return nil, fmt.Errorf("transport: ServerURL is required")
	}
	base, err := url.Parse(strings.TrimSuffix(o.ServerURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("transport: parsing ServerURL %q: %w", o.ServerURL, err)
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, fmt.Errorf("transport: ServerURL scheme must be https (or http for tests), got %q", base.Scheme)
	}
	store := o.Store
	if store == nil {
		store = NewFileCertStore("", "", "")
	}
	cert, err := store.LoadClientCert()
	if err != nil && !errors.Is(err, ErrNoCert) {
		return nil, err
	}
	roots, err := store.TrustRoots()
	if err != nil && !errors.Is(err, ErrNoTrustRoots) {
		return nil, err
	}
	tlsCfg := &tls.Config{
		RootCAs:            roots,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: o.InsecureSkipVerify, //nolint:gosec // explicit, WARN-logged config key (see main.go)
	}
	c := &Client{
		base:    base,
		bucket:  o.Bucket,
		user:    o.BasicAuthUser,
		pass:    o.BasicAuthPass,
		cert:    cert,
		logger:  &logSink{f: o.Logf},
		nowFn:   o.Now,
		maxBody: 64 << 20,
		altsvc:  *newAltsvcState(),
	}
	if c.nowFn == nil {
		c.nowFn = time.Now
	}
	// One shared TCP transport: keep-alives, and the h3 handshake only
	// when a client cert exists (mTLS is the ONLY h3 auth - without a
	// cert there is nothing to upgrade WITH, so the transport is never
	// built and alt-svc is never acted on).
	c.hc = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg.Clone(),
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 8,
		},
	}
	if cert != nil {
		h3tls := tlsCfg.Clone()
		h3tls.Certificates = []tls.Certificate{*cert}
		h3tls.NextProtos = []string{http3.NextProtoH3}
		c.h3t = &http3.Transport{TLSClientConfig: h3tls}
	}
	return c, nil
}

// Close releases the h3 transport's UDP socket. The TCP transport needs
// no explicit close.
func (c *Client) Close() {
	if c.h3t != nil {
		_ = c.h3t.Close()
	}
}

// resourceURL builds the absolute URL for key ("" = bucket root).
// mode B (the daemon's shape): every key lives under /<bucket>/.
func (c *Client) resourceURL(key string) string {
	p := "/" + c.bucket + "/"
	if key != "" {
		p += strings.TrimPrefix(key, "/")
		// collections keep their trailing slash through the caller
		if !strings.HasSuffix(p, "/") && strings.HasSuffix(key, "/") {
			p += "/"
		}
	}
	return c.base.String() + (&url.URL{Path: p}).EscapedPath()
}

// ErrAuthFailed is the DISTINCT h3 auth failure: the QUIC TLS handshake
// died with a crypto error (missing/wrong client cert - there is no
// HTTP 401 over h3). Callers surface it differently from network errors.
var ErrAuthFailed = errors.New("transport: h3 handshake rejected (certificate auth failed)")

// asAuthFailed wraps err in ErrAuthFailed when it carries a quic-go
// crypto TransportError; other errors pass through untouched.
func asAuthFailed(err error) error {
	if err == nil {
		return nil
	}
	if terr, ok := errors.AsType[*quic.TransportError](err); ok && terr.ErrorCode.IsCryptoError() {
		return fmt.Errorf("%w (%s)", ErrAuthFailed, terr.Error())
	}
	return err
}

// errClass buckets transport errors for the retry-safety table.
type errClass int

const (
	classDial errClass = iota // connection-level, request provably NOT sent
	classSent                 // request sent (HTTP status / stream / body error)
)

// roundTrip sends ONE request over the chosen transport. The h3 upgrade
// happens only from the eligible state; an h3 attempt that fails at the
// dial/handshake layer falls back to TCP within THIS call when the
// request had not been delivered (upgrade path only - see the state
// machine in altsvc.go). Every response's alt-svc header feeds the
// sticky state.
func (c *Client) roundTrip(ctx context.Context, req *http.Request) (*http.Response, error) {
	port, upgradable := c.altsvc.useH3()
	if port != "" && c.h3t != nil {
		resp, err := c.doH3(ctx, req, port)
		if err == nil {
			c.altsvc.h3Up()
			c.observeAltSvc(resp)
			return resp, nil
		}
		// Classify BEFORE deciding anything: only a pre-send failure
		// (dial/handshake) is safe to re-drive over TCP. Anything after
		// the request was sent (stream reset mid-flight, status, body)
		// is returned untouched - a PUT could have applied server-side.
		class, authFail := classifyH3Err(err)
		if authFail {
			// mTLS is the only h3 auth: a crypto handshake failure is
			// an auth failure, surfaced distinctly. Still blacklists h3.
			c.altsvc.h3Down(c.nowFn())
			return nil, asAuthFailed(err)
		}
		if class == classDial {
			// Request never left: sticky-degrade to TCP. An upgrade
			// attempt additionally falls back WITHIN this call, so the
			// operation still succeeds (leaf-06 fallback requirement).
			c.altsvc.h3Down(c.nowFn())
			c.logger.printf("transport: h3 failed pre-send (%v); falling back to TCP for %s", err, fallbackWindow)
			if upgradable {
				return c.doTCP(ctx, req)
			}
			return nil, fmt.Errorf("transport: h3 request failed pre-send: %w", err)
		}
		// Sent (stream error mid-flight): DO NOT retry - no double-apply.
		c.altsvc.h3Down(c.nowFn())
		return nil, err
	}
	return c.doTCP(ctx, req)
}

// doTCP sends over the TCP transport with the single idempotent-read
// retry on connection reset.
func (c *Client) doTCP(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := c.hc.Do(req.WithContext(ctx))
	if err != nil {
		// The ONE in-transport retry: idempotent READS get a single
		// reconnect on connection reset (leaf-06 requirement; leaf 07
		// owns every other retry decision).
		if isConnReset(err) && isReadVerb(req.Method) {
			resp, err = c.hc.Do(req.WithContext(ctx))
		}
		if err != nil {
			return nil, err
		}
	}
	c.observeAltSvc(resp)
	return resp, nil
}

// doH3 sends the request over HTTP/3. The h3 frontend is SINGLE-BUCKET
// (mode B: every path is already re-rooted at /), while the TCP webdav
// entry is conventionally multi-bucket (mode A: /<bucket>/<key>) - the
// bucket prefix is stripped here so the same key lands on the same
// object over either transport.
func (c *Client) doH3(ctx context.Context, req *http.Request, port string) (*http.Response, error) {
	h3URL := *req.URL
	h3URL.Host = joinHostPort(req.URL.Hostname(), port)
	h3URL.Path = strings.TrimPrefix(req.URL.Path, "/"+c.bucket)
	if h3URL.Path == "" {
		h3URL.Path = "/"
	}
	h3req := req.Clone(ctx)
	h3req.URL = &h3URL
	return c.h3t.RoundTrip(h3req)
}

// ErrNotModified is returned by Get when the server answers 304 to an
// If-None-Match revalidation (the cached copy is still valid).
var ErrNotModified = errors.New("transport: not modified (304)")

// classifyH3Err buckets an h3 error: (class, isAuthFailure). Dial =
// UDP unreachable / handshake refused before any HTTP byte flowed.
// quic-go surfaces TLS handshake failures as TransportError with a
// crypto error code - the auth-failure shape (leaf-06 requirement).
func classifyH3Err(err error) (errClass, bool) {
	if err == nil {
		return classDial, false
	}
	if terr, ok := errors.AsType[*quic.TransportError](err); ok {
		if terr.ErrorCode.IsCryptoError() {
			return classDial, true // TLS alert: cert refused - request never applied
		}
		return classDial, false
	}
	// Stream/body errors mean the request WAS in flight; everything
	// else quic-go returns before an HTTP stream opens is dial-class.
	if _, ok := errors.AsType[*quic.StreamError](err); ok {
		return classSent, false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The caller cancelled: do not loop anything, and do not retry.
		return classSent, false
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return classDial, false
	}
	if strings.Contains(err.Error(), "handshake") || strings.Contains(err.Error(), "udp") ||
		strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "network") {
		return classDial, false
	}
	return classSent, false
}

// isConnReset reports whether err is a transport-level connection reset
// (not an HTTP status).
func isConnReset(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") || strings.Contains(s, "EOF") && strings.Contains(s, "server closed")
}

// isReadVerb: only reads may auto-retry on connection reset.
func isReadVerb(method string) bool {
	switch method {
	case http.MethodGet, "PROPFIND", http.MethodOptions, http.MethodHead:
		return true
	}
	return false
}

// observeAltSvc feeds a response's alt-svc header into the sticky state.
// Only cert-bearing clients can act on the advertisement.
func (c *Client) observeAltSvc(resp *http.Response) {
	if resp == nil {
		return
	}
	h := resp.Header.Get("Alt-Svc")
	if h == "" {
		return
	}
	if c.altsvc.observe(h, c.cert != nil) {
		c.logger.printf("transport: alt-svc advertises h3; upgrade eligible")
	}
}

// joinHostPort re-attaches a port, preserving IPv6 bracketing.
func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// doAuth applies the TCP auth shape: Basic when configured. Over h3 the
// mTLS handshake already authenticated us; sending Basic there too is
// harmless but pointless, so only the TCP path sets it (do() routes).
func (c *Client) doAuth(req *http.Request) {
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
}

// newRequest builds the wire request for key with method/body.
func (c *Client) newRequest(ctx context.Context, method, key string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.resourceURL(key), body)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// do issues one request over the chosen transport with auth applied.
func (c *Client) do(ctx context.Context, method, key string, body io.Reader, contentLen int64, hdr map[string]string) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, key, body)
	if err != nil {
		return nil, err
	}
	if contentLen >= 0 {
		req.ContentLength = contentLen // NEVER chunked: the gateway 400s chunked PUTs
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c.doAuth(req)
	return c.roundTrip(ctx, req)
}

// drainAndClose discards a response body we do not read (keeps the
// connection reusable).
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// statusErr maps an HTTP status onto the Transport interface's error
// vocabulary.
func statusErr(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return ErrNotExist
	case http.StatusPreconditionFailed, http.StatusLocked:
		return ErrConflict
	case http.StatusConflict:
		return ErrNoParent
	case http.StatusMethodNotAllowed:
		return ErrExist
	default:
		return fmt.Errorf("transport: unexpected HTTP %d for %s %s", resp.StatusCode, resp.Request.Method, resp.Request.URL.Path)
	}
}

// --- Transport: Get -------------------------------------------------------

// Get implements Transport: streaming GET with Range support and
// If-None-Match revalidation (304 maps to ErrNotModified).
func (c *Client) Get(ctx context.Context, key string, req *RangeRequest) (io.ReadCloser, *ObjectInfo, error) {
	hdr := map[string]string{}
	if req != nil {
		hdr["Range"] = fmt.Sprintf("bytes=%d-%d", req.Start, req.End)
	}
	resp, err := c.do(ctx, http.MethodGet, key, nil, -1, hdr)
	if err != nil {
		return nil, nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		// fall through to the success path
	case http.StatusNotModified:
		drainAndClose(resp)
		return nil, nil, ErrNotModified
	default:
		err := statusErr(resp)
		drainAndClose(resp)
		return nil, nil, err
	}
	info := &ObjectInfo{
		Size:        resp.ContentLength,
		ModTime:     parseHTTPTime(resp.Header.Get("Last-Modified")),
		ETag:        normalizeETag(resp.Header.Get("ETag")),
		ContentType: resp.Header.Get("Content-Type"),
	}
	return resp.Body, info, nil
}

// --- Transport: Put -------------------------------------------------------

// Put implements Transport. Content-Length is ALWAYS set (chunked is a
// 400); ifMatch non-empty sends If-Match, ifMatch empty AND ifNoneStar
// (see PutOpts) sends If-None-Match: *.
func (c *Client) Put(ctx context.Context, key string, body io.ReadSeeker, etag string) (string, error) {
	return c.PutOpts(ctx, key, body, etag, false)
}

// PutOpts is Put with explicit control over the create precondition: the
// sync engine's matrix-8 upload uses If-None-Match:* (create only), the
// plain overwrite uses If-Match. size < 0 means "seek to measure".
func (c *Client) PutOpts(ctx context.Context, key string, body io.ReadSeeker, ifMatch string, ifNoneStar bool) (string, error) {
	size, err := body.Seek(0, io.SeekEnd)
	if err != nil {
		return "", fmt.Errorf("transport: put %s: measuring body: %w", key, err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("transport: put %s: rewinding body: %w", key, err)
	}
	hdr := map[string]string{"Content-Type": "application/octet-stream"}
	switch {
	case ifMatch != "":
		hdr["If-Match"] = quoteETag(ifMatch)
	case ifNoneStar:
		hdr["If-None-Match"] = "*"
	}
	resp, err := c.do(ctx, http.MethodPut, key, body, size, hdr)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return "", statusErr(resp)
	}
	return normalizeETag(resp.Header.Get("ETag")), nil
}

// --- Transport: Mkdir -----------------------------------------------------

// Mkdir implements Transport (webdav MKCOL).
func (c *Client) Mkdir(ctx context.Context, key string) error {
	resp, err := c.do(ctx, "MKCOL", key, nil, -1, nil)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	switch resp.StatusCode {
	case http.StatusCreated:
		return nil
	case http.StatusMethodNotAllowed:
		return ErrExist
	case http.StatusConflict:
		return ErrNoParent
	default:
		return statusErr(resp)
	}
}

// --- Transport: Delete ----------------------------------------------------

// Delete implements Transport. The interface carries an etag argument the
// gateway does not enforce on DELETE (no If-Match there); it is accepted
// for interface parity and ignored.
func (c *Client) Delete(ctx context.Context, key string, _ string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, -1, nil)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusConflict, http.StatusFailedDependency:
		return ErrNotEmpty
	default:
		return statusErr(resp)
	}
}

// BatchResult is one per-item outcome of BatchDelete (internal/batchops
// ItemResult shape: flat, manifest order, no server-side expansion).
type BatchResult struct {
	Index  int    `json:"index"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

// batchOp is one manifest row: op:delete + from (internal/batchops
// Operation's delete shape, field order pinned by the wire test).
type batchOp struct {
	Op   string `json:"op"`
	From string `json:"from"`
}

// BatchDelete removes many keys in ONE request via POST ?batch. A 400
// (malformed manifest) means NOTHING executed; a 200 carries per-item
// results the caller must inspect (StatusOK vs StatusError/Conflict).
func (c *Client) BatchDelete(ctx context.Context, keys []string) ([]BatchResult, error) {
	ops := make([]batchOp, len(keys))
	for i, k := range keys {
		ops[i] = batchOp{Op: "delete", From: k}
	}
	manifest, err := json.Marshal(struct {
		Operations []batchOp `json:"operations"`
	}{Operations: ops})
	if err != nil {
		return nil, fmt.Errorf("transport: batch manifest: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "", bytes.NewReader(manifest), int64(len(manifest)),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	if err != nil {
		return nil, fmt.Errorf("transport: batch response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transport: batch: unexpected HTTP %d: %s", resp.StatusCode, truncate(string(body), 256))
	}
	var parsed struct {
		Results []BatchResult `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("transport: batch response parse: %w", err)
	}
	return parsed.Results, nil
}

// --- Transport: Move ------------------------------------------------------

// Move implements Transport (webdav MOVE): Destination + Overwrite:T,
// If-Match enforced on the DESTINATION.
func (c *Client) Move(ctx context.Context, oldKey, newKey, destETag string) error {
	hdr := map[string]string{
		"Destination": c.resourceURL(newKey),
		"Overwrite":   "T",
	}
	if destETag != "" {
		hdr["If-Match"] = quoteETag(destETag)
	}
	resp, err := c.do(ctx, "MOVE", oldKey, nil, -1, hdr)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusPreconditionFailed:
		return ErrConflict
	case http.StatusNotFound:
		return ErrNotExist
	case http.StatusConflict:
		return ErrNoParent
	default:
		return statusErr(resp)
	}
}

// --- Transport: Propfind --------------------------------------------------

// Propfind implements Transport: Depth 1 (recursive is reserved for a
// later leaf; the sync engine only walks Depth 1). Entry 0 is the key
// itself; the rest are the sorted children, matching the memfs stub's
// contract. A Depth-0 PROPFIND of a FILE (key without trailing slash,
// not found as a collection) yields the file's own entry.
func (c *Client) Propfind(ctx context.Context, key string, recursive bool) ([]Entry, error) {
	if recursive {
		return nil, fmt.Errorf("transport: recursive propfind is not supported (depth-1 only; leaf-04 walks Depth 1)")
	}
	listKey := key
	isFileProbe := key != "" && !strings.HasSuffix(key, "/")
	resp, err := c.do(ctx, "PROPFIND", listKey, nil, -1, map[string]string{
		"Depth": "1",
	})
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	drainAndClose(resp)
	if err != nil {
		return nil, fmt.Errorf("transport: propfind %s: reading 207: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		// A file-shaped key may still exist as a FILE (a Depth-1
		// PROPFIND of a file's URL answers 404 on some servers, 207 on
		// the gateway): one Depth-0 probe decides.
		if isFileProbe {
			return c.propfindFile(ctx, key)
		}
		return nil, ErrNotExist
	}
	if resp.StatusCode == http.StatusMultiStatus || (resp.StatusCode == http.StatusOK && len(bytes.TrimSpace(body)) > 0) {
		// 207 is the RFC answer; a bare 200 with an XML body is a known
		// other-server quirk accepted for robustness.
		if rows, perr := parseMultistatus(body); perr == nil {
			return c.rowsToEntries(rows, key), nil
		} else if resp.StatusCode == http.StatusMultiStatus {
			return nil, fmt.Errorf("transport: propfind %s: %w", key, perr)
		}
	}
	return nil, statusErr(resp)
}

// propfindFile probes a single file with Depth 0 and returns its one
// entry (the interface's "entry 0 is the key itself" contract).
func (c *Client) propfindFile(ctx context.Context, key string) ([]Entry, error) {
	resp, err := c.do(ctx, "PROPFIND", key, nil, -1, map[string]string{"Depth": "0"})
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	drainAndClose(resp)
	if err != nil {
		return nil, fmt.Errorf("transport: propfind %s: reading 207: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotExist
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, statusErr(resp)
	}
	rows, err := parseMultistatus(body)
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("transport: propfind %s: empty 207", key)
	}
	entries := c.rowsToEntries(rows, parentDirOf(key))
	if len(entries) == 0 {
		return nil, ErrNotExist
	}
	return entries[:1], nil
}

// rowsToEntries maps parsed 207 rows onto the Entry contract: entry 0 is
// key itself, children follow sorted; keys are relative to the bucket
// root with directories ending in "/". Rows outside the requested
// collection (the server may echo absolute hrefs) are dropped.
func (c *Client) rowsToEntries(rows []msRow, key string) []Entry {
	prefix := "/" + c.bucket + "/"
	selfKey := key
	selfKey = strings.TrimSuffix(selfKey, "/")
	var self, children []Entry
	seen := map[string]bool{}
	for _, r := range rows {
		k, isDir, ok := c.rowToKey(r)
		if !ok {
			continue
		}
		if reservedKey(k) {
			continue
		}
		dirKey := k
		if !isDir {
			dirKey = ""
		}
		_ = dirKey
		if selfKey != "" && (k == selfKey || k == selfKey+"/") {
			self = []Entry{{
				Key:     selfKey,
				IsDir:   isDir,
				Size:    r.size,
				ModTime: r.modTime,
				ETag:    r.etag,
			}}
			continue
		}
		if selfKey == "" && (k == "" || k == "/") {
			// Listing the bucket root: entry 0 is "" itself.
			self = []Entry{{
				Key:     "",
				IsDir:   true,
				Size:    r.size,
				ModTime: r.modTime,
				ETag:    r.etag,
			}}
			continue
		}
		rel := k
		if rel == "/" || rel == "" {
			continue
		}
		rel = strings.TrimPrefix(rel, prefix)
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" || rel == selfKey || seen[rel] {
			continue
		}
		// Depth 1: nested grandchildren surface as their first segment.
		name := rel
		isDirChild := isDir
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			name = rel[:i]
			isDirChild = true
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		childKey := name
		if isDirChild {
			childKey = name + "/"
		}
		children = append(children, Entry{
			Key:     childKey,
			IsDir:   isDirChild,
			Size:    r.size,
			ModTime: r.modTime,
			ETag:    r.etag,
		})
	}
	all := append(self, children...)
	sortEntries(all)
	return dedupeAdjacent(all)
}

// rowToKey decodes one 207 row's href to a bucket-relative key
// ("" = the bucket root itself). ok=false: the href is outside the
// bucket or unparseable.
func (c *Client) rowToKey(r msRow) (string, bool, bool) {
	href := r.href
	if href == "" {
		return "", false, false
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", false, false
	}
	p := u.Path
	prefix := "/" + c.bucket
	if p != prefix && !strings.HasPrefix(p, prefix+"/") {
		return "", false, false
	}
	k := strings.TrimPrefix(p, prefix)
	k = strings.TrimPrefix(k, "/")
	return k, r.isDir, true
}

// sortEntries orders entries by Key (the stub's sorted contract).
func sortEntries(entries []Entry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Key < entries[j-1].Key; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// dedupeAdjacent collapses duplicate keys (a dir can appear both as its
// own row and as a collapsed grandchild parent).
func dedupeAdjacent(entries []Entry) []Entry {
	out := entries[:0:0]
	var last string
	for i, e := range entries {
		if i > 0 && e.Key == last {
			continue
		}
		last = e.Key
		out = append(out, e)
	}
	return out
}

// parentDirOf splits "a/b/c" -> "a/b"; "a" -> ""; matches the stub.
func parentDirOf(key string) string {
	key = strings.TrimSuffix(key, "/")
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return ""
}

// --- LOCK / UNLOCK ---------------------------------------------------------

// Lock takes an exclusive write lock on key (RFC 4918 §9.10 as the
// gateway pins it: exclusive-only, Depth 0, Second-timeout). It returns
// the opaque lock token to pass in the If header of guarded writes and
// to Unlock.
func (c *Client) Lock(ctx context.Context, key string, timeout time.Duration) (token string, err error) {
	if timeout <= 0 || timeout > time.Hour {
		timeout = 10 * time.Minute
	}
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<lockinfo><lockscope><exclusive/></lockscope><locktype><write/></locktype>` +
		`<owner><href>zeta-cache</href></owner></lockinfo>`
	resp, err := c.do(ctx, "LOCK", key, strings.NewReader(body), int64(len(body)), map[string]string{
		"Depth":   "0",
		"Timeout": "Second-" + strconv.Itoa(int(timeout/time.Second)),
	})
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return "", statusErr(resp)
	}
	tok := strings.Trim(strings.TrimSpace(resp.Header.Get("Lock-Token")), "<>")
	if tok == "" {
		// The gateway always sets the header; a body-only fallback keeps
		// us honest against other webdav servers.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		tok = parseLockTokenFromBody(b)
	}
	if tok == "" {
		return "", fmt.Errorf("transport: lock %s: 200 without a token", key)
	}
	return tok, nil
}

// Unlock releases the lock identified by token. 204 = released; a wrong
// or expired token maps to ErrConflict (the gateway pins 409).
func (c *Client) Unlock(ctx context.Context, key, token string) error {
	resp, err := c.do(ctx, "UNLOCK", key, nil, -1, map[string]string{
		"Lock-Token": "<" + token + ">",
	})
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return ErrConflict
	default:
		return statusErr(resp)
	}
}

// LockToken returns the If-header value guarding writes under token:
// `If: (<token>)` per the gateway's lockif.go.
func LockToken(token string) string { return "(" + token + ")" }

// parseLockTokenFallback extracts <opaquelocktoken:...> from a
// lockdiscovery body when the Lock-Token header is absent.
func parseLockTokenFromBody(b []byte) string {
	s := string(b)
	i := strings.Index(s, "opaquelocktoken:")
	if i < 0 {
		return ""
	}
	j := strings.IndexAny(s[i:], "> <")
	if j < 0 {
		return strings.TrimSpace(s[i:])
	}
	return s[i : i+j]
}

// --- header helpers --------------------------------------------------------

// normalizeETag strips quotes/weakness (the wire form is quoted; the
// interface contract is the raw MD5).
func normalizeETag(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "W/")
	return strings.Trim(h, `"`)
}

// quoteETag re-adds the wire quotes for If-Match (the gateway compares
// with etagListContains, quote-tolerant, but the RFC form is quoted).
func quoteETag(etag string) string {
	if etag == "" || etag == "*" || strings.HasPrefix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}

// parseHTTPTime parses Last-Modified (http.TimeFormat); zero on failure.
func parseHTTPTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if t, err := http.ParseTime(v); err == nil {
		return t
	}
	return time.Time{}
}

// truncate bounds an error-embedded body.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
