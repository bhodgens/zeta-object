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
			name:      "padded blob same canonical as raw",
			input:     fakeType1 + " " + base64.StdEncoding.EncodeToString([]byte("fake-key-blob-1")),
			wantCanon: fakeType1 + " " + blob,
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
			// Fingerprint is deterministic: SHA256 of the decoded blob.
			raw, _ := base64.RawStdEncoding.DecodeString(blob)
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
