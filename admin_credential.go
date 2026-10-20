// admin_credential.go — the rotated bootstrap-admin credential store
// (accessstor appliance Contract G4 option a, POST /api/admin/password).
//
// Mechanism: the env identity's secret (ZETAOBJECT_SECRET_KEY, default
// zetaadmin) is the bootstrap admin credential. The process environment is
// fixed at start, so a rotation cannot go through the env var. Instead the
// new secret is persisted to a 0600 file NEXT TO the config file (on the
// appliance: the zas-config dataset, so the credential travels with the
// pool) and loadCredentials consults that file with precedence over the
// env value. Activation is the SAME path POST /auth/reload uses
// (reloadIdentityRegistry: loadConfig → loadCredentials → rebuild →
// atomic swap), so the new credential takes effect WITHOUT a restart and
// survives restarts because startup runs the identical loadCredentials.
//
// Security posture: the file is written atomically (writeFileAtomic:
// temp + rename, 0600), the secret is never logged, never appears in an
// error message, and the masked /config snapshot keeps showing "********"
// (maskedCopyLocked masks whatever serverCredentials holds).
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// adminCredentialFileName is the rotated-secret file stored beside the
// server config file (filepath.Dir(serverConfigPath)). It is a FILENAME,
// not a credential value.
const adminCredentialFileName = "admin-credential" // #nosec G104,G101 -- filename constant, no secret value

// adminCredentialMu serializes writers of the credential file (the admin
// route is the only writer today; the mutex keeps a future second writer
// from interleaving).
var adminCredentialMu sync.Mutex

// adminCredentialPath resolves the credential file location from the
// config path main() recorded. An empty serverConfigPath (a unit-test
// process that never loaded a config) has no config directory to anchor
// to; callers treat that as "not configured" and fail loud.
func adminCredentialPath() (string, error) {
	if strings.TrimSpace(serverConfigPath) == "" {
		return "", fmt.Errorf("no server config path recorded; cannot locate the admin credential file")
	}
	return filepath.Join(filepath.Dir(serverConfigPath), adminCredentialFileName), nil
}

// persistAdminCredential writes the new secret to the credential file
// atomically with 0600. The error never contains the secret.
func persistAdminCredential(newSecret string) error {
	if strings.TrimSpace(newSecret) == "" {
		return fmt.Errorf("refusing to persist an empty admin credential")
	}
	path, err := adminCredentialPath()
	if err != nil {
		return err
	}
	adminCredentialMu.Lock()
	defer adminCredentialMu.Unlock()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating config directory for the admin credential: %w", err)
		}
	}
	return writeFileAtomic(path, []byte(newSecret), 0o600)
}

// readAdminCredential returns the persisted rotated secret when the
// credential file exists and carries a non-empty value. A missing file is
// the normal pre-rotation state (not an error, not logged); any other read
// failure is warned about WITHOUT the file contents and falls back to the
// env/default resolution (fail-open to the previous mechanism, never to a
// fabricated credential).
func readAdminCredential() (string, bool) {
	path, err := adminCredentialPath()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		log.Printf("Warning: admin credential file %s is unreadable (%v); using the environment credential", path, err)
		return "", false
	}
	secret := strings.TrimRight(string(data), "\r\n")
	if strings.TrimSpace(secret) == "" {
		log.Printf("Warning: admin credential file %s is empty; using the environment credential", path)
		return "", false
	}
	return secret, true
}
