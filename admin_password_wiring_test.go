package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
)

// installAdminCredentialTestEnv points serverConfigPath at a temp config and
// snapshots/restores every global the rotation touches.
func installAdminCredentialTestEnv(t *testing.T, cfgJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	prevPath, prevReg, prevCfg, prevCreds := serverConfigPath, identityRegistry, *serverConfig(), serverCredentials
	t.Cleanup(func() {
		serverConfigPath, identityRegistry, *serverConfig(), serverCredentials = prevPath, prevReg, prevCfg, prevCreds
	})
	serverConfigPath = path
	setServerConfig(defaultServerConfig())
	reg, err := auth.NewMultiRegistry(nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(reg)
	installTestConfigStore(t, defaultServerConfig())
	return dir
}

// throwaway values only — never a real credential.
const (
	pwOld = "old-throwaway-VALUE"
	pwNew = "new-throwaway-VALUE"
)

// TestAdminSetAdminPasswordRotatesTheEffectiveCredential is the end-to-end
// Contract G4 proof: the service persists the rotated secret beside the
// config file and activates it through the reload path, so the LIVE
// registry answers Basic auth with the NEW secret and rejects the old one —
// without a restart.
func TestAdminSetAdminPasswordRotatesTheEffectiveCredential(t *testing.T) {
	dir := installAdminCredentialTestEnv(t, `{}`)
	t.Setenv("ZETAOBJECT_ACCESS_KEY", "zetaadmin")
	t.Setenv("ZETAOBJECT_SECRET_KEY", pwOld)
	loadCredentials()

	if err := adminSetAdminPasswordService(context.Background(), pwNew); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}

	// Persisted atomically beside the config file, 0600, exact content.
	data, err := os.ReadFile(filepath.Join(dir, adminCredentialFileName))
	if err != nil {
		t.Fatalf("credential file: %v", err)
	}
	if string(data) != pwNew {
		t.Fatal("credential file does not carry the rotated secret")
	}
	info, err := os.Stat(filepath.Join(dir, adminCredentialFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file perm = %o, want 600", perm)
	}

	// The live registry now resolves the env identity with the NEW secret
	// and rejects the OLD one (constant-time compare inside the registry).
	if _, ok := identityRegistry.LookupByBasicCredential("zetaadmin", pwNew); !ok {
		t.Fatal("rotated credential does not authenticate against the live registry")
	}
	if _, ok := identityRegistry.LookupByBasicCredential("zetaadmin", pwOld); ok {
		t.Fatal("the previous credential still authenticates after rotation")
	}

	// The persisted rotation survives a restart: a fresh startup sequence
	// (loadCredentials) resolves the rotated secret from the file, with
	// precedence over the still-set env var.
	loadCredentials()
	if serverCredentials.SecretAccessKey != pwNew {
		t.Fatal("loadCredentials did not prefer the persisted rotated secret")
	}
}

// TestAdminSetAdminPasswordRolledBackOnActivationFailure pins the
// fail-closed rollback: a config file that makes the reload fail leaves the
// OLD credential serving AND removes the persisted rotation (no half state
// for the next restart to pick up). The error message must not contain the
// password.
func TestAdminSetAdminPasswordRolledBackOnActivationFailure(t *testing.T) {
	dir := installAdminCredentialTestEnv(t, `{}`)
	t.Setenv("ZETAOBJECT_ACCESS_KEY", "zetaadmin")
	t.Setenv("ZETAOBJECT_SECRET_KEY", pwOld)
	loadCredentials()
	// Mirror startup: the live registry is built AFTER loadCredentials.
	startReg, err := buildIdentityRegistry()
	if err != nil {
		t.Fatalf("startup registry: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(startReg)

	// Break the config file AFTER startup so the reload's loadConfig fails
	// (a duplicate access key is the validator's named-offender path).
	broken := `{"identities":[{"name":"a","accessKey":"zetaadmin","secretKey":"sk-a"},{"name":"b","accessKey":"zetaadmin","secretKey":"sk-b"}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	err = adminSetAdminPasswordService(context.Background(), pwNew)
	if err == nil {
		t.Fatal("activation against a broken config must fail")
	}
	se := &admin.ServiceError{}
	if !errors.As(err, &se) || se.Code != "ReloadFailed" {
		t.Fatalf("error = %v, want a ReloadFailed ServiceError", err)
	}
	if containsSecret(se.Message, pwNew) {
		t.Fatal("error message leaked the password")
	}
	// The persisted rotation is gone and the live store snapped back.
	if _, statErr := os.Stat(filepath.Join(dir, adminCredentialFileName)); !os.IsNotExist(statErr) {
		t.Fatal("persisted rotation was not rolled back")
	}
	if serverCredentials.SecretAccessKey != pwOld {
		t.Fatal("live credential store did not snap back to the env value")
	}
	// The old credential still authenticates.
	if _, ok := identityRegistry.LookupByBasicCredential("zetaadmin", pwOld); !ok {
		t.Fatal("old credential stopped authenticating during a failed rotation")
	}
}

// TestAdminSetAdminPasswordNeverLogsSecret guards the no-log contract at the
// seam the package can observe: the service's success error is nil and the
// persisted file is the ONLY artifact carrying the secret (no config.json
// echo — the rotation must not leak into the config document /config/save
// persists).
func TestAdminSetAdminPasswordNeverLeaksIntoConfig(t *testing.T) {
	dir := installAdminCredentialTestEnv(t, `{}`)
	t.Setenv("ZETAOBJECT_ACCESS_KEY", "zetaadmin")
	t.Setenv("ZETAOBJECT_SECRET_KEY", pwOld)
	loadCredentials()

	if err := adminSetAdminPasswordService(context.Background(), pwNew); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}
	// Persist the live config the way POST /config/save does; the rotated
	// secret must NOT appear in the config document.
	if err := configStore.Persist(serverConfigPath); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	cfgData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if containsSecret(string(cfgData), pwNew) {
		t.Fatal("rotated secret leaked into the config document")
	}
	// And the masked /config view keeps masking the env pair's secret.
	masked := configStore.maskedCopyLocked()
	for _, id := range masked.Identities {
		if containsSecret(id.SecretKey, pwNew) || containsSecret(id.SecretKey, pwOld) {
			t.Fatalf("masked config view leaked a secret value for identity %q", id.Name)
		}
	}
}

// containsSecret is a tiny helper kept separate so the assertion reads
// honestly at each call site.
func containsSecret(haystack, needle string) bool {
	return needle != "" && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
