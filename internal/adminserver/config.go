package adminserver

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
)

// config.go — fail-loud console configuration (admin-server Contract 1).
//
// The console is a separate, loopback-bound process. Its config is decoded
// with DisallowUnknownFields (matching the gateway's own config style) so a
// typo can never silently disable a setting, and every required key is
// validated at load: an empty operator token, gateway URL, or any of the
// three TLS file paths aborts startup naming the missing key. The listen
// address must resolve to a loopback address unless allowNonLoopback is set.

const (
	// ConfigEnvVar names the environment variable that overrides the console
	// config path, matching the gateway's ZETAOBJECT_CONFIG convention.
	ConfigEnvVar = "ZETAOBJECT_ADMIN_CONFIG"

	// DefaultConfigFile is the documented default config path, used when
	// ZETAOBJECT_ADMIN_CONFIG is unset.
	DefaultConfigFile = "admin-config.json"
)

// Config is the console's configuration (Contract 1). The JSON keys are
// pinned: listenAddr, gatewayUrl, caFile, clientCert, clientKey,
// operatorToken, allowNonLoopback.
type Config struct {
	ListenAddr string `json:"listenAddr"` // host:port the console binds
	GatewayURL string `json:"gatewayUrl"` // gateway management base URL
	CAFile     string `json:"caFile"`     // trusted CA for the gateway's mTLS
	ClientCert string `json:"clientCert"` // console client certificate
	ClientKey  string `json:"clientKey"`  // console client private key
	// OperatorToken is the operator's shared secret. An empty value is a
	// startup abort — never an unauthenticated console.
	OperatorToken string `json:"operatorToken"`
	// CertFile and KeyFile are the console LISTENER's own server certificate
	// pair (distinct from ClientCert/ClientKey, which are the console's
	// identity TO the gateway). They are OPTIONAL: both set means the console
	// serves HTTPS; neither set means plain HTTP, which is only allowed on a
	// loopback listener.
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// AllowNonLoopback permits a non-loopback listenAddr (default false). It
	// requires the listener certificate pair: the console never serves plain
	// HTTP off-host.
	AllowNonLoopback bool `json:"allowNonLoopback"`
}

// ConfigPath resolves the console config path: the ZETAOBJECT_ADMIN_CONFIG
// environment value when set, otherwise DefaultConfigFile.
func ConfigPath() string {
	if p := os.Getenv(ConfigEnvVar); p != "" {
		return p
	}
	return DefaultConfigFile
}

// LoadConfig reads, strictly decodes, and validates a console config file.
// Any read, decode, or validation failure is returned to the caller (the
// binary treats it as a startup abort).
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading admin config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing admin config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate enforces the required-key, listener-certificate, and loopback
// rules (Contract 1, including the console TLS/cookie policy).
func (c *Config) validate() error {
	required := []struct{ name, value string }{
		{"operatorToken", c.OperatorToken},
		{"gatewayUrl", c.GatewayURL},
		{"caFile", c.CAFile},
		{"clientCert", c.ClientCert},
		{"clientKey", c.ClientKey},
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("admin config: required key %q is empty", r.name)
		}
	}
	// certFile/keyFile are OPTIONAL, but they are a pair: one without the
	// other is a configuration bug, never a silent fallback.
	if (c.CertFile == "") != (c.KeyFile == "") {
		return fmt.Errorf("admin config: certFile and keyFile must be set together")
	}
	if !c.TLSEnabled() && c.AllowNonLoopback {
		return fmt.Errorf("admin config: allowNonLoopback requires certFile and keyFile; " +
			"the console refuses to serve plain HTTP off-host")
	}
	if c.AllowNonLoopback {
		return nil
	}
	return validateLoopbackAddr(c.ListenAddr)
}

// TLSEnabled reports whether the console serves HTTPS: both halves of its own
// listener certificate pair are set.
func (c *Config) TLSEnabled() bool {
	return c.CertFile != "" && c.KeyFile != ""
}

// TLSConfig returns the console listener's TLS configuration when TLS is
// enabled (TLS 1.2 minimum), otherwise nil (plain HTTP). It never reads the
// certificate material; the caller passes the file paths to ListenAndServeTLS.
func (c *Config) TLSConfig() *tls.Config {
	if !c.TLSEnabled() {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// validateLoopbackAddr rejects a listenAddr that does not resolve to a
// loopback address. Port 0 is allowed (tests); an empty host (":port") binds
// every interface and is therefore rejected.
func validateLoopbackAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin config: listenAddr %q is not a host:port address: %w", addr, err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("admin config: listenAddr %q has a non-numeric port", addr)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("admin config: listenAddr %q is not a loopback address; set allowNonLoopback to override", addr)
	}
	return nil
}

// isLoopbackHost reports whether host (the host portion of a listenAddr) is a
// loopback address: "localhost", an IPv4 127.0.0.0/8 literal, or "::1".
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
