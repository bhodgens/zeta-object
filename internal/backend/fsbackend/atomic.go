// Package fsbackend — atomic.go: atomic write + sidecar serialization
// helpers, the in-package reproduction of package main's storage.go
// techniques (the leaf spec forbids importing package main).
package fsbackend

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// writeFileAtomic writes data to path atomically: temp file in the same
// directory, write, Sync, Close, Rename over path. Perm applied to both.
// Identical technique to package main's storage.go (temp+fsync+rename), so
// a crash or concurrent reader can never observe a torn file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return fmt.Errorf("generating temp file suffix for %s: %w", path, err)
	}
	tmpPath := path + ".tmp-" + hex.EncodeToString(randBytes[:])

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec // G703: callers pass paths derived from validated object keys (validateKey).
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil { //nolint:gosec // G703: validated caller path.
		f.Close()
		os.Remove(tmpPath) //nolint:gosec // G703: tmpPath built from validated caller path.
		return fmt.Errorf("writing temp file %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil { //nolint:gosec // G703: validated caller path.
		f.Close()
		os.Remove(tmpPath) //nolint:gosec // G703: validated caller path.
		return fmt.Errorf("syncing temp file %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil { //nolint:gosec // G703: validated caller path.
		os.Remove(tmpPath) //nolint:gosec // G703: validated caller path.
		return fmt.Errorf("closing temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil { //nolint:gosec // G703: validated caller path.
		os.Remove(tmpPath) //nolint:gosec // G703: validated caller path.
		return fmt.Errorf("renaming %s over %s: %w", tmpPath, path, err)
	}
	return nil
}

// writeFileAtomicJSON marshals v with MarshalIndent("", "  ") (the exact
// pre-seam sidecar serialization) then writeFileAtomic.
func writeFileAtomicJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling JSON for %s: %w", path, err)
	}
	return writeFileAtomic(path, data, perm)
}
