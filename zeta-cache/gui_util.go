package main

// gui_util.go - leaf 08's small helpers: the cache-file removal shared by
// the evict path (scheduler.evictOne's contract, mirrored here because
// the scheduler keeps its quota engine unexported).

import (
	"fmt"
	"os"
	"path/filepath"
)

// removeCacheFile deletes <cacheDir>/files/<key> (absent = fine).
func removeCacheFile(cacheDir, key string) error {
	p := filepath.Join(cacheDir, "files", filepath.FromSlash(key))
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("evict %s: unlink: %w", key, err)
	}
	return nil
}
