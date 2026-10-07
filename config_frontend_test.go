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

// loadConfigForTest loads a config file into the *serverConfig() global and
// returns the normalized result. loadConfig keeps its landed signature
// (error-only, global-populating) — see drift note in the leaf report.
func loadConfigForTest(t *testing.T, path string) ServerConfig {
	t.Helper()
	if err := loadConfig(path); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return *serverConfig()
}

func TestLoadConfig_Frontends(t *testing.T) {
	tests := []struct {
		name       string
		configJSON string
		envAddr    string // ZETAOBJECT_LISTEN_ADDR override, "" = unset
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
				t.Setenv("ZETAOBJECT_LISTEN_ADDR", tt.envAddr)
			}
			cfg := loadConfigForTest(t, writeTempConfig(t, tt.configJSON))
			if len(cfg.Frontends) != len(tt.want) {
				t.Fatalf("Frontends = %+v, want %+v", cfg.Frontends, tt.want)
			}
			for i := range tt.want {
				// Field-wise compare: FrontendConfig carries an Options map
				// (sftp-ftp-2026-09 leaf 01), which makes the struct
				// non-comparable.
				if cfg.Frontends[i].Type != tt.want[i].Type ||
					cfg.Frontends[i].ListenAddr != tt.want[i].ListenAddr ||
					cfg.Frontends[i].Bucket != tt.want[i].Bucket ||
					!stringMapsEqual(cfg.Frontends[i].Options, tt.want[i].Options) {
					t.Fatalf("Frontends[%d] = %+v, want %+v", i, cfg.Frontends[i], tt.want[i])
				}
			}
			if cfg.ListenAddr != tt.wantAddr {
				t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, tt.wantAddr)
			}
		})
	}
}

// ZETAOBJECT_LISTEN_ADDR keeps overriding only the default listener address
// (the env override itself is applied in main() via applyListenAddrOverride;
// loadConfig stays env-free).
func TestLoadConfig_EnvAddrOverridesDefaultListenerOnly(t *testing.T) {
	cfg := loadConfigForTest(t, writeTempConfig(t,
		`{"frontends":[{"type":"s3"},{"type":"webdav","listenAddr":":8444"}]}`))
	if cfg.ListenAddr != ":8443" {
		t.Fatalf("ListenAddr = %q, want :8443 (loadConfig ignores env)", cfg.ListenAddr)
	}
	t.Setenv("ZETAOBJECT_LISTEN_ADDR", ":9999")
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

// stringMapsEqual compares two possibly-nil string maps by content
// (FrontendConfig.Options made the struct non-comparable).
func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// sftp-ftp-2026-09 leaf 01 Contract B: the options map decodes on frontends
// entries; absent options decodes to nil (backward compatible).
func TestLoadConfig_FrontendOptions(t *testing.T) {
	cfg := loadConfigForTest(t, writeTempConfig(t,
		`{"frontends":[{"type":"ftp","listenAddr":":2121","options":{"passivePortMin":"50000","passivePortMax":"50100"}},{"type":"s3"}]}`))
	if len(cfg.Frontends) != 2 {
		t.Fatalf("Frontends = %+v, want 2 entries", cfg.Frontends)
	}
	ftp := cfg.Frontends[0]
	if ftp.Type != "ftp" || ftp.ListenAddr != ":2121" {
		t.Fatalf("ftp entry = %+v", ftp)
	}
	want := map[string]string{"passivePortMin": "50000", "passivePortMax": "50100"}
	if !stringMapsEqual(ftp.Options, want) {
		t.Fatalf("Options = %+v, want %+v", ftp.Options, want)
	}
	if cfg.Frontends[1].Options != nil {
		t.Fatalf("absent options must decode to nil, got %+v", cfg.Frontends[1].Options)
	}
}

// TestLoadConfig_ZmetadKeys pins the leaf-04 zmetad config contract
// (zmetad-provider-2026-09 Contract 4): both keys parse when present,
// and absent keys fall back to the defaults at config load - one place
// owns the defaults, so the wiring and provider always see concrete
// values.
func TestLoadConfig_ZmetadKeys(t *testing.T) {
	t.Run("absent keys get defaults", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"dataDir":"./data/"}`))
		if cfg.ZmetadDBPath != defaultZmetadDBPath {
			t.Fatalf("ZmetadDBPath = %q, want default %q", cfg.ZmetadDBPath, defaultZmetadDBPath)
		}
		if cfg.ZmetadBinary != defaultZmetadBinary {
			t.Fatalf("ZmetadBinary = %q, want default %q", cfg.ZmetadBinary, defaultZmetadBinary)
		}
		if defaultZmetadDBPath != "/var/lib/zfs/zmetad.db" || defaultZmetadBinary != "zmetad" {
			t.Fatalf("defaults drifted from Contract 4: db=%q binary=%q", defaultZmetadDBPath, defaultZmetadBinary)
		}
	})
	t.Run("explicit values are honored", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t,
			`{"zmetad_db_path":"/tmp/custom/zmetad.db","zmetad_binary":"/usr/local/bin/zmetad"}`))
		if cfg.ZmetadDBPath != "/tmp/custom/zmetad.db" {
			t.Fatalf("ZmetadDBPath = %q, want the configured path", cfg.ZmetadDBPath)
		}
		if cfg.ZmetadBinary != "/usr/local/bin/zmetad" {
			t.Fatalf("ZmetadBinary = %q, want the configured binary", cfg.ZmetadBinary)
		}
	})
	t.Run("empty strings fall back to defaults", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t,
			`{"zmetad_db_path":"","zmetad_binary":""}`))
		if cfg.ZmetadDBPath != defaultZmetadDBPath || cfg.ZmetadBinary != defaultZmetadBinary {
			t.Fatalf("empty zmetad keys must normalize to defaults, got db=%q binary=%q",
				cfg.ZmetadDBPath, cfg.ZmetadBinary)
		}
	})
	t.Run("missing config file keeps defaults", func(t *testing.T) {
		cfg := loadConfigForTest(t, filepath.Join(t.TempDir(), "does-not-exist.json"))
		if cfg.ZmetadDBPath != defaultZmetadDBPath || cfg.ZmetadBinary != defaultZmetadBinary {
			t.Fatalf("defaults not applied for missing config: db=%q binary=%q",
				cfg.ZmetadDBPath, cfg.ZmetadBinary)
		}
	})
}

func TestLoadConfig_RegionKey(t *testing.T) {
	t.Run("absent key gets default", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"dataDir":"./data/"}`))
		if cfg.Region != defaultS3Region {
			t.Fatalf("Region = %q, want default %q", cfg.Region, defaultS3Region)
		}
	})
	t.Run("explicit value is honored", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"region":"eu-west-1"}`))
		if cfg.Region != "eu-west-1" {
			t.Fatalf("Region = %q, want the configured value", cfg.Region)
		}
	})
	t.Run("value is lowercased at load", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"region":"EU-West-1"}`))
		if cfg.Region != "eu-west-1" {
			t.Fatalf("Region = %q, want normalized %q (SigV4 regions are lowercase)", cfg.Region, "eu-west-1")
		}
	})
	t.Run("empty string falls back to default", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"region":""}`))
		if cfg.Region != defaultS3Region {
			t.Fatalf("Region = %q, want default %q", cfg.Region, defaultS3Region)
		}
	})
}
