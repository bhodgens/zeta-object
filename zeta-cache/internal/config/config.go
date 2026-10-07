package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Auth holds one of the two supported credential shapes. Validate enforces
// that exactly one shape is configured: Basic (accessKey+secretKey) for TCP,
// or mTLS (clientCert+clientKey file paths) for HTTP/3.
type Auth struct {
	AccessKey  string `json:"accessKey,omitempty"`
	SecretKey  string `json:"secretKey,omitempty"`
	ClientCert string `json:"clientCert,omitempty"`
	ClientKey  string `json:"clientKey,omitempty"`
}

// Quota fields are parsed by leaf 01; enforcement is leaf 07
// (quota-eviction-scheduler). HighWaterPercent / LowWaterPercent default
// to the locked 90 / 70 (leaf: "high-water ~90% starts eviction, low-water
// ~70% is the eviction target"). TombstoneRetentionDays defaults to the
// locked 30-day deletion grace.
type Quota struct {
	MaxCacheBytes   int64  `json:"maxCacheBytes,omitempty"`
	MinFreeDevBytes int64  `json:"minFreeDeviceBytes,omitempty"`
	Policy          string `json:"policy,omitempty"`
	HighWaterPct    int    `json:"highWaterPercent,omitempty"`
	LowWaterPct     int    `json:"lowWaterPercent,omitempty"`
	TombstoneDays   int    `json:"tombstoneRetentionDays,omitempty"`
}

// Config is the zeta-cache daemon configuration. Every key is known; unknown
// keys abort startup (Load uses DisallowUnknownFields, mirroring the
// gateway's ServerConfig validator).
type Config struct {
	ServerURL string `json:"serverUrl"`
	Bucket    string `json:"bucket"`
	Auth      Auth   `json:"auth"`
	// CAFile is the PEM bundle the gateway's server certificate is
	// verified against (self-signed private gateway: point it at the
	// gateway's cert). Empty = the system trust pool.
	CAFile string `json:"caFile,omitempty"`
	// InsecureSkipVerify disables server-certificate verification. There
	// is NO reason to set it outside throwaway loops: Load allows it but
	// callers MUST log a WARNING when it is set (main.go does; the
	// config-level default is false and stays undocumented in the
	// examples).
	InsecureSkipVerify bool     `json:"insecureSkipVerify,omitempty"`
	CacheDir           string   `json:"cacheDir,omitempty"`
	IndexDB            string   `json:"indexDB,omitempty"`
	Mountpoint         string   `json:"mountpoint"`
	IPCSocket          string   `json:"ipcSocket,omitempty"`
	Quota              Quota    `json:"quota"`
	LogLevel           string   `json:"logLevel,omitempty"`
	Pins               []string `json:"pins,omitempty"`
}

// defaultCacheDir returns the per-OS app-data path for the cache.
func defaultCacheDir() string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "zeta-cache")
	case "windows":
		// Windows is not a v1 target; a sane fallback keeps the default
		// well-defined if the module is ever built there.
		return filepath.Join(os.TempDir(), "zeta-cache")
	default:
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "zeta-cache")
		}
		return filepath.Join(os.TempDir(), "zeta-cache")
	}
}

// defaultRuntimeDir returns the per-OS user runtime dir for the IPC socket.
func defaultRuntimeDir() string {
	switch runtime.GOOS {
	case "darwin":
		return os.TempDir()
	default:
		if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
			return d
		}
		return os.TempDir()
	}
}

// Load reads and validates the config file at path. It is fail-loud:
//   - unreadable file -> error
//   - unknown JSON key -> error naming the key
//   - missing required value, both/neither auth shape, or an invalid
//     quota.policy -> error
//
// Defaults are applied for cacheDir, indexDB, ipcSocket, and logLevel.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.ServerURL == "" {
		return fmt.Errorf("config: serverUrl is required")
	}
	if c.Bucket == "" {
		return fmt.Errorf("config: bucket is required")
	}
	if c.Mountpoint == "" {
		return fmt.Errorf("config: mountpoint is required")
	}
	basic := c.Auth.AccessKey != "" || c.Auth.SecretKey != ""
	mtls := c.Auth.ClientCert != "" || c.Auth.ClientKey != ""
	switch {
	case basic && mtls:
		return fmt.Errorf("config: auth: exactly one credential shape is allowed (accessKey/secretKey OR clientCert/clientKey), both are set")
	case !basic && !mtls:
		return fmt.Errorf("config: auth: one credential shape is required (accessKey/secretKey OR clientCert/clientKey), none is set")
	case basic && (c.Auth.AccessKey == "" || c.Auth.SecretKey == ""):
		return fmt.Errorf("config: auth: both accessKey and secretKey are required for the Basic shape")
	case mtls && (c.Auth.ClientCert == "" || c.Auth.ClientKey == ""):
		return fmt.Errorf("config: auth: both clientCert and clientKey are required for the mTLS shape")
	}
	if c.CacheDir == "" {
		c.CacheDir = defaultCacheDir()
	}
	if c.IndexDB == "" {
		c.IndexDB = filepath.Join(c.CacheDir, "index.db")
	}
	if c.IPCSocket == "" {
		c.IPCSocket = filepath.Join(defaultRuntimeDir(), "zeta-cache.ipc")
	}
	switch c.Quota.Policy {
	case "":
		c.Quota.Policy = "lru" // locked default policy (leaf 07)
	case "lru", "size", "size+age":
	default:
		return fmt.Errorf("config: quota.policy %q is not one of size/size+age/lru", c.Quota.Policy)
	}
	if c.Quota.HighWaterPct == 0 {
		c.Quota.HighWaterPct = 90
	}
	if c.Quota.LowWaterPct == 0 {
		c.Quota.LowWaterPct = 70
	}
	if c.Quota.HighWaterPct <= c.Quota.LowWaterPct || c.Quota.HighWaterPct > 100 {
		return fmt.Errorf("config: quota: highWaterPercent (%d) must be above lowWaterPercent (%d) and at most 100",
			c.Quota.HighWaterPct, c.Quota.LowWaterPct)
	}
	if c.Quota.TombstoneDays == 0 {
		c.Quota.TombstoneDays = 30
	}
	if c.Quota.TombstoneDays < 0 {
		return fmt.Errorf("config: quota: tombstoneRetentionDays (%d) must be >= 0", c.Quota.TombstoneDays)
	}
	switch c.LogLevel {
	case "":
		c.LogLevel = "info"
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: logLevel %q is not one of debug/info/warn/error", c.LogLevel)
	}
	return nil
}
