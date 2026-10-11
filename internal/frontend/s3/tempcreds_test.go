// tempcreds_test.go — the SigV4 path's ZSTSID- branch (leaf 02 Task 2):
// valid temp cred authorizes per grants, expired -> reject, tampered ->
// reject, registry-unavailable -> fail closed, normal keys unaffected
// (regression). Full pipeline through the Frontend's Authenticator, using
// the production sts.CredentialResolver over a REAL FederationRegistry
// (a local JWKS-serving test IdP, the leaf-01 harness shape).
package s3_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/frontend/sts"
)

// stsTestServerKey is the test server signing key.
var stsTestServerKey = []byte("s3-tempcreds-test-signing-key")

// installTempCreds installs src for the test and restores the hook.
func installTempCreds(t *testing.T, src sts.TempCredentialSource) {
	t.Helper()
	s3.InstallTempCredentialSource(src)
	t.Cleanup(func() { s3.InstallTempCredentialSource(nil) })
}

// ---- local test IdP (leaf-01 harness shape, minimal RSA/RS256) ----

type stsTestIdP struct {
	srv    *httptest.Server
	issuer string
	key    *rsa.PrivateKey
	kid    string
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newSTSTestIdP(t *testing.T) *stsTestIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	idp := &stsTestIdP{key: key, kid: "sts-test-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   idp.issuer,
			"jwks_uri": idp.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": idp.kid, "alg": "RS256", "use": "sig",
			"n": b64u(idp.key.N.Bytes()),
			"e": b64u(bigEndianExp(idp.key.E)),
		}}})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	idp.issuer = idp.srv.URL
	return idp
}

func bigEndianExp(e int) []byte {
	var out []byte
	for x := e; x > 0; x >>= 8 {
		out = append([]byte{byte(x)}, out...)
	}
	if len(out) == 0 {
		out = []byte{0}
	}
	return out
}

// registry builds the FederationRegistry against the IdP.
func (p *stsTestIdP) registry(t *testing.T) *auth.FederationRegistry {
	t.Helper()
	reg, err := auth.NewFederationRegistry(auth.FederationConfig{
		Mode: "oidc", IssuerURL: p.issuer, ClientID: "zeta-object", Audience: "zeta-object",
	})
	if err != nil {
		t.Fatalf("NewFederationRegistry: %v", err)
	}
	return reg
}

// mintIDToken signs one RS256 ID token.
func (p *stsTestIdP) mintIDToken(t *testing.T, username string, groups []string, exp time.Time) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": p.kid})
	payload, _ := json.Marshal(map[string]any{
		"iss": p.issuer, "aud": "zeta-object", "sub": "sub-1",
		"preferred_username": username, "groups": groups,
		"exp": exp.Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
	})
	signingInput := b64u(header) + "." + b64u(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + b64u(sig)
}

// principalFor rebuilds the fed: principal the registry returns (host
// only, mirroring leaf 01's hostOf).
func principalFor(issuer, username string) string {
	host := issuer
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	return "fed:" + host + "/" + username
}

// signTempCredRequest signs an arbitrary method/path/body with a temp
// credential (the same canonical construction the shared helpers use).
func signTempCredRequest(t *testing.T, method, path, accessKey, secret, body string) *http.Request {
	t.Helper()
	amzDate := time.Now().UTC()
	dateStamp := amzDate.Format("20060102")
	host := "localhost:8443"
	payloadHash := hashSHA256Hex([]byte(body))

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("x-amz-date", amzDate.Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", payloadHash)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + host + "\nx-amz-content-sha256:" + payloadHash +
		"\nx-amz-date:" + amzDate.Format("20060102T150405Z") + "\n"
	canonicalRequest := strings.Join([]string{method, path, "", canonicalHeaders, signedHeaders, payloadHash}, "\n")
	scope := dateStamp + "/us-east-1/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate.Format("20060102T150405Z"), scope, hashSHA256Hex([]byte(canonicalRequest)),
	}, "\n")
	kDate := hmacSHA256Hex([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256Hex(kDate, "us-east-1")
	kService := hmacSHA256Hex(kRegion, "s3")
	kSigning := hmacSHA256Hex(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256Hex(kSigning, stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+dateStamp+
		"/us-east-1/s3/aws4_request, SignedHeaders="+signedHeaders+", Signature="+signature)
	return req
}

// mintTempCred drives the EXACT exchange derivation (MintAccessKey) so
// the test holds the same secret a SigV4 client would have received.
func mintTempCred(t *testing.T, key []byte, principal string, expiry time.Time, groups []string) (keyID, secret string) {
	t.Helper()
	k, s, err := sts.MintAccessKey(key, principal, expiry, groups)
	if err != nil {
		t.Fatalf("MintAccessKey: %v", err)
	}
	return k, s
}

// TestSigV4_TempCredentials_Table is the Task 2 table.
func TestSigV4_TempCredentials_Table(t *testing.T) {
	idp := newSTSTestIdP(t)
	reg := idp.registry(t)
	slot := sts.NewSigningKeySlot()
	slot.Store(&stsTestServerKey)
	resolver := sts.NewCredentialResolver(func() *auth.FederationRegistry { return reg }, slot)
	installTempCreds(t, resolver)

	username := "alice"
	groups := []string{"bkt-one"}
	principal := principalFor(idp.issuer, username)

	t.Run("valid temp cred authorizes per grants", func(t *testing.T) {
		keyID, secret := mintTempCred(t, stsTestServerKey, principal, time.Now().Add(time.Hour), groups)
		a := s3.New(nil).Authenticator()
		req := signTempCredRequest(t, "PUT", "/fed:bkt-one/obj.txt", keyID, secret, "hello world")
		id, err := a.Authenticate(req)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		// The identity's AccessKeyID is the ZSTSID- key id AS-IS: the
		// writer xattr breadcrumb (user.zeta.writer.<keyID>) records it.
		if id.AccessKeyID != keyID {
			t.Errorf("AccessKeyID = %q, want the presented ZSTSID- key id", id.AccessKeyID)
		}
		// Group grants key on the leaf-01 prefix form ("fed:"+group) —
		// GrantsFor's doc: the prefixes ARE the bucket names of the
		// frozen map. The per-user grant "<username>/" strips to the
		// bare username bucket.
		if !id.CanWrite("fed:bkt-one") || !id.CanRead("fed:bkt-one") {
			t.Error("group grant fed:bkt-one lost (want readwrite)")
		}
		if id.CanRead("other-bucket") {
			t.Error("identity can read an ungranted bucket")
		}
		if id.CanRead("bkt-one") {
			t.Error("grant bkt-one present without the fed: prefix (bucket-name drift)")
		}
		// The per-user grant: "alice/" bucket form.
		if !id.CanWrite("alice") {
			t.Error("per-user grant alice/ lost")
		}
	})

	t.Run("expired temp cred rejected", func(t *testing.T) {
		keyID, secret := mintTempCred(t, stsTestServerKey, principal, time.Now().Add(-time.Minute), groups)
		a := s3.New(nil).Authenticator()
		req := signTempCredRequest(t, "GET", "/bkt-one/obj.txt", keyID, secret, "")
		if _, err := a.Authenticate(req); err == nil {
			t.Fatal("expired credential accepted")
		}
	})

	t.Run("tampered payload rejected", func(t *testing.T) {
		_, secret := mintTempCred(t, stsTestServerKey, principal, time.Now().Add(time.Hour), groups)
		// Forge: a DIFFERENT principal's key id minted under an attacker
		// key (exactly what a holder without the server key produces).
		forgedKeyID, _, err := sts.MintAccessKey([]byte("attacker-key"), "fed:other.host/mallory", time.Now().Add(time.Hour), groups)
		if err != nil {
			t.Fatalf("mint forged: %v", err)
		}
		a := s3.New(nil).Authenticator()
		req := signTempCredRequest(t, "GET", "/bkt-one/obj.txt", forgedKeyID, secret, "")
		if _, err := a.Authenticate(req); err == nil {
			t.Fatal("tampered credential accepted")
		}
	})

	t.Run("registry unavailable fails closed", func(t *testing.T) {
		keyID, secret := mintTempCred(t, stsTestServerKey, principal, time.Now().Add(time.Hour), groups)
		installTempCreds(t, sts.NewCredentialResolver(func() *auth.FederationRegistry { return nil }, slot))
		a := s3.New(nil).Authenticator()
		req := signTempCredRequest(t, "GET", "/bkt-one/obj.txt", keyID, secret, "")
		if _, err := a.Authenticate(req); err == nil {
			t.Fatal("temp cred accepted with no registry (grants unresolvable)")
		}
	})

	t.Run("rotated signing key invalidates outstanding credential", func(t *testing.T) {
		keyID, secret := mintTempCred(t, stsTestServerKey, principal, time.Now().Add(time.Hour), groups)
		rotated := []byte("a-rotated-signing-key")
		slot.Store(&rotated)
		t.Cleanup(func() { slot.Store(&stsTestServerKey) })
		a := s3.New(nil).Authenticator()
		req := signTempCredRequest(t, "GET", "/bkt-one/obj.txt", keyID, secret, "")
		if _, err := a.Authenticate(req); err == nil {
			t.Fatal("credential accepted after signing-key rotation")
		}
	})

	t.Run("normal access keys unaffected (regression)", func(t *testing.T) {
		reg := registryFixture(t)
		installRegistry(t, reg)
		a := s3.New(nil).Authenticator()
		req := buildSignedRequestHelper(t, "AKBOT", "sk-bot", nil)
		id, err := a.Authenticate(req)
		if err != nil {
			t.Fatalf("registry key rejected with temp hook installed: %v", err)
		}
		if id.AccessKeyID != "AKBOT" {
			t.Errorf("AccessKeyID = %q", id.AccessKeyID)
		}
	})

	t.Run("ZSTSID key with no source installed is unknown (regression)", func(t *testing.T) {
		installTempCreds(t, nil)
		keyID, secret := mintTempCred(t, stsTestServerKey, principal, time.Now().Add(time.Hour), groups)
		a := s3.New(nil).Authenticator()
		req := signTempCredRequest(t, "GET", "/bkt-one/obj.txt", keyID, secret, "")
		if _, err := a.Authenticate(req); err == nil {
			t.Fatal("ZSTSID- key accepted with no resolver installed")
		}
	})
}

// TestSigV4_ExchangeToSigV4RoundTrip drives the FULL leaf-02 loop at the
// unit level: mint an ID token (test IdP), derive the credential exactly
// as the /sts exchange would, then authenticate a SigV4 request with it.
// The wire-level exchange itself is covered by the sts package's
// httptest suite; this pin proves the two halves share one derivation.
func TestSigV4_ExchangeToSigV4RoundTrip(t *testing.T) {
	idp := newSTSTestIdP(t)
	reg := idp.registry(t)
	slot := sts.NewSigningKeySlot()
	slot.Store(&stsTestServerKey)
	installTempCreds(t, sts.NewCredentialResolver(func() *auth.FederationRegistry { return reg }, slot))

	token := idp.mintIDToken(t, "bob", []string{"team-x"}, time.Now().Add(5*time.Minute))
	principal, claims, err := reg.ValidateIDToken(t.Context(), token)
	if err != nil {
		t.Fatalf("ValidateIDToken: %v", err)
	}
	// The exchange's exact mint step (sts.mint's core): principal +
	// default-claim groups + TTL.
	user, err := sts.UsernameFromPrincipal(principal)
	if err != nil {
		t.Fatalf("UsernameFromPrincipal: %v", err)
	}
	groups, _ := claims[sts.ClaimGroups].([]any)
	var gl []string
	for _, g := range groups {
		if s, ok := g.(string); ok {
			gl = append(gl, s)
		}
	}
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	keyID, secret, err := sts.MintAccessKey(stsTestServerKey, string(principal), expiry, gl)
	if err != nil {
		t.Fatalf("MintAccessKey: %v", err)
	}
	if keyID != sts.AccessKeyPrefix+strings.TrimPrefix(keyID, sts.AccessKeyPrefix) || user != "bob" {
		t.Fatalf("unexpected mint shape")
	}

	a := s3.New(nil).Authenticator()
	req := signTempCredRequest(t, "PUT", "/fed:team-x/report.txt", keyID, secret, "data")
	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate with exchanged credential: %v", err)
	}
	if !id.CanWrite("fed:team-x") {
		t.Error("exchanged credential cannot write the granted bucket")
	}
}
