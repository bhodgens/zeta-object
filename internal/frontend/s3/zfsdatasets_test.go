// zfsdatasets_test.go — dataset ops seam tests (zfs-bucket-datasets
// tree leaf 02). NO real zfs: the zfsDatasetRunner seam is scripted per
// test with a recording fake. Covers: argv passthrough, the ~10s
// timeout the fake observes, the dedicated semaphore bound (max
// in-flight == 4 with a blocking fake), create/destroy/exists argvs,
// pre-exec name validation (zero runner calls on rejection), the
// snapshots refusal sentinel (errors.Is + count in message, NO destroy
// exec), never "-r" in any destroy argv, exists mapping any zfs
// non-zero exit to (false, nil) with no stderr matching, and the
// Install/Uninstall hook lifecycle.

package s3

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptZfsDatasetRunner replaces the zfsDatasetRunner seam with a
// scripted fake that records every argv (the [binary, args...] vector)
// and returns the configured stdout/stderr/error. Restores the seam on
// cleanup.
func scriptZfsDatasetRunner(t *testing.T, stdout, stderr string, err error) *[][]string {
	t.Helper()
	got := &[][]string{}
	orig := zfsDatasetRunner
	zfsDatasetRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
		*got = append(*got, append([]string{binary}, args...))
		return []byte(stdout), stderr, err
	}
	t.Cleanup(func() { zfsDatasetRunner = orig })
	return got
}

// assertNoRecursiveFlag is the package-level argv guard shared by the
// destroy tests: NO recursive flag ("-r"/"-R") may ever appear in any
// recorded zfs argv — snapshots are host policy, never auto-removed.
func assertNoRecursiveFlag(t *testing.T, argvs *[][]string) {
	t.Helper()
	for _, argv := range *argvs {
		for _, a := range argv {
			if a == "-r" || a == "-R" {
				t.Fatalf("recursive flag %q in argvs: %v", a, *argvs)
			}
		}
	}
}

// fakeZfsExitError builds an error that looks like a real zfs non-zero
// exit (an *exec.ExitError from a process that ran and failed), the
// shape the production runner returns when zfs itself rejects.
func fakeZfsExitError(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("false")
	_ = cmd.Run()
	if cmd.ProcessState == nil {
		t.Skip("cannot build an ExitError on this host")
	}
	return &exec.ExitError{ProcessState: cmd.ProcessState}
}

// TestZfsDatasetRunnerPassthrough pins the seam shape: runZfsDataset
// passes binary + argv through untouched and returns the runner's
// outputs verbatim.
func TestZfsDatasetRunnerPassthrough(t *testing.T) {
	got := scriptZfsDatasetRunner(t, "out", "err", nil)
	stdout, stderr, err := runZfsDataset(context.Background(), "zfs", "list", "-H", "pool/data")
	if err != nil {
		t.Fatalf("runZfsDataset: %v", err)
	}
	if string(stdout) != "out" || stderr != "err" {
		t.Fatalf("passthrough broken: stdout=%q stderr=%q", stdout, stderr)
	}
	if len(*got) != 1 || strings.Join((*got)[0], " ") != "zfs list -H pool/data" {
		t.Fatalf("argv not passed through: %v", *got)
	}
}

// TestZfsDatasetTimeout asserts the fake observes a ctx deadline of
// ~10s (zfsDatasetCmdTimeout).
func TestZfsDatasetTimeout(t *testing.T) {
	orig := zfsDatasetRunner
	zfsDatasetRunner = func(ctx context.Context, _ string, _ ...string) ([]byte, string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, "", errors.New("no deadline applied")
		}
		d := time.Until(deadline)
		if d < 9*time.Second || d > 11*time.Second {
			return nil, "", fmt.Errorf("deadline %v not ~10s", d)
		}
		return nil, "", nil
	}
	t.Cleanup(func() { zfsDatasetRunner = orig })
	if _, _, err := runZfsDataset(context.Background(), "zfs", "list", "pool/data"); err != nil {
		t.Fatalf("timeout discipline broken: %v", err)
	}
}

// TestZfsDatasetSemaphoreBound fires 8 concurrent runZfsDataset calls
// with a blocking fake and asserts max in-flight == 4 (the dedicated
// bound, not shared with zfssnapshots.go).
func TestZfsDatasetSemaphoreBound(t *testing.T) {
	const total = 8
	const wantMax = 4

	block := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0

	orig := zfsDatasetRunner
	zfsDatasetRunner = func(_ context.Context, _ string, _ ...string) ([]byte, string, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil, "", nil
	}
	t.Cleanup(func() { zfsDatasetRunner = orig })

	var wg sync.WaitGroup
	started := make(chan struct{}, total)
	for range total {
		wg.Go(func() {
			started <- struct{}{}
			_, _, _ = runZfsDataset(context.Background(), "zfs", "list", "pool/data")
		})
	}
	// Wait until wantMax calls hold the semaphore and the rest are queued.
	for range wantMax {
		<-started
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := inFlight
		mu.Unlock()
		if n == wantMax {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	if inFlight != wantMax {
		mu.Unlock()
		close(block)
		t.Fatalf("in-flight %d, want %d", inFlight, wantMax)
	}
	mu.Unlock()
	close(release)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != wantMax {
		t.Fatalf("max in-flight %d, want exactly %d", maxInFlight, wantMax)
	}
}

// TestZfsDatasetCreate covers Task 2's create table: argv, return
// value, runner-error wrapping, and PRE-exec rejection of invalid
// bucket names (zero runner calls).
func TestZfsDatasetCreate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got := scriptZfsDatasetRunner(t, "", "", nil)
		ds, err := zfsDatasetCreate(context.Background(), "zfs", "pool/data", "bkt")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if ds != "pool/data/bkt" {
			t.Fatalf("returned dataset %q, want pool/data/bkt", ds)
		}
		if len(*got) != 1 || strings.Join((*got)[0], " ") != "zfs create pool/data/bkt" {
			t.Fatalf("create argv: %v", *got)
		}
	})

	t.Run("invalid names rejected pre-exec", func(t *testing.T) {
		for _, name := range []string{"", "a/b", "a@b", ".hidden"} {
			got := scriptZfsDatasetRunner(t, "", "", nil)
			_, err := zfsDatasetCreate(context.Background(), "zfs", "pool/data", name)
			if err == nil {
				t.Fatalf("bucket %q accepted", name)
			}
			if len(*got) != 0 {
				t.Fatalf("bucket %q reached the runner: %v", name, *got)
			}
		}
	})

	t.Run("runner error wraps stderr", func(t *testing.T) {
		scriptZfsDatasetRunner(t, "", "cannot create 'pool/data/bkt': dataset exists", errors.New("exit status 1"))
		_, err := zfsDatasetCreate(context.Background(), "zfs", "pool/data", "bkt")
		if err == nil {
			t.Fatal("create succeeded on runner error")
		}
		if !strings.Contains(err.Error(), "dataset exists") {
			t.Fatalf("stderr not wrapped in error: %v", err)
		}
	})
}

// TestZfsDatasetDestroy covers Task 2's destroy table: snapshots
// refusal (errors.Is + count in the message, NO destroy exec), the
// no-snapshot destroy argv, pre-exec dataset validation, list-runner
// failure, and NEVER "-r" in any argv across ALL destroy tests.
func TestZfsDatasetDestroy(t *testing.T) {
	assertNoRecursive := func(t *testing.T, argvs *[][]string) {
		t.Helper()
		for _, argv := range *argvs {
			for _, a := range argv {
				if a == "-r" || a == "-R" {
					t.Fatalf("recursive flag %q in destroy argvs: %v", a, *argvs)
				}
			}
		}
	}

	t.Run("snapshots exist -> refusal, no destroy exec", func(t *testing.T) {
		got := scriptZfsDatasetRunner(t, "pool/data/bkt@snap1\npool/data/bkt@snap2\n", "", nil)
		err := zfsDatasetDestroy(context.Background(), "zfs", "pool/data/bkt")
		if err == nil {
			t.Fatal("destroy succeeded with snapshots present")
		}
		if !errors.Is(err, ErrDatasetHasSnapshots) {
			t.Fatalf("errors.Is(ErrDatasetHasSnapshots) false: %v", err)
		}
		if !strings.Contains(err.Error(), "2") {
			t.Fatalf("snapshot count not in message: %v", err)
		}
		assertNoRecursive(t, got)
		// Only the snapshot-list exec may have run.
		if len(*got) != 1 || strings.Join((*got)[0], " ") != "zfs list -H -o name -t snapshot -d 1 pool/data/bkt" {
			t.Fatalf("argvs: %v", *got)
		}
	})

	t.Run("no snapshots -> destroy exec", func(t *testing.T) {
		got := scriptZfsDatasetRunner(t, "", "", nil)
		if err := zfsDatasetDestroy(context.Background(), "zfs", "pool/data/bkt"); err != nil {
			t.Fatalf("destroy: %v", err)
		}
		assertNoRecursive(t, got)
		want := [][]string{
			{"zfs", "list", "-H", "-o", "name", "-t", "snapshot", "-d", "1", "pool/data/bkt"},
			{"zfs", "destroy", "pool/data/bkt"},
		}
		if len(*got) != len(want) {
			t.Fatalf("argvs: %v", *got)
		}
		for i, argv := range want {
			if strings.Join((*got)[i], " ") != strings.Join(argv, " ") {
				t.Fatalf("argv[%d]: got %v, want %v", i, (*got)[i], argv)
			}
		}
	})

	t.Run("invalid dataset names rejected pre-exec", func(t *testing.T) {
		for _, name := range []string{"", "a@b", "-ds"} {
			got := scriptZfsDatasetRunner(t, "", "", nil)
			err := zfsDatasetDestroy(context.Background(), "zfs", name)
			if err == nil {
				t.Fatalf("dataset %q accepted", name)
			}
			if len(*got) != 0 {
				t.Fatalf("dataset %q reached the runner: %v", name, *got)
			}
		}
	})

	t.Run("list runner failure is an error, not the snapshots path", func(t *testing.T) {
		got := scriptZfsDatasetRunner(t, "", "boom", errors.New("exit status 1"))
		err := zfsDatasetDestroy(context.Background(), "zfs", "pool/data/bkt")
		if err == nil {
			t.Fatal("destroy succeeded on list failure")
		}
		if errors.Is(err, ErrDatasetHasSnapshots) {
			t.Fatalf("list failure misread as snapshots refusal: %v", err)
		}
		assertNoRecursive(t, got)
		if len(*got) != 1 {
			t.Fatalf("destroy ran after list failure: %v", *got)
		}
	})
}

// TestZfsDatasetExists covers Task 3's exists table: present dataset
// true; ANY zfs non-zero exit (false, nil) — no stderr string matching;
// runner/timeout failures error.
func TestZfsDatasetExists(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		got := scriptZfsDatasetRunner(t, "pool/data/bkt\n", "", nil)
		ok, err := zfsDatasetExists(context.Background(), "zfs", "pool/data/bkt")
		if err != nil || !ok {
			t.Fatalf("exists: got (%v, %v)", ok, err)
		}
		if len(*got) != 1 || strings.Join((*got)[0], " ") != "zfs list -H -o name pool/data/bkt" {
			t.Fatalf("exists argv: %v", *got)
		}
	})

	t.Run("zfs non-zero exit -> false nil", func(t *testing.T) {
		scriptZfsDatasetRunner(t, "", "cannot open 'pool/data/bkt': dataset does not exist", fakeZfsExitError(t))
		ok, err := zfsDatasetExists(context.Background(), "zfs", "pool/data/bkt")
		if err != nil || ok {
			t.Fatalf("zfs failure must map to (false, nil), got (%v, %v)", ok, err)
		}
	})

	t.Run("timeout failure errors", func(t *testing.T) {
		scriptZfsDatasetRunner(t, "", "", context.DeadlineExceeded)
		if ok, err := zfsDatasetExists(context.Background(), "zfs", "pool/data/bkt"); err == nil {
			t.Fatalf("timeout mapped to (%v, nil), want error", ok)
		}
	})

	t.Run("runner failure errors", func(t *testing.T) {
		scriptZfsDatasetRunner(t, "", "", errors.New("exec: no such file"))
		if ok, err := zfsDatasetExists(context.Background(), "zfs", "pool/data/bkt"); err == nil {
			t.Fatalf("runner failure mapped to (%v, nil), want error", ok)
		}
	})

	t.Run("invalid dataset name rejected pre-exec", func(t *testing.T) {
		for _, name := range []string{"", "a@b", "-ds"} {
			got := scriptZfsDatasetRunner(t, "", "", nil)
			if _, err := zfsDatasetExists(context.Background(), "zfs", name); err == nil {
				t.Fatalf("dataset %q accepted", name)
			}
			if len(*got) != 0 {
				t.Fatalf("dataset %q reached the runner: %v", name, *got)
			}
		}
	})
}

// TestInstallZfsDatasetProvisioner covers Task 3's hook lifecycle:
// invalid args error with hooks staying nil; valid install wires all
// three hooks through the ops layer; Uninstall restores nil.
func TestInstallZfsDatasetProvisioner(t *testing.T) {
	t.Cleanup(UninstallZfsDatasetProvisioner)

	t.Run("invalid args", func(t *testing.T) {
		if err := InstallZfsDatasetProvisioner("", "zfs"); err == nil {
			t.Fatal("empty parentDataset accepted")
		}
		if err := InstallZfsDatasetProvisioner("pool/data", ""); err == nil {
			t.Fatal("empty zfsBinary accepted")
		}
		if zfsBucketCreate != nil || zfsBucketDestroy != nil || zfsBucketDatasetExists != nil {
			t.Fatal("hooks installed despite invalid args")
		}
	})

	t.Run("valid install drives the ops layer", func(t *testing.T) {
		if err := InstallZfsDatasetProvisioner("pool/data", "zfs"); err != nil {
			t.Fatalf("install: %v", err)
		}
		if zfsBucketCreate == nil || zfsBucketDestroy == nil || zfsBucketDatasetExists == nil {
			t.Fatal("hooks not all installed")
		}

		got := scriptZfsDatasetRunner(t, "pool/data/bkt\n", "", nil)
		ds, err := zfsBucketCreate(context.Background(), "bkt")
		if err != nil || ds != "pool/data/bkt" {
			t.Fatalf("zfsBucketCreate: (%q, %v)", ds, err)
		}
		if len(*got) != 1 || strings.Join((*got)[0], " ") != "zfs create pool/data/bkt" {
			t.Fatalf("create hook argv: %v", *got)
		}

		ok, err := zfsBucketDatasetExists(context.Background(), "pool/data/bkt")
		if err != nil || !ok {
			t.Fatalf("zfsBucketDatasetExists: (%v, %v)", ok, err)
		}

		// Re-script the runner: the fixture above returns a non-blank
		// stdout (the exists probe's row) for EVERY call it records,
		// which the destroy path must read as "snapshots exist". The
		// destroy drive needs an empty-snapshot fixture.
		got = scriptZfsDatasetRunner(t, "", "", nil)
		if err := zfsBucketDestroy(context.Background(), "pool/data/bkt"); err != nil {
			t.Fatalf("zfsBucketDestroy: %v", err)
		}
		assertNoRecursiveFlag(t, got)
	})

	t.Run("uninstall restores nil", func(t *testing.T) {
		UninstallZfsDatasetProvisioner()
		if zfsBucketCreate != nil || zfsBucketDestroy != nil || zfsBucketDatasetExists != nil {
			t.Fatal("hooks not nil after UninstallZfsDatasetProvisioner")
		}
	})
}
