package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// zfsCmdTimeout bounds every zfs invocation. Probes run at startup once per
// bucket; history reads are per-request — both must never hang the server.
const zfsCmdTimeout = 5 * time.Second

// ZFSBinaryMissingError means the zfs CLI is not on PATH.
type ZFSBinaryMissingError struct{ Err error }

func (e *ZFSBinaryMissingError) Error() string {
	return "metadata: zfs binary not found on PATH"
}
func (e *ZFSBinaryMissingError) Unwrap() error { return e.Err }

// ResolveDataset returns the ZFS dataset name mounting path, or an error.
// It shells out to `zfs get -H -o value name,mountpoint <path>` — pure Go
// (os/exec is stdlib), no cgo, preserving the static binary. path must be
// symlink-resolved (filepath.EvalSymlinks) by the caller.
func ResolveDataset(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", errors.New("metadata: empty bucket path")
	}
	ctx, cancel := context.WithTimeout(ctx, zfsCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "zfs", "get", "-H", "-o", "value", "name,mountpoint", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if _, lookErr := exec.LookPath("zfs"); lookErr != nil {
			return "", &ZFSBinaryMissingError{Err: lookErr}
		}
		return "", fmt.Errorf("metadata: zfs get name,mountpoint %s: %w (stderr: %s)",
			path, err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		return "", fmt.Errorf("metadata: unexpected zfs get output for %s: %q", path, stdout.String())
	}
	name, mountpoint := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	// The dataset's mountpoint must actually be the bucket path (or an
	// ancestor anchor); zfs get on a path inside the dataset resolves it.
	if mountpoint == "" || name == "" || name == "-" {
		return "", fmt.Errorf("metadata: path %s is not in a ZFS dataset", path)
	}
	return name, nil
}
