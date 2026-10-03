// zfsdatasets.go — ZFS bucket-dataset ops seam (zfs-bucket-datasets
// tree leaf 02, Contract 2). Provisioning a bucket as its own ZFS
// dataset: `zfs create <parent>/<bucket>`, a destroy that REFUSES when
// snapshots exist (sentinel error, count in the message, never `-r`),
// and a deterministic exists probe (`zfs list` on the DATASET NAME,
// never on a path — `zfs list <plaindir>` would return the PARENT
// dataset, and reading that as the bucket's dataset is the
// destroy-the-wrong-dataset hazard this seam exists to prevent).
//
// Exec discipline copies zfssnapshots.go with its OWN runner var and
// its OWN semaphore: package-level zfsDatasetRunner (tests replace),
// argv-only exec.CommandContext (never a shell string), 10s timeout,
// stdout+stderr captured, dedicated bounded semaphore so this seam's
// concurrent-process bound is independent of the snapshots store's.
// The feature is wired ON by InstallZfsDatasetProvisioner, which
// installs the three hook vars leaf 03's handlers read; nil hooks mean
// the legacy plain-dir path (feature off).
package s3

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrDatasetHasSnapshots is returned by zfsDatasetDestroy when the
// dataset's snapshot list is non-empty: destroy is refused (leaf 03
// maps this to 409 BucketHasSnapshots). Snapshots are host policy —
// NEVER destroy -r, NEVER auto-remove snapshots.
var ErrDatasetHasSnapshots = errors.New("s3: dataset has snapshots")

// zfsDatasetCmdTimeout bounds every zfs exec from this seam. Dataset
// ops run inside bucket create/delete requests, so a hung zfs must
// fail the request, not pin the handler.
const zfsDatasetCmdTimeout = 10 * time.Second

// zfsDatasetExecSem bounds concurrent zfs processes from this seam
// (same discipline as zfsSnapExecSem, an INDEPENDENT bound): every
// bucket create/delete can exec zfs, so an authenticated client
// fan-out must not spawn an unbounded number of concurrent processes.
var zfsDatasetExecSem = make(chan struct{}, 4)

// zfsDatasetRunner is the seam tests replace. Production execs the
// configured zfs binary: argv-only (never a shell string — bucket
// names are handler-validated and re-checked here, dataset names are
// derived, never resolved from a path), context-bounded, stdout and
// stderr captured (stderr rides in wrapped errors; zfs's wording is
// never matched programmatically).
var zfsDatasetRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, binary, args...) // #nosec G204 // binary is config-controlled; args are argv-only; bucket/dataset names are handler-validated and re-checked here
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// runZfsDataset applies the zfsDatasetCmdTimeout bound, then bounds
// the exec through zfsDatasetExecSem, honoring ctx: a request that
// already timed out or was canceled never queues behind the bound.
func runZfsDataset(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, zfsDatasetCmdTimeout)
	defer cancel()
	select {
	case zfsDatasetExecSem <- struct{}{}:
		defer func() { <-zfsDatasetExecSem }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	return zfsDatasetRunner(ctx, binary, args...)
}

// isDatasetNameUnsafe validates a DATASET name argument: non-empty, no
// '@' (a snapshot arg is a programming error in these ops), no leading
// '-' (an argv flag injection). Dataset names are derived from the
// configured parent + a validated bucket name, never from user paths.
func isDatasetNameUnsafe(name string) bool {
	return name == "" || strings.Contains(name, "@") || strings.HasPrefix(name, "-")
}

// isBucketNameUnsafe is the create-side validation (defense in depth —
// the handler already ran validateBucketName): non-empty, no '/',
// no '@', no leading '.'.
func isBucketNameUnsafe(name string) bool {
	return name == "" || strings.ContainsAny(name, "/@") || strings.HasPrefix(name, ".")
}

// zfsDatasetCreate execs `<binary> create <parent>/<bucket>` and
// returns the created dataset name "<parent>/<bucket>". The bucket
// name is re-validated BEFORE any exec (zero runner calls on
// rejection); a runner failure wraps the error with zfs's stderr.
func zfsDatasetCreate(ctx context.Context, binary, parent, bucket string) (string, error) {
	if isDatasetNameUnsafe(parent) || isBucketNameUnsafe(bucket) {
		return "", fmt.Errorf("s3: invalid dataset create arguments (parent=%q bucket=%q)", parent, bucket)
	}
	dataset := parent + "/" + bucket
	// `zfs create` prints nothing on success: drop the stdout return
	// positionally (blank identifier, no ignored-error semantics).
	_, stderr, err := runZfsDataset(ctx, binary, "create", dataset)
	if err != nil {
		return "", fmt.Errorf("s3: zfs create %s: %w (stderr: %s)", dataset, err, stderr)
	}
	return dataset, nil
}

// zfsDatasetDestroy destroys the dataset WITHOUT snapshots: it first
// execs `list -H -o name -t snapshot -d 1 <dataset>`; any non-blank
// row means snapshots exist and destroy is refused with an error
// wrapping ErrDatasetHasSnapshots (the count rides in the message for
// the 409 body) and NO destroy exec. The destroy argv NEVER contains
// "-r" (or -R): snapshots are host policy, never auto-removed.
func zfsDatasetDestroy(ctx context.Context, binary, dataset string) error {
	if isDatasetNameUnsafe(dataset) {
		return fmt.Errorf("s3: invalid dataset destroy argument (dataset=%q)", dataset)
	}
	stdout, stderr, err := runZfsDataset(ctx, binary, "list", "-H", "-o", "name", "-t", "snapshot", "-d", "1", dataset)
	if err != nil {
		return fmt.Errorf("s3: zfs list snapshots %s: %w (stderr: %s)", dataset, err, stderr)
	}
	n := countNonBlankLines(stdout)
	if n > 0 {
		return fmt.Errorf("s3: destroy %s refused: %d snapshot(s): %w", dataset, n, ErrDatasetHasSnapshots)
	}
	if _, stderr, err = runZfsDataset(ctx, binary, "destroy", dataset); err != nil {
		return fmt.Errorf("s3: zfs destroy %s: %w (stderr: %s)", dataset, err, stderr)
	}
	return nil
}

// countNonBlankLines counts stdout rows that carry content (some zfs
// builds pad output; blank lines are not snapshot rows).
func countNonBlankLines(stdout []byte) int {
	n := 0
	for line := range strings.SplitSeq(string(stdout), "\n") {
		if strings.TrimSpace(strings.TrimRight(line, "\r")) != "" {
			n++
		}
	}
	return n
}

// zfsDatasetExists probes for the dataset BY NAME:
// `list -H -o name <dataset>`. Exit 0 with non-empty stdout is true.
// ANY zfs non-zero exit is (false, nil) — NOT an error, and there is
// NO stderr string matching: the name is deterministic
// (<parent>/<bucket>), so a failed list simply means the bucket dir is
// a pre-feature plain dir. Only runner/timeout failures (the exec
// never completed) return an error: a canceled or timed-out probe must
// fail loud, not read as "plain dir".
func zfsDatasetExists(ctx context.Context, binary, dataset string) (bool, error) {
	if isDatasetNameUnsafe(dataset) {
		return false, fmt.Errorf("s3: invalid dataset exists argument (dataset=%q)", dataset)
	}
	stdout, _, err := runZfsDataset(ctx, binary, "list", "-H", "-o", "name", dataset)
	if err == nil {
		return strings.TrimSpace(string(stdout)) != "", nil
	}
	// zfs itself ran and rejected the name: not our dataset (no stderr
	// string matching — zfs's wording is not a contract).
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return false, nil
	}
	// Runner/timeout/cancellation failure: error, never a false "absent".
	return false, fmt.Errorf("s3: zfs list %s: %w", dataset, err)
}

// Hook vars leaf 03's bucket handlers read. nil = feature off (legacy
// plain-dir mkdir / RemoveAll path). Declared HERE so leaf 03's diff
// stays handler-only; leaf 03 may read them but must not move them.
var (
	zfsBucketCreate        func(ctx context.Context, bucket string) (string, error)
	zfsBucketDestroy       func(ctx context.Context, dataset string) error
	zfsBucketDatasetExists func(ctx context.Context, dataset string) (bool, error)
)

// InstallZfsDatasetProvisioner wires the feature ON: validates the
// arguments (parentDataset must be a clean dataset name, zfsBinary
// non-empty) and installs the three hook vars above. It does NOT exec
// (startup already validated ZFS). An error leaves the hooks nil.
func InstallZfsDatasetProvisioner(parentDataset, zfsBinary string) error {
	if isDatasetNameUnsafe(parentDataset) {
		return fmt.Errorf("s3: invalid parent dataset %q", parentDataset)
	}
	if zfsBinary == "" {
		return errors.New("s3: empty zfs binary")
	}
	zfsBucketCreate = func(ctx context.Context, bucket string) (string, error) {
		return zfsDatasetCreate(ctx, zfsBinary, parentDataset, bucket)
	}
	zfsBucketDestroy = func(ctx context.Context, dataset string) error {
		return zfsDatasetDestroy(ctx, zfsBinary, dataset)
	}
	zfsBucketDatasetExists = func(ctx context.Context, dataset string) (bool, error) {
		return zfsDatasetExists(ctx, zfsBinary, dataset)
	}
	return nil
}

// UninstallZfsDatasetProvisioner restores the nil hooks (tests, and
// startup rollback on a later init failure).
func UninstallZfsDatasetProvisioner() {
	zfsBucketCreate = nil
	zfsBucketDestroy = nil
	zfsBucketDatasetExists = nil
}
