// config_sts.go — the zeta_sts config block (accessstor appliance tree
// leaf 02). The signing key is a PATH REFERENCE, never inline secret
// material: config.json must not carry credential content (the leaf-01/03
// precedent). The file loads through the EXISTING reload path — the
// SIGHUP / POST /auth/reload sequence re-reads it and swaps the process
// signing-key slot, so a rotation invalidates outstanding ZSTSID-
// credentials fail-closed without a restart.
//
// Keys (all RESTART-REQUIRED at the config store, consistent with
// zeta_federation leaf 01 — the frontend and its listener are built once
// at startup; EXCEPT the signing-key file, which reloads):
//
//	zeta_sts.enabled           bool, default false
//	zeta_sts.ttl               Go duration string, default 3600s
//	zeta_sts.max_ttl           Go duration string, default 43200s
//	zeta_sts.signing_key_file  PATH to the HMAC server key (PEM or raw)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// parseSTSConfigBlock validates the raw JSON block and resolves it to an
// STSConfig (through the struct's own decoder, so the store path and the
// startup path share ONE grammar). An absent block (nil) is disabled.
func parseSTSConfigBlock(raw json.RawMessage) (STSConfig, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return STSConfig{}, nil
	}
	var cfg STSConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return STSConfig{}, fmt.Errorf("invalid zeta_sts: %w", err)
	}
	if cfg.TTLRaw != "" {
		d, err := time.ParseDuration(cfg.TTLRaw)
		if err != nil {
			return STSConfig{}, fmt.Errorf("invalid zeta_sts.ttl %q: %w", cfg.TTLRaw, err)
		}
		if d <= 0 {
			return STSConfig{}, fmt.Errorf("invalid zeta_sts.ttl %q: must be positive", cfg.TTLRaw)
		}
	}
	if cfg.MaxTTLRaw != "" {
		d, err := time.ParseDuration(cfg.MaxTTLRaw)
		if err != nil {
			return STSConfig{}, fmt.Errorf("invalid zeta_sts.max_ttl %q: %w", cfg.MaxTTLRaw, err)
		}
		if d <= 0 {
			return STSConfig{}, fmt.Errorf("invalid zeta_sts.max_ttl %q: must be positive", cfg.MaxTTLRaw)
		}
	}
	if cfg.TTL > 0 && cfg.MaxTTL > 0 && cfg.TTL > cfg.MaxTTL {
		return STSConfig{}, fmt.Errorf("invalid zeta_sts: ttl %s exceeds max_ttl %s", cfg.TTL, cfg.MaxTTL)
	}
	cfg.SigningKeyFile = strings.TrimSpace(cfg.SigningKeyFile)
	return cfg, nil
}

// resolveSigningKey reads the HMAC server key file (PEM or raw bytes —
// both work: the derivation HMACs the raw file bytes). The path is
// REQUIRED when STS is enabled; the file must exist and be non-empty.
func resolveSigningKey(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("main: zeta_sts.signing_key_file is required when sts is enabled")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("main: reading zeta_sts.signing_key_file %q: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("main: zeta_sts.signing_key_file %q is empty", path)
	}
	return raw, nil
}
