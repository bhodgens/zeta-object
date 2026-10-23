// sts.go — the /sts token-exchange frontend (accessstor appliance tree
// leaf 02, Task 3): a dedicated TLS listener (the TLSListenerFrontend
// seam, the admin frontend's discipline: PRE-BUILT *tls.Config with
// ClientCAs carried, GetConfigForClient for CA rotation, never a mutation
// after first use; loopback-only by default with an explicit non-loopback
// opt-in; JSON never S3 XML).
//
// Wire shape (FROZEN, master Contract 2 / leaf Contract 2):
//
//	POST /sts
//	grant_type=urn:ietf:params:oauth:grant-type:token-exchange
//	subject_token=<JWT>
//	subject_token_type=urn:ietf:params:oauth:token-type:id_token
//	  (saml2-bearer accepted -> 400 unsupported_grant_type until the
//	   broker leaf lands; Dex converts SAML upstream)
//	optional: requested_ttl (seconds, <= MaxTTL else 403 ttl_exceeded)
//
//	200 JSON:
//	{"access_key_id":"ZSTSID-...","secret_access_key":"...",
//	 "session_token":"...","expiration":"RFC3339",
//	 "principal":"fed:host/user"}
//	errors (JSON): 400 unsupported_grant_type | 401 invalid_token |
//	403 audience_mismatch | 403 ttl_exceeded
//
// session_token is returned for AWS-SDK wire compatibility ONLY; the
// SigV4 path does not require it for ZSTSID- keys (the key id + derived
// secret authenticate; there is no server-side token store to check it
// against — charter-clean statelessness, documented in README).
//
// Audit: every exchange attempt that reaches the handler appends the
// standard 8-key record with op=sts_exchange through the injected
// write-only seam (same discipline as admin op=audit). Nothing reads the
// log back.
package sts

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// Wire-form constants (RFC 8693 subset). The "token"-looking literals
// are protocol vocabulary (URN identifiers, not credentials — gosec G101
// is a false positive on the URN strings; nolint on the block).
const (
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: RFC 8693 URN identifier, not a credential
	TokenTypeIDToken       = "urn:ietf:params:oauth:token-type:id_token"       //nolint:gosec // G101: RFC token-type URN, not a credential
	TokenTypeSAML2Bearer   = "urn:ietf:params:oauth:token-type:saml2-bearer"   //nolint:gosec // G101: RFC token-type URN, not a credential
	// Path is the only route this frontend serves.
	Path = "/sts"

	// DefaultTTL / DefaultMaxTTL are the leaf defaults (3600s / 43200s).
	DefaultTTL    = 3600 * time.Second
	DefaultMaxTTL = 43200 * time.Second
)

// Options configures the sts frontend (the admin Options discipline).
type Options struct {
	// ListenAddr is REQUIRED and loopback-validated unless
	// AllowNonLoopback (port 0 accepted).
	ListenAddr string
	// ClientCAFile is the OPTIONAL PEM bundle of trusted client CAs.
	// Empty = server-auth only (no mTLS); set it to require and verify
	// client certificates like the admin listener.
	ClientCAFile     string
	AllowNonLoopback bool
	// CertFile/KeyFile are the process server-certificate pair, loaded
	// by THIS package so the pre-built listener config can carry
	// ClientCAs (a ListenAndServeTLS(cert, key) path would rebuild the
	// config and silently drop them).
	CertFile string
	KeyFile  string
	// TTL / MaxTTL: mint default and request cap. Zero = leaf defaults.
	TTL    time.Duration
	MaxTTL time.Duration

	// Registry returns the LIVE FederationRegistry (nil = construction
	// error: the exchange is pointless without federation).
	Registry RegistryFunc
	// GroupsClaim / UsernameClaim mirror zeta_federation's optional claim
	// names so mint packs the credential with the same groups list the
	// federation config reads (defaults applied by federationGroups).
	GroupsClaim string
	// ServerKey is the process signing-key slot (atomic so the existing
	// reload path can swap it without touching the listener).
	ServerKey *atomic.Pointer[[]byte]
	// Audit is the injected write-only audit append (package main
	// injects s3.AppendAudit; this package never imports package s3).
	// Called EXACTLY ONCE per request that reaches the handler, with
	// op = "sts_exchange". nil disables auditing.
	Audit AuditFunc
}

// AuditFunc is the write-only audit append seam. principal is the
// resolved federated principal (or "" pre-validation), op is
// "sts_exchange", denied is true when status >= 400.
type AuditFunc func(principal, method, bucket, key, op string, status int, denied bool)

// stsFrontend implements frontend.Frontend + frontend.TLSListenerFrontend.
type stsFrontend struct {
	opts     Options
	ttl      time.Duration
	maxTTL   time.Duration
	caPool   atomic.Pointer[x509.CertPool]
	mtlsMode bool
}

var (
	_ frontend.Frontend            = (*stsFrontend)(nil)
	_ frontend.TLSListenerFrontend = (*stsFrontend)(nil)
)

// ClientCAReloader is satisfied by shape (same signature as
// admin.ClientCAReloader) so the existing /auth/reload fan covers this
// listener when a ClientCAFile is configured. It is a separate
// declaration, not an import: the reload path stays off the frozen
// frontend seams.
type ClientCAReloader interface {
	ReloadClientCA() error
}

// KnownOptionKeys lists every option key the sts frontend accepts from a
// frontends entry's options map. Unknown keys are a loud startup error
// naming the key and this set (frontends.go validateOptions).
var KnownOptionKeys = map[string]bool{
	"clientCAFile":     true,
	"allowNonLoopback": true,
}

// New constructs the sts frontend. Fail-loud: empty listen address,
// missing cert pair, an unreadable client CA bundle, a non-loopback
// address without the opt-out, a missing registry or signing-key slot —
// all abort construction.
func New(opts Options) (frontend.Frontend, error) {
	if strings.TrimSpace(opts.ListenAddr) == "" {
		return nil, fmt.Errorf("sts: listenAddr is required (an empty Addr is a construction error)")
	}
	if err := validateListenAddr(opts.ListenAddr, opts.AllowNonLoopback); err != nil {
		return nil, err
	}
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, fmt.Errorf("sts: certFile/keyFile are required (the /sts listener is TLS-only)")
	}
	if opts.Registry == nil {
		return nil, fmt.Errorf("sts: a federation registry is required (enable zeta_federation)")
	}
	if opts.ServerKey == nil {
		return nil, fmt.Errorf("sts: a signing-key slot is required (zeta_sts.signing_key_file)")
	}
	if key := opts.ServerKey.Load(); key == nil || len(*key) == 0 {
		return nil, fmt.Errorf("sts: the signing key is not loaded (zeta_sts.signing_key_file)")
	}
	f := &stsFrontend{opts: opts, ttl: opts.TTL, maxTTL: opts.MaxTTL}
	if f.ttl <= 0 {
		f.ttl = DefaultTTL
	}
	if f.maxTTL <= 0 {
		f.maxTTL = DefaultMaxTTL
	}
	if f.ttl > f.maxTTL {
		return nil, fmt.Errorf("sts: ttl %s exceeds max_ttl %s", f.ttl, f.maxTTL)
	}
	if strings.TrimSpace(opts.ClientCAFile) != "" {
		pool, err := loadClientCAs(opts.ClientCAFile)
		if err != nil {
			return nil, err
		}
		f.caPool.Store(pool)
		f.mtlsMode = true
	}
	return f, nil
}

// Name returns the wire identity of this frontend ("sts" — a separate
// frontend name; an explicit frontends array must keep s3 listed).
func (f *stsFrontend) Name() string { return "sts" }

// Capabilities: the exchange surface expresses no S3-protocol capability.
func (f *stsFrontend) Capabilities() frontend.ProtocolCaps { return frontend.ProtocolCaps{} }

// Addr returns this frontend's dedicated listen address.
func (f *stsFrontend) Addr() string { return f.opts.ListenAddr }

// Authenticator returns nil: authentication is the token exchange in the
// handler, not the shared SigV4 identity model (the admin precedent).
func (f *stsFrontend) Authenticator() auth.Authenticator { return nil }

// Handler returns the JSON exchange handler.
func (f *stsFrontend) Handler() http.Handler { return http.HandlerFunc(f.serveHTTP) }

// TLSConfig builds the dedicated listener's PRE-BUILT TLS configuration:
// TLS 1.2 minimum, the process server-certificate pair, and — in mTLS
// mode — required client certificates against the CA bundle. The config
// is never mutated after first use: a reloaded CA pool surfaces through
// GetConfigForClient, which hands every handshake a fresh per-connection
// config pinned to the CURRENT pool.
func (f *stsFrontend) TLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.opts.CertFile, f.opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("sts: loading server certificate pair (%s, %s): %w", f.opts.CertFile, f.opts.KeyFile, err)
	}
	base := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if f.mtlsMode {
		base.ClientAuth = tls.RequireAndVerifyClientCert
		base.ClientCAs = f.caPool.Load()
		base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return f.connectionTLSConfig(cert), nil
		}
	}
	return base, nil
}

// connectionTLSConfig builds the per-connection config in mTLS mode,
// pinned to the CURRENT cached CA pool (never mutates the base config).
func (f *stsFrontend) connectionTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    f.caPool.Load(),
		Certificates: []tls.Certificate{cert},
	}
}

// ReloadClientCA re-reads the configured clientCAFile and swaps the pool
// (the ClientCAReloader shape; only reachable in mTLS mode).
func (f *stsFrontend) ReloadClientCA() error {
	if !f.mtlsMode {
		return fmt.Errorf("sts: no clientCAFile configured")
	}
	pool, err := loadClientCAs(f.opts.ClientCAFile)
	if err != nil {
		return err
	}
	f.caPool.Store(pool)
	return nil
}

// errorEnvelope is the management-plane JSON error shape (never S3 XML).
type errorEnvelope struct {
	Error stsError `json:"error"`
}

type stsError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError renders one JSON error and returns the status for the audit
// record.
func writeError(w http.ResponseWriter, status int, code, message string) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorEnvelope{Error: stsError{Code: code, Message: message}}); err != nil {
		// The status line and headers are already committed; a short
		// write cannot be recovered, and the error carries nothing
		// actionable (same posture as admin's writeJSON).
		return status
	}
	return status
}

// exchangeRequest is the parsed RFC 8693 form.
type exchangeRequest struct {
	GrantType        string
	SubjectToken     string
	SubjectTokenType string
	RequestedTTL     time.Duration // 0 = server default
}

// exchangeResponse is the frozen 200 body.
type exchangeResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	Expiration      string `json:"expiration"`
	Principal       string `json:"principal"`
}

// serveHTTP is the whole wire surface: exactly POST /sts.
func (f *stsFrontend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != Path {
		f.audit(w, r, "", writeError(w, http.StatusNotFound, "NotFound", "the sts frontend serves only POST "+Path))
		return
	}
	if r.Method != http.MethodPost {
		f.audit(w, r, "", writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "token exchange requires POST"))
		return
	}
	req, status, code, msg := f.parseForm(r)
	if status != 0 {
		f.audit(w, r, "", writeError(w, status, code, msg))
		return
	}
	principal, claims, err := f.registry().ValidateIDToken(r.Context(), req.SubjectToken)
	if err != nil {
		// audience_mismatch is distinguished on the wire (Contract 2);
		// every other validation failure is a generic 401 invalid_token
		// that leaks nothing about which check failed.
		if strings.Contains(err.Error(), "audience") {
			f.audit(w, r, "", writeError(w, http.StatusForbidden, "audience_mismatch", "token audience does not match"))
			return
		}
		f.audit(w, r, "", writeError(w, http.StatusUnauthorized, "invalid_token", "subject_token validation failed"))
		return
	}
	resp, status, code, msg := f.mint(principal, claims, req.RequestedTTL)
	if status != 0 {
		f.audit(w, r, string(principal), writeError(w, status, code, msg))
		return
	}
	f.audit(w, r, string(principal), func() int {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		//nolint:gosec // G117: session_token is minted random material handed TO its owner over TLS — the exchange's entire purpose
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			// The status line is already committed; a short write after
			// WriteHeader cannot be recovered (same posture as admin's
			// writeJSON). The client saw the 200; the audit record
			// reflects what was sent.
			return http.StatusOK
		}
		return http.StatusOK
	}())
}

// registry returns the live federation registry (nil-safe fail-closed).
func (f *stsFrontend) registry() *auth.FederationRegistry { return f.opts.Registry() }

// audit appends the 8-key record (best-effort; nil sink = disabled).
func (f *stsFrontend) audit(w http.ResponseWriter, r *http.Request, principal string, status int) {
	if f.opts.Audit == nil {
		return
	}
	// The 8-key schema is (ts, principal, method, bucket, key, op,
	// status, denied); bucket/key are empty for an exchange.
	denied := status >= 400
	f.opts.Audit(principal, r.Method, "", "", "sts_exchange", status, denied)
}

// parseForm enforces the RFC 8693 request shape.
func (f *stsFrontend) parseForm(r *http.Request) (exchangeRequest, int, string, string) {
	if err := r.ParseForm(); err != nil {
		return exchangeRequest{}, http.StatusBadRequest, "invalid_request", "request body is not a valid form"
	}
	req := exchangeRequest{
		GrantType:        r.PostFormValue("grant_type"),
		SubjectToken:     r.PostFormValue("subject_token"),
		SubjectTokenType: r.PostFormValue("subject_token_type"),
	}
	if req.GrantType != GrantTypeTokenExchange {
		return exchangeRequest{}, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be " + GrantTypeTokenExchange
	}
	if strings.TrimSpace(req.SubjectToken) == "" {
		return exchangeRequest{}, http.StatusUnauthorized, "invalid_token", "subject_token is required"
	}
	switch req.SubjectTokenType {
	case TokenTypeIDToken:
		// The only type this leaf serves.
	case TokenTypeSAML2Bearer:
		// Accepted and rejected with unsupported_grant_type (Contract 2):
		// the Dex broker converts SAML to OIDC upstream; direct SAML
		// consumption arrives with the broker leaf.
		return exchangeRequest{}, http.StatusBadRequest, "unsupported_grant_type", "saml2-bearer is not served directly: configure the SAML upstream in the identity broker (Dex)"
	case "":
		return exchangeRequest{}, http.StatusBadRequest, "invalid_request", "subject_token_type is required"
	default:
		return exchangeRequest{}, http.StatusBadRequest, "unsupported_grant_type", "subject_token_type must be " + TokenTypeIDToken
	}
	if raw := r.PostFormValue("requested_ttl"); raw != "" {
		sec, err := parseSeconds(raw)
		if err != nil || sec <= 0 {
			return exchangeRequest{}, http.StatusBadRequest, "invalid_request", "requested_ttl must be a positive integer of seconds"
		}
		req.RequestedTTL = time.Duration(sec) * time.Second
	}
	return req, 0, "", ""
}

// mint validates TTL policy and derives the credential. Pure derivation —
// nothing is stored.
func (f *stsFrontend) mint(principal auth.Principal, claims map[string]any, requested time.Duration) (exchangeResponse, int, string, string) {
	ttl := f.ttl
	if requested > 0 {
		if requested > f.maxTTL {
			return exchangeResponse{}, http.StatusForbidden, "ttl_exceeded",
				fmt.Sprintf("requested_ttl %d exceeds the maximum %d seconds", int(requested.Seconds()), int(f.maxTTL.Seconds()))
		}
		ttl = requested
	}
	keyPtr := f.opts.ServerKey.Load()
	if keyPtr == nil || len(*keyPtr) == 0 {
		return exchangeResponse{}, http.StatusInternalServerError, "server_error", "signing key unavailable"
	}
	if _, err := UsernameFromPrincipal(principal); err != nil {
		return exchangeResponse{}, http.StatusUnauthorized, "invalid_token", "principal is not a federated identity"
	}
	// The credential carries the group list so the stateless SigV4 path
	// can re-resolve grants per request. Custom federation claim names
	// are folded in here (the exchange has the full claim set).
	groups := federationGroups(claims, f.opts)
	expiry := time.Now().Add(ttl).Truncate(time.Second)
	keyID, secret, err := MintAccessKey(*keyPtr, string(principal), expiry, groups)
	if err != nil {
		return exchangeResponse{}, http.StatusInternalServerError, "server_error", "credential derivation failed"
	}
	token, err := SessionToken()
	if err != nil {
		return exchangeResponse{}, http.StatusInternalServerError, "server_error", "session token derivation failed"
	}
	return exchangeResponse{
		AccessKeyID:     keyID,
		SecretAccessKey: secret,
		SessionToken:    token,
		Expiration:      expiry.UTC().Format(time.RFC3339),
		Principal:       string(principal),
	}, 0, "", ""
}

// federationGroups extracts the group list from the validated claim set,
// honoring customized federation claim names. The DEFAULT names are also
// what the stateless resolver reconstructs; a claim source that puts
// groups under a custom name still works here, but the resolver's
// reconstruction only knows the defaults — such a deployment gets the
// per-user grant (prefix count mismatch fails closed for the group
// grants), which is the documented limitation of custom claim names in
// this leaf.
func federationGroups(claims map[string]any, opts Options) []string {
	name := ClaimGroups
	if opts.GroupsClaim != "" {
		name = opts.GroupsClaim
	}
	raw, ok := claims[name].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, g := range raw {
		if s, ok := g.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
