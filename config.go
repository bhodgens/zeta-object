package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
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
)

// ServerConfig holds the server configuration loaded from JSON
type ServerConfig struct {
	DataDir string            `json:"dataDir"` // Root directory for bucket storage
	Buckets map[string]string `json:"buckets"` // Custom bucket name -> path mappings
}

var serverConfig = ServerConfig{
	DataDir: defaultDataDir,
	Buckets: make(map[string]string),
}

// loadConfig loads server configuration from a JSON file
func loadConfig(configPath string) error {
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		log.Printf("Config file %s not found, using defaults", configPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reading config file: %w", err)
	}

	if err := json.Unmarshal(data, &serverConfig); err != nil {
		return fmt.Errorf("error parsing config file: %w", err)
	}

	// Ensure DataDir has trailing slash
	if serverConfig.DataDir != "" && !strings.HasSuffix(serverConfig.DataDir, "/") {
		serverConfig.DataDir += "/"
	}
	if serverConfig.DataDir == "" {
		serverConfig.DataDir = defaultDataDir
	}
	if serverConfig.Buckets == nil {
		serverConfig.Buckets = make(map[string]string)
	}

	log.Printf("Loaded config: DataDir=%s, CustomBuckets=%d", serverConfig.DataDir, len(serverConfig.Buckets))
	return nil
}

// Credentials store (simple hardcoded version)
// TODO: Load from environment variables or config file
var serverCredentials = struct {
	AccessKeyID     string
	SecretAccessKey string
}{
	AccessKeyID:     getEnvOrDefault("MINIS3_ACCESS_KEY", "minioadmin"),
	SecretAccessKey: getEnvOrDefault("MINIS3_SECRET_KEY", "minioadmin"),
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// Regex for parsing the AWS V4 Authorization header
var authHeaderRegex = regexp.MustCompile(
	`^AWS4-HMAC-SHA256 Credential=([^/]+)/([^/]+)/([^/]+)/s3/aws4_request, SignedHeaders=([^,]+), Signature=(.+)$`,
)
