// basic_test.go — BasicAuthenticator (pluggable-authentication tree leaf 03
// Task 1): header parsing, typed rejections, registry hit carrying grants,
// bad-username/bad-password indistinguishability.
package auth_test

import (
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

func newTestRegistry(t *testing.T) *auth.MultiRegistry {
	t.Helper()
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "env", AccessKey: "AKRW", SecretKey: "sk-rw"},
		{Name: "ro", AccessKey: "AKRO", SecretKey: "sk-ro", Grants: map[string]string{"neuro": "readonly"}},
	})
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}
	return reg
}

// basicHeader builds an Authorization header value from a user:password
// pair (encoded=false injects deliberately malformed payloads).
func basicHeader(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func TestBasicAuthenticator(t *testing.T) {
	b := auth.NewBasicAuthenticator(newTestRegistry(t))
	cases := []struct {
		name    string
		header  string
		wantErr error
		wantAK  string
	}{
		{
			name:   "valid wildcard identity",
			header: basicHeader("AKRW", "sk-rw"),
			wantAK: "AKRW",
		},
		{
			name:   "valid scoped identity carries grants",
			header: basicHeader("AKRO", "sk-ro"),
			wantAK: "AKRO",
		},
		{name: "missing header", header: "", wantErr: auth.ErrBasicMissing},
		{name: "bearer scheme", header: "Bearer abc", wantErr: auth.ErrBasicMalformed},
		{name: "garbage base64", header: "Basic !!!!not-base64!!!!", wantErr: auth.ErrBasicMalformed},
		{name: "no colon in pair", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("justausername")), wantErr: auth.ErrBasicMalformed},
		{name: "unknown user", header: basicHeader("AKNOPE", "x"), wantErr: auth.ErrBadCredentials},
		{name: "wrong password", header: basicHeader("AKRW", "wrong"), wantErr: auth.ErrBadCredentials},
		{name: "empty password", header: basicHeader("AKRW", ""), wantErr: auth.ErrBadCredentials},
		{
			name:   "unpadded base64 tolerated",
			header: "Basic " + base64.RawStdEncoding.EncodeToString([]byte("AKRW:sk-rw")),
			wantAK: "AKRW",
		},
		{
			name:   "extra whitespace tolerated",
			header: "Basic   " + base64.StdEncoding.EncodeToString([]byte("AKRW:sk-rw")) + " ",
			wantAK: "AKRW",
		},
		{
			name:    "password containing colon splits on first colon",
			header:  "Basic " + base64.StdEncoding.EncodeToString([]byte("AKRW:sk:rw:extra")),
			wantErr: auth.ErrBadCredentials, // "sk:rw:extra" != "sk-rw" — parse must not fail
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			id, err := b.Authenticate(r)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if id.AccessKeyID != tc.wantAK {
				t.Fatalf("AccessKeyID = %q, want %q", id.AccessKeyID, tc.wantAK)
			}
		})
	}
}

// TestBasicAuthenticatorScopedGrants pins that the Basic surface returns
// the SAME grant decisions as the access-key surface for the same identity.
func TestBasicAuthenticatorScopedGrants(t *testing.T) {
	b := auth.NewBasicAuthenticator(newTestRegistry(t))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", basicHeader("AKRO", "sk-ro"))
	id, err := b.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !id.CanRead("neuro") {
		t.Error("AKRO cannot read neuro")
	}
	if id.CanWrite("neuro") || id.CanRead("other") {
		t.Errorf("grants wrong: %+v", id.BucketGrants)
	}
}

// TestBasicBadUserBadPasswordIndistinguishable pins the sentinel contract:
// both failure shapes are exactly ErrBadCredentials (never a distinct
// unknown-user error).
func TestBasicBadUserBadPasswordIndistinguishable(t *testing.T) {
	b := auth.NewBasicAuthenticator(newTestRegistry(t))
	mk := func(user, pass string) error {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", basicHeader(user, pass))
		_, err := b.Authenticate(r)
		return err
	}
	badUser := mk("AKNOPE", "whatever")
	badPass := mk("AKRW", "wrong")
	if !errors.Is(badUser, auth.ErrBadCredentials) || !errors.Is(badPass, auth.ErrBadCredentials) {
		t.Fatalf("badUser=%v badPass=%v, both must be ErrBadCredentials", badUser, badPass)
	}
	if badUser.Error() != badPass.Error() {
		t.Fatalf("error strings differ: %q vs %q", badUser, badPass)
	}
}

// Compile-time: satisfies the frozen v1 Authenticator seam.
var _ auth.Authenticator = (*auth.BasicAuthenticator)(nil)
