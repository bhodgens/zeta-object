package adminserver

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes body to a temp admin config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "admin-config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return p
}

const validConfigJSON = `{
  "listenAddr": "127.0.0.1:0",
  "gatewayUrl": "https://127.0.0.1:9708",
  "caFile": "/tmp/ca.pem",
  "clientCert": "/tmp/client.pem",
  "clientKey": "/tmp/client-key.pem",
  "operatorToken": "secret-operator-token",
  "allowNonLoopback": false
}`

func TestConfigValidLoads(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, validConfigJSON))
	if err != nil {
		t.Fatalf("LoadConfig(valid): %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:0" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1:0", cfg.ListenAddr)
	}
	if cfg.GatewayURL != "https://127.0.0.1:9708" {
		t.Errorf("GatewayURL = %q", cfg.GatewayURL)
	}
	if cfg.OperatorToken != "secret-operator-token" {
		t.Errorf("OperatorToken not loaded")
	}
	if cfg.AllowNonLoopback {
		t.Errorf("AllowNonLoopback = true, want false")
	}
}

func TestConfigUnknownKeyAborts(t *testing.T) {
	body := `{
  "listenAddr": "127.0.0.1:0",
  "gatewayUrl": "https://127.0.0.1:9708",
  "caFile": "/tmp/ca.pem",
  "clientCert": "/tmp/client.pem",
  "clientKey": "/tmp/client-key.pem",
  "operatorToken": "secret-operator-token",
  "bogusKey": 1
}`
	if _, err := LoadConfig(writeConfig(t, body)); err == nil {
		t.Fatal("LoadConfig with unknown key: want error, got nil")
	}
}

func TestConfigMissingRequiredKey(t *testing.T) {
	cases := map[string]struct {
		json string
		key  string
	}{
		"operatorToken": {
			json: `{"listenAddr":"127.0.0.1:0","gatewayUrl":"https://x","caFile":"/a","clientCert":"/b","clientKey":"/c"}`,
			key:  "operatorToken",
		},
		"gatewayUrl": {
			json: `{"listenAddr":"127.0.0.1:0","caFile":"/a","clientCert":"/b","clientKey":"/c","operatorToken":"t"}`,
			key:  "gatewayUrl",
		},
		"caFile": {
			json: `{"listenAddr":"127.0.0.1:0","gatewayUrl":"https://x","clientCert":"/b","clientKey":"/c","operatorToken":"t"}`,
			key:  "caFile",
		},
		"clientCert": {
			json: `{"listenAddr":"127.0.0.1:0","gatewayUrl":"https://x","caFile":"/a","clientKey":"/c","operatorToken":"t"}`,
			key:  "clientCert",
		},
		"clientKey": {
			json: `{"listenAddr":"127.0.0.1:0","gatewayUrl":"https://x","caFile":"/a","clientCert":"/b","operatorToken":"t"}`,
			key:  "clientKey",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.json))
			if err == nil {
				t.Fatalf("missing %s: want error, got nil", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error %q does not name the missing key %q", err, tc.key)
			}
		})
	}
}

func TestConfigListenAddrLoopbackGuard(t *testing.T) {
	base := `"gatewayUrl":"https://x","caFile":"/a","clientCert":"/b","clientKey":"/c","operatorToken":"t"`

	accepted := []string{"127.0.0.1:0", "localhost:0", "[::1]:0", "127.0.0.1:9443"}
	for _, addr := range accepted {
		t.Run("accept/"+addr, func(t *testing.T) {
			body := `{"listenAddr":"` + addr + `",` + base + `}`
			if _, err := LoadConfig(writeConfig(t, body)); err != nil {
				t.Fatalf("loopback %q rejected: %v", addr, err)
			}
		})
	}

	rejected := []string{"0.0.0.0:9443", "192.168.1.5:9443", ":9443", "[::]:9443"}
	for _, addr := range rejected {
		t.Run("reject/"+addr, func(t *testing.T) {
			body := `{"listenAddr":"` + addr + `",` + base + `}`
			_, err := LoadConfig(writeConfig(t, body))
			if err == nil {
				t.Fatalf("non-loopback %q accepted, want abort", addr)
			}
			if !strings.Contains(err.Error(), addr) {
				t.Fatalf("error %q does not name the address %q", err, addr)
			}
		})
	}
}

func TestConfigAllowNonLoopbackOverridesGuard(t *testing.T) {
	body := `{"listenAddr":"0.0.0.0:9443","allowNonLoopback":true,` +
		`"certFile":"/tmp/console.pem","keyFile":"/tmp/console-key.pem",` +
		`"gatewayUrl":"https://x","caFile":"/a","clientCert":"/b","clientKey":"/c","operatorToken":"t"}`
	cfg, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("allowNonLoopback override with a listener certificate rejected: %v", err)
	}
	if !cfg.AllowNonLoopback {
		t.Fatal("AllowNonLoopback not loaded")
	}
	if !cfg.TLSEnabled() {
		t.Fatal("listener certificate pair not loaded")
	}
}

func TestConfigAllowNonLoopbackWithoutCertificateAborts(t *testing.T) {
	body := `{"listenAddr":"0.0.0.0:9443","allowNonLoopback":true,` +
		`"gatewayUrl":"https://x","caFile":"/a","clientCert":"/b","clientKey":"/c","operatorToken":"t"}`
	_, err := LoadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatal("allowNonLoopback without a listener certificate: want abort, got nil")
	}
	if !strings.Contains(err.Error(), "certFile") {
		t.Fatalf("abort %q does not name the missing certificate", err)
	}
}

func TestConfigCertificatePairMustBeComplete(t *testing.T) {
	for name, pair := range map[string]string{
		"certOnly": `"certFile":"/tmp/console.pem",`,
		"keyOnly":  `"keyFile":"/tmp/console-key.pem",`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"listenAddr":"127.0.0.1:9443",` + pair +
				`"gatewayUrl":"https://x","caFile":"/a","clientCert":"/b","clientKey":"/c","operatorToken":"t"}`
			if _, err := LoadConfig(writeConfig(t, body)); err == nil {
				t.Fatalf("half a listener certificate pair (%s): want abort, got nil", name)
			}
		})
	}
}

func TestConfigTLSTransportSelection(t *testing.T) {
	plain := &Config{}
	if plain.TLSEnabled() {
		t.Error("TLSEnabled() = true with no certificate pair")
	}
	if plain.TLSConfig() != nil {
		t.Error("TLSConfig() non-nil with no certificate pair")
	}

	tlsCfg := &Config{CertFile: "/t/c.pem", KeyFile: "/t/k.pem"}
	if !tlsCfg.TLSEnabled() {
		t.Error("TLSEnabled() = false with a full certificate pair")
	}
	tc := tlsCfg.TLSConfig()
	if tc == nil {
		t.Fatal("TLSConfig() nil with a full certificate pair")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLSConfig MinVersion = %x, want TLS 1.2", tc.MinVersion)
	}
}

func TestConfigPathUsesEnvOverride(t *testing.T) {
	t.Setenv(ConfigEnvVar, "/tmp/custom-admin.json")
	if got := ConfigPath(); got != "/tmp/custom-admin.json" {
		t.Fatalf("ConfigPath() = %q, want env override", got)
	}
	t.Setenv(ConfigEnvVar, "")
	if got := ConfigPath(); got != DefaultConfigFile {
		t.Fatalf("ConfigPath() = %q, want default %q", got, DefaultConfigFile)
	}
}

func TestConfigMissingFileAborts(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("missing config file: want error, got nil")
	}
}
