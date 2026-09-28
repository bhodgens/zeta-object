package metadata

import (
	"path/filepath"
	"testing"
)

func TestDetectZFSOnNonZFSPath(t *testing.T) {
	// tmpfs/apfs/ext4/whatever the test host uses: never ZFS.
	isZFS, err := DetectZFS(t.TempDir())
	if err != nil {
		t.Fatalf("DetectZFS: %v", err)
	}
	if isZFS {
		t.Fatal("DetectZFS reported ZFS for temp dir")
	}
}

func TestDetectZFSMissingPath(t *testing.T) {
	if _, err := DetectZFS(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error for missing path")
	}
}
