// registry_test.go — MultiRegistry construction + lookups (pluggable-
// authentication tree leaf 01 Task 2): fail-loud validation, grant
// translation, constant-time Basic comparison structure, CredentialSource
// compatibility.
package auth_test

import (
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// testIdentities is the canonical fixture: an env-like wildcard pair plus
// two configured identities (one scoped readonly).
func testIdentities() []auth.IdentityConfig {
	return []auth.IdentityConfig{
		{Name: "env", AccessKey: "AKENV", SecretKey: "sk-env", Grants: map[string]string{"*": "readwrite"}},
		{Name: "ci-bot", AccessKey: "AKCI", SecretKey: "sk-ci"},
		{Name: "scraper", AccessKey: "AKRO", SecretKey: "sk-ro", Grants: map[string]string{"photos": "readonly"}},
	}
}

func TestMultiRegistryValidSet(t *testing.T) {
	reg, err := auth.NewMultiRegistry(testIdentities())
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}
	// Access-key lookup carries grants.
	id, ok := reg.LookupByAccessKey("AKRO")
	if !ok {
		t.Fatal("AKRO not found")
	}
	if !id.CanRead("photos") {
		t.Error("AKRO cannot read photos")
	}
	if id.CanWrite("photos") {
		t.Error("AKRO can write photos (readonly grant)")
	}
	if id.CanRead("other") {
		t.Error("AKRO reads ungranted bucket")
	}
	// Absent grants ⇒ wildcard readwrite.
	id, ok = reg.LookupByAccessKey("AKCI")
	if !ok {
		t.Fatal("AKCI not found")
	}
	if !id.CanRead("anything") || !id.CanWrite("anything") {
		t.Error("absent grants should mean wildcard readwrite")
	}
	// Miss.
	if _, ok := reg.LookupByAccessKey("AKNOPE"); ok {
		t.Error("unknown key accepted")
	}
	// Identity shape: AccessKeyID set, BucketGrants populated.
	if id.AccessKeyID != "AKCI" || id.BucketGrants == nil {
		t.Errorf("identity = %+v", id)
	}
}

func TestMultiRegistryValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		cfg  []auth.IdentityConfig
		want string // error substring
	}{
		{"empty name", []auth.IdentityConfig{{AccessKey: "a", SecretKey: "s"}}, "name is required"},
		{"empty accessKey", []auth.IdentityConfig{{Name: "x", SecretKey: "s"}}, "accessKey is required"},
		{"empty secretKey", []auth.IdentityConfig{{Name: "x", AccessKey: "a"}}, "secretKey is required"},
		{"duplicate name", []auth.IdentityConfig{
			{Name: "x", AccessKey: "a1", SecretKey: "s"},
			{Name: "x", AccessKey: "a2", SecretKey: "s"},
		}, `name "x"`},
		{"duplicate accessKey", []auth.IdentityConfig{
			{Name: "x", AccessKey: "a1", SecretKey: "s"},
			{Name: "y", AccessKey: "a1", SecretKey: "s"},
		}, "more than once"},
		{"bad grant value", []auth.IdentityConfig{
			{Name: "x", AccessKey: "a", SecretKey: "s", Grants: map[string]string{"b": "admin"}},
		}, "unknown grant value"},
		{"empty grant key", []auth.IdentityConfig{
			{Name: "x", AccessKey: "a", SecretKey: "s", Grants: map[string]string{"": "readonly"}},
		}, "must not be empty"},
		{"duplicate ssh key", []auth.IdentityConfig{
			{Name: "x", AccessKey: "a1", SecretKey: "s", SSHPublicKeys: []string{"ssh-ed25519 AAAA"}},
			{Name: "y", AccessKey: "a2", SecretKey: "s", SSHPublicKeys: []string{"ssh-ed25519 AAAA"}},
		}, "more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.NewMultiRegistry(tc.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

// TestMultiRegistryWildcardReadonlyGrantLegal pins that a "*" grant with
// value "readonly" is valid (scope-wide readonly identity).
func TestMultiRegistryWildcardReadonlyGrantLegal(t *testing.T) {
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "ro", AccessKey: "AK", SecretKey: "sk", Grants: map[string]string{"*": "readonly"}},
	})
	if err != nil {
		t.Fatalf("wildcard readonly rejected: %v", err)
	}
	id, ok := reg.LookupByAccessKey("AK")
	if !ok || !id.CanRead("any-bucket") || id.CanWrite("any-bucket") {
		t.Fatalf("wildcard readonly wrong: %+v ok=%v", id, ok)
	}
}

func TestMultiRegistryBasicCredential(t *testing.T) {
	reg, err := auth.NewMultiRegistry(testIdentities())
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}
	// Hit: username == accessKey, password == secretKey.
	id, ok := reg.LookupByBasicCredential("AKRO", "sk-ro")
	if !ok {
		t.Fatal("valid Basic credential rejected")
	}
	if !id.CanRead("photos") || id.CanWrite("photos") {
		t.Errorf("Basic hit carries wrong grants: %+v", id.BucketGrants)
	}
	// Wrong password.
	if _, ok := reg.LookupByBasicCredential("AKRO", "wrong"); ok {
		t.Error("wrong password accepted")
	}
	// Unknown user.
	if _, ok := reg.LookupByBasicCredential("AKNOPE", "sk-ro"); ok {
		t.Error("unknown user accepted")
	}
	// Empty password.
	if _, ok := reg.LookupByBasicCredential("AKRO", ""); ok {
		t.Error("empty password accepted")
	}
}

func TestMultiRegistryPublicKey(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIF4k3fakeblobAAAAtestkey test@host"
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "sftp-user", AccessKey: "AKSFTP", SecretKey: "sk", SSHPublicKeys: []string{key}},
	})
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}
	id, ok := reg.LookupByPublicKey(key)
	if !ok {
		t.Fatal("registered key not found")
	}
	if id.AccessKeyID != "AKSFTP" {
		t.Errorf("identity = %+v", id)
	}
	if _, ok := reg.LookupByPublicKey("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAInother"); ok {
		t.Error("unregistered key accepted")
	}
}

// TestMultiRegistrySatisfiesCredentialSource pins the frozen v1 interface
// compatibility: the S3 adapter's legacy path keeps working against a
// MultiRegistry.
func TestMultiRegistrySatisfiesCredentialSource(t *testing.T) {
	var _ auth.CredentialSource = (*auth.MultiRegistry)(nil)
	reg, err := auth.NewMultiRegistry(testIdentities())
	if err != nil {
		t.Fatalf("NewMultiRegistry: %v", err)
	}
	secret, ok := reg.SecretKey("AKRO")
	if !ok || secret != "sk-ro" {
		t.Fatalf("SecretKey(AKRO) = %q %v, want sk-ro true", secret, ok)
	}
	if _, ok := reg.SecretKey("AKNOPE"); ok {
		t.Error("SecretKey unknown key accepted")
	}
}

func TestEnvPairShape(t *testing.T) {
	pair := auth.EnvPair("minioadmin", "minioadmin")
	if pair.Name != "env" {
		t.Errorf("Name = %q, want env", pair.Name)
	}
	if pair.AccessKey != "minioadmin" || pair.SecretKey != "minioadmin" {
		t.Errorf("pair = %+v", pair)
	}
	if pair.Grants["*"] != auth.GrantReadWrite {
		t.Errorf("grants = %+v, want wildcard readwrite", pair.Grants)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{pair})
	if err != nil {
		t.Fatalf("env-only registry: %v", err)
	}
	id, ok := reg.LookupByAccessKey("minioadmin")
	if !ok || !id.CanRead("b") || !id.CanWrite("b") {
		t.Fatalf("env identity = %+v ok=%v", id, ok)
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "env" {
		t.Errorf("Names() = %v", names)
	}
}
