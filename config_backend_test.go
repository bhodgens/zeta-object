package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadConfigBackendKeysBackwardCompat pins the leaf-03 config schema:
// both the legacy bare-string bucket value and the new object form parse,
// and the backends map loads. JSON keys are frozen here.
func TestLoadConfigBackendKeysBackwardCompat(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		verify  func(t *testing.T, c ServerConfig)
		wantErr bool
	}{
		{
			name: "legacy string bucket value still parses",
			json: `{"dataDir":"./data/","buckets":{"photos":"/mnt/photos"}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Buckets["photos"] != "/mnt/photos" {
					t.Fatalf("Buckets[photos] = %q, want /mnt/photos", c.Buckets["photos"])
				}
			},
		},
		{
			name: "object bucket value with backend",
			json: `{"buckets":{"photos":{"path":"/mnt/photos","backend":"fs"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Buckets["photos"] != "/mnt/photos" {
					t.Fatalf("Buckets[photos] = %q, want /mnt/photos", c.Buckets["photos"])
				}
				if c.BucketBackends["photos"] != "fs" {
					t.Fatalf("BucketBackends[photos] = %q, want fs", c.BucketBackends["photos"])
				}
			},
		},
		{
			name: "object bucket value without backend defaults empty",
			json: `{"buckets":{"photos":{"path":"/mnt/photos"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Buckets["photos"] != "/mnt/photos" {
					t.Fatalf("Buckets[photos] = %q, want /mnt/photos", c.Buckets["photos"])
				}
				if c.BucketBackends["photos"] != "" {
					t.Fatalf("BucketBackends[photos] = %q, want empty", c.BucketBackends["photos"])
				}
			},
		},
		{
			name: "backends map loads",
			json: `{"backends":{"fs":{"root":"./data/"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Backends["fs"].Root != "./data/" {
					t.Fatalf("Backends[fs].Root = %q, want ./data/", c.Backends["fs"].Root)
				}
			},
		},
		{
			name: "backends options load",
			json: `{"backends":{"proxy":{"root":"","options":{"endpoint":"http://s3.local"}}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Backends["proxy"].Options["endpoint"] != "http://s3.local" {
					t.Fatalf("options lost: %+v", c.Backends["proxy"])
				}
			},
		},
		{
			name: "no new keys at all parses as before",
			json: `{"dataDir":"./d/","listenAddr":":9999"}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.DataDir != "./d/" || c.ListenAddr != ":9999" {
					t.Fatalf("basic keys broken: %+v", c)
				}
				if c.Buckets == nil {
					t.Fatal("Buckets should decode to empty, not break")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c ServerConfig
			err := json.Unmarshal([]byte(tc.json), &c)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil {
				tc.verify(t, c)
			}
		})
	}
}

// TestLoadConfigNullBucketValueFails: a JSON null where a bucket value was
// expected is a parse error NAMING the bucket — never a silent empty-path
// entry that half-initializes into a dataDir collision (bughunt E6).
func TestLoadConfigNullBucketValueFails(t *testing.T) {
	origConfig := serverConfig
	defer func() { serverConfig = origConfig }()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"buckets":{"photos":null}}`), 0644); err != nil {
		t.Fatal(err)
	}
	err := loadConfig(cfgPath)
	if err == nil {
		t.Fatal("loadConfig accepted a null bucket value; want parse error")
	}
	if !strings.Contains(err.Error(), "photos") {
		t.Fatalf("err = %q, want it to name the bucket", err.Error())
	}

	// The same via raw json.Unmarshal (no loadConfig normalization).
	var c ServerConfig
	if err := json.Unmarshal([]byte(`{"buckets":{"photos":null}}`), &c); err == nil {
		t.Fatal("UnmarshalJSON accepted a null bucket value")
	} else if !strings.Contains(err.Error(), "photos") {
		t.Fatalf("err = %q, want it to name the bucket", err.Error())
	}
}

// TestLoadConfigUnknownTopLevelKeyFails: DisallowUnknownFields makes a
// typo'd top-level key ("dataDirr") a loud startup failure instead of a
// silently-ignored default (bughunt E6).
func TestLoadConfigUnknownTopLevelKeyFails(t *testing.T) {
	origConfig := serverConfig
	defer func() { serverConfig = origConfig }()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"dataDirr":"./typo/"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(cfgPath); err == nil {
		t.Fatal("loadConfig accepted an unknown top-level key; want error")
	}
}

// TestLoadConfigEmptyBucketValueFails: an explicit empty object value
// (neither path nor backend) must also fail rather than half-initialize.
func TestLoadConfigEmptyBucketObjectFails(t *testing.T) {
	var c ServerConfig
	if err := json.Unmarshal([]byte(`{"buckets":{"photos":{}}}`), &c); err == nil {
		t.Fatal("UnmarshalJSON accepted an empty bucket object; want error")
	}
}

// TestBucketAuditReadsParsing (auth extensions leaf 10): the buckets
// object form accepts "auditReads" (bool); a wrong type fails loud naming
// the bucket.
func TestBucketAuditReadsParsing(t *testing.T) {
	raw := []byte(`{"dataDir": "d/", "buckets": {"plain": "/tmp/plain", "watched": {"path": "/tmp/watched", "auditReads": true}}}`)
	var cfg ServerConfig
	if err := json.Unmarshal(stripJSON5Comments(raw), &cfg); err != nil {
		t.Fatalf("valid auditReads config rejected: %v", err)
	}
	if !cfg.BucketAuditReads["watched"] {
		t.Fatal("auditReads=true did not fan out to BucketAuditReads")
	}
	if cfg.BucketAuditReads["plain"] {
		t.Fatal("plain bucket erroneously marked auditReads")
	}
	// Wrong type: fail-loud (JSON decode error naming the bucket).
	bad := []byte(`{"buckets": {"oops": {"path": "/tmp/x", "auditReads": "yes"}}}`)
	var cfg2 ServerConfig
	if err := json.Unmarshal(stripJSON5Comments(bad), &cfg2); err == nil {
		t.Fatal("non-bool auditReads accepted")
	} else if !strings.Contains(err.Error(), "oops") {
		t.Fatalf("type error does not name the bucket: %v", err)
	}
}

// TestAuditLogConfigParsing (auth extensions leaf 10): the top-level
// "auditLog" object parses; unknown keys inside it fail loud.
func TestAuditLogConfigParsing(t *testing.T) {
	raw := []byte(`{"dataDir": "d/", "auditLog": {"path": "/var/log/zeta/audit.jsonl"}}`)
	var cfg ServerConfig
	if err := json.Unmarshal(stripJSON5Comments(raw), &cfg); err != nil {
		t.Fatalf("auditLog config rejected: %v", err)
	}
	if cfg.AuditLog == nil || cfg.AuditLog.Path != "/var/log/zeta/audit.jsonl" {
		t.Fatalf("auditLog not parsed: %+v", cfg.AuditLog)
	}
	// Absent = disabled.
	var cfg2 ServerConfig
	if err := json.Unmarshal(stripJSON5Comments([]byte(`{}`)), &cfg2); err != nil {
		t.Fatal(err)
	}
	if cfg2.AuditLog != nil {
		t.Fatal("absent auditLog should be nil (disabled)")
	}
	// Unknown key inside auditLog: fail loud.
	bad := []byte(`{"auditLog": {"path": "/x", "bogus": 1}}`)
	var cfg3 ServerConfig
	if err := json.Unmarshal(stripJSON5Comments(bad), &cfg3); err == nil {
		t.Fatal("unknown auditLog key accepted")
	}
}
