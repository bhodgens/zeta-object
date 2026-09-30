package metadata

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// TestParseZfsGetNameMountpoint pins the exact `zfs get -H -o value
// name,mountpoint <path>` output shape. zfs(8) semantics: -H is scripted
// mode (no header line, tab-separated values), and with -o value plus two
// properties the CLI prints one value per line, in property-request order —
// so a healthy invocation yields exactly:
//
//	tank/data\n/mnt/tank/data\n
//
// Tolerance pinned here: trailing empty and whitespace-only lines are
// ignored (real hosts have been observed to pad scripted output); interior
// blank lines are skipped too, since name and mountpoint are never blank —
// unmounted datasets report "-", "none", or "legacy" instead.
func TestParseZfsGetNameMountpoint(t *testing.T) {
	tests := []struct {
		name           string
		in             string
		wantName       string
		wantMountpoint string
		wantErr        bool
	}{
		{
			name:           "canonical two lines",
			in:             "tank/data\n/mnt/tank/data\n",
			wantName:       "tank/data",
			wantMountpoint: "/mnt/tank/data",
		},
		{
			name:           "no trailing newline",
			in:             "tank/data\n/mnt/tank/data",
			wantName:       "tank/data",
			wantMountpoint: "/mnt/tank/data",
		},
		{
			name:           "trailing empty lines tolerated",
			in:             "tank/data\n/mnt/tank/data\n\n\n",
			wantName:       "tank/data",
			wantMountpoint: "/mnt/tank/data",
		},
		{
			name:           "trailing whitespace-only lines tolerated",
			in:             "tank/data\n/mnt/tank/data\n   \n	\n",
			wantName:       "tank/data",
			wantMountpoint: "/mnt/tank/data",
		},
		{
			name:           "interior whitespace-only line tolerated",
			in:             "tank/data\n   \n/mnt/tank/data\n",
			wantName:       "tank/data",
			wantMountpoint: "/mnt/tank/data",
		},
		{
			name:           "surrounding whitespace on values trimmed",
			in:             "  tank/data \n /mnt/tank/data	\n",
			wantName:       "tank/data",
			wantMountpoint: "/mnt/tank/data",
		},
		{
			name:    "single line is an error",
			in:      "tank/data\n",
			wantErr: true,
		},
		{
			name:    "three value lines is an error",
			in:      "tank/data\n/mnt/tank/data\nextra\n",
			wantErr: true,
		},
		{
			name:    "empty output is an error",
			in:      "\n\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, mp, err := parseZfsGetNameMountpoint(tt.in, "/bucket")
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseZfsGetNameMountpoint err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if name != tt.wantName || mp != tt.wantMountpoint {
				t.Fatalf("got (%q, %q), want (%q, %q)", name, mp, tt.wantName, tt.wantMountpoint)
			}
		})
	}
}

func TestMountpointContains(t *testing.T) {
	tests := []struct {
		mountpoint, path string
		want             bool
	}{
		{"/mnt/tank/data", "/mnt/tank/data", true},
		{"/mnt/tank", "/mnt/tank/data", true},
		{"/mnt/tank", "/mnt/tank/data/deep/k", true},
		{"/mnt/tank", "/mnt/tankdata", false},  // prefix but not path-prefix
		{"/mnt/tank", "/mnt/tank/datax", true}, // datax is a genuine child
		{"/mnt/tank/data", "/mnt/tank", false},
		{"/mnt/tank", "/opt/other", false},
		{"", "", true}, // degenerate: equal empty strings
	}
	for _, tc := range tests {
		if got := mountpointContains(tc.mountpoint, tc.path); got != tc.want {
			t.Errorf("mountpointContains(%q, %q) = %v, want %v", tc.mountpoint, tc.path, got, tc.want)
		}
	}
}

// TestResolveDatasetMountMismatch drives ResolveDataset's containment gate
// through the only seam it has (the zfs binary is not stubbable here), using
// a fake zfs script on PATH whose output mimics a dataset whose mountpoint
// does not contain the queried path.
func TestResolveDatasetMountMismatch(t *testing.T) {
	if _, err := exec.LookPath("zfs"); err != nil {
		// No real zfs: substitute a fake for the duration of the test.
		dir := t.TempDir()
		script := "#!/bin/sh\necho 'tank/other'\necho '/mnt/tank/other'\n"
		if err := writeFakeZfs(t, dir, script); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		_, err := ResolveDataset(context.Background(), "/mnt/tank/data/bucket-a")
		var mismatch *DatasetMountMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("ResolveDataset err = %v, want DatasetMountMismatchError", err)
		}
		if mismatch.Dataset != "tank/other" || mismatch.Mountpoint != "/mnt/tank/other" ||
			mismatch.Path != "/mnt/tank/data/bucket-a" {
			t.Fatalf("mismatch fields wrong: %+v", mismatch)
		}
		return
	}
	// Real zfs present: can't fake output; containment on a live host is
	// exercised by zfs_integration_test.go.
	t.Skip("zfs binary present; fake-binary mismatch test not applicable")
}

// writeFakeZfs writes an executable zfs shell-script stub and marks it
// executable for the current user.
func writeFakeZfs(t *testing.T, dir, script string) error {
	t.Helper()
	p := filepath.Join(dir, "zfs")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		return err
	}
	return nil
}

// TestTypedErrorStrings pins the Error()/Unwrap() of the metadata package's
// typed errors (0%-covered on CI where the zfs binary is absent; these
// constructors are pure, so direct construction covers them without zfs).
func TestTypedErrorStrings(t *testing.T) {
	statErr := &PathStatError{Path: "/buckets/a", Err: os.ErrPermission}
	if got := statErr.Error(); got == "" {
		t.Error("PathStatError.Error() returned empty string")
	}
	if !errors.Is(statErr, os.ErrPermission) {
		t.Error("PathStatError.Unwrap lost the wrapped error")
	}

	binErr := &ZFSBinaryMissingError{Err: exec.ErrNotFound}
	if got := binErr.Error(); got == "" {
		t.Error("ZFSBinaryMissingError.Error() returned empty string")
	}
	if !errors.Is(binErr, exec.ErrNotFound) {
		t.Error("ZFSBinaryMissingError.Unwrap lost the wrapped error")
	}

	mmErr := &DatasetMountMismatchError{Path: "/p", Dataset: "tank/ds", Mountpoint: "/mp"}
	want := "metadata: dataset tank/ds mountpoint /mp does not contain path /p"
	if got := mmErr.Error(); got != want {
		t.Errorf("DatasetMountMismatchError.Error() = %q, want %q", got, want)
	}
}
