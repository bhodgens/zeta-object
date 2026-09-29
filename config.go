package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

// config.go — server configuration, credentials, and shared constants

const (
	defaultDataDir    = "./data/"
	defaultConfigFile = "config.json"
	defaultListenAddr = ":8443"
	defaultCertFile   = "certs/cert.pem"
	defaultKeyFile    = "certs/key.pem"
	defaultAccessKey  = "minioadmin"
)

// ServerConfig holds the server configuration loaded from JSON
type ServerConfig struct {
	DataDir    string            `json:"dataDir"`    // Root directory for bucket storage
	Buckets    map[string]string `json:"buckets"`    // Custom bucket name -> path mappings
	ListenAddr string            `json:"listenAddr"` // Host:port to listen on (default ":8443")
	CertFile   string            `json:"certFile"`   // TLS certificate path (default certs/cert.pem)
	KeyFile    string            `json:"keyFile"`    // TLS private key path (default certs/key.pem)

	// Frontends selects which protocol frontends run and where they
	// listen (leaf 03). Absent/empty ⇒ [{"type":"s3"}] on the default
	// listener — exact backward compatibility.
	Frontends []FrontendConfig `json:"frontends,omitempty"`

	// Backends maps a backend type name to its construction config
	// (leaf 03; frozen JSON keys). Absent ⇒ every bucket uses the
	// default backend ("fs").
	Backends map[string]BackendCfg `json:"backends"`
	// BucketBackends records each bucket's selected backend name (the
	// object form of the buckets value). Absent/empty ⇒ default backend.
	BucketBackends map[string]string `json:"-"`
}

// FrontendConfig is one entry of the "frontends" config array (leaf 03).
// Type names a registered frontend factory ("s3" today; "webdav", "ftp"
// later). ListenAddr empty = share the default listener's mux; set it to
// give this frontend its own dedicated TLS listener.
type FrontendConfig struct {
	Type       string `json:"type"`
	ListenAddr string `json:"listenAddr,omitempty"`
}

// BackendCfg is the per-backend-type config from config.json "backends".
// Root is the storage root; Options carries backend-specific string
// settings (fs ignores them in v1 — they exist for future backends).
type BackendCfg struct {
	Root    string            `json:"root"`
	Options map[string]string `json:"options"`
}

// bucketCfg is the JSON decoding form of a buckets map value: either the
// legacy bare string ("photos": "/mnt/photos") or the object form
// ("photos": {"path": ..., "backend": ...}).
type bucketCfg struct {
	Path    string `json:"path"`
	Backend string `json:"backend"`
}

// UnmarshalJSON accepts both encodings. The legacy string form decodes to
// Path with an empty Backend (default backend at resolve time).
func (b *bucketCfg) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(data, &b.Path)
	}
	type plain bucketCfg
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	b.Path, b.Backend = p.Path, p.Backend
	return nil
}

// bucketsRaw is the raw buckets map shape used only for JSON decoding.
type bucketsRaw map[string]bucketCfg

// UnmarshalJSON decodes either encoding of each value and fans the result
// out into the legacy Buckets (name → path) and BucketBackends
// (name → backend) fields so every existing caller of serverConfig.Buckets
// keeps its exact behavior.
func (m bucketsRaw) apply(cfg *ServerConfig) {
	if cfg.Buckets == nil {
		cfg.Buckets = make(map[string]string, len(m))
	}
	if len(m) == 0 {
		return
	}
	if cfg.BucketBackends == nil {
		cfg.BucketBackends = make(map[string]string)
	}
	for name, bc := range m {
		cfg.Buckets[name] = bc.Path
		if bc.Backend != "" {
			cfg.BucketBackends[name] = bc.Backend
		}
	}
}

var serverConfig = ServerConfig{
	DataDir:    defaultDataDir,
	Buckets:    make(map[string]string),
	ListenAddr: defaultListenAddr,
	CertFile:   defaultCertFile,
	KeyFile:    defaultKeyFile,
}

// defaultServerConfig returns a fully-populated ServerConfig with all
// defaults applied. loadConfig always starts from a fresh copy so a failed
// parse can never leave the global partially mutated.
func defaultServerConfig() ServerConfig {
	return ServerConfig{
		DataDir:    defaultDataDir,
		Buckets:    make(map[string]string),
		ListenAddr: defaultListenAddr,
		CertFile:   defaultCertFile,
		KeyFile:    defaultKeyFile,
	}
}

// loadConfig loads server configuration from a JSON file into a FRESH local
// struct; the global is only replaced once the file parsed and normalized
// successfully. A missing file keeps the defaults; any other read or parse
// error is returned to the caller (main treats it as fatal).
func loadConfig(configPath string) error {
	cfg := defaultServerConfig()

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		log.Printf("Config file %s not found, using defaults", configPath)
		serverConfig = cfg
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reading config file %s: %w", configPath, err)
	}

	if err := json.Unmarshal(stripJSON5Comments(data), &cfg); err != nil {
		return fmt.Errorf("error parsing config file %s: %w", configPath, err)
	}

	// Normalize: defaults for empty values, trailing slash on DataDir
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir
	} else if !strings.HasSuffix(cfg.DataDir, "/") {
		cfg.DataDir += "/"
	}
	if cfg.Buckets == nil {
		cfg.Buckets = make(map[string]string)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.CertFile == "" {
		cfg.CertFile = defaultCertFile
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = defaultKeyFile
	}
	// Absent/empty frontends array == S3 on the default listener (leaf 03
	// backward-compatibility rule).
	if len(cfg.Frontends) == 0 {
		cfg.Frontends = []FrontendConfig{{Type: "s3"}}
	}

	serverConfig = cfg
	log.Printf("Loaded config: DataDir=%s, ListenAddr=%s, CertFile=%s, KeyFile=%s, CustomBuckets=%d",
		serverConfig.DataDir, serverConfig.ListenAddr, serverConfig.CertFile,
		serverConfig.KeyFile, len(serverConfig.Buckets))
	return nil
}

// UnmarshalJSON decodes a ServerConfig, accepting BOTH encodings of the
// "buckets" values (legacy bare string and the object form with an
// optional "backend" key). The object form's backend selections land in
// BucketBackends; the path form stays in the legacy Buckets field so every
// existing caller keeps its exact behavior.
func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	type alias struct {
		DataDir    string                `json:"dataDir"`
		ListenAddr string                `json:"listenAddr"`
		CertFile   string                `json:"certFile"`
		KeyFile    string                `json:"keyFile"`
		Frontends  []FrontendConfig      `json:"frontends"`
		Backends   map[string]BackendCfg `json:"backends"`
		Buckets    bucketsRaw            `json:"buckets"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	c.DataDir = a.DataDir
	c.ListenAddr = a.ListenAddr
	c.CertFile = a.CertFile
	c.KeyFile = a.KeyFile
	c.Frontends = a.Frontends
	c.Backends = a.Backends
	a.Buckets.apply(c)
	return nil
}

// Credentials store. Package-level var remains the storage, but the values
// are populated explicitly by loadCredentials() (called from main) so tests
// can vary the environment. init()-time env reads are gone.
var serverCredentials = struct {
	AccessKeyID     string
	SecretAccessKey string
}{
	AccessKeyID:     defaultAccessKey,
	SecretAccessKey: defaultAccessKey,
}

// loadCredentials reads MINIS3_ACCESS_KEY / MINIS3_SECRET_KEY. An env var
// that is SET but EMPTY is warned about and falls back to the default
// (minioadmin) instead of silently behaving like an unset variable.
func loadCredentials() {
	serverCredentials.AccessKeyID = credentialFromEnv("MINIS3_ACCESS_KEY")
	serverCredentials.SecretAccessKey = credentialFromEnv("MINIS3_SECRET_KEY")
}

// credentialFromEnv returns the env value, the default when unset, and warns
// + defaults when the variable is set but empty.
func credentialFromEnv(key string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		return defaultAccessKey
	}
	if value == "" {
		log.Printf("Warning: environment variable %s is set but empty; using default %q", key, defaultAccessKey)
		return defaultAccessKey
	}
	return value
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
