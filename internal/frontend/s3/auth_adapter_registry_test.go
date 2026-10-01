// auth_adapter_registry_test.go — registry-backed SigV4 resolution
// (pluggable-authentication tree leaf 02 Task 1): access keys resolve
// through the IdentityRegistry, success carries grants, unknown keys stay
// InvalidAccessKeyId, and the legacy CredentialSource fallback still works.
package s3_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// registryFixture is the canonical registry for adapter tests: env-like
// wildcard identity + a readonly-scoped identity.
func registryFixture(t *testing.T) *auth.MultiRegistry {
	t.Helper()
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "env", AccessKey: "AKENV", SecretKey: "sk-env"},
		{Name: "ro", AccessKey: "AKBOT", SecretKey: "sk-bot", Grants: rawGrants(map[string]string{"b1": "readonly"})},
	})
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}
	return reg
}

// installRegistry installs reg for the duration of the test and restores
// the previous hook state afterwards (the seam is process-global).
func installRegistry(t *testing.T, reg auth.IdentityRegistry) {
	t.Helper()
	s3.InstallIdentityRegistry(reg)
	t.Cleanup(func() { s3.InstallIdentityRegistry(nil) })
}

func TestSigV4RegistryBackedAuthentication(t *testing.T) {
	reg := registryFixture(t)
	installRegistry(t, reg)

	f := s3.New(nil) // no WithCredentialSource: registry is the source of record
	a := f.Authenticator()

	t.Run("registry identity authenticates with grants", func(t *testing.T) {
		req := buildSignedRequestHelper(t, "AKBOT", "sk-bot", nil)
		id, err := a.Authenticate(req)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if id.AccessKeyID != "AKBOT" {
			t.Fatalf("AccessKeyID = %q", id.AccessKeyID)
		}
		if !id.CanRead("b1") {
			t.Error("grants lost: cannot read b1")
		}
		if id.CanWrite("b1") {
			t.Error("readonly identity got write grant")
		}
	})

	t.Run("wildcard registry identity", func(t *testing.T) {
		req := buildSignedRequestHelper(t, "AKENV", "sk-env", nil)
		id, err := a.Authenticate(req)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !id.CanRead("any-bucket") || !id.CanWrite("any-bucket") {
			t.Errorf("wildcard identity denied: %+v", id.BucketGrants)
		}
	})

	t.Run("unknown key still InvalidAccessKeyId", func(t *testing.T) {
		req := buildSignedRequestHelper(t, "AKNOPE", "sk-nope", nil)
		id, err := a.Authenticate(req)
		if err == nil {
			t.Fatal("unknown key accepted")
		}
		if id.AccessKeyID != "" || id.BucketGrants != nil {
			t.Errorf("identity returned on failure: %+v", id)
		}
		if !strings.Contains(err.Error(), "InvalidAccessKeyId") {
			t.Errorf("err = %v, want InvalidAccessKeyId", err)
		}
	})
}

func TestSigV4RegistryPresignedCarriesGrants(t *testing.T) {
	reg := registryFixture(t)
	installRegistry(t, reg)
	f := s3.New(nil)
	a := f.Authenticator()
	req := buildPresignedRequestHelper(t, "AKBOT", "sk-bot", false)
	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate presigned: %v", err)
	}
	if id.AccessKeyID != "AKBOT" || !id.CanRead("b1") || id.CanWrite("b1") {
		t.Fatalf("presigned identity = %+v", id)
	}
}

func TestSigV4LegacyCredentialSourceFallback(t *testing.T) {
	// No registry installed (the default zero state): the legacy
	// WithCredentialSource path still authenticates and synthesizes the
	// wildcard identity — pre-tree behavior byte-for-byte.
	f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	a := f.Authenticator()
	req := buildSignedRequestHelper(t, "minioadmin", "minioadmin", nil)
	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("legacy fallback broke: %v", err)
	}
	if id.AccessKeyID != "minioadmin" {
		t.Fatalf("AccessKeyID = %q", id.AccessKeyID)
	}
	if !id.CanRead("anything") || !id.CanWrite("anything") {
		t.Errorf("legacy fallback must be wildcard: %+v", id.BucketGrants)
	}
}

// rawGrants adapts the legacy string-literal grant map to the dual-form
// config type (leaf 09): semantics identical, fewer literal bytes.
func rawGrants(m map[string]string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v)
		out[k] = b
	}
	return out
}
