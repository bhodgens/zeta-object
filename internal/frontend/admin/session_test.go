// session_test.go — the stateless HMAC cookie helper (leaf 03 Task 1):
// round-trip, tamper rejection, expiry rejection, wrong-key rejection,
// malformed payloads, and the cookie flag posture.
package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var (
	testNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	testKey = []byte("test-server-key")
)

// TestSession_RoundTrip pins mint→verify for a fresh, unexpired session.
func TestSession_RoundTrip(t *testing.T) {
	value, err := mintSessionCookieValue(testKey, "fed:idp.example/alice", testNow, defaultSessionTTL)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := verifySessionCookieValue(testKey, value, testNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != "fed:idp.example/alice" {
		t.Fatalf("principal = %q, want fed:idp.example/alice", got)
	}
	// Two mints differ: the nonce must never repeat in the clear.
	other, _ := mintSessionCookieValue(testKey, "fed:idp.example/alice", testNow, defaultSessionTTL)
	if other == value {
		t.Fatal("two mints produced identical cookie values (nonce missing?)")
	}
}

// TestSession_TamperRejection: any flipped byte anywhere invalidates the
// cookie; the failure is a generic error with no failure-mode detail.
func TestSession_TamperRejection(t *testing.T) {
	value, err := mintSessionCookieValue(testKey, "fed:idp.example/alice", testNow, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if strings.Count(value, ".") != 1 {
		t.Fatalf("cookie value %q is not payload.signature", value)
	}
	for name, mutated := range map[string]string{
		"truncated":      value[:len(value)/2],
		"no signature":   strings.SplitN(value, ".", 2)[0],
		"garbage":        "not.a-cookie",
		"empty":          "",
		"reordered-macs": value + "." + value,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifySessionCookieValue(testKey, mutated, testNow); err == nil {
				t.Fatalf("tampered value %q verified", mutated)
			}
		})
	}
	// Flip one character of the payload: the base64 decode may still
	// succeed but the MAC must fail.
	payload, sig, _ := strings.Cut(value, ".")
	flipped := "A" + payload[1:]
	if _, err := verifySessionCookieValue(testKey, flipped+"."+sig, testNow); err == nil {
		t.Fatal("flipped payload byte verified")
	}
	// Flip one character of the signature.
	flippedSig := "A" + sig[1:]
	if _, err := verifySessionCookieValue(testKey, payload+"."+flippedSig, testNow); err == nil {
		t.Fatal("flipped signature byte verified")
	}
}

// TestSession_ExpiryRejection pins that a still-signed expired session is
// rejected AFTER the MAC verifies.
func TestSession_ExpiryRejection(t *testing.T) {
	value, err := mintSessionCookieValue(testKey, "fed:idp.example/alice", testNow, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := verifySessionCookieValue(testKey, value, testNow.Add(2*time.Hour)); err == nil {
		t.Fatal("expired session verified")
	}
	// Boundary: exactly at expiry is rejected (expiry is exclusive).
	if _, err := verifySessionCookieValue(testKey, value, testNow.Add(time.Hour)); err == nil {
		t.Fatal("session at exact expiry verified")
	}
	if _, err := verifySessionCookieValue(testKey, value, testNow.Add(time.Hour-time.Second)); err != nil {
		t.Fatalf("session one second before expiry rejected: %v", err)
	}
}

// TestSession_WrongKeyRejection pins that a cookie minted under one server
// key never verifies under another.
func TestSession_WrongKeyRejection(t *testing.T) {
	value, err := mintSessionCookieValue(testKey, "fed:idp.example/alice", testNow, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := verifySessionCookieValue([]byte("another-key"), value, testNow); err == nil {
		t.Fatal("wrong-key session verified")
	}
}

// TestSession_MintValidation pins the fail-loud mint paths: empty
// principal and a principal carrying the field separator.
func TestSession_MintValidation(t *testing.T) {
	if _, err := mintSessionCookieValue(testKey, "", testNow, time.Hour); err == nil {
		t.Fatal("empty principal minted")
	}
	if _, err := mintSessionCookieValue(testKey, "fed:host|evil", testNow, time.Hour); err == nil {
		t.Fatal("principal containing the field separator minted")
	}
}

// TestStateCookie_RoundTripAndExpiry covers the OIDC state triple.
func TestStateCookie_RoundTripAndExpiry(t *testing.T) {
	in := oidcFlowState{State: "state-123", Nonce: "nonce-456", Redirect: "http://127.0.0.1:9000/auth/callback"}
	value, err := mintStateCookieValue(testKey, in, testNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := verifyStateCookieValue(testKey, value, testNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != in {
		t.Fatalf("round trip = %+v, want %+v", got, in)
	}
	// Expired state rejected.
	if _, err := verifyStateCookieValue(testKey, value, testNow.Add(fedStateTTL+time.Second)); err == nil {
		t.Fatal("expired state verified")
	}
	// Exactly at the state TTL is rejected (exclusive expiry).
	if _, err := verifyStateCookieValue(testKey, value, testNow.Add(fedStateTTL)); err == nil {
		t.Fatal("state at exact TTL verified")
	}
	// Wrong key rejected.
	if _, err := verifyStateCookieValue([]byte("nope"), value, testNow); err == nil {
		t.Fatal("wrong-key state verified")
	}
	// Empty state/nonce/redirect refused at mint.
	for name, s := range map[string]oidcFlowState{
		"empty state":    {Nonce: "n", Redirect: "r"},
		"empty nonce":    {State: "s", Redirect: "r"},
		"empty redirect": {State: "s", Nonce: "n"},
	} {
		if _, err := mintStateCookieValue(testKey, s, testNow); err == nil {
			t.Fatalf("%s: mint accepted", name)
		}
	}
	// A redirect containing the separator survives via base64 framing.
	withPipe := oidcFlowState{State: "s", Nonce: "n", Redirect: "http://h/cb|extra"}
	out, err := mintStateCookieValue(testKey, withPipe, testNow)
	if err != nil {
		t.Fatalf("mint redirect with separator: %v", err)
	}
	back, err := verifyStateCookieValue(testKey, out, testNow)
	if err != nil || back.Redirect != withPipe.Redirect {
		t.Fatalf("redirect round trip = %+v err %v", back, err)
	}
}

// TestCookieFlags pins the HttpOnly/Secure/SameSite=Lax posture and the
// scoped paths for both cookies, plus the MaxAge of the state cookie.
func TestCookieFlags(t *testing.T) {
	rec := httptest.NewRecorder()
	setSessionCookie(rec, "v", defaultSessionTTL)
	setStateCookie(rec, "s")
	clearSessionCookie(rec)
	clearStateCookie(rec)
	res := rec.Result()
	if len(res.Cookies()) != 4 {
		t.Fatalf("expected 4 Set-Cookie headers, got %d", len(res.Cookies()))
	}
	for _, c := range res.Cookies() {
		if !c.HttpOnly {
			t.Fatalf("%s: not HttpOnly", c.Name)
		}
		if !c.Secure {
			t.Fatalf("%s: not Secure", c.Name)
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("%s: SameSite = %v, want Lax", c.Name, c.SameSite)
		}
	}
	byName := map[string]*http.Cookie{}
	for _, c := range res.Cookies()[:2] { // set session, set state
		byName[c.Name] = c
	}
	if byName[fedSessionCookieName].Path != "/" {
		t.Fatalf("session cookie path = %q", byName[fedSessionCookieName].Path)
	}
	if byName[fedStateCookieName].Path != "/auth/callback" {
		t.Fatalf("state cookie path = %q", byName[fedStateCookieName].Path)
	}
	if byName[fedStateCookieName].MaxAge != int(fedStateTTL.Seconds()) {
		t.Fatalf("state cookie MaxAge = %d, want %d", byName[fedStateCookieName].MaxAge, int(fedStateTTL.Seconds()))
	}
	if byName[fedSessionCookieName].MaxAge != int(defaultSessionTTL.Seconds()) {
		t.Fatalf("session cookie MaxAge = %d, want %d", byName[fedSessionCookieName].MaxAge, int(defaultSessionTTL.Seconds()))
	}
	if byName[fedSessionCookieName+"-Cleared"] != nil {
		t.Fatal("unexpected cookie")
	}
}
