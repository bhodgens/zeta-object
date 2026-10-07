// config_identities_test.go — the identities + auth config surface
// (pluggable-authentication tree leaf 01 Task 3): back-compat golden parse,
// env merge, duplicate fail-loud, validation abort, auth.mode vocabulary.
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// withIdentityEnv sets ZETAOBJECT_* credentials for the test's duration.
func withIdentityEnv(t *testing.T, ak, sk string) {
	t.Helper()
	t.Setenv("ZETAOBJECT_ACCESS_KEY", ak)
	t.Setenv("ZETAOBJECT_SECRET_KEY", sk)
}

// TestConfigIdentitiesGoldenBackCompat pins the migration contract at the
// parse level: config without identities/auth keys decodes exactly as
// before (all legacy fields intact, zero new identities).
func TestConfigIdentitiesGoldenBackCompat(t *testing.T) {
	raw := `{"dataDir":"./d/","listenAddr":":9999","certFile":"c.pem","keyFile":"k.pem",
		"buckets":{"photos":"/mnt/photos"},"backends":{"fs":{"root":"./d/"}},
		"frontends":[{"type":"s3"}]}`
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("legacy config failed to parse: %v", err)
	}
	if cfg.DataDir != "./d/" || cfg.ListenAddr != ":9999" || cfg.CertFile != "c.pem" || cfg.KeyFile != "k.pem" {
		t.Errorf("legacy fields lost: %+v", cfg)
	}
	if cfg.Buckets["photos"] != "/mnt/photos" {
		t.Errorf("buckets lost: %+v", cfg.Buckets)
	}
	if cfg.Backends["fs"].Root != "./d/" {
		t.Errorf("backends lost: %+v", cfg.Backends)
	}
	if len(cfg.Frontends) != 1 || cfg.Frontends[0].Type != "s3" {
		t.Errorf("frontends lost: %+v", cfg.Frontends)
	}
	if len(cfg.Identities) != 0 {
		t.Errorf("identities should be empty: %+v", cfg.Identities)
	}
	if cfg.Auth.Mode != "" {
		t.Errorf("auth.mode should be empty: %+v", cfg.Auth)
	}
}

// TestConfigIdentitiesParse pins the new keys' JSON decoding.
func TestConfigIdentitiesParse(t *testing.T) {
	raw := `{"identities":[
		{"name":"ci-bot","accessKey":"AKCI","secretKey":"sk-ci"},
		{"name":"scraper","accessKey":"AKRO","secretKey":"sk-ro","grants":{"photos":"readonly"}},
		{"name":"sftp","accessKey":"AKSFTP","secretKey":"sk-sftp","sshPublicKeys":["ssh-ed25519 AAAAB3NzaC1lZDI1NTE5 fake blob"]}
	],"auth":{"mode":""}}`
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Identities) != 3 {
		t.Fatalf("identities = %d, want 3", len(cfg.Identities))
	}
	ci := cfg.Identities[0]
	if ci.Name != "ci-bot" || ci.AccessKey != "AKCI" || ci.SecretKey != "sk-ci" {
		t.Errorf("identity 0 = %+v", ci)
	}
	if raw := cfg.Identities[1].Grants["photos"]; string(raw) != `"readonly"` {
		t.Errorf("grants lost: %s", raw)
	}
	if len(cfg.Identities[2].SSHPublicKeys) != 1 {
		t.Errorf("sshPublicKeys lost: %+v", cfg.Identities[2])
	}
}

// TestBuildIdentityRegistryEnvMerge pins the merge rule: env pair is ALWAYS
// identity "env" with wildcard readwrite, config identities sit alongside.
func TestBuildIdentityRegistryEnvMerge(t *testing.T) {
	withIdentityEnv(t, "AKENV", "sk-env")
	loadCredentials()
	saved := *serverConfig()
	defer func() { setServerConfig(saved) }()
	setServerConfig(defaultServerConfig())
	setServerConfigFieldT(t, func(c *ServerConfig) {
		c.Identities = []auth.IdentityConfig{
			{Name: "ci-bot", AccessKey: "AKCI", SecretKey: "sk-ci"},
			{Name: "scraper", AccessKey: "AKRO", SecretKey: "sk-ro", Grants: rawGrants(map[string]string{"photos": "readonly"})},
		}
	})
	reg, err := buildIdentityRegistry()
	if err != nil {
		t.Fatalf("buildIdentityRegistry: %v", err)
	}
	// 3 identities: env + 2 configured.
	envID, ok := reg.LookupByAccessKey("AKENV")
	if !ok {
		t.Fatal("env identity missing")
	}
	if !envID.CanRead("anything") || !envID.CanWrite("anything") {
		t.Error("env identity is not wildcard readwrite")
	}
	for _, ak := range []string{"AKCI", "AKRO"} {
		if _, ok := reg.LookupByAccessKey(ak); !ok {
			t.Errorf("configured identity %s missing", ak)
		}
	}
	roID, _ := reg.LookupByAccessKey("AKRO")
	if !roID.CanRead("photos") || roID.CanWrite("photos") {
		t.Error("scoped grants wrong")
	}
}

// TestBuildIdentityRegistryEnvOnly pins env-only deployments: exactly one
// wildcard "env" identity — byte-identical to the pre-tree behavior.
func TestBuildIdentityRegistryEnvOnly(t *testing.T) {
	withIdentityEnv(t, "AKONLY", "sk-only")
	loadCredentials()
	saved := *serverConfig()
	defer func() { setServerConfig(saved) }()
	setServerConfig(defaultServerConfig())
	reg, err := buildIdentityRegistry()
	if err != nil {
		t.Fatalf("env-only build failed: %v", err)
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "env" {
		t.Fatalf("Names() = %v, want [env]", names)
	}
	id, ok := reg.LookupByAccessKey("AKONLY")
	if !ok || !id.CanWrite("any-bucket") {
		t.Fatalf("env identity = %+v ok=%v", id, ok)
	}
}

// TestBuildIdentityRegistryDuplicates pins the fail-loud duplicate rule.
func TestBuildIdentityRegistryDuplicates(t *testing.T) {
	withIdentityEnv(t, "AKENV", "sk-env")
	loadCredentials()
	saved := *serverConfig()
	defer func() { setServerConfig(saved) }()

	t.Run("config vs env", func(t *testing.T) {
		setServerConfig(defaultServerConfig())
		setServerConfigFieldT(t, func(c *ServerConfig) {
			c.Identities = []auth.IdentityConfig{
				{Name: "clash", AccessKey: "AKENV", SecretKey: "sk-x"},
			}
		})
		_, err := buildIdentityRegistry()
		if err == nil {
			t.Fatal("duplicate config-vs-env accepted")
		}
		if !strings.Contains(err.Error(), "more than once") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("config vs config", func(t *testing.T) {
		setServerConfig(defaultServerConfig())
		setServerConfigFieldT(t, func(c *ServerConfig) {
			c.Identities = []auth.IdentityConfig{
				{Name: "a", AccessKey: "AKX", SecretKey: "sk-a"},
				{Name: "b", AccessKey: "AKX", SecretKey: "sk-b"},
			}
		})
		_, err := buildIdentityRegistry()
		if err == nil {
			t.Fatal("duplicate config-vs-config accepted")
		}
	})
}

// TestBuildIdentityRegistryValidation pins the invalid-input aborts.
func TestBuildIdentityRegistryValidation(t *testing.T) {
	withIdentityEnv(t, "AKENV", "sk-env")
	loadCredentials()
	saved := *serverConfig()
	defer func() { setServerConfig(saved) }()

	cases := []struct {
		name       string
		identities []auth.IdentityConfig
		mode       string
		wantErr    string
	}{
		{"empty required field", []auth.IdentityConfig{{Name: "x", AccessKey: "", SecretKey: "s"}}, "", "accessKey is required"},
		{"invalid grant value", []auth.IdentityConfig{{Name: "x", AccessKey: "AK", SecretKey: "s", Grants: rawGrants(map[string]string{"b": "admin"})}}, "", "unknown grant value"},
		{"bad auth.mode", nil, "off", "invalid auth.mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setServerConfig(defaultServerConfig())
			setServerConfigFieldT(t, func(c *ServerConfig) { c.Identities = tc.identities })
			serverConfig().Auth.Mode = tc.mode
			_, err := buildIdentityRegistry()
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestEnvPairLegacyPrefixes pins that both ZETAOBJECT_* and MINIS3_*
// resolve to the same env identity (Z wins when both are set).
func TestEnvPairLegacyPrefixes(t *testing.T) {
	t.Run("MINIS3-only", func(t *testing.T) {
		os.Unsetenv("ZETAOBJECT_ACCESS_KEY")
		os.Unsetenv("ZETAOBJECT_SECRET_KEY")
		t.Setenv("MINIS3_ACCESS_KEY", "ak-legacy")
		t.Setenv("MINIS3_SECRET_KEY", "sk-legacy")
		loadCredentials()
		if serverCredentials.AccessKeyID != "ak-legacy" {
			t.Fatalf("legacy prefix lost: %q", serverCredentials.AccessKeyID)
		}
	})
	t.Run("Z wins over MINIS3", func(t *testing.T) {
		t.Setenv("ZETAOBJECT_ACCESS_KEY", "ak-new")
		t.Setenv("ZETAOBJECT_SECRET_KEY", "sk-new")
		t.Setenv("MINIS3_ACCESS_KEY", "ak-old")
		t.Setenv("MINIS3_SECRET_KEY", "sk-old")
		loadCredentials()
		if serverCredentials.AccessKeyID != "ak-new" {
			t.Fatalf("ZETAOBJECT should win: %q", serverCredentials.AccessKeyID)
		}
	})
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
