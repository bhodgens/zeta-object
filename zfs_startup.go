package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/metadata"
)

// zfsStartupCmdTimeout bounds the startup-time `zfs list` exec (zfs
// bucket datasets leaf 01). Generous by design: `zfs list` on a mounted
// path is milliseconds; startup validation runs exactly once.
const zfsStartupCmdTimeout = 10 * time.Second

// detectZFSFn is the startup seam for metadata.DetectZFS (tests fake
// it: DetectZFS returns (false, nil) on non-Linux build targets, so
// the validation below is untestable on the dev host without this
// seam). Production never reassigns it.
var detectZFSFn = metadata.DetectZFS

// zfsDatasetForMountFn is the startup seam for zfsDatasetForMount
// (tests fake the exec). Production never reassigns it.
var zfsDatasetForMountFn = zfsDatasetForMount

// lookPathFn is the startup seam for exec.LookPath (tests fake it: dev
// hosts have no zfs binary on PATH). Production never reassigns it.
var lookPathFn = exec.LookPath

// zfsBucketsParentDataset holds the startup-resolved dataDir parent
// dataset ("" when the feature is off). Set once by
// validateZfsBucketDatasets from main(); leaf 03's wiring consumes it
// to install the dataset provisioner without re-resolving.
var zfsBucketsParentDataset string

// validateZfsBucketDatasets is the fail-loud startup check for
// zfs_bucket_datasets (zfs bucket datasets leaf 01, master Contract 1).
// Feature off: no-op returning "" — the default path is untouched.
// Feature on: the zfs binary must exist on PATH (LookPath — a missing
// binary aborts startup, never a lazy first-request 500), dataDir must
// pass the DetectZFS statfs probe, and `zfs list -H -o name -t
// filesystem <absDataDir>` must resolve a non-empty dataset name. Any
// failure returns an error naming the failed check and the path; main
// aborts startup on it. On success the resolved parent dataset is
// returned and stored in zfsBucketsParentDataset ("" when off).
// ZETAOBJECT_ASSUME_ZFS does NOT bypass any of this — that env gate is
// a zmetad-DB-probe-only hint.
func validateZfsBucketDatasets(ctx context.Context, cfg *ServerConfig) (string, error) {
	if !cfg.ZfsBucketDatasets {
		zfsBucketsParentDataset = ""
		return "", nil
	}

	// Fail-loud binary check: the configured zfs CLI must resolve on
	// PATH before the listener opens (no lazy first-request 500s).
	if _, err := lookPathFn(cfg.ZfsBinary); err != nil {
		return "", fmt.Errorf("zfs_bucket_datasets: zfs binary %q not found on PATH: %w", cfg.ZfsBinary, err)
	}

	absDataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return "", fmt.Errorf("zfs_bucket_datasets: cannot resolve dataDir %q: %w", cfg.DataDir, err)
	}

	isZFS, err := detectZFSFn(absDataDir)
	if err != nil {
		return "", fmt.Errorf("zfs_bucket_datasets: ZFS detection failed for %q: %w", absDataDir, err)
	}
	if !isZFS {
		return "", fmt.Errorf("zfs_bucket_datasets: %q is not on ZFS (dataDir must be a ZFS mountpoint when zfs_bucket_datasets is enabled)", absDataDir)
	}

	parent, err := zfsDatasetForMountFn(ctx, cfg.ZfsBinary, absDataDir)
	if err != nil {
		return "", fmt.Errorf("zfs_bucket_datasets: resolve dataset for %q: %w", absDataDir, err)
	}
	if parent == "" {
		return "", fmt.Errorf("zfs_bucket_datasets: %q is not a ZFS mountpoint (dataset name did not resolve)", absDataDir)
	}

	zfsBucketsParentDataset = parent
	return parent, nil
}

// validateZfsNativeTags is the fail-loud startup check for zfs_native_tags
// (the ZFS-native object-tag store, zfs-metadata#13 / DB layout 9).
// Feature off: no-op. Feature on: zfs_bucket_datasets must be enabled and
// its validation must have resolved a parent dataset (the tag store only
// ever targets <parent>/<bucket> datasets), the zmetad binary must
// resolve on PATH (the tag CLI is the store's ONLY write/read path —
// a missing binary would make every ?tagging request a lazy 500), and the
// zmetad database path must be non-empty (it is config-defaulted, so an
// empty value is a wiring bug, not a user error). Any failure returns an
// error naming the failed check; main aborts startup on it.
func validateZfsNativeTags(cfg *ServerConfig) error {
	if !cfg.ZfsNativeTags {
		return nil
	}
	if !cfg.ZfsBucketDatasets {
		return fmt.Errorf("zfs_native_tags: zfs_bucket_datasets must be enabled (the tag store targets bucket datasets)")
	}
	if zfsBucketsParentDataset == "" {
		return fmt.Errorf("zfs_native_tags: no parent dataset resolved (zfs_bucket_datasets validation did not run or failed)")
	}
	if _, err := lookPathFn(cfg.ZmetadBinary); err != nil {
		return fmt.Errorf("zfs_native_tags: zmetad binary %q not found on PATH: %w", cfg.ZmetadBinary, err)
	}
	if cfg.ZmetadDBPath == "" {
		return fmt.Errorf("zfs_native_tags: zmetad_db_path is empty")
	}
	return nil
}

// zfsDatasetForMount resolves the ZFS dataset name backing path by
// execing the zfs CLI (argv-only, never a shell string — binary is
// config-controlled, path is abs(dataDir)): `zfs list -H -o name -t
// filesystem <path>`, bounded by zfsStartupCmdTimeout. stderr is
// captured into errors. Empty stdout with exit 0 means the path is not
// a ZFS mountpoint and IS an error — never a silent empty dataset name.
func zfsDatasetForMount(ctx context.Context, binary, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, zfsStartupCmdTimeout)
	defer cancel()
	args := []string{"list", "-H", "-o", "name", "-t", "filesystem", path}
	cmd := exec.CommandContext(ctx, binary, args...) // #nosec G204 // argv-only; binary is config-controlled, path is abs(dataDir)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			return "", fmt.Errorf("zfs list for %q timed out after %s: %w", path, zfsStartupCmdTimeout, err)
		}
		if stderrText != "" {
			return "", fmt.Errorf("zfs list for %q failed: %w: %s", path, err, stderrText)
		}
		return "", fmt.Errorf("zfs list for %q failed: %w", path, err)
	}
	name := strings.TrimSpace(stdout.String())
	if name == "" {
		return "", fmt.Errorf("zfs list for %q returned no dataset: not a ZFS mountpoint", path)
	}
	if idx := strings.IndexAny(name, "\r\n"); idx >= 0 {
		name = strings.TrimSpace(name[:idx])
	}
	return name, nil
}
