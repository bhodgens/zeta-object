package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// storage.go — atomic write helpers and per-object write serialization.
//
// All object-data and metadata JSON writes go through writeFileAtomic /
// writeFileAtomicJSON instead of os.WriteFile, so a crash or concurrent
// reader can never observe a torn (partially written) file.

// writeFileAtomic writes data to path atomically: temp file in the same
// directory, write, Sync, Close, Rename over path. Perm applied to both.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return fmt.Errorf("generating temp file suffix for %s: %w", path, err)
	}
	tmpPath := path + ".tmp-" + hex.EncodeToString(randBytes[:])

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp file %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming %s over %s: %w", tmpPath, path, err)
	}
	return nil
}

// writeFileAtomicJSON marshals v with MarshalIndent("", "  ") then writeFileAtomic.
func writeFileAtomicJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling JSON for %s: %w", path, err)
	}
	return writeFileAtomic(path, data, perm)
}

// objectLocks serializes writers per object path.
// NOTE: multipart upload metadata files use getMultipartLock instead
// (multipart_handlers.go — they already do).
var objectLocks sync.Map // map[string]*sync.Mutex — keyed by object path

// lockObject serializes writers per object path. Returns the unlock func.
// Uses sync.Map of *sync.Mutex keyed by path.
func lockObject(path string) func() {
	mu, _ := objectLocks.LoadOrStore(path, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return func() { mu.(*sync.Mutex).Unlock() }
}
