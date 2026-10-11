// config_sts_test.go — the zeta_sts config block parser (leaf 02):
// table-driven validation, restart-required classification at the config
// store, and the signing-key file resolution contract.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSTSConfigBlock_Table(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
		want    STSConfig
	}{
		{"absent", "", "", STSConfig{}},
		{"disabled empty", `{}`, "", STSConfig{}},
		{"enabled minimal", `{"enabled":true}`, "", STSConfig{Enabled: true}},
		{"full", `{"enabled":true,"ttl":"30m","max_ttl":"12h","signing_key_file":"/keys/sts.pem"}`,
			"", STSConfig{Enabled: true, TTL: 30 * time.Minute, MaxTTL: 12 * time.Hour, SigningKeyFile: "/keys/sts.pem"}},
		{"invalid ttl", `{"enabled":true,"ttl":"soon"}`, "invalid zeta_sts.ttl", STSConfig{}},
		{"negative ttl", `{"enabled":true,"ttl":"-5m"}`, "must be positive", STSConfig{}},
		{"invalid max_ttl", `{"enabled":true,"max_ttl":"x"}`, "invalid zeta_sts.max_ttl", STSConfig{}},
		{"ttl exceeds max", `{"enabled":true,"ttl":"48h","max_ttl":"12h"}`, "exceeds max_ttl", STSConfig{}},
		{"unknown key", `{"enabled":true,"bogus":1}`, "invalid zeta_sts", STSConfig{}},
		{"not an object", `"yes"`, "invalid zeta_sts", STSConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			got, err := parseSTSConfigBlock(raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Enabled != tc.want.Enabled || got.TTL != tc.want.TTL ||
				got.MaxTTL != tc.want.MaxTTL || got.SigningKeyFile != tc.want.SigningKeyFile {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveSigningKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sts.pem")
	if err := os.WriteFile(path, []byte("signing-key-material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := resolveSigningKey(path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(raw) != "signing-key-material\n" {
		t.Errorf("key bytes changed: %q", raw)
	}
	if _, err := resolveSigningKey(""); err == nil {
		t.Error("empty path accepted")
	}
	if _, err := resolveSigningKey(filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("missing file accepted")
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSigningKey(empty); err == nil {
		t.Error("empty file accepted")
	}
}

// TestConfigStore_ZetaSTSRestartRequired pins the store contract: the
// zeta_sts patch applies to the DESIRED config and is reported under
// RestartRequired, never as applied and never live.
func TestConfigStore_ZetaSTSRestartRequired(t *testing.T) {
	cfg := defaultServerConfig()
	store := NewConfigStore(&cfg)
	patch := ConfigPatch{JSON: json.RawMessage(`{"zeta_sts":{"enabled":true,"ttl":"30m"}}`)}
	applied, restartRequired, err := store.Apply(patch)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v, want empty", applied)
	}
	if len(restartRequired) != 1 || restartRequired[0] != "zeta_sts" {
		t.Fatalf("restartRequired = %v, want [zeta_sts]", restartRequired)
	}
	if store.Snapshot().ZetaSTS != nil {
		t.Error("live config must NOT carry the restart-required value")
	}
	found := false
	for _, k := range store.RestartRequired() {
		if k == "zeta_sts" {
			found = true
		}
	}
	if !found {
		t.Error("RestartRequired() lost zeta_sts")
	}
}

// TestServerConfig_ZetaSTSJSONRoundTrip pins the JSON key names (the
// exact config surface the orchestrator's appliance config will use).
func TestServerConfig_ZetaSTSJSONRoundTrip(t *testing.T) {
	doc := `{"zeta_sts":{"enabled":true,"ttl":"45m","max_ttl":"24h","signing_key_file":"/keys/sts.pem"}}`
	var cfg ServerConfig
	if err := cfg.UnmarshalJSON([]byte(doc)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if cfg.ZetaSTS == nil || !cfg.ZetaSTS.Enabled {
		t.Fatal("zeta_sts not decoded")
	}
	if cfg.ZetaSTS.TTL != 45*time.Minute || cfg.ZetaSTS.MaxTTL != 24*time.Hour {
		t.Errorf("durations = %s/%s", cfg.ZetaSTS.TTL, cfg.ZetaSTS.MaxTTL)
	}
	if cfg.ZetaSTS.SigningKeyFile != "/keys/sts.pem" {
		t.Errorf("signing_key_file = %q", cfg.ZetaSTS.SigningKeyFile)
	}
	// Absent block = nil (disabled).
	var cfg2 ServerConfig
	if err := cfg2.UnmarshalJSON([]byte(`{}`)); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if cfg2.ZetaSTS != nil {
		t.Error("absent zeta_sts decoded non-nil")
	}
}
