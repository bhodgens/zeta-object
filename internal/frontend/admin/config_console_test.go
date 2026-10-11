// config_console_test.go — the zeta_console_oidc config block parser
// (leaf 03). Block lives in config_console.go (OUTSIDE config.go /
// config_store.go) because leaf 02 edits those in parallel; the
// orchestrator merges it into the canonical parse/apply paths.
package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConsoleOIDCBlock(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
		wantCfg ConsoleOIDCConfig
	}{
		{"absent block", ``, "", ConsoleOIDCConfig{}},
		{"empty object", `{}`, "", ConsoleOIDCConfig{}},
		{
			"enabled minimal",
			`{"enabled":true,"client_id":"zeta-console","client_secret_file":"/etc/accessstor/dex-secret"}`,
			"",
			ConsoleOIDCConfig{Enabled: true, ClientID: "zeta-console", ClientSecretFile: "/etc/accessstor/dex-secret"},
		},
		{
			"session ttl",
			`{"enabled":true,"client_id":"c","client_secret_file":"/s","session_ttl":"30m"}`,
			"",
			ConsoleOIDCConfig{Enabled: true, ClientID: "c", ClientSecretFile: "/s", SessionTTL: 30 * time.Minute},
		},
		{"invalid ttl", `{"enabled":true,"session_ttl":"soon"}`, "invalid zeta_console_oidc.session_ttl", ConsoleOIDCConfig{}},
		{"zero ttl", `{"enabled":true,"session_ttl":"0s"}`, "must be positive", ConsoleOIDCConfig{}},
		{
			"negative ttl",
			`{"enabled":true,"session_ttl":"-5m"}`,
			"must be positive",
			ConsoleOIDCConfig{},
		},
		{"unknown field rejected", `{"enabled":true,"secret":"inline-secret"}`, "unknown field", ConsoleOIDCConfig{}},
		{"not an object", `"yes"`, "invalid zeta_console_oidc", ConsoleOIDCConfig{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseConsoleOIDCBlock(json.RawMessage(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if cfg != tt.wantCfg {
				t.Fatalf("cfg = %+v, want %+v", cfg, tt.wantCfg)
			}
		})
	}
	// The inline-secret attempt above proves the shape: there is NO field
	// that could carry a secret value in config. Pin it structurally.
	if _, err := parseConsoleOIDCBlock(json.RawMessage(`{"client_secret":"x"}`)); err == nil {
		t.Fatal("a client_secret JSON field was accepted — secrets must be file references only")
	}
}

func TestResolveClientSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dex-secret")
	if err := os.WriteFile(path, []byte("s3cret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveClientSecret(path)
	if err != nil || got != "s3cret-value" {
		t.Fatalf("got (%q, %v), want trimmed s3cret-value", got, err)
	}
	if _, err := resolveClientSecret(""); err == nil || !strings.Contains(err.Error(), "client_secret_file is required") {
		t.Fatalf("empty path: err = %v", err)
	}
	if _, err := resolveClientSecret(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing file: err = %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveClientSecret(empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty file: err = %v", err)
	}
}
