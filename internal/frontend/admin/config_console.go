// config_console.go — the zeta_console_oidc config block (accessstor
// appliance tree leaf 03). It lives in its own file, OUTSIDE config.go and
// config_store.go, because leaf 02 edits those files in parallel; the
// orchestrator merges this block into the canonical parse/apply paths.
//
// Keys (all RESTART-REQUIRED, consistent with zeta_federation leaf 01 —
// the registry and its discovery fetch are built once at startup):
//
//	zeta_console_oidc.enabled             bool, default false
//	zeta_console_oidc.session_ttl         Go duration string, default 12h
//	zeta_console_oidc.client_secret_file  path to the OIDC client secret.
//	                                      A PATH REFERENCE, never an inline
//	                                      secret: config.json must not
//	                                      carry credential material.
package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// consoleOIDCBlock is the JSON shape of the zeta_console_oidc object.
type consoleOIDCBlock struct {
	Enabled          bool   `json:"enabled"`
	SessionTTL       string `json:"session_ttl,omitempty"`
	ClientSecretFile string `json:"client_secret_file,omitempty"`
	// ClientID mirrors zeta_federation.clientID for the console flow (the
	// FederationRegistry config is unexported, so wiring re-states it here).
	ClientID string `json:"client_id,omitempty"`
}

// parseConsoleOIDCBlock validates the raw JSON block and resolves it to a
// ConsoleOIDCConfig. Unknown fields are rejected fail-loud (the posture
// used across the config surface). An absent block (nil) is disabled.
func parseConsoleOIDCBlock(raw json.RawMessage) (ConsoleOIDCConfig, error) {
	if len(raw) == 0 {
		return ConsoleOIDCConfig{}, nil
	}
	var b consoleOIDCBlock
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return ConsoleOIDCConfig{}, fmt.Errorf("invalid zeta_console_oidc: %w", err)
	}
	cfg := ConsoleOIDCConfig{
		Enabled:          b.Enabled,
		ClientSecretFile: strings.TrimSpace(b.ClientSecretFile),
		ClientID:         strings.TrimSpace(b.ClientID),
	}
	if b.SessionTTL != "" {
		ttl, err := time.ParseDuration(b.SessionTTL)
		if err != nil {
			return ConsoleOIDCConfig{}, fmt.Errorf("invalid zeta_console_oidc.session_ttl %q: %w", b.SessionTTL, err)
		}
		if ttl <= 0 {
			return ConsoleOIDCConfig{}, fmt.Errorf("invalid zeta_console_oidc.session_ttl %q: must be positive", b.SessionTTL)
		}
		cfg.SessionTTL = ttl
	}
	return cfg, nil
}

// resolveClientSecret reads the client secret file. The path is REQUIRED
// when console OIDC is enabled; the file must exist, be non-empty, and
// carry no surrounding whitespace (a trailing newline from an editor is
// tolerated and trimmed).
func resolveClientSecret(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("admin: zeta_console_oidc.client_secret_file is required when console OIDC is enabled")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("admin: reading zeta_console_oidc.client_secret_file %q: %w", path, err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", fmt.Errorf("admin: zeta_console_oidc.client_secret_file %q is empty", path)
	}
	return secret, nil
}
