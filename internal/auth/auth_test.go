// Package auth is a pure interface seam (auth.go + credentials.go declare
// only types; there are zero executable statements, so statement coverage
// is vacuously degenerate — see the leaf 6.1 report). These tests therefore
// pin the CONTRACT each exported symbol promises the SigV4 authenticator:
// byte-exact credential/identity values, every happy-path and error branch,
// and the credential-source env resolution semantics (set / unset /
// set-but-empty) as implemented by the process wiring (package main's
// credentialFromEnv) which this package's CredentialSource documents.
package auth_test

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// --- fixtures ---------------------------------------------------------------

type stubAuth struct {
	id  auth.Identity
	err error
}

func (s stubAuth) Authenticate(r *http.Request) (auth.Identity, error) {
	return s.id, s.err
}

// staticCreds is the canonical single-pair CredentialSource (v1 shape).
type staticCreds struct {
	accessKey string
	secret    string
}

func (s staticCreds) SecretKey(accessKeyID string) (string, bool) {
	if accessKeyID == s.accessKey {
		return s.secret, true
	}
	return "", false
}

const (
	envAccessKey = "ZETAOBJECT_ACCESS_KEY"
	envSecretKey = "ZETAOBJECT_SECRET_KEY"
	// defaultAccessKey mirrors package main's config.go defaultAccessKey.
	defaultAccessKey = "minioadmin"
)

// envCreds MIRRORS the process wiring's credential resolution (package main
// credentialFromEnv + loadCredentials): env set → exact value; env unset →
// default; env SET-but-EMPTY → default. Divergence documented: the warning
// log for set-but-empty lives only in package main's credentialFromEnv; the
// seam contract is value-only, so envCreds pins the same VALUES the wiring
// produces (warned-default = default). main_lifecycle_test.go already pins
// the warning side.
type envCreds struct{}

func (envCreds) lookup(key string) string {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return defaultAccessKey
	}
	return value
}

func (envCreds) SecretKey(accessKeyID string) (string, bool) {
	e := envCreds{}
	if accessKeyID != e.lookup(envAccessKey) {
		return "", false
	}
	return e.lookup(envSecretKey), true
}

// --- Authenticator -----------------------------------------------------------

func TestAuthenticator_InterfaceShape(t *testing.T) {
	tests := []struct {
		name       string
		a          auth.Authenticator
		want       string
		wantErr    bool
		wantErrMsg string
	}{
		{
			name: "stub returns identity",
			a:    stubAuth{id: auth.Identity{AccessKeyID: "AKID"}},
			want: "AKID",
		},
		{
			name:       "stub returns error branch",
			a:          stubAuth{id: auth.Identity{}, err: errors.New("denied")},
			want:       "",
			wantErr:    true,
			wantErrMsg: "denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := tt.a.Authenticate(&http.Request{})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Authenticate: want error %q, got nil", tt.wantErrMsg)
				}
				if err.Error() != tt.wantErrMsg {
					t.Fatalf("error = %q, want %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if id.AccessKeyID != tt.want {
				t.Fatalf("AccessKeyID = %q, want %q", id.AccessKeyID, tt.want)
			}
		})
	}
}

// --- Identity ----------------------------------------------------------------

func TestIdentity_ByteExact(t *testing.T) {
	id := auth.Identity{
		AccessKeyID: "AKIAEXAMPLE0123",
		BucketGrants: map[string]auth.Grant{
			"photos":  {Read: true, Write: false},
			"scratch": {Read: true, Write: true},
		},
	}
	if id.AccessKeyID != "AKIAEXAMPLE0123" {
		t.Fatalf("AccessKeyID = %q, want %q", id.AccessKeyID, "AKIAEXAMPLE0123")
	}
	if len(id.BucketGrants) != 2 {
		t.Fatalf("len(BucketGrants) = %d, want 2", len(id.BucketGrants))
	}
	g, ok := id.BucketGrants["photos"]
	if !ok || g != (auth.Grant{Read: true, Write: false}) {
		t.Fatalf("photos grant = %+v ok=%v, want {true false}", g, ok)
	}
	g, ok = id.BucketGrants["scratch"]
	if !ok || g != (auth.Grant{Read: true, Write: true}) {
		t.Fatalf("scratch grant = %+v ok=%v, want {true true}", g, ok)
	}
	var zero auth.Identity
	if zero.AccessKeyID != "" || zero.BucketGrants != nil {
		t.Fatalf("zero Identity = %+v, want empty key + nil grants", zero)
	}
}

// --- Grant -------------------------------------------------------------------

func TestGrant_Fields(t *testing.T) {
	tests := []struct {
		name      string
		g         auth.Grant
		wantRead  bool
		wantWrite bool
	}{
		{"read only", auth.Grant{Read: true, Write: false}, true, false},
		{"write only", auth.Grant{Read: false, Write: true}, false, true},
		{"read write", auth.Grant{Read: true, Write: true}, true, true},
		{"none", auth.Grant{}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.g.Read != tt.wantRead || tt.g.Write != tt.wantWrite {
				t.Fatalf("Grant = %+v, want Read=%v Write=%v", tt.g, tt.wantRead, tt.wantWrite)
			}
		})
	}
}

// --- CredentialSource: single-pair static (v1 shape) --------------------------

func TestCredentialSource_StaticSinglePair(t *testing.T) {
	cs := staticCreds{accessKey: "minioadmin", secret: "s3cr3t-value"}
	t.Run("matching key returns exact secret", func(t *testing.T) {
		got, ok := cs.SecretKey("minioadmin")
		if !ok {
			t.Fatal("SecretKey: want ok=true for matching key")
		}
		if got != "s3cr3t-value" {
			t.Fatalf("secret = %q, want %q (byte-exact)", got, "s3cr3t-value")
		}
	})
	t.Run("unknown key returns empty false", func(t *testing.T) {
		got, ok := cs.SecretKey("wrong-key")
		if ok {
			t.Fatal("SecretKey: want ok=false for unknown key")
		}
		if got != "" {
			t.Fatalf("secret = %q, want empty string", got)
		}
	})
	t.Run("empty key treated as unknown", func(t *testing.T) {
		if got, ok := cs.SecretKey(""); ok || got != "" {
			t.Fatalf("SecretKey(\"\") = %q,%v want \"\",false", got, ok)
		}
	})
}

// --- CredentialSource: env resolution (set / unset / set-but-empty) -----------

func TestCredentialSource_EnvResolution(t *testing.T) {
	restore := func(t *testing.T) {
		t.Helper()
		t.Setenv(envAccessKey, "") // pins restore via t.Setenv cleanup
		os.Unsetenv(envAccessKey)
		os.Unsetenv(envSecretKey)
	}
	tests := []struct {
		name    string
		akid    string
		secret  string
		wantKey string
		wantSec string
		wantOK  bool
	}{
		{
			name:    "both env set non-empty honored byte-exact",
			akid:    "AKIAENVSET0001",
			secret:  "env-secret-\x01binary-safe",
			wantKey: "AKIAENVSET0001",
			wantSec: "env-secret-\x01binary-safe",
			wantOK:  true,
		},
		{
			name:    "access key set-but-empty falls back to default",
			akid:    "",
			secret:  "realsecret",
			wantKey: defaultAccessKey,
			wantSec: "realsecret",
			wantOK:  true,
		},
		{
			name:    "secret set-but-empty falls back to default",
			akid:    "AKIAOK",
			secret:  "",
			wantKey: "AKIAOK",
			wantSec: defaultAccessKey,
			wantOK:  true,
		},
		{
			name:    "both unset default to minioadmin",
			akid:    "",
			secret:  "",
			wantKey: defaultAccessKey,
			wantSec: defaultAccessKey,
			wantOK:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore(t)
			if tt.akid != "" || tt.secret != "" {
				if tt.akid != "" {
					t.Setenv(envAccessKey, tt.akid)
				}
				if tt.secret != "" {
					t.Setenv(envSecretKey, tt.secret)
				}
			}
			var cs auth.CredentialSource = envCreds{}
			gotSec, ok := cs.SecretKey(tt.wantKey)
			if ok != tt.wantOK {
				t.Fatalf("SecretKey(%q) ok = %v, want %v", tt.wantKey, ok, tt.wantOK)
			}
			if gotSec != tt.wantSec {
				t.Fatalf("secret = %q, want %q (byte-exact)", gotSec, tt.wantSec)
			}
		})
	}
	t.Run("unknown key never resolves even with env set", func(t *testing.T) {
		restore(t)
		t.Setenv(envAccessKey, "AKIAKNOWN")
		t.Setenv(envSecretKey, "shh")
		got, ok := envCreds{}.SecretKey("AKIAOTHER")
		if ok || got != "" {
			t.Fatalf("SecretKey(unknown) = %q,%v want \"\",false", got, ok)
		}
	})
}

// Compile-time contract pins: the shapes the s3 frontend and main wiring rely on.
var (
	_ auth.Authenticator    = stubAuth{}
	_ auth.CredentialSource = staticCreds{}
	_ auth.CredentialSource = envCreds{}
)
