package main

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// main_lifecycle_test.go — leaf 2.6 tests for server lifecycle + config.

func TestNewServer(t *testing.T) {
	handler := http.NewServeMux()
	srv := newServer(":18443", handler, "certs/cert.pem", "certs/key.pem")

	if srv.Addr != ":18443" {
		t.Errorf("Addr = %q, want %q", srv.Addr, ":18443")
	}
	if srv.Handler != handler {
		t.Error("Handler not set to provided handler")
	}
	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig is nil")
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLSConfig.MinVersion = %x, want %x (TLS 1.2)", srv.TLSConfig.MinVersion, tls.VersionTLS12)
	}
	if srv.ReadTimeout != serverReadTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, serverReadTimeout)
	}
	if srv.ReadHeaderTimeout != serverReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, serverReadHeaderTimeout)
	}
	if srv.WriteTimeout != serverWriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, serverWriteTimeout)
	}
	if srv.IdleTimeout != serverIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, serverIdleTimeout)
	}
	// Spot-check the concrete values so a const drift is caught
	if serverReadTimeout != 30*time.Second {
		t.Errorf("serverReadTimeout = %v, want 30s", serverReadTimeout)
	}
	if serverReadHeaderTimeout != 10*time.Second {
		t.Errorf("serverReadHeaderTimeout = %v, want 10s", serverReadHeaderTimeout)
	}
	if serverWriteTimeout != 5*time.Minute {
		t.Errorf("serverWriteTimeout = %v, want 5m", serverWriteTimeout)
	}
	if serverIdleTimeout != 120*time.Second {
		t.Errorf("serverIdleTimeout = %v, want 120s", serverIdleTimeout)
	}
	if serverMinTLSVersion != tls.VersionTLS12 {
		t.Errorf("serverMinTLSVersion = %x, want %x", serverMinTLSVersion, tls.VersionTLS12)
	}
}

// TestLoadConfigDefaultsWhenFileMissing: missing file → defaults, no error.
func TestLoadConfigDefaultsWhenFileMissing(t *testing.T) {
	// Snapshot & restore the global so other tests are unaffected
	origConfig := *serverConfig()
	defer func() { setServerConfig(origConfig) }()

	if err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.json")); err != nil {
		t.Fatalf("loadConfig on missing file returned error: %v", err)
	}
	if serverConfig().DataDir != defaultDataDir {
		t.Errorf("DataDir = %q, want default %q", serverConfig().DataDir, defaultDataDir)
	}
	if serverConfig().ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want default %q", serverConfig().ListenAddr, defaultListenAddr)
	}
	if serverConfig().CertFile != defaultCertFile {
		t.Errorf("CertFile = %q, want default %q", serverConfig().CertFile, defaultCertFile)
	}
	if serverConfig().KeyFile != defaultKeyFile {
		t.Errorf("KeyFile = %q, want default %q", serverConfig().KeyFile, defaultKeyFile)
	}
	if serverConfig().Buckets == nil {
		t.Error("Buckets map should be initialized, not nil")
	}
}

// TestLoadConfigNoPartialMutation: invalid JSON must leave the previous
// (valid) config fully intact — no partial mutation of the global.
func TestLoadConfigNoPartialMutation(t *testing.T) {
	origConfig := *serverConfig()
	defer func() { setServerConfig(origConfig) }()

	// Seed the global with known-valid values
	setServerConfig(defaultServerConfig())
	setServerConfigField(func(c *ServerConfig) { c.DataDir = "/original/data/" })
	setServerConfigField(func(c *ServerConfig) { c.ListenAddr = ":9999" })
	setServerConfigField(func(c *ServerConfig) { c.CertFile = "/original/cert.pem" })
	setServerConfigField(func(c *ServerConfig) { c.KeyFile = "/original/key.pem" })

	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.json")
	badJSONs := []string{
		`{invalid json`,
		`{"dataDir": ,}`,
		`{"dataDir": "untouched but buckets broken", "buckets": [1,2]}`,
	}
	for i, bad := range badJSONs {
		if err := os.WriteFile(badPath, []byte(bad), 0644); err != nil {
			t.Fatal(err)
		}
		if err := loadConfig(badPath); err == nil {
			t.Fatalf("case %d: loadConfig should fail on invalid JSON", i)
		}
		// Global must be untouched
		if serverConfig().DataDir != "/original/data/" {
			t.Errorf("case %d: DataDir mutated to %q", i, serverConfig().DataDir)
		}
		if serverConfig().ListenAddr != ":9999" {
			t.Errorf("case %d: ListenAddr mutated to %q", i, serverConfig().ListenAddr)
		}
		if serverConfig().CertFile != "/original/cert.pem" {
			t.Errorf("case %d: CertFile mutated to %q", i, serverConfig().CertFile)
		}
		if serverConfig().KeyFile != "/original/key.pem" {
			t.Errorf("case %d: KeyFile mutated to %q", i, serverConfig().KeyFile)
		}
	}
}

// TestLoadConfigParsesNewKeys: listenAddr/certFile/keyFile JSON keys parse
// and normalize correctly.
func TestLoadConfigParsesNewKeys(t *testing.T) {
	origConfig := *serverConfig()
	defer func() { setServerConfig(origConfig) }()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	content := `{
		"dataDir": "` + filepath.ToSlash(dir) + `/customdata",
		"listenAddr": ":18443",
		"certFile": "tls/server.crt",
		"keyFile": "tls/server.key"
	}`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(cfgPath); err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if serverConfig().DataDir != filepath.ToSlash(dir)+"/customdata/" {
		t.Errorf("DataDir = %q (want trailing slash appended)", serverConfig().DataDir)
	}
	if serverConfig().ListenAddr != ":18443" {
		t.Errorf("ListenAddr = %q, want :18443", serverConfig().ListenAddr)
	}
	if serverConfig().CertFile != "tls/server.crt" {
		t.Errorf("CertFile = %q, want tls/server.crt", serverConfig().CertFile)
	}
	if serverConfig().KeyFile != "tls/server.key" {
		t.Errorf("KeyFile = %q, want tls/server.key", serverConfig().KeyFile)
	}

	// Empty listenAddr/cert keys normalize to defaults
	content = `{"listenAddr": "", "certFile": "", "keyFile": ""}`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(cfgPath); err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if serverConfig().ListenAddr != defaultListenAddr {
		t.Errorf("empty listenAddr: got %q, want default", serverConfig().ListenAddr)
	}
	if serverConfig().CertFile != defaultCertFile {
		t.Errorf("empty certFile: got %q, want default", serverConfig().CertFile)
	}
	if serverConfig().KeyFile != defaultKeyFile {
		t.Errorf("empty keyFile: got %q, want default", serverConfig().KeyFile)
	}
}

// TestLoadConfigReadErrorNotIsNotExist: a read error (e.g. directory as
// config path) must return an error, not silently use defaults.
func TestLoadConfigReadErrorNotIsNotExist(t *testing.T) {
	origConfig := *serverConfig()
	defer func() { setServerConfig(origConfig) }()

	// Reading a directory yields a read error that is not IsNotExist
	if err := loadConfig(t.TempDir()); err == nil {
		t.Error("loadConfig on a directory path should return an error")
	}
}

// TestLoadCredentialsEmptyEnvWarns: SET-but-EMPTY env vars fall back to the
// default with a warning; unset vars fall back silently.
func TestLoadCredentialsEmptyEnvWarns(t *testing.T) {
	origAK := serverCredentials.AccessKeyID
	origSK := serverCredentials.SecretAccessKey
	defer func() {
		serverCredentials.AccessKeyID = origAK
		serverCredentials.SecretAccessKey = origSK
	}()

	t.Run("set but empty falls back to default", func(t *testing.T) {
		t.Setenv("ZETAOBJECT_ACCESS_KEY", "")
		t.Setenv("ZETAOBJECT_SECRET_KEY", "realsecret")
		loadCredentials()
		if serverCredentials.AccessKeyID != defaultAccessKey {
			t.Errorf("AccessKeyID = %q, want default %q", serverCredentials.AccessKeyID, defaultAccessKey)
		}
		if serverCredentials.SecretAccessKey != "realsecret" {
			t.Errorf("SecretAccessKey = %q, want realsecret", serverCredentials.SecretAccessKey)
		}
	})

	t.Run("unset uses default", func(t *testing.T) {
		os.Unsetenv("ZETAOBJECT_ACCESS_KEY")
		os.Unsetenv("ZETAOBJECT_SECRET_KEY")
		loadCredentials()
		if serverCredentials.AccessKeyID != defaultAccessKey {
			t.Errorf("AccessKeyID = %q, want default", serverCredentials.AccessKeyID)
		}
		if serverCredentials.SecretAccessKey != defaultAccessKey {
			t.Errorf("SecretAccessKey = %q, want default", serverCredentials.SecretAccessKey)
		}
	})

	t.Run("set non-empty is honored", func(t *testing.T) {
		t.Setenv("ZETAOBJECT_ACCESS_KEY", "mykey")
		t.Setenv("ZETAOBJECT_SECRET_KEY", "mysecret")
		loadCredentials()
		if serverCredentials.AccessKeyID != "mykey" {
			t.Errorf("AccessKeyID = %q, want mykey", serverCredentials.AccessKeyID)
		}
		if serverCredentials.SecretAccessKey != "mysecret" {
			t.Errorf("SecretAccessKey = %q, want mysecret", serverCredentials.SecretAccessKey)
		}
	})
}

// TestEnvPrefixFallback: the deprecated MINIS3_* prefix still works, and
// ZETAOBJECT_* wins when both prefixes are set. Remove with the fallback in
// two minor releases after the zeta-object rename.
func TestEnvPrefixFallback(t *testing.T) {
	origAK := serverCredentials.AccessKeyID
	origSK := serverCredentials.SecretAccessKey
	defer func() {
		serverCredentials.AccessKeyID = origAK
		serverCredentials.SecretAccessKey = origSK
	}()

	t.Run("legacy MINIS3_ is honored when ZETAOBJECT_ unset", func(t *testing.T) {
		os.Unsetenv("ZETAOBJECT_ACCESS_KEY")
		os.Unsetenv("ZETAOBJECT_SECRET_KEY")
		t.Setenv("MINIS3_ACCESS_KEY", "legacykey")
		t.Setenv("MINIS3_SECRET_KEY", "legacysecret")
		loadCredentials()
		if serverCredentials.AccessKeyID != "legacykey" {
			t.Errorf("AccessKeyID = %q, want legacykey from MINIS3_ACCESS_KEY", serverCredentials.AccessKeyID)
		}
		if serverCredentials.SecretAccessKey != "legacysecret" {
			t.Errorf("SecretAccessKey = %q, want legacysecret from MINIS3_SECRET_KEY", serverCredentials.SecretAccessKey)
		}
	})

	t.Run("ZETAOBJECT_ wins when both are set", func(t *testing.T) {
		t.Setenv("ZETAOBJECT_ACCESS_KEY", "newkey")
		t.Setenv("ZETAOBJECT_SECRET_KEY", "newsecret")
		t.Setenv("MINIS3_ACCESS_KEY", "legacykey")
		t.Setenv("MINIS3_SECRET_KEY", "legacysecret")
		loadCredentials()
		if serverCredentials.AccessKeyID != "newkey" {
			t.Errorf("AccessKeyID = %q, want newkey (ZETAOBJECT_ must win)", serverCredentials.AccessKeyID)
		}
		if serverCredentials.SecretAccessKey != "newsecret" {
			t.Errorf("SecretAccessKey = %q, want newsecret (ZETAOBJECT_ must win)", serverCredentials.SecretAccessKey)
		}
	})

	t.Run("legacy config path MINIS3_CONFIG honored", func(t *testing.T) {
		os.Unsetenv("ZETAOBJECT_CONFIG")
		t.Setenv("MINIS3_CONFIG", "/tmp/legacy-config.json")
		if got := getEnvOrDefaultLegacy("ZETAOBJECT_CONFIG", "config.json"); got != "/tmp/legacy-config.json" {
			t.Errorf("getEnvOrDefaultLegacy = %q, want legacy MINIS3_CONFIG value", got)
		}
	})

	t.Run("new config path wins over legacy", func(t *testing.T) {
		t.Setenv("ZETAOBJECT_CONFIG", "/tmp/new-config.json")
		t.Setenv("MINIS3_CONFIG", "/tmp/legacy-config.json")
		if got := getEnvOrDefaultLegacy("ZETAOBJECT_CONFIG", "config.json"); got != "/tmp/new-config.json" {
			t.Errorf("getEnvOrDefaultLegacy = %q, want new ZETAOBJECT_CONFIG value", got)
		}
	})
}

// TestServerConfigExampleJSONParses: config.json.example stays valid against
// the ServerConfig struct (guards doc drift).
func TestServerConfigExampleJSONParses(t *testing.T) {
	data, err := os.ReadFile("config.json.example")
	if err != nil {
		t.Skipf("config.json.example not found: %v", err)
	}
	var cfg ServerConfig
	if err := json.Unmarshal(stripJSON5Comments(data), &cfg); err != nil {
		t.Errorf("config.json.example does not parse into ServerConfig: %v", err)
	}
}

// TestNewServerServesOverTLS12 smoke: httptest can use the tls.Config from
// newServer (proves the config is valid for a real TLS listener).
func TestNewServerTLSConfigUsable(t *testing.T) {
	srv := newServer(":0", http.NewServeMux(), "", "")
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.TLS = srv.TLSConfig
	ts.StartTLS()
	defer ts.Close()

	client := ts.Client()
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("request over TLS failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Error("connection did not negotiate TLS >= 1.2")
	}
}
