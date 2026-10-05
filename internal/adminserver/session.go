package adminserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// session.go — operator session and CSRF (admin-server Contract 3).
//
// The console keeps ZERO persistent state: a session is an HMAC-signed cookie
// payload plus an entry in an in-memory map, and the HMAC key is generated
// fresh per process start. A restart therefore invalidates every existing
// session. No secret (key, token, or cookie value) is ever logged.

const (
	// SessionCookieName is the session cookie name (Contract 3).
	SessionCookieName = "zeta_session"
	// CSRFCookieName carries the CSRF token to the UI's JavaScript. It is the
	// deliberately NON-HttpOnly companion to the session cookie (the token is
	// meant to be read by same-origin JS); the session cookie stays HttpOnly.
	CSRFCookieName = "zeta_csrf"
	// CSRFHeaderName is the header every mutating request must present.
	CSRFHeaderName = "X-CSRF-Token"

	// SessionLifetime is the absolute session lifetime (pinned constant).
	SessionLifetime = 12 * time.Hour
	// SessionIdleExpiry is the idle timeout (pinned constant): a session not
	// used within this window is refused.
	SessionIdleExpiry = 30 * time.Minute
)

// errBadSession is the single opaque rejection reason for any malformed,
// tampered, expired, unknown, or idle-expired cookie.
var errBadSession = errors.New("invalid session")

// sessionPayload is the authenticated content of the session cookie. It
// carries an absolute expiry, a random id, and the session's CSRF token.
type sessionPayload struct {
	ID   string `json:"id"`
	Exp  int64  `json:"exp"` // unix seconds
	CSRF string `json:"csrf"`
}

// sessionState is the in-memory record for a live session (idle tracking).
type sessionState struct {
	lastSeen time.Time
}

// sessionStore holds the per-process HMAC key and the live-session map.
type sessionStore struct {
	mu       sync.Mutex
	key      []byte
	sessions map[string]sessionState
	now      func() time.Time // injectable clock for tests
}

// newSessionStore builds a store with a fresh 32-byte HMAC key. A failure to
// read randomness is fatal to construction (never a weaker fallback key).
func newSessionStore() (*sessionStore, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating session key: %w", err)
	}
	return &sessionStore{
		key:      key,
		sessions: make(map[string]sessionState),
		now:      time.Now,
	}, nil
}

// randomToken returns n cryptographically-random bytes as a hex string.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// sign encodes and authenticates a payload as "base64url(json).base64url(mac)".
func (s *sessionStore) sign(p sessionPayload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encoding session payload: %w", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(enc))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return enc + "." + sig, nil
}

// parse verifies and decodes a signed cookie value. Every failure mode returns
// the same opaque error.
func (s *sessionStore) parse(value string) (sessionPayload, error) {
	var p sessionPayload
	dot := strings.LastIndexByte(value, '.')
	if dot <= 0 || dot == len(value)-1 {
		return p, errBadSession
	}
	enc, sig := value[:dot], value[dot+1:]

	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(enc))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want) {
		return p, errBadSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return p, errBadSession
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, errBadSession
	}
	return p, nil
}

// mint creates a new session, sets both cookies, and returns the CSRF token.
// The Secure attribute follows the response transport: a cookie is Secure only
// when this response arrived over TLS, so a plain-loopback console (which
// Safari would otherwise reject a Secure cookie on) still works.
func (s *sessionStore) mint(w http.ResponseWriter, r *http.Request) (string, error) {
	secure := r.TLS != nil
	id, err := randomToken(16)
	if err != nil {
		return "", err
	}
	csrf, err := randomToken(16)
	if err != nil {
		return "", err
	}
	now := s.now()
	payload := sessionPayload{ID: id, Exp: now.Add(SessionLifetime).Unix(), CSRF: csrf}
	value, err := s.sign(payload)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.sessions[id] = sessionState{lastSeen: now}
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- HttpOnly and SameSite=Strict are set here; Secure is set per transport (true on TLS, false on the loopback-only plain-HTTP mode), which gosec cannot prove statically
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	// The CSRF companion cookie is intentionally readable by the UI's
	// same-origin JavaScript (double-submit style), so it is NOT HttpOnly.
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- CSRF token is deliberately readable by same-origin UI JavaScript (double-submit); SameSite=Strict is set
		Name:     CSRFCookieName,
		Value:    csrf,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	return csrf, nil
}

// validate returns the CSRF token for a valid request session, refreshing the
// idle timer. Any missing, malformed, tampered, expired, unknown, or
// idle-expired cookie returns ok=false.
func (s *sessionStore) validate(r *http.Request) (csrf string, ok bool) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return "", false
	}
	p, err := s.parse(c.Value)
	if err != nil {
		return "", false
	}

	now := s.now()
	if !now.Before(time.Unix(p.Exp, 0)) {
		return "", false // absolute lifetime elapsed
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	st, present := s.sessions[p.ID]
	if !present {
		return "", false // not this process's session (or evicted)
	}
	if now.Sub(st.lastSeen) > SessionIdleExpiry {
		delete(s.sessions, p.ID)
		return "", false // idle-expired
	}
	st.lastSeen = now
	s.sessions[p.ID] = st
	return p.CSRF, true
}

// clear removes a session and expires both cookies. Like mint, the Secure
// attribute follows the response transport.
func (s *sessionStore) clear(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if p, err := s.parse(c.Value); err == nil {
			s.mu.Lock()
			delete(s.sessions, p.ID)
			s.mu.Unlock()
		}
	}
	for _, name := range []string{SessionCookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- session cookie is HttpOnly; the CSRF companion is deliberately readable (double-submit); both set SameSite=Strict
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == SessionCookieName,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

// tokenEqual compares two secrets in constant time. Both operands are hashed
// to fixed-length digests first (the repo's internal/auth style), so the
// comparison never depends on the inputs' lengths or an early mismatch.
func tokenEqual(presented, expected string) bool {
	ph := sha256.Sum256([]byte(presented))
	eh := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(ph[:], eh[:]) == 1
}
