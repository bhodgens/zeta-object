// config_versioning_test.go — the zfs_versioning config key
// (s3-versioning-2026-10 tree, leaf 03 / Contract 3): default at load,
// explicit values honored, unknown values abort startup, and the key
// survives the DisallowUnknownFields UnmarshalJSON alias.
package main

import (
	"strings"
	"testing"
)

// TestLoadConfig_ZfsVersioningKey pins the Contract 3 config contract:
// absent/empty -> "snapshots" (the default), explicit vocabulary values
// pass through, and anything else fails loadConfig loudly.
func TestLoadConfig_ZfsVersioningKey(t *testing.T) {
	t.Run("absent key gets default", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"dataDir":"./data/"}`))
		if cfg.ZfsVersioning != defaultZfsVersioning {
			t.Fatalf("ZfsVersioning = %q, want default %q", cfg.ZfsVersioning, defaultZfsVersioning)
		}
		if defaultZfsVersioning != "snapshots" {
			t.Fatalf("default drifted from Contract 3: %q", defaultZfsVersioning)
		}
	})

	t.Run("explicit snapshots honored", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"zfs_versioning":"snapshots"}`))
		if cfg.ZfsVersioning != "snapshots" {
			t.Fatalf("ZfsVersioning = %q, want snapshots", cfg.ZfsVersioning)
		}
	})

	t.Run("explicit sidecar honored", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"zfs_versioning":"sidecar"}`))
		if cfg.ZfsVersioning != "sidecar" {
			t.Fatalf("ZfsVersioning = %q, want sidecar", cfg.ZfsVersioning)
		}
	})

	t.Run("explicit both honored", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"zfs_versioning":"both"}`))
		if cfg.ZfsVersioning != "both" {
			t.Fatalf("ZfsVersioning = %q, want both", cfg.ZfsVersioning)
		}
	})

	t.Run("empty string falls back to default", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfig(t, `{"zfs_versioning":""}`))
		if cfg.ZfsVersioning != defaultZfsVersioning {
			t.Fatalf("empty zfs_versioning must normalize to the default, got %q", cfg.ZfsVersioning)
		}
	})

	t.Run("unknown value aborts load", func(t *testing.T) {
		if err := loadConfig(writeTempConfig(t, `{"zfs_versioning":"shapshots"}`)); err == nil {
			t.Fatal("unknown zfs_versioning must fail loadConfig")
		} else if !strings.Contains(err.Error(), "zfs_versioning") {
			t.Fatalf("error must name the offending key, got: %v", err)
		}
	})

	t.Run("missing config file keeps default", func(t *testing.T) {
		cfg := loadConfigForTest(t, writeTempConfigNamed(t))
		if cfg.ZfsVersioning != defaultZfsVersioning {
			t.Fatalf("defaults not applied for missing config: %q", cfg.ZfsVersioning)
		}
	})
}

// writeTempConfigNamed returns a config path that does not exist (the
// missing-file default path through loadConfig).
func writeTempConfigNamed(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/does-not-exist.json"
}

// TestUnmarshalJSON_AcceptsZfsVersioning pins the alias decode: the
// DisallowUnknownFields path must accept the key (a reject would abort
// every config carrying it before loadConfig's validation runs).
func TestUnmarshalJSON_AcceptsZfsVersioning(t *testing.T) {
	var c ServerConfig
	if err := c.UnmarshalJSON([]byte(`{"zfs_versioning":"both"}`)); err != nil {
		t.Fatalf("UnmarshalJSON rejected zfs_versioning: %v", err)
	}
	if c.ZfsVersioning != "both" {
		t.Fatalf("ZfsVersioning = %q, want both", c.ZfsVersioning)
	}
}
