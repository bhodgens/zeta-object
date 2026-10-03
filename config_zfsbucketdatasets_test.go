package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigZfsBucketDatasetsKeysDecode pins the zfs-bucket-datasets leaf
// 01 frozen JSON keys: `zfs_bucket_datasets` (bool, default false) and
// `zfs_binary` (string, default "zfs"). Absent keys get defaults at
// config load; explicit values decode verbatim; unknown top-level keys
// still abort (DisallowUnknownFields unchanged).
func TestConfigZfsBucketDatasetsKeysDecode(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		verify  func(t *testing.T, c ServerConfig)
		wantErr bool
	}{
		{
			name: "absent keys get defaults",
			json: `{"dataDir":"./data/"}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.ZfsBucketDatasets {
					t.Fatal("ZfsBucketDatasets = true, want default false")
				}
				if c.ZfsBinary != defaultZfsBinary {
					t.Fatalf("ZfsBinary = %q, want default %q", c.ZfsBinary, defaultZfsBinary)
				}
			},
		},
		{
			name: "explicit feature on + custom binary",
			json: `{"dataDir":"./data/","zfs_bucket_datasets":true,"zfs_binary":"/usr/local/sbin/zfs"}`,
			verify: func(t *testing.T, c ServerConfig) {
				if !c.ZfsBucketDatasets {
					t.Fatal("ZfsBucketDatasets = false, want true")
				}
				if c.ZfsBinary != "/usr/local/sbin/zfs" {
					t.Fatalf("ZfsBinary = %q, want /usr/local/sbin/zfs", c.ZfsBinary)
				}
			},
		},
		{
			name: "explicit feature on with default binary omitted",
			json: `{"dataDir":"./data/","zfs_bucket_datasets":true}`,
			verify: func(t *testing.T, c ServerConfig) {
				if !c.ZfsBucketDatasets {
					t.Fatal("ZfsBucketDatasets = false, want true")
				}
				if c.ZfsBinary != defaultZfsBinary {
					t.Fatalf("ZfsBinary = %q, want %q (explicit empty falls back to default)", c.ZfsBinary, defaultZfsBinary)
				}
			},
		},
		{
			name:    "unknown sibling key inside root still aborts",
			json:    `{"dataDir":"./data/","zfs_bucket_datasetz":true}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfgPath, []byte(tc.json), 0644); err != nil {
				t.Fatal(err)
			}
			err := loadConfig(cfgPath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("loadConfig(%s) succeeded; want error", tc.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig(%s): %v", tc.json, err)
			}
			if tc.verify != nil {
				tc.verify(t, serverConfig)
			}
		})
	}
}

// TestConfigZfsBucketDatasetsUnmarshalAlias pins the custom
// UnmarshalJSON path: both new fields ride through the alias struct's
// DisallowUnknownFields decoding, so a config with the new keys parses
// via json.Unmarshal and a typo still fails.
func TestConfigZfsBucketDatasetsUnmarshalAlias(t *testing.T) {
	var c ServerConfig
	if err := json.Unmarshal([]byte(`{"zfs_bucket_datasets":true,"zfs_binary":"zfz"}`), &c); err != nil {
		t.Fatalf("UnmarshalJSON rejected known keys: %v", err)
	}
	if !c.ZfsBucketDatasets || c.ZfsBinary != "zfz" {
		t.Fatalf("decoded ZfsBucketDatasets=%v ZfsBinary=%q; want true/\"zfz\"", c.ZfsBucketDatasets, c.ZfsBinary)
	}
	var typo ServerConfig
	err := json.Unmarshal([]byte(`{"zfs_bucket_datasetsz":true}`), &typo)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("typo key error = %v, want unknown-field error", err)
	}
}
