// tempcreds_test.go — the stateless credential derivation/parsing core
// (leaf 02 Task 1): round-trip, tamper rejection, expiry surface, prefix
// and malformed-input rejection, nonce uniqueness, and the payload shape
// the SigV4 path relies on.
package sts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testServerKey(t *testing.T) []byte {
	t.Helper()
	return []byte("sts-test-signing-key-0123456789abcdef")
}

// TestTempCreds_Table pins the derive/parse contract per case.
func TestTempCreds_Table(t *testing.T) {
	key := testServerKey(t)
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	groups := []string{"team-a", "team-b"}

	validKeyID, validSecret, err := MintAccessKey(key, "fed:idp.example/alice", expiry, groups)
	if err != nil {
		t.Fatalf("MintAccessKey: %v", err)
	}

	t.Run("round trip", func(t *testing.T) {
		principal, gotExpiry, nonce, gotGroups, err := ParseAccessKey(validKeyID)
		if err != nil {
			t.Fatalf("ParseAccessKey: %v", err)
		}
		if principal != "fed:idp.example/alice" {
			t.Errorf("principal = %q", principal)
		}
		if !gotExpiry.Equal(expiry) {
			t.Errorf("expiry = %v, want %v", gotExpiry, expiry)
		}
		if len(nonce) != nonceLen {
			t.Errorf("nonce len = %d, want %d", len(nonce), nonceLen)
		}
		if strings.Join(gotGroups, ",") != strings.Join(groups, ",") {
			t.Errorf("groups = %v, want %v", gotGroups, groups)
		}
	})

	t.Run("secret is derived bytes of the payload", func(t *testing.T) {
		payload, err := payloadOf(validKeyID)
		if err != nil {
			t.Fatalf("payloadOf: %v", err)
		}
		want := base64.RawURLEncoding.EncodeToString(DeriveSecret(key, payload))
		if validSecret != want {
			t.Error("minted secret does not match DeriveSecret of the encoded payload")
		}
	})

	t.Run("wrong key derives different secret", func(t *testing.T) {
		otherKey := []byte("a-different-server-key")
		otherSecret, err := SecretString(otherKey, validKeyID)
		if err != nil {
			t.Fatalf("SecretString: %v", err)
		}
		if otherSecret == validSecret {
			t.Fatal("secrets under different keys must differ")
		}
	})

	t.Run("tampered payload yields different secret", func(t *testing.T) {
		// Flip one character inside the base64 principal segment: the
		// secret must change (the signature check downstream then fails).
		raw := validKeyID[len(AccessKeyPrefix):]
		mutated := raw
		for i := range len(raw) {
			if raw[i] == 'A' {
				mutated = raw[:i] + "B" + raw[i+1:]
				break
			}
		}
		if mutated == raw {
			t.Skip("no mutable character in this payload")
		}
		got, err := SecretString(key, AccessKeyPrefix+mutated)
		if err != nil {
			// A mutation can also break segment validity — still a reject.
			return
		}
		if got == validSecret {
			t.Fatal("tampered payload derived the original secret")
		}
	})

	t.Run("expiry surfaces but is not enforced by parse", func(t *testing.T) {
		past := time.Now().Add(-time.Minute).Truncate(time.Second)
		keyID, _, err := MintAccessKey(key, "fed:idp.example/bob", past, nil)
		if err != nil {
			t.Fatalf("MintAccessKey: %v", err)
		}
		_, gotExpiry, _, _, err := ParseAccessKey(keyID)
		if err != nil {
			t.Fatalf("ParseAccessKey: %v", err)
		}
		if !gotExpiry.Before(time.Now()) {
			t.Errorf("expiry = %v, want in the past", gotExpiry)
		}
	})

	t.Run("non-ZSTSID prefix rejected", func(t *testing.T) {
		for _, keyID := range []string{"AKIAEXAMPLE", "", "zstsid-lowercase", "ZSTSID"} {
			_, _, _, _, err := ParseAccessKey(keyID)
			if err == nil {
				t.Errorf("ParseAccessKey(%q) accepted a non-ZSTSID key", keyID)
			}
		}
	})

	t.Run("malformed payloads rejected", func(t *testing.T) {
		cases := map[string]string{
			"empty payload":         AccessKeyPrefix,
			"too few segments":      AccessKeyPrefix + "aaa.bbb",
			"too many segments":     AccessKeyPrefix + "a.b.c.d.e",
			"undecodable principal": AccessKeyPrefix + "!!.invalid.MTA.-",
			"undecodable nonce":     AccessKeyPrefix + "YWxpY2UuMTcwMDAwLSEhaW52YWxpZC4t",
			"non-numeric expiry":    AccessKeyPrefix + "alice.notanumber.MTA.-",
			"non-positive expiry":   AccessKeyPrefix + "YWxpY2U.MTA.MTA.-",
			"empty principal":       AccessKeyPrefix + ".MTA.MTA.-",
			"empty nonce":           AccessKeyPrefix + "YWxpY2U.MTA..-",
			"undecodable groups":    AccessKeyPrefix + "YWxpY2U.MTA.MTA.!!bad!!",
			"groups not json":       AccessKeyPrefix + "YWxpY2U.MTA.MTA.Ww",
		}
		for name, keyID := range cases {
			if _, _, _, _, err := ParseAccessKey(keyID); err == nil {
				t.Errorf("%s: ParseAccessKey(%q) accepted", name, keyID)
			}
		}
	})

	t.Run("nonce uniqueness - two derives differ", func(t *testing.T) {
		k1, s1, err := MintAccessKey(key, "fed:idp.example/alice", expiry, groups)
		if err != nil {
			t.Fatalf("mint 1: %v", err)
		}
		k2, s2, err := MintAccessKey(key, "fed:idp.example/alice", expiry, groups)
		if err != nil {
			t.Fatalf("mint 2: %v", err)
		}
		if k1 == k2 || s1 == s2 {
			t.Fatal("two mints with identical inputs produced identical credentials (nonce not random)")
		}
	})

	t.Run("secret derivable from key id alone", func(t *testing.T) {
		// The SigV4 path's exact contract: re-derive from the presented
		// key id, get the string the exchange returned.
		got, err := SecretString(key, validKeyID)
		if err != nil {
			t.Fatalf("SecretString: %v", err)
		}
		if got != validSecret {
			t.Fatal("SecretString mismatch")
		}
	})

	t.Run("payload segments are base64url and dot-delimited", func(t *testing.T) {
		raw := strings.TrimPrefix(validKeyID, AccessKeyPrefix)
		parts := strings.Split(raw, ".")
		if len(parts) != 4 {
			t.Fatalf("segment count = %d, want 4", len(parts))
		}
		for i, p := range parts {
			if p == "-" {
				continue
			}
			if _, err := base64.RawURLEncoding.DecodeString(p); err != nil {
				t.Errorf("segment %d not base64url: %v", i, err)
			}
		}
		if _, err := json.Marshal(parts); err != nil {
			t.Errorf("payload not representable: %v", err)
		}
	})
}

// TestGrantPrefixes pins the request-time reconstruction of leaf 01's
// deterministic grant rule (sorted, deduped, "fed:"+g then "<user>/").
func TestGrantPrefixes(t *testing.T) {
	cases := []struct {
		name     string
		username string
		groups   []string
		want     []string
	}{
		{"groups and user", "alice", []string{"team-b", "team-a"}, []string{"alice/", "fed:team-a", "fed:team-b"}},
		{"no groups", "bob", nil, []string{"bob/"}},
		{"dedup", "carol", []string{"dup", "dup"}, []string{"carol/", "fed:dup"}},
		{"empty group skipped", "dave", []string{""}, []string{"dave/"}},
		{"nothing at all", "", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GrantPrefixes(tc.username, tc.groups)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("GrantPrefixes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBucketForPrefix(t *testing.T) {
	if got := BucketForPrefix("alice/"); got != "alice" {
		t.Errorf("user prefix -> %q", got)
	}
	if got := BucketForPrefix("fed:team-a"); got != "fed:team-a" {
		t.Errorf("group prefix -> %q", got)
	}
}

func TestUsernameFromPrincipal(t *testing.T) {
	u, err := UsernameFromPrincipal("fed:idp.example.com/alice")
	if err != nil || u != "alice" {
		t.Errorf("got %q, %v", u, err)
	}
	if _, err := UsernameFromPrincipal("zetaadmin"); err == nil {
		t.Error("non-fed principal accepted")
	}
	if _, err := UsernameFromPrincipal("fed:host-only"); err == nil {
		t.Error("principal without username accepted")
	}
}

func TestMintAccessKey_RejectsBadInput(t *testing.T) {
	if _, _, err := MintAccessKey(testServerKey(t), "  ", time.Now(), nil); err == nil {
		t.Error("empty principal accepted")
	}
	if _, _, err := MintAccessKey(nil, "fed:h/u", time.Now(), nil); err == nil {
		t.Error("empty signing key accepted")
	}
}

// TestMaxKeyIDLen pins the xattr-budget arithmetic (mirrors
// internal/auth registry.go's boundary check for configured keys).
func TestMaxKeyIDLen(t *testing.T) {
	if len("user.zeta.writer.")+MaxKeyIDLen >= 255 {
		t.Fatalf("MaxKeyIDLen %d breaches the xattr-name budget", MaxKeyIDLen)
	}
}
