// publickey_test.go — CanonicalizePublicKey + PublicKeyAuthenticator
// (pluggable-authentication tree leaf 04): authorized_keys-format
// canonicalization, fingerprints, typed errors, log-safe rendering.
// Fixtures are hand-embedded obviously-fake blobs — never real key material.
package auth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// fakeBlob builds a valid-base64 obviously-fake key blob.
func fakeBlob(tag string) string {
	return base64.RawStdEncoding.EncodeToString([]byte("fake-key-blob-" + tag))
}

const (
	fakeType1 = "ssh-ed25519"
	fakeType2 = "ssh-rsa"
)

func TestCanonicalizePublicKey(t *testing.T) {
	blob := fakeBlob("1")
	cases := []struct {
		name      string
		input     string
		wantCanon string
		wantErr   bool
		wantErrIs error
	}{
		{
			name:      "bare type+blob",
			input:     fakeType1 + " " + blob,
			wantCanon: fakeType1 + " " + blob,
		},
		{
			name:      "comment stripped",
			input:     fakeType1 + " " + blob + " user@host",
			wantCanon: fakeType1 + " " + blob,
		},
		{
			name:      "trailing newline tolerated",
			input:     fakeType1 + " " + blob + " user@host\n",
			wantCanon: fakeType1 + " " + blob,
		},
		{
			name:      "options prefix stripped",
			input:     `environment="X" no-port-forwarding ` + fakeType1 + " " + blob + " comment",
			wantCanon: fakeType1 + " " + blob,
		},
		{
			name:      "whitespace variance collapses",
			input:     fakeType1 + "\t" + blob + "   ",
			wantCanon: fakeType1 + " " + blob,
		},
		{
			name:      "keytype case normalized",
			input:     "SSH-ED25519 " + blob,
			wantCanon: fakeType1 + " " + blob,
		},
		{
			// A2 pin: blob length is 16 (NOT a multiple of 3), so the padded
			// StdEncoding form genuinely differs from the raw form. Both must
			// canonicalize to keytype + StdEncoding(decoded blob).
			name:      "padded blob same canonical as raw",
			input:     fakeType1 + " " + base64.StdEncoding.EncodeToString([]byte("fake-key-blob-1x")),
			wantCanon: fakeType1 + " " + base64.StdEncoding.EncodeToString([]byte("fake-key-blob-1x")),
		},
		{name: "empty", input: "", wantErr: true},
		{name: "garbage", input: "not-a-key", wantErr: true},
		{name: "no blob", input: fakeType1, wantErr: true},
		{name: "non-base64 blob", input: fakeType1 + " !!!!", wantErr: true},
		{name: "unknown keytype", input: "weird-alg " + blob, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon, fp, err := auth.CanonicalizePublicKey(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got canon=%q fp=%q", canon, fp)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if canon != tc.wantCanon {
				t.Fatalf("canonical = %q, want %q", canon, tc.wantCanon)
			}
			if !strings.HasPrefix(fp, "SHA256:") {
				t.Fatalf("fingerprint = %q, want SHA256: prefix", fp)
			}
			// Fingerprint is deterministic: SHA256 of the decoded blob,
			// taken from the canonical form's blob field.
			parts := strings.SplitN(canon, " ", 2)
			raw, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatalf("canonical blob not valid std base64: %v", err)
			}
			sum := sha256.Sum256(raw)
			want := "SHA256:" + base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString(sum[:])
			if fp != want {
				t.Fatalf("fingerprint = %q, want %q", fp, want)
			}
		})
	}
}

// TestCanonicalizeSameBlobDifferentComments pins the dedupe target: the
// same blob with different comments canonicalizes identically; a different
// blob does not.
func TestCanonicalizeSameBlobDifferentComments(t *testing.T) {
	blob := fakeBlob("2")
	c1, _, err := auth.CanonicalizePublicKey(fakeType1 + " " + blob + " a@host")
	if err != nil {
		t.Fatal(err)
	}
	c2, _, err := auth.CanonicalizePublicKey(fakeType1 + " " + blob + " b@host")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatalf("same blob, different comments: %q vs %q", c1, c2)
	}
	c3, _, err := auth.CanonicalizePublicKey(fakeType2 + " " + fakeBlob("3") + " a@host")
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c3 {
		t.Fatal("different blobs canonicalized the same")
	}
}

// TestCanonicalizePaddingVariantsMatch pins the A2 fix with a blob whose
// length is NOT a multiple of 3, so padded and raw encodings really differ:
// padded-registration + raw-presentation and raw-registration +
// padded-presentation must both match, with one canonical form and one
// fingerprint for the same key bytes.
func TestCanonicalizePaddingVariantsMatch(t *testing.T) {
	blobBytes := []byte("fake-key-blob-not-mult-3!") // 25 bytes
	raw := base64.RawStdEncoding.EncodeToString(blobBytes)
	padded := base64.StdEncoding.EncodeToString(blobBytes)
	if raw == padded {
		t.Fatal("test fixture broken: raw and padded encodings are identical")
	}

	canonPadded, fpPadded, err := auth.CanonicalizePublicKey(fakeType1 + " " + padded)
	if err != nil {
		t.Fatal(err)
	}
	canonRaw, fpRaw, err := auth.CanonicalizePublicKey(fakeType1 + " " + raw)
	if err != nil {
		t.Fatal(err)
	}
	if canonPadded != canonRaw {
		t.Fatalf("canonical forms differ: %q vs %q", canonPadded, canonRaw)
	}
	if fpPadded != fpRaw {
		t.Fatalf("fingerprints differ: %q vs %q", fpPadded, fpRaw)
	}
	// Canonical form is the std-padded re-encoding of the decoded blob.
	if canonPadded != fakeType1+" "+padded {
		t.Fatalf("canonical = %q, want %q", canonPadded, fakeType1+" "+padded)
	}
}

// TestPublicKeyPaddingRoundTrip pins the registry-level A2 fix: a key
// registered in one padding variant authenticates when presented in the
// other, in both directions.
func TestPublicKeyPaddingRoundTrip(t *testing.T) {
	blobBytes := []byte("fake-key-blob-not-mult-3!")
	raw := base64.RawStdEncoding.EncodeToString(blobBytes)
	padded := base64.StdEncoding.EncodeToString(blobBytes)

	newReg := func(regKey string) *auth.MultiRegistry {
		t.Helper()
		reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
			{Name: "sftp-user", AccessKey: "AKSFTP", SecretKey: "sk", SSHPublicKeys: []string{fakeType1 + " " + regKey}},
		})
		if err != nil {
			t.Fatalf("NewMultiRegistry: %v", err)
		}
		return reg
	}

	// Padded registration, raw presentation.
	reg := newReg(padded)
	if _, err := reg.AuthenticatePublicKey(fakeType1 + " " + raw); err != nil {
		t.Fatalf("padded-registered key rejected when presented raw: %v", err)
	}
	// Raw registration, padded presentation.
	reg = newReg(raw)
	if _, err := reg.AuthenticatePublicKey(fakeType1 + " " + padded); err != nil {
		t.Fatalf("raw-registered key rejected when presented padded: %v", err)
	}
}

func TestMultiRegistryAuthenticatePublicKey(t *testing.T) {
	blob := fakeBlob("reg")
	withComment := fakeType1 + " " + blob + " ops@host"
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{
			Name: "sftp-user", AccessKey: "AKSFTP", SecretKey: "sk",
			Grants:        map[string]string{"dropbox": "readwrite"},
			SSHPublicKeys: []string{withComment},
		},
	})
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}

	// Bare form hits the commented registered line.
	id, err := reg.AuthenticatePublicKey(fakeType1 + " " + blob)
	if err != nil {
		t.Fatalf("AuthenticatePublicKey: %v", err)
	}
	if id.AccessKeyID != "AKSFTP" || !id.CanWrite("dropbox") {
		t.Errorf("identity = %+v", id)
	}

	// Valid but unregistered → ErrKeyUnknown.
	if _, err := reg.AuthenticatePublicKey(fakeType1 + " " + fakeBlob("other")); !errors.Is(err, auth.ErrKeyUnknown) {
		t.Fatalf("err = %v, want ErrKeyUnknown", err)
	}

	// Malformed → ErrKeyMalformed (never a silent miss).
	if _, err := reg.AuthenticatePublicKey("garbage-line"); !errors.Is(err, auth.ErrKeyMalformed) {
		t.Fatalf("err = %v, want ErrKeyMalformed", err)
	}

	// Compile-time: MultiRegistry implements the hook.
	var _ auth.PublicKeyAuthenticator = reg
}

// TestLogSafeKey pins the log-safe rendering: no key material leaks.
func TestLogSafeKey(t *testing.T) {
	blob := fakeBlob("log")
	_, fp, err := auth.CanonicalizePublicKey(fakeType1 + " " + blob)
	if err != nil {
		t.Fatal(err)
	}
	line := auth.LogSafeKey("AKSFTP", fp)
	if !strings.Contains(line, "accessKeyID=AKSFTP") || !strings.Contains(line, "fingerprint="+fp) {
		t.Fatalf("LogSafeKey = %q", line)
	}
	if strings.Contains(line, blob) || strings.Contains(line, fakeType1) {
		t.Fatalf("key material leaked into %q", line)
	}
}

// A3: a quoted option value containing a keytype-looking token + base64
// must NOT hijack the parse — the real key after the options is the one
// bound. (The old strings.Fields split found the inner token first.)
func TestCanonicalizeQuotedOptionDoesNotHijack(t *testing.T) {
	goodBlob := fakeBlob("good")
	evilBlob := fakeBlob("evil")
	line := `command="echo ssh-ed25519 ` + evilBlob + ` extra",restrict ssh-ed25519 ` + goodBlob + ` comment`
	canonical, _, err := auth.CanonicalizePublicKey(line)
	if err != nil {
		t.Fatalf("CanonicalizePublicKey(%q) error = %v", line, err)
	}
	if !strings.Contains(canonical, goodBlob) {
		t.Fatalf("canonical %q does not bind the REAL key blob", canonical)
	}
	if strings.Contains(canonical, evilBlob) {
		t.Fatalf("canonical %q bound the quoted option's blob", canonical)
	}
}

// A3 companion: unquoted multi-option lines (the common real-world shape)
// still parse, and the keytype is found after the options.
func TestCanonicalizeUnquotedOptionsStillParse(t *testing.T) {
	blob := fakeBlob("opts")
	line := `no-port-forwarding,command="internal-sftp" ssh-ed25519 ` + blob
	canonical, _, err := auth.CanonicalizePublicKey(line)
	if err != nil {
		t.Fatalf("CanonicalizePublicKey(%q) error = %v", line, err)
	}
	want := "ssh-ed25519 " + blob
	if canonical != want {
		t.Fatalf("canonical = %q, want %q", canonical, want)
	}
}
