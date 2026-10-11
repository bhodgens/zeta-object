// oidc.go — federated web-console sessions on the admin listener
// (accessstor appliance tree leaf 03, Task 2).
//
// GET  /auth/login     302 to the IdP authorization endpoint; state+nonce
//
//	travel in a 10-minute HMAC-signed cookie.
//
// GET  /auth/callback  server-side code exchange, FederationRegistry.
//
//	ValidateIDToken, fixed-redirect 302 to the console
//	root (NEVER a query-supplied target: no open
//	redirect).
//
// POST /auth/logout    clears the session cookie.
//
// Everything is driven by discovery (authorization_endpoint,
// token_endpoint) — no hardcoded IdP paths beyond /.well-known — so any
// standard provider works, Dex included.
//
// Config surface (restart-required, parsed in config_console.go):
//
//	zeta_console_oidc.enabled             bool, default false
//	zeta_console_oidc.session_ttl         duration string, default 12h
//	zeta_console_oidc.client_secret_file  PATH REFERENCE to the IdP
//	                                      client secret — the secret
//	                                      itself is never inline in
//	                                      config.json.
//
// The issuer/client-id/audience come from the existing zeta_federation
// block (leaf 01); wiring passes the client id through here so the
// FederationRegistry's config stays unexported.
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// ConsoleOIDCConfig is the zeta_console_oidc config block. Zero value =
// console federation disabled.
type ConsoleOIDCConfig struct {
	Enabled    bool
	SessionTTL time.Duration // 0 = defaultSessionTTL
	// ClientSecretFile is the PATH to the OIDC client secret. The secret is
	// read at startup and held only in memory — never logged, never echoed
	// into a response, never stored in config.json.
	ClientSecretFile string
	// ClientID mirrors zeta_federation.clientID (the OIDC client); it is
	// carried separately because FederationRegistry's config is unexported.
	ClientID string
}

// federationDeps is everything the OIDC flow needs from wiring. Enabled
// with a nil registry, an incomplete discovery, or an empty secret is a
// fail-loud construction error.
type federationDeps struct {
	Cfg       ConsoleOIDCConfig
	Registry  *auth.FederationRegistry
	Discovery *oidcDiscovery
	Secret    string // resolved client secret (from ClientSecretFile)
}

// oidcDiscovery is the subset of the IdP discovery document the flow uses.
type oidcDiscovery struct {
	AuthorizationEndpoint string
	TokenEndpoint         string
	Issuer                string
}

// fedSessions is the per-frontend OIDC state. Immutable after
// construction; the HTTP client is concurrency-safe.
type fedSessions struct {
	deps       federationDeps
	key        [sha256.Size]byte
	httpClient *http.Client
	now        func() time.Time // test seam
	mtls       *adminFrontend   // set once, right after New() builds both
}

// newAdminFederation validates the console-OIDC wiring. A disabled config
// returns (nil, nil) — the /auth/* and fed-session paths then behave
// exactly as before this leaf (mTLS only, 404 on /auth/*).
func newAdminFederation(deps federationDeps) (*fedSessions, error) {
	if !deps.Cfg.Enabled {
		return nil, nil
	}
	if deps.Registry == nil {
		return nil, fmt.Errorf("admin: zeta_console_oidc.enabled requires the zeta_federation registry")
	}
	if deps.Cfg.ClientID == "" {
		return nil, fmt.Errorf("admin: zeta_console_oidc.enabled requires a client id (zeta_federation.clientID)")
	}
	if deps.Discovery == nil ||
		deps.Discovery.AuthorizationEndpoint == "" ||
		deps.Discovery.TokenEndpoint == "" {
		return nil, fmt.Errorf("admin: IdP discovery for console OIDC lacks authorization_endpoint/token_endpoint")
	}
	if deps.Secret == "" {
		return nil, fmt.Errorf("admin: zeta_console_oidc.client_secret_file is required and must be non-empty when console OIDC is enabled")
	}
	f := &fedSessions{
		deps:       deps,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		now:        time.Now,
	}
	// The server key is DERIVED from the client secret plus a fixed label:
	// no key material is persisted anywhere (charter-clean), and a secret
	// rotation invalidates outstanding sessions (fail-closed).
	f.key = sha256.Sum256([]byte("zeta-object fed_session v1:" + deps.Secret))
	return f, nil
}

// enabled reports whether console federation is active (nil-safe).
func (f *fedSessions) enabled() bool { return f != nil }

// sessionTTL resolves the configured TTL with the 12h default.
func (f *fedSessions) sessionTTL() time.Duration {
	if f.deps.Cfg.SessionTTL > 0 {
		return f.deps.Cfg.SessionTTL
	}
	return defaultSessionTTL
}

// authenticateFedSession validates the request's fed_session cookie and
// returns the federated principal. No cookie / bad cookie / expired cookie
// is a nil identity — callers render their own 401 so the mTLS precedence
// rule stays in one place (fedAuth below).
func (f *fedSessions) authenticateFedSession(r *http.Request) *fedIdentity {
	if !f.enabled() {
		return nil
	}
	c, err := r.Cookie(fedSessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	principal, err := verifySessionCookieValue(f.key[:], c.Value, f.now())
	if err != nil {
		return nil
	}
	return &fedIdentity{Principal: auth.Principal(principal)}
}

// fedIdentity is one authenticated federated principal.
type fedIdentity struct {
	Principal auth.Principal
}

// fedAuth is the auth wrapper for /auth/* and /capabilities. mTLS wins
// when both credentials are present (leaf contract: "mTLS path unchanged
// and takes precedence"); otherwise a valid fed_session authenticates as
// the federated principal. principal is the audit principal (cert CN or
// fed:<host>/<user>); fed reports which path authenticated.
func (f *fedSessions) fedAuth(fn func(w http.ResponseWriter, r *http.Request, principal string, fed bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// mTLS first: the exact existing authenticate() behavior, and
		// precedence over any cookie.
		principal, status, code := f.mtls.authenticate(r)
		if status == 0 {
			fn(w, r, principal, false)
			return
		}
		if status != http.StatusUnauthorized {
			// A verified certificate that fails the allow-list is a 403 —
			// a cookie must never upgrade that decision.
			writeError(w, status, code, messageForCode(code))
			return
		}
		if id := f.authenticateFedSession(r); id != nil {
			fn(w, r, string(id.Principal), true)
			return
		}
		writeError(w, http.StatusUnauthorized, "Unauthorized", "client certificate or console session required")
	}
}

// consoleAudit appends the standard 8-key audit record for console auth
// events (login/logout) through the frontend's injected audit seam — the
// SAME seam, exactly once per authenticated request, op=admin. No new
// audit readers are introduced. It requires f.mtls (set at wiring).
func (f *fedSessions) consoleAudit(principal, method string, status int) {
	if f.mtls == nil || f.mtls.opts.Audit == nil {
		return
	}
	f.mtls.opts.Audit(principal, method, "", "", string(auth.OpAdmin), status, status >= 400)
}

// handleAuthLogin serves GET /auth/login: mint state+nonce, set the signed
// state cookie, and 302 to the IdP's authorization endpoint from DISCOVERY.
func (f *fedSessions) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	state, err := newNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "login failed")
		return
	}
	nonce, err := newNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "login failed")
		return
	}
	callback := f.callbackURL(r)
	stateCookie, err := mintStateCookieValue(f.key[:], oidcFlowState{
		State:    state,
		Nonce:    nonce,
		Redirect: callback,
	}, f.now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "login failed")
		return
	}
	setStateCookie(w, stateCookie)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", f.deps.Cfg.ClientID)
	q.Set("redirect_uri", callback)
	q.Set("scope", "openid")
	q.Set("state", state)
	q.Set("nonce", nonce)
	http.Redirect(w, r, f.deps.Discovery.AuthorizationEndpoint+"?"+q.Encode(), http.StatusFound)
}

// callbackURL renders this listener's /auth/callback absolute URL from the
// request Host over HTTPS (the admin listener is always TLS in production).
func (f *fedSessions) callbackURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		// httptest and plain-HTTP unit-test harnesses.
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

// handleAuthCallback serves GET /auth/callback: validate state cookie and
// query state (single use), exchange the code server-side, validate the ID
// token through the FederationRegistry (signature, iss/aud/exp via leaf
// 01) plus the flow nonce, mint fed_session, 302 to the FIXED console
// root "/".
func (f *fedSessions) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		f.consoleAudit("", http.MethodPost, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, "LoginFailed", "identity provider refused the login")
		return
	}
	code := q.Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "callback carries no code")
		return
	}
	c, err := r.Cookie(fedStateCookieName)
	if err != nil || c.Value == "" {
		f.consoleAudit("", http.MethodPost, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, "Unauthorized", "missing login state")
		return
	}
	st, err := verifyStateCookieValue(f.key[:], c.Value, f.now())
	if err != nil {
		f.consoleAudit("", http.MethodPost, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, "Unauthorized", "invalid or expired login state")
		return
	}
	clearStateCookie(w) // single use: a replayed state finds no cookie
	if q.Get("state") != st.State {
		f.consoleAudit("", http.MethodPost, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, "Unauthorized", "state mismatch")
		return
	}
	idToken, err := f.exchangeCode(r.Context(), code, st.Redirect)
	if err != nil {
		// The exchange failure text NEVER reaches the response (it can
		// carry token-endpoint detail); a generic envelope is returned.
		f.consoleAudit("", http.MethodPost, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, "LoginFailed", "code exchange or token validation failed")
		return
	}
	principal, claims, err := f.deps.Registry.ValidateIDToken(r.Context(), idToken)
	if err != nil {
		f.consoleAudit("", http.MethodPost, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, "LoginFailed", "code exchange or token validation failed")
		return
	}
	if err := verifyNonce(claims, st.Nonce); err != nil {
		f.consoleAudit(string(principal), http.MethodPost, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, "LoginFailed", "token nonce mismatch")
		return
	}
	value, err := mintSessionCookieValue(f.key[:], string(principal), f.now(), f.sessionTTL())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "login failed")
		return
	}
	setSessionCookie(w, value, f.sessionTTL())
	f.consoleAudit(string(principal), http.MethodPost, http.StatusOK)
	// FIXED redirect target: the console root. Query parameters are never
	// consulted (no open redirect).
	http.Redirect(w, r, "/", http.StatusFound)
}

// verifyNonce checks the ID token's nonce claim against the flow state.
// A token without a nonce fails closed.
func verifyNonce(claims map[string]any, want string) error {
	got, _ := claims["nonce"].(string)
	if got == "" || got != want {
		return fmt.Errorf("nonce mismatch")
	}
	return nil
}

// exchangeCode POSTs the authorization code to the discovery token
// endpoint (server-side; the code and secret never appear in a URL) and
// returns the id_token.
func (f *fedSessions) exchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", f.deps.Cfg.ClientID)
	form.Set("client_secret", f.deps.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.deps.Discovery.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint status %s", resp.Status)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("token endpoint response: %w", err)
	}
	if tok.IDToken == "" {
		return "", fmt.Errorf("token endpoint returned no id_token")
	}
	return tok.IDToken, nil
}

// handleAuthLogout serves POST /auth/logout: clears the session cookie and
// answers JSON. CSRF posture: the session cookie is SameSite=Lax and the
// route is POST-only; a forged logout cannot escalate privilege (it can
// only end a session) and the JSON envelope stays consistent.
func (f *fedSessions) handleAuthLogout(w http.ResponseWriter, r *http.Request, principal string, fed bool) {
	clearSessionCookie(w)
	// The audit record is written by dispatchFed (exactly once, for both
	// the mTLS and the fed path); nothing is appended here.
	if fed {
		_ = principal // the fed principal is the audit principal
	}
	writeJSON(w, http.StatusOK, map[string]bool{"loggedOut": true})
}
