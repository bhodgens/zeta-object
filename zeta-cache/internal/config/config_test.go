package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustWrite writes content to a temp file and returns its path.
func mustWrite(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zeta-cache.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// validConfig is the minimal passing config (Basic auth shape).
const validConfig = `{
  "serverUrl": "https://example.net:8443",
  "bucket": "bkt",
  "auth": {"accessKey": "ak", "secretKey": "sk"},
  "mountpoint": "/tmp/zeta-mnt"
}`

func TestLoadValid(t *testing.T) {
	cfg, err := Load(mustWrite(t, validConfig))
	if err != nil {
		t.Fatalf("Load(valid) = %v, want nil", err)
	}
	if cfg.ServerURL != "https://example.net:8443" || cfg.Bucket != "bkt" || cfg.Mountpoint != "/tmp/zeta-mnt" {
		t.Errorf("basic fields wrong: %+v", cfg)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("logLevel default = %q, want info", cfg.LogLevel)
	}
	// Defaults must be populated for cacheDir/indexDB/ipcSocket.
	for _, key := range []string{cfg.CacheDir, cfg.IndexDB, cfg.IPCSocket} {
		if key == "" {
			t.Errorf("default for cacheDir/indexDB/ipcSocket missing: %+v", cfg)
		}
	}
	if cfg.IndexDB != filepath.Join(cfg.CacheDir, "index.db") {
		t.Errorf("indexDB default = %q, want cacheDir/index.db", cfg.IndexDB)
	}
}

func TestLoadUnknownKeyAbortsAndNamesIt(t *testing.T) {
	_, err := Load(mustWrite(t, `{
  "serverUrl": "https://example.net",
  "bucket": "bkt",
  "mountpoint": "/mnt",
  "auth": {"accessKey": "ak", "secretKey": "sk"},
  "serverUrlr": "typo"
}`))
	if err == nil {
		t.Fatal("Load(unknown key) = nil error, want fail-loud abort")
	}
	if !strings.Contains(err.Error(), "serverUrlr") {
		t.Errorf("error %q does not name the offending key serverUrlr", err)
	}
}

func TestLoadBothAuthShapesRejected(t *testing.T) {
	_, err := Load(mustWrite(t, `{
  "serverUrl": "https://example.net",
  "bucket": "bkt",
  "mountpoint": "/mnt",
  "auth": {"accessKey": "ak", "secretKey": "sk", "clientCert": "/c.pem", "clientKey": "/k.pem"}
}`))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("Load(both auth shapes) = %v, want 'exactly one' error", err)
	}
}

func TestLoadNoAuthShapeRejected(t *testing.T) {
	_, err := Load(mustWrite(t, `{
  "serverUrl": "https://example.net",
  "bucket": "bkt",
  "mountpoint": "/mnt",
  "auth": {}
}`))
	if err == nil || !strings.Contains(err.Error(), "one credential shape is required") {
		t.Errorf("Load(no auth) = %v, want required error", err)
	}
}

func TestLoadHalfAuthShapeRejected(t *testing.T) {
	for name, auth := range map[string]string{
		"basic without secret": `{"accessKey": "ak"}`,
		"mtls without key":     `{"clientCert": "/c.pem"}`,
	} {
		body := `{"serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt", "auth": ` + auth + `}`
		if _, err := Load(mustWrite(t, body)); err == nil {
			t.Errorf("%s: Load = nil error, want rejection", name)
		}
	}
}

func TestLoadMTLSShapeAccepted(t *testing.T) {
	cfg, err := Load(mustWrite(t, `{
  "serverUrl": "https://example.net",
  "bucket": "bkt",
  "mountpoint": "/mnt",
  "auth": {"clientCert": "/c.pem", "clientKey": "/k.pem"}
}`))
	if err != nil {
		t.Fatalf("Load(mtls) = %v, want nil", err)
	}
	if cfg.Auth.ClientCert != "/c.pem" || cfg.Auth.ClientKey != "/k.pem" {
		t.Errorf("mtls fields lost: %+v", cfg.Auth)
	}
}

func TestLoadRequiredFields(t *testing.T) {
	for _, key := range []string{"serverUrl", "bucket", "mountpoint"} {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(validConfig), &cfg); err != nil {
			t.Fatal(err)
		}
		delete(cfg, key)
		body, _ := json.Marshal(cfg)
		_, err := Load(mustWrite(t, string(body)))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("missing %s: Load = %v, want error naming %s", key, err, key)
		}
	}
}

func TestLoadQuotaAndLogLevelValidation(t *testing.T) {
	if _, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "quota": {"maxCacheBytes": 100, "minFreeDeviceBytes": 10, "policy": "bogus"}
}`)); err == nil || !strings.Contains(err.Error(), "quota.policy") {
		t.Errorf("bad quota.policy = %v, want error", err)
	}
	if _, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "quota": {"policy": "lifo"}
}`)); err == nil || !strings.Contains(err.Error(), "quota.policy") {
		t.Errorf("lifo (leaf-01 placeholder set) = %v, want error naming quota.policy", err)
	}
	if _, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "quota": {"highWaterPercent": 50, "lowWaterPercent": 80}
}`)); err == nil || !strings.Contains(err.Error(), "highWaterPercent") {
		t.Errorf("high <= low = %v, want error", err)
	}
	if _, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "quota": {"highWaterPercent": 110}
}`)); err == nil || !strings.Contains(err.Error(), "highWaterPercent") {
		t.Errorf("high > 100 = %v, want error", err)
	}
	if _, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "quota": {"tombstoneRetentionDays": -1}
}`)); err == nil || !strings.Contains(err.Error(), "tombstoneRetentionDays") {
		t.Errorf("negative retention = %v, want error", err)
	}
	if _, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "logLevel": "loud"
}`)); err == nil || !strings.Contains(err.Error(), "logLevel") {
		t.Errorf("bad logLevel = %v, want error", err)
	}
}

func TestLoadQuotaDefaultsAndPolicySet(t *testing.T) {
	for _, policy := range []string{"size", "size+age", "lru"} {
		cfg, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "quota": {"policy": "`+policy+`"}
}`))
		if err != nil {
			t.Fatalf("policy %s: %v", policy, err)
		}
		if cfg.Quota.Policy != policy {
			t.Errorf("policy = %q, want %q", cfg.Quota.Policy, policy)
		}
	}
	// Empty policy defaults to lru; water marks and retention default to
	// the locked 90/70/30.
	cfg, err := Load(mustWrite(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Quota.Policy != "lru" {
		t.Errorf("default policy = %q, want lru", cfg.Quota.Policy)
	}
	if cfg.Quota.HighWaterPct != 90 || cfg.Quota.LowWaterPct != 70 || cfg.Quota.TombstoneDays != 30 {
		t.Errorf("quota defaults wrong: %+v", cfg.Quota)
	}
}

func TestLoadPinsAndQuotaParsed(t *testing.T) {
	cfg, err := Load(mustWrite(t, `{
  "serverUrl": "https://x", "bucket": "b", "mountpoint": "/mnt",
  "auth": {"accessKey": "a", "secretKey": "s"},
  "quota": {"maxCacheBytes": 100, "minFreeDeviceBytes": 10, "policy": "lru"},
  "pins": ["a/", "b/c"]
}`))
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if len(cfg.Pins) != 2 || cfg.Pins[1] != "b/c" {
		t.Errorf("pins not parsed: %v", cfg.Pins)
	}
	if cfg.Quota.MaxCacheBytes != 100 || cfg.Quota.MinFreeDevBytes != 10 || cfg.Quota.Policy != "lru" {
		t.Errorf("quota not parsed: %+v", cfg.Quota)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("Load(missing file) = nil error, want fail-loud")
	}
}
