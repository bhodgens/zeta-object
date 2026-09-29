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

// DatasetMountMismatchError means `zfs get` resolved a dataset whose
// mountpoint does not contain the bucket path. Returning the dataset for
// such a path would scope history/probe to the wrong dataset, so this is
// a hard typed error, not a degradation.
type DatasetMountMismatchError struct {
	Path       string
	Dataset    string
	Mountpoint string
}

func (e *DatasetMountMismatchError) Error() string {
	return fmt.Sprintf("metadata: dataset %s mountpoint %s does not contain path %s",
		e.Dataset, e.Mountpoint, e.Path)
}

// parseZfsGetNameMountpoint parses `zfs get -H -o value name,mountpoint <path>`
// stdout. With -H (no headers) and -o value, zfs emits exactly one line per
// requested property, in request order: line 1 = name, line 2 = mountpoint
// (zfs(8): "-H_scripted mode... no headers"; values are printed one per line
// when multiple properties are given via -o). Hardening: trailing empty or
// whitespace-only lines (some zfs builds pad output) are ignored; any
// interior blank line is skipped too — name and mountpoint can never be
// blank in practice (unmounted datasets report "-", "none", or "legacy").
func parseZfsGetNameMountpoint(stdout, path string) (name, mountpoint string, err error) {
	var vals []string
	for ln := range strings.SplitSeq(stdout, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			vals = append(vals, t)
		}
	}
	if len(vals) != 2 {
		return "", "", fmt.Errorf("metadata: unexpected zfs get output for %s: %q", path, stdout)
	}
	return vals[0], vals[1], nil
}

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
	name, mountpoint, err := parseZfsGetNameMountpoint(stdout.String(), path)
	if err != nil {
		return "", err
	}
	if mountpoint == "" || name == "" || name == "-" {
		return "", fmt.Errorf("metadata: path %s is not in a ZFS dataset", path)
	}
	// The dataset's mountpoint must actually be the bucket path (or an
	// ancestor of it): zfs get on a path inside the dataset resolves to
	// that dataset, but never trust that blindly — a mismatch here would
	// attribute history and probes to the wrong dataset.
	if !mountpointContains(mountpoint, path) {
		return "", &DatasetMountMismatchError{Path: path, Dataset: name, Mountpoint: mountpoint}
	}
	return name, nil
}

// mountpointContains reports whether the bucket path is the mountpoint or
// lies beneath it. Containment is filepath-compatible: mountpoint == path,
// or strings.HasPrefix(path, mountpoint+"/") — the separator guard stops
// /mnt/tank from claiming /mnt/tankdata.
func mountpointContains(mountpoint, path string) bool {
	return mountpoint == path || strings.HasPrefix(path, mountpoint+"/")
}
