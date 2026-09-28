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
	awsAlgorithm      = "AWS4-HMAC-SHA256"
	defaultRegion     = "us-east-1" // Default region for our S3 server
	serviceName       = "s3"
	unsignedPayload   = "UNSIGNED-PAYLOAD"
	streamingPayload  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	iso8601Format     = "20060102T150405Z"
	shortDateFormat   = "20060102"
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

	serverConfig = cfg
	log.Printf("Loaded config: DataDir=%s, ListenAddr=%s, CertFile=%s, KeyFile=%s, CustomBuckets=%d",
		serverConfig.DataDir, serverConfig.ListenAddr, serverConfig.CertFile,
		serverConfig.KeyFile, len(serverConfig.Buckets))
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
