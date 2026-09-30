// frontend_test.go — leaf 01 Task 1: frozen-contract tests over New,
// Name/Capabilities/Authenticator, and registry round-trip.
package webdav

import (
	"errors"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend"
)

func TestNew_NilBackendFails(t *testing.T) {
	if _, err := New(nil, Config{}, WithAuthenticator(newStubAuth())); err == nil {
		t.Fatal("New(nil backend) must fail")
	}
}

func TestNew_NilAuthenticatorFails(t *testing.T) {
	be := newStubBackend()
	if _, err := New(be, Config{}); err == nil {
		t.Fatal("New without an authenticator must fail")
	}
}

func TestName(t *testing.T) {
	f, err := New(newStubBackend(), Config{}, WithAuthenticator(newStubAuth()))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Name(); got != "webdav" {
		t.Fatalf("Name() = %q, want %q", got, "webdav")
	}
}

func TestCapabilities_ModeFlip(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want frontend.ProtocolCaps
	}{
		{
			name: "mode A multi-bucket",
			cfg:  Config{},
			want: frontend.ProtocolCaps{Buckets: true, ConditionalReads: true},
		},
		{
			name: "mode B single-bucket",
			cfg:  Config{Bucket: "photos"},
			want: frontend.ProtocolCaps{ConditionalReads: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := New(newStubBackend(), tt.cfg, WithAuthenticator(newStubAuth()))
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Capabilities(); got != tt.want {
				t.Fatalf("Capabilities() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAuthenticatorInjected(t *testing.T) {
	be := newStubBackend()
	stub := newStubAuth()
	f, err := New(be, Config{}, WithAuthenticator(stub))
	if err != nil {
		t.Fatal(err)
	}
	if f.Authenticator() != stub {
		t.Fatal("Authenticator() must return the injected authenticator")
	}
}

func TestNew_WhitespaceBucketFails(t *testing.T) {
	for _, bucket := range []string{" ", "\t", "  \t "} {
		if _, err := New(newStubBackend(), Config{Bucket: bucket}, WithAuthenticator(newStubAuth())); err == nil {
			t.Fatalf("New(Bucket=%q) must fail (mode must be deliberate)", bucket)
		}
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	f, err := New(newStubBackend(), Config{}, WithAuthenticator(newStubAuth()))
	if err != nil {
		t.Fatal(err)
	}
	reg := frontend.NewRegistry()
	if err := reg.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := reg.Lookup("webdav")
	if !ok {
		t.Fatal("Lookup(webdav) missed")
	}
	if got.Name() != "webdav" {
		t.Fatalf("round-trip Name = %q", got.Name())
	}
	// Duplicate registration returns the registry's duplicate error.
	if err := reg.Register(f); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate Register err = %v, want duplicate-name error", err)
	}
}

func TestNew_ErrorMentionsWhitespaceBucket(t *testing.T) {
	_, err := New(newStubBackend(), Config{Bucket: " "}, WithAuthenticator(newStubAuth()))
	if err == nil || !errors.Is(err, err) { //nolint:staticcheck // presence check only
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("error should name the whitespace rule: %v", err)
	}
}
