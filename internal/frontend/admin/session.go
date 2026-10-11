// session.go — stateless HMAC-signed cookies for the federated web console
// (accessstor appliance tree leaf 03, Task 1).
//
// Two cookies live here:
//
//	fed_session     the console session: base64url(principal|expiry|nonce)
//	                + "." + base64url(HMAC-SHA256(serverKey, payloadB64)),
//	                TTL 12h default. No session store exists anywhere — the
//	                charter forbids persistent server-owned state, so the
//	                cookie is fully self-contained and the server key is
//	                DERIVED (see oidcFlow.deriveServerKey), never stored.
//	fed_oidc_state  the OIDC authorization-request anti-CSRF state+nonce,
//	                same HMAC discipline, 10-minute TTL.
//
// Derivation discipline mirrors leaf 02's temp credentials: MAC over the
// base64url-encoded payload (so every byte in the cookie value is
// cookie-safe and the framing is unambiguous), verification is
// timing-safe (hmac.Equal), and expiry is checked AFTER the MAC so a
// tampered expiry never reaches the time comparison as trusted input.
package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// fedSessionCookieName is the federated console session cookie.
	fedSessionCookieName = "fed_session"
	// fedStateCookieName carries the OIDC state+nonce through the IdP round
	// trip. It is scoped to the callback path so it is never sent elsewhere.
	fedStateCookieName = "fed_oidc_state"
	// fedStateTTL bounds the authorization round trip (leaf spec: 10 min).
	fedStateTTL = 10 * time.Minute
	// defaultSessionTTL is the fed_session TTL when the config leaves it
	// unset (leaf spec: 12h default).
	defaultSessionTTL = 12 * time.Hour
	// nonceBytes is the per-cookie randomness (128-bit nonce).
	nonceBytes = 16
)

// signCookieValue renders payload as base64url(payload) + "." +
// base64url(HMAC-SHA256(key, base64url(payload))).
func signCookieValue(key []byte, payload string) string {
	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payloadB64))
	return payloadB64 + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyCookieValue verifies the HMAC (timing-safe) and returns the decoded
// payload. Tampered, truncated, or wrong-key values are indistinguishable
// errors — nothing about the failure mode leaks.
func verifyCookieValue(key []byte, value string) (string, error) {
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return "", fmt.Errorf("malformed cookie value")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return "", fmt.Errorf("malformed cookie signature")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payloadB64))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", fmt.Errorf("cookie signature mismatch")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", fmt.Errorf("malformed cookie payload")
	}
	return string(raw), nil
}

// newNonce returns nonceBytes random bytes (base64url-encoded by callers).
func newNonce() (string, error) {
	b := make([]byte, nonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating cookie nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// mintSessionCookieValue mints one fed_session value for principal expiring
// at now+ttl. A principal containing the field separator is a mint-time
// error (fail-loud, never a silently truncated identity).
func mintSessionCookieValue(key []byte, principal string, now time.Time, ttl time.Duration) (string, error) {
	if principal == "" {
		return "", fmt.Errorf("session cookie: empty principal")
	}
	if strings.Contains(principal, "|") {
		return "", fmt.Errorf("session cookie: principal %q contains the field separator", principal)
	}
	nonce, err := newNonce()
	if err != nil {
		return "", err
	}
	payload := principal + "|" + strconv.FormatInt(now.Add(ttl).Unix(), 10) + "|" + nonce
	return signCookieValue(key, payload), nil
}

// verifySessionCookieValue verifies the fed_session value and returns the
// principal. Expired and tampered values are both errors.
func verifySessionCookieValue(key []byte, value string, now time.Time) (string, error) {
	payload, err := verifyCookieValue(key, value)
	if err != nil {
		return "", err
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed session payload")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("malformed session expiry")
	}
	if !now.Before(time.Unix(exp, 0)) {
		return "", fmt.Errorf("session expired")
	}
	return parts[0], nil
}

// oidcFlowState is what the fed_oidc_state cookie carries through the IdP
// round trip: the anti-CSRF state token, the nonce the ID token must echo,
// and the redirect_uri the code exchange must repeat exactly.
type oidcFlowState struct {
	State    string
	Nonce    string
	Redirect string
}

// mintStateCookieValue signs one state+nonce+redirect triple expiring at
// now+fedStateTTL.
func mintStateCookieValue(key []byte, s oidcFlowState, now time.Time) (string, error) {
	for _, f := range []struct{ name, val string }{{"state", s.State}, {"nonce", s.Nonce}} {
		if f.val == "" {
			return "", fmt.Errorf("state cookie: empty %s", f.name)
		}
		if strings.Contains(f.val, "|") {
			return "", fmt.Errorf("state cookie: %s contains the field separator", f.name)
		}
	}
	if s.Redirect == "" {
		return "", fmt.Errorf("state cookie: empty redirect")
	}
	redirectB64 := base64.RawURLEncoding.EncodeToString([]byte(s.Redirect))
	payload := s.State + "|" + s.Nonce + "|" + redirectB64 + "|" + strconv.FormatInt(now.Add(fedStateTTL).Unix(), 10)
	return signCookieValue(key, payload), nil
}

// verifyStateCookieValue verifies the state cookie and returns its fields.
// A stale (older than fedStateTTL) or tampered cookie is an error.
func verifyStateCookieValue(key []byte, value string, now time.Time) (oidcFlowState, error) {
	payload, err := verifyCookieValue(key, value)
	if err != nil {
		return oidcFlowState{}, err
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 4 {
		return oidcFlowState{}, fmt.Errorf("malformed state payload")
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return oidcFlowState{}, fmt.Errorf("malformed state expiry")
	}
	if !now.Before(time.Unix(exp, 0)) {
		return oidcFlowState{}, fmt.Errorf("state expired")
	}
	redirect, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return oidcFlowState{}, fmt.Errorf("malformed state redirect")
	}
	return oidcFlowState{State: parts[0], Nonce: parts[1], Redirect: string(redirect)}, nil
}

// setStateCookie writes the OIDC state cookie: HttpOnly, Secure,
// SameSite=Lax (the CSRF posture the leaf review checklist demands),
// scoped to the callback path, bounded by the state TTL.
func setStateCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     fedStateCookieName,
		Value:    value,
		Path:     "/auth/callback",
		MaxAge:   int(fedStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// setSessionCookie writes the fed_session cookie: HttpOnly, Secure,
// SameSite=Lax, site-wide path, lifetime = the session TTL.
func setSessionCookie(w http.ResponseWriter, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     fedSessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie expires both cookies (logout, and the state cookie's
// single-use clearing on callback). MaxAge<0 instructs the browser to drop
// the cookie immediately.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     fedSessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     fedStateCookieName,
		Value:    "",
		Path:     "/auth/callback",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
