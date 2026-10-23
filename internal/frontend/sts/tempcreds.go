// Package sts implements the STS token-exchange frontend (accessstor
// appliance tree leaf 02): clients present an OIDC ID token, the
// FederationRegistry (leaf 01) validates it, and the endpoint mints
// STATELESS temporary SigV4 credentials.
//
// Stateless discipline (charter-clean by design): the access key id is
// "ZSTSID-" + a self-describing payload; the secret is an HMAC of that
// payload under a server signing key. No session table, no stored state —
// the SigV4 path (internal/frontend/s3) re-derives the secret from the
// key id alone, enforces expiry, and re-resolves grants from the
// federation registry at request time. Revocation = grant removal (next
// request sees it); expiry = hard stop.
package sts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// AccessKeyPrefix marks a temporary STS credential on the wire. The S3
// frontend's SigV4 path branches on exactly this prefix.
const AccessKeyPrefix = "ZSTSID-"

// ClaimUsername / ClaimGroups are the federation claim names the
// STATELESS request-time reconstruction uses. They MUST mirror leaf 01's
// defaults (auth defaultUsernameClaim / defaultGroupsClaim — unexported
// there, pinned here; a drift breaks the GrantPrefixes pairing and the
// SigV4 path fails closed by length check). Custom claim names in
// zeta_federation only affect the exchange-time mapping, where the FULL
// claim set is available.
const (
	ClaimUsername = "preferred_username"
	ClaimGroups   = "groups"
)

// MaxKeyIDLen keeps the credential inside the xattr-name budget: the
// writer breadcrumb is "user.zeta.writer.<keyID>" (17-byte prefix) and
// xattr names cap at 255 bytes (internal/auth registry.go pins the same
// arithmetic for configured keys). Two bytes of margin.
const MaxKeyIDLen = 255 - len("user.zeta.writer.") - 2

// nonceLen is the mint-time nonce size (bytes): 128 bits of entropy so
// two mints for the same principal+expiry never collide.
const nonceLen = 16

// ErrNotTempCredential reports a key id without the ZSTSID- prefix (the
// caller falls through to the normal access-key path).
var ErrNotTempCredential = errors.New("sts: access key is not a ZSTSID- temporary credential")

// DeriveSecret HMACs the credential's canonical payload string under the
// server signing key. The result is the raw 32-byte MAC — the exchange
// returns it base64url-encoded (MintAccessKey), and the SigV4 path
// re-derives the same bytes through SecretString.
//
// DEVIATION from the leaf contract sketch (serverKey, principal, expiry,
// nonce): the canonical input is the FULL encoded payload, group list
// included. The groups ride inside the credential (stateless request-time
// grant re-resolution reconstructs claims from the key id alone), so they
// must be tamper-evident — an HMAC over only principal|expiry|nonce would
// let a holder rewrite their own group list undetected.
func DeriveSecret(serverKey []byte, canonicalPayload string) []byte {
	mac := hmac.New(sha256.New, serverKey)
	mac.Write([]byte(canonicalPayload))
	return mac.Sum(nil)
}

// MintAccessKey mints one temporary credential: the ZSTSID- key id
// encoding (principal, expiry, nonce, groups) and the derived secret in
// the exact base64url form the exchange returns. groups may be nil. The
// mint is pure derivation — nothing is stored.
func MintAccessKey(serverKey []byte, principal string, expiry time.Time, groups []string) (keyID, secret string, err error) {
	if strings.TrimSpace(principal) == "" {
		return "", "", fmt.Errorf("sts: principal is required")
	}
	if len(serverKey) == 0 {
		return "", "", fmt.Errorf("sts: signing key is empty")
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("sts: nonce: %w", err)
	}
	payload := payloadFor(principal, expiry, nonce, groups)
	keyID = AccessKeyPrefix + payload
	if len(keyID) > MaxKeyIDLen {
		return "", "", fmt.Errorf("sts: credential key id is %d bytes (max %d to stay inside the user.zeta.writer xattr-name budget); reduce the group list", len(keyID), MaxKeyIDLen)
	}
	return keyID, base64.RawURLEncoding.EncodeToString(DeriveSecret(serverKey, payload)), nil
}

// ParseAccessKey decodes a ZSTSID- key id into its parts. The prefix is
// mandatory (anything else is ErrNotTempCredential). Expiry is NOT
// checked here — the caller enforces it (the contract assigns the hard
// stop to the SigV4 path, and only it knows "now" for the request).
func ParseAccessKey(keyID string) (principal string, expiry time.Time, nonce []byte, groups []string, err error) {
	payload, ok := strings.CutPrefix(keyID, AccessKeyPrefix)
	if !ok {
		return "", time.Time{}, nil, nil, ErrNotTempCredential
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 {
		return "", time.Time{}, nil, nil, fmt.Errorf("sts: credential payload has %d segments, want 4", len(parts))
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", time.Time{}, nil, nil, fmt.Errorf("sts: principal segment: %w", err)
	}
	if len(pb) == 0 {
		return "", time.Time{}, nil, nil, fmt.Errorf("sts: principal segment is empty")
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, nil, nil, fmt.Errorf("sts: expiry segment: %w", err)
	}
	if unix <= 0 {
		return "", time.Time{}, nil, nil, fmt.Errorf("sts: expiry segment %d is not a positive unix timestamp", unix)
	}
	nonce, err = base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", time.Time{}, nil, nil, fmt.Errorf("sts: nonce segment: %w", err)
	}
	if len(nonce) == 0 {
		return "", time.Time{}, nil, nil, fmt.Errorf("sts: nonce segment is empty")
	}
	if parts[3] != "-" {
		gb, err := base64.RawURLEncoding.DecodeString(parts[3])
		if err != nil {
			return "", time.Time{}, nil, nil, fmt.Errorf("sts: groups segment: %w", err)
		}
		if err := json.Unmarshal(gb, &groups); err != nil {
			return "", time.Time{}, nil, nil, fmt.Errorf("sts: groups segment: %w", err)
		}
	}
	return string(pb), time.Unix(unix, 0), nonce, groups, nil
}

// SecretString re-derives the SigV4 signing secret for a ZSTSID- key id
// and returns it base64url-encoded — the same string the exchange handed
// out. Malformed key ids are an error; authenticity is NOT decided here
// (the SigV4 request signature is the authenticity proof: a tampered
// payload re-derives a different secret and the signature check fails).
func SecretString(serverKey []byte, keyID string) (string, error) {
	payload, err := payloadOf(keyID)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(DeriveSecret(serverKey, payload)), nil
}

// payloadOf strips the prefix and re-validates the structure cheaply
// (segment count and decodability) so DeriveSecret never sees arbitrary
// attacker bytes without the parse the mint used.
func payloadOf(keyID string) (string, error) {
	payload, ok := strings.CutPrefix(keyID, AccessKeyPrefix)
	if !ok {
		return "", ErrNotTempCredential
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 {
		return "", fmt.Errorf("sts: credential payload has %d segments, want 4", len(parts))
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		return "", fmt.Errorf("sts: principal segment: %w", err)
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return "", fmt.Errorf("sts: nonce segment: %w", err)
	}
	return payload, nil
}

// payloadFor renders the canonical credential payload:
//
//	b64u(principal) "." unix-expiry "." b64u(nonce) "." b64u(groupsJSON)|"-"
//
// Every segment is base64url (no "." in the alphabet), so the payload is
// self-delimiting and the WHOLE string is what DeriveSecret HMACs.
func payloadFor(principal string, expiry time.Time, nonce []byte, groups []string) string {
	g := "-"
	if len(groups) > 0 {
		if b, err := json.Marshal(groups); err == nil {
			g = base64.RawURLEncoding.EncodeToString(b)
		}
	}
	return base64.RawURLEncoding.EncodeToString([]byte(principal)) + "." +
		strconv.FormatInt(expiry.Unix(), 10) + "." +
		base64.RawURLEncoding.EncodeToString(nonce) + "." + g
}

// GrantPrefixes mirrors leaf 01's deterministic GrantsFor rule: each group
// contributes the bucket-style prefix "fed:"+g, the username contributes
// "<username>/", duplicates collapse, and the result is sorted. The SigV4
// path pairs these names (in order) with the registry's GrantsFor VALUES —
// the length check at the call site fails closed on any drift between the
// two rules.
func GrantPrefixes(username string, groups []string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, g := range groups {
		if strings.TrimSpace(g) != "" {
			add("fed:" + g)
		}
	}
	if strings.TrimSpace(username) != "" {
		add(username + "/")
	}
	sort.Strings(out)
	return out
}

// BucketForPrefix translates a grant prefix into the bucket name the
// frozen v1 grant map keys on: a trailing "/" (leaf 01's per-user form
// "<username>/") is stripped, group prefixes ("fed:team-a") pass through
// as-is. The appliance maps federated shares to buckets by the same rule.
func BucketForPrefix(prefix string) string {
	return strings.TrimSuffix(prefix, "/")
}

// UsernameFromPrincipal recovers the username from leaf 01's principal
// form "fed:<issuer-host>/<username>". The host never contains "/", so
// the FIRST slash after the "fed:" prefix is the separator.
func UsernameFromPrincipal(p auth.Principal) (string, error) {
	rest, ok := strings.CutPrefix(string(p), "fed:")
	if !ok || rest == "" {
		return "", fmt.Errorf("sts: principal %q is not fed:<host>/<username>", string(p))
	}
	host, user, found := strings.Cut(rest, "/")
	if !found || host == "" || user == "" {
		return "", fmt.Errorf("sts: principal %q is not fed:<host>/<username>", string(p))
	}
	return user, nil
}

// SessionToken mints the opaque session_token the exchange returns for
// AWS-SDK wire compatibility. It is NOT validated anywhere — the SigV4
// path authenticates the ZSTSID- key id + derived secret alone (documented
// leaf note). Random bytes, stateless, no server-side counterpart.
func SessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sts: session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
