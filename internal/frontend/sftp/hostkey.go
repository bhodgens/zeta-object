// hostkey.go — SSH host-key load-or-generate (leaf 04 config contract):
// a missing hostKeyFile is generated (ed25519) at that path on first start
// and REUSED thereafter — never a random temp key per boot (clients pin
// host keys). The persisted fingerprint makes the identity observable.
package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
)

// loadOrGenerateHostKey returns the ssh.Signer for cfg's hostKeyFile,
// generating and persisting an ed25519 key (0600) when the file is absent.
// A path whose parent directory cannot be created is a loud startup error.
func loadOrGenerateHostKey(hostKeyPath string) (signer ssh.Signer, generated bool, fingerprint string, err error) {
	data, readErr := os.ReadFile(hostKeyPath)
	if readErr == nil {
		parsed, parseErr := ssh.ParsePrivateKey(data)
		if parseErr != nil {
			return nil, false, "", fmt.Errorf("sftp: parsing host key %s: %w", hostKeyPath, parseErr)
		}
		return parsed, false, fingerprintOf(parsed), nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return nil, false, "", fmt.Errorf("sftp: reading host key %s: %w", hostKeyPath, readErr)
	}
	// Generate + persist. The write is exclusive (O_EXCL, 0600) so two
	// first-time starts racing on the same path generate two keys but
	// persist exactly ONE file: the loser re-reads and uses the key that
	// won the race. O_EXCL also refuses to write through a symlink or
	// over an existing file of any kind — never clobber a host identity
	// clients may have pinned.
	if dir := filepath.Dir(hostKeyPath); dir != "" && dir != "." {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, false, "", fmt.Errorf("sftp: creating host-key directory %s: %w", dir, mkErr)
		}
	}
	_, priv, genErr := ed25519.GenerateKey(rand.Reader)
	if genErr != nil {
		return nil, false, "", fmt.Errorf("sftp: generating host key: %w", genErr)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, false, "", fmt.Errorf("sftp: marshaling host key: %w", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	pemBytes := pem.EncodeToMemory(block)
	wf, writeErr := os.OpenFile(hostKeyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if writeErr != nil {
		if !errors.Is(writeErr, os.ErrExist) {
			return nil, false, "", fmt.Errorf("sftp: writing host key %s: %w", hostKeyPath, writeErr)
		}
		// Lost the race: a file appeared (or a symlink dangled) at the
		// path between our ReadFile and here. Re-read and parse the file
		// that won — reuse its identity, never overwrite it. The winner
		// may have created the file but not yet flushed it, so the first
		// read can come back empty/unparseable: retry briefly before
		// failing (T6 — a transient startup race must not kill the
		// frontend).
		var parsed ssh.Signer
		var parseErr error
		for range 5 {
			again, rereadErr := os.ReadFile(hostKeyPath)
			if rereadErr == nil {
				parsed, parseErr = ssh.ParsePrivateKey(again)
				if parseErr == nil {
					return parsed, false, fingerprintOf(parsed), nil
				}
			} else {
				parseErr = rereadErr
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil, false, "", fmt.Errorf("sftp: re-reading host key %s after create race: %w", hostKeyPath, parseErr)
	}
	if _, writeErr = wf.Write(pemBytes); writeErr != nil {
		closeErr := wf.Close()
		if closeErr != nil {
			// Write already failed; the close error is secondary. Both
			// are reported so neither is silently dropped.
			return nil, false, "", fmt.Errorf("sftp: writing host key %s: %w (close: %w)", hostKeyPath, writeErr, closeErr)
		}
		return nil, false, "", fmt.Errorf("sftp: writing host key %s: %w", hostKeyPath, writeErr)
	}
	if closeErr := wf.Close(); closeErr != nil {
		return nil, false, "", fmt.Errorf("sftp: closing host key %s: %w", hostKeyPath, closeErr)
	}
	parsed, parseErr := ssh.ParsePrivateKey(pemBytes)
	if parseErr != nil {
		return nil, false, "", fmt.Errorf("sftp: parsing generated host key: %w", parseErr)
	}
	return parsed, true, fingerprintOf(parsed), nil
}

// fingerprintOf renders the SHA256 fingerprint (log-safe identity pin).
func fingerprintOf(signer ssh.Signer) string {
	return ssh.FingerprintSHA256(signer.PublicKey())
}

// base64Encode is the std-base64 helper for key blobs.
func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
