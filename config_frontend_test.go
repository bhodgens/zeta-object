package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempConfig writes a config.json into a temp dir and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadConfigForTest loads a config file into the serverConfig global and
// returns the normalized result. loadConfig keeps its landed signature
// (error-only, global-populating) — see drift note in the leaf report.
func loadConfigForTest(t *testing.T, path string) ServerConfig {
	t.Helper()
	if err := loadConfig(path); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return serverConfig
}

func TestLoadConfig_Frontends(t *testing.T) {
	tests := []struct {
		name       string
		configJSON string
		envAddr    string // MINIS3_LISTEN_ADDR override, "" = unset
		want       []FrontendConfig
		wantAddr   string // effective default listen addr after normalization
	}{
		{
			name:       "absent frontends defaults to s3 on default addr",
			configJSON: `{"dataDir":"./data/"}`,
			want:       []FrontendConfig{{Type: "s3"}},
			wantAddr:   ":8443",
		},
		{
			name:       "explicit s3 only",
			configJSON: `{"frontends":[{"type":"s3"}]}`,
			want:       []FrontendConfig{{Type: "s3"}},
			wantAddr:   ":8443",
		},
		{
			name:       "s3 with dedicated second listener",
			configJSON: `{"frontends":[{"type":"s3"},{"type":"webdav","listenAddr":":8444"}]}`,
			want: []FrontendConfig{
				{Type: "s3"},
				{Type: "webdav", ListenAddr: ":8444"},
			},
			wantAddr: ":8443",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envAddr != "" {
				t.Setenv("MINIS3_LISTEN_ADDR", tt.envAddr)
			}
			cfg := loadConfigForTest(t, writeTempConfig(t, tt.configJSON))
			if len(cfg.Frontends) != len(tt.want) {
				t.Fatalf("Frontends = %+v, want %+v", cfg.Frontends, tt.want)
			}
			for i := range tt.want {
				if cfg.Frontends[i] != tt.want[i] {
					t.Fatalf("Frontends[%d] = %+v, want %+v", i, cfg.Frontends[i], tt.want[i])
				}
			}
			if cfg.ListenAddr != tt.wantAddr {
				t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, tt.wantAddr)
			}
		})
	}
}

// MINIS3_LISTEN_ADDR keeps overriding only the default listener address
// (the env override itself is applied in main() via applyListenAddrOverride;
// loadConfig stays env-free).
func TestLoadConfig_EnvAddrOverridesDefaultListenerOnly(t *testing.T) {
	cfg := loadConfigForTest(t, writeTempConfig(t,
		`{"frontends":[{"type":"s3"},{"type":"webdav","listenAddr":":8444"}]}`))
	if cfg.ListenAddr != ":8443" {
		t.Fatalf("ListenAddr = %q, want :8443 (loadConfig ignores env)", cfg.ListenAddr)
	}
	t.Setenv("MINIS3_LISTEN_ADDR", ":9999")
	applyListenAddrOverride(&cfg)
	if cfg.ListenAddr != ":9999" {
		t.Fatalf("ListenAddr = %q, want :9999 (env override)", cfg.ListenAddr)
	}
	if len(cfg.Frontends) != 2 || cfg.Frontends[1].ListenAddr != ":8444" {
		t.Fatalf("Frontends = %+v, want per-frontend listenAddr untouched", cfg.Frontends)
	}
}

// TestLoadConfigUnknownBucketObjectKeyFails pins the regression-review
// E6 follow-up: unknown keys inside the per-bucket object form must fail
// the parse (a typo like "pth" would otherwise silently drop the custom
// path).
func TestLoadConfigUnknownBucketObjectKeyFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := `{"buckets": {"photos": {"pth": "/tmp/x"}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(cfgPath); err == nil {
		t.Fatal("loadConfig with unknown key inside bucket object form: got nil error, want failure")
	}
	// The known-good object form must still parse.
	good := `{"buckets": {"photos": {"path": "` + dir + `"}}}`
	if err := os.WriteFile(cfgPath, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(cfgPath); err != nil {
		t.Fatalf("loadConfig with valid bucket object form: %v", err)
	}
}
