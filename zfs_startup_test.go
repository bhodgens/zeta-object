package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// fakeLookPath is the always-succeeds LookPath stub installed by
// restoreZfsStartupFakes (dev hosts have no zfs on PATH).
func fakeLookPath(string) (string, error) { return "/usr/sbin/zfs", nil }

// restoreZfsStartupFakes puts the package-level startup seams back to
// production after a test swapped them.
func restoreZfsStartupFakes(t *testing.T) {
	t.Helper()
	origDetect := detectZFSFn
	origResolve := zfsDatasetForMountFn
	origLook := lookPathFn
	t.Cleanup(func() {
		detectZFSFn = origDetect
		zfsDatasetForMountFn = origResolve
		lookPathFn = origLook
	})
	// Dev hosts (macOS) have no zfs binary on PATH; the LookPath branch
	// is exercised separately in TestValidateZfsBucketDatasetsLookPath.
	lookPathFn = fakeLookPath
}

// TestValidateZfsBucketDatasets pins the zfs-bucket-datasets leaf 01
// startup contract: feature off = no-op; feature on = dataDir must pass
// DetectZFS AND resolve a dataset via `zfs list -H -o name -t
// filesystem`, and the zfs binary must exist on PATH. Every failure
// names the failed check; the resolved parent dataset comes back for
// leaf 03's wiring.
func TestValidateZfsBucketDatasets(t *testing.T) {
	restoreZfsStartupFakes(t)

	cases := []struct {
		name             string
		featureOn        bool
		detectFn         func(string) (bool, error)
		detectErr        error
		detectResult     bool
		resolveFn        func(ctx context.Context, binary, path string) (string, error)
		wantParent       string
		wantErrSubstr    string
		wantDetectCalls  int
		wantResolveCalls int
	}{
		{
			name:             "feature off is a no-op, fakes never called",
			featureOn:        false,
			wantParent:       "",
			wantDetectCalls:  0,
			wantResolveCalls: 0,
		},
		{
			name:      "feature on + DetectZFS false -> error naming the path",
			featureOn: true,
			detectFn:  func(string) (bool, error) { return false, nil },
			resolveFn: func(context.Context, string, string) (string, error) {
				t.Fatal("zfsDatasetForMountFn called after DetectZFS said not-ZFS")
				return "", nil
			},
			wantErrSubstr:    "is not on ZFS",
			wantDetectCalls:  1,
			wantResolveCalls: 0,
		},
		{
			name:             "feature on + DetectZFS error -> error naming the path",
			featureOn:        true,
			detectFn:         func(string) (bool, error) { return false, errors.New("statfs failed") },
			wantErrSubstr:    "/data/dir",
			wantDetectCalls:  1,
			wantResolveCalls: 0,
		},
		{
			name:      "feature on + resolver error -> wrapped error",
			featureOn: true,
			detectFn:  func(string) (bool, error) { return true, nil },
			resolveFn: func(context.Context, string, string) (string, error) {
				return "", errors.New("exit status 1: cannot open")
			},
			wantErrSubstr:    "resolve dataset",
			wantDetectCalls:  1,
			wantResolveCalls: 1,
		},
		{
			name:      "feature on + empty dataset name -> not a ZFS mountpoint",
			featureOn: true,
			detectFn:  func(string) (bool, error) { return true, nil },
			resolveFn: func(context.Context, string, string) (string, error) {
				return "", nil
			},
			wantErrSubstr:    "not a ZFS mountpoint",
			wantDetectCalls:  1,
			wantResolveCalls: 1,
		},
		{
			name:      "feature on + both checks OK -> nil, parent returned",
			featureOn: true,
			detectFn:  func(string) (bool, error) { return true, nil },
			resolveFn: func(_ context.Context, binary, path string) (string, error) {
				if binary != "zfs" {
					t.Fatalf("resolve binary = %q, want zfs", binary)
				}
				if path != "/data/dir" {
					t.Fatalf("resolve path = %q, want /data/dir", path)
				}
				return "pool/data/dir", nil
			},
			wantParent:       "pool/data/dir",
			wantDetectCalls:  1,
			wantResolveCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detectCalls, resolveCalls := 0, 0
			detectZFSFn = func(string) (bool, error) {
				detectCalls++
				return tc.detectResult, tc.detectErr
			}
			if tc.detectFn != nil {
				// Wrap so the call-count var stays authoritative even
				// when the case supplies its own body.
				body := tc.detectFn
				detectZFSFn = func(p string) (bool, error) {
					detectCalls++
					return body(p)
				}
			}
			zfsDatasetForMountFn = func(context.Context, string, string) (string, error) {
				resolveCalls++
				if tc.resolveFn != nil {
					return tc.resolveFn(context.Background(), "zfs", "/data/dir")
				}
				return "", nil
			}
			if tc.resolveFn != nil {
				// Wrap so the call-count var stays authoritative even
				// when the case supplies its own body.
				body := tc.resolveFn
				zfsDatasetForMountFn = func(ctx context.Context, binary, path string) (string, error) {
					resolveCalls++
					return body(ctx, binary, path)
				}
			}

			cfg := &ServerConfig{
				DataDir:           "/data/dir",
				ZfsBucketDatasets: tc.featureOn,
				ZfsBinary:         "zfs",
			}
			parent, err := validateZfsBucketDatasets(context.Background(), cfg)

			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("validateZfsBucketDatasets = nil error, want one containing %q", tc.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErrSubstr)
				}
			} else if err != nil {
				t.Fatalf("validateZfsBucketDatasets: unexpected error %v", err)
			}
			if parent != tc.wantParent {
				t.Fatalf("parent = %q, want %q", parent, tc.wantParent)
			}
			if detectCalls != tc.wantDetectCalls {
				t.Fatalf("detectZFSFn calls = %d, want %d", detectCalls, tc.wantDetectCalls)
			}
			if resolveCalls != tc.wantResolveCalls {
				t.Fatalf("zfsDatasetForMountFn calls = %d, want %d", resolveCalls, tc.wantResolveCalls)
			}
		})
	}
}

// TestValidateZfsBucketDatasetsLookPath pins the fail-loud binary check:
// when the feature is on, a missing zfs binary aborts startup (no lazy
// first-request 500s) — even when both dataset checks would pass.
func TestValidateZfsBucketDatasetsLookPath(t *testing.T) {
	restoreZfsStartupFakes(t)

	cases := []struct {
		name       string
		binary     string
		wantErrSub string
	}{
		{
			name:   "existing binary on PATH passes",
			binary: "go", // any binary guaranteed present for tests
		},
		{
			name:       "missing binary aborts",
			binary:     "zeta-object-no-such-zfs-binary-9x7",
			wantErrSub: "zeta-object-no-such-zfs-binary-9x7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The real LookPath is the thing under test here — undo the
			// always-succeed fake the restore helper installed.
			lookPathFn = exec.LookPath
			t.Cleanup(func() { lookPathFn = fakeLookPath })
			detectZFSFn = func(string) (bool, error) { return true, nil }
			zfsDatasetForMountFn = func(context.Context, string, string) (string, error) {
				return "pool/data/dir", nil
			}
			cfg := &ServerConfig{
				DataDir:           "/data/dir",
				ZfsBucketDatasets: true,
				ZfsBinary:         tc.binary,
			}
			_, err := validateZfsBucketDatasets(context.Background(), cfg)
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wantErrSub)
			}
		})
	}
}

// TestValidateZfsBucketDatasetsSetsParentVar pins Task 3: the resolved
// parent dataset is stored in the package var leaf 03 consumes; the
// feature-off path clears it to "".
func TestValidateZfsBucketDatasetsSetsParentVar(t *testing.T) {
	restoreZfsStartupFakes(t)
	origParent := zfsBucketsParentDataset
	t.Cleanup(func() { zfsBucketsParentDataset = origParent })

	detectZFSFn = func(string) (bool, error) { return true, nil }
	zfsDatasetForMountFn = func(context.Context, string, string) (string, error) {
		return "pool/data/dir", nil
	}

	// Feature off clears the var without touching the fakes.
	zfsBucketsParentDataset = "stale"
	if _, err := validateZfsBucketDatasets(context.Background(), &ServerConfig{DataDir: "/data/dir"}); err != nil {
		t.Fatalf("feature-off validation failed: %v", err)
	}
	if zfsBucketsParentDataset != "" {
		t.Fatalf("parent var = %q after feature-off validation, want empty", zfsBucketsParentDataset)
	}

	// Feature on stores the resolved parent.
	cfg := &ServerConfig{DataDir: "/data/dir", ZfsBucketDatasets: true, ZfsBinary: "zfs"}
	if _, err := validateZfsBucketDatasets(context.Background(), cfg); err != nil {
		t.Fatalf("feature-on validation failed: %v", err)
	}
	if zfsBucketsParentDataset != "pool/data/dir" {
		t.Fatalf("parent var = %q, want pool/data/dir", zfsBucketsParentDataset)
	}
}

// TestZfsDatasetForMountEmptyStdoutIsError pins the real resolver's
// output handling: a zfs exit 0 with EMPTY stdout means the path is not
// a ZFS mountpoint, and that is an ERROR, never a silent empty dataset
// name (master Contract 1).
func TestZfsDatasetForMountEmptyStdoutIsError(t *testing.T) {
	// Drive the real function against a binary that exits 0 with empty
	// stdout: `true` via a PATH-shim is overkill — an empty-dir lookup
	// does it. Instead exec /usr/bin/true with the argv shape of zfs
	// (it ignores args and prints nothing).
	parent, err := zfsDatasetForMount(context.Background(), "true", "/data/dir")
	if err == nil {
		t.Fatalf("zfsDatasetForMount(true) = %q, nil; want empty-stdout error", parent)
	}
	if !strings.Contains(err.Error(), "not a ZFS mountpoint") {
		t.Fatalf("error = %v, want not-a-ZFS-mountpoint", err)
	}
	if parent != "" {
		t.Fatalf("parent = %q on error, want empty", parent)
	}

	// A missing binary is an error carrying the exec failure.
	if _, err := zfsDatasetForMount(context.Background(), "zeta-object-no-such-zfs-binary-9x7", "/data/dir"); err == nil {
		t.Fatal("missing binary: zfsDatasetForMount = nil error, want exec error")
	}
}
