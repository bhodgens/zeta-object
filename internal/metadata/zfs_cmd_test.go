package metadata

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestResolveDatasetNoZFSBinary(t *testing.T) {
	if _, err := exec.LookPath("zfs"); err == nil {
		t.Skip("zfs binary present; negative-path test not applicable")
	}
	_, err := ResolveDataset(context.Background(), t.TempDir())
	var missing *ZFSBinaryMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("ResolveDataset err = %v, want ZFSBinaryMissingError", err)
	}
}

func TestResolveDatasetEmptyPath(t *testing.T) {
	_, err := ResolveDataset(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}
