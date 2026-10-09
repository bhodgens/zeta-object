package s3

// zfstagseam_test.go — the zfs_native_tags selection + gate tests
// (zfs-metadata#13 consumer leaf). NO real zfs/zmetad: the dataset
// exists-probe and the tag runner are fakes, and the t.TempDir fake
// zmetad binary exercises the REAL exec path (runner unmocked — the
// zfs-features test pattern).
//
// Covers: selection (feature off -> sidecar; dataset-backed ->
// zmetadTagStore; plain-dir bucket -> sidecar; failed probe ->
// failingTagStore, never a silent sidecar fallback), the bucket-name
// derivation rule (custom bucket paths outside dataDir never select
// zmetad), installer validation (empty/unsafe args rejected), the
// uninstall reset, and the end-to-end exec round-trip through a fake
// zmetad binary writing a state file.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installTestTagGate installs the gate with a scripted dataset probe.
func installTestTagGate(t *testing.T, parent, dataDir string, exists func(ctx context.Context, dataset string) (bool, error)) {
	t.Helper()
	if err := InstallZmetadTagStore(parent, "zfs", "zmetad", "/tmp/unused.db", dataDir, exists); err != nil {
		t.Fatalf("InstallZmetadTagStore: %v", err)
	}
	t.Cleanup(UninstallZmetadTagStore)
}

func TestTagStoreSelection_FeatureOffIsSidecar(t *testing.T) {
	UninstallZmetadTagStore()
	store := tagStoreFor("/some/dataDir/bkt")
	if _, ok := store.(sidecarTagStore); !ok {
		t.Fatalf("feature off must select sidecarTagStore, got %T", store)
	}
}

func TestTagStoreSelection_DatasetBackedGetsZmetad(t *testing.T) {
	installTestTagGate(t, "testpool/zval", "/dataDir/", func(context.Context, string) (bool, error) {
		return true, nil
	})
	store := tagStoreFor("/dataDir/bkt")
	if _, ok := store.(*zmetadTagStore); !ok {
		t.Fatalf("dataset-backed bucket must select zmetadTagStore, got %T", store)
	}
}

func TestTagStoreSelection_PlainDirGetsSidecar(t *testing.T) {
	installTestTagGate(t, "testpool/zval", "/dataDir/", func(context.Context, string) (bool, error) {
		return false, nil
	})
	store := tagStoreFor("/dataDir/plainbkt")
	if _, ok := store.(sidecarTagStore); !ok {
		t.Fatalf("plain-dir bucket must select sidecarTagStore, got %T", store)
	}
}

func TestTagStoreSelection_ProbeFailureFailsLoud(t *testing.T) {
	installTestTagGate(t, "testpool/zval", "/dataDir/", func(context.Context, string) (bool, error) {
		return false, errors.New("zfs list timed out")
	})
	store := tagStoreFor("/dataDir/bkt")
	if _, ok := store.(failingTagStore); !ok {
		t.Fatalf("failed probe must select failingTagStore (never a silent sidecar fallback), got %T", store)
	}
	if _, err := store.Get("k"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("failingTagStore.Get must carry the probe error, got %v", err)
	}
}

func TestTagStoreSelection_CustomBucketPathNeverZmetad(t *testing.T) {
	installTestTagGate(t, "testpool/zval", "/dataDir/", func(context.Context, string) (bool, error) {
		return true, nil
	})
	// A bucket path outside dataDir (custom mapping) cannot be derived to
	// a bucket name: sidecar, no probe call.
	store := tagStoreFor("/elsewhere/custombkt")
	if _, ok := store.(sidecarTagStore); !ok {
		t.Fatalf("custom bucket path must select sidecarTagStore, got %T", store)
	}
}

func TestTagStoreSelection_DatasetNameDerivedNotPathResolved(t *testing.T) {
	// The exists probe must receive the DETERMINISTIC <parent>/<bucket>
	// name — never a path (zfs list on a plain dir returns the PARENT).
	var got string
	installTestTagGate(t, "testpool/zval", "/dataDir/", func(_ context.Context, dataset string) (bool, error) {
		got = dataset
		return true, nil
	})
	_ = tagStoreFor("/dataDir/my.bkt")
	if got != "testpool/zval/my.bkt" {
		t.Fatalf("probe dataset = %q, want testpool/zval/my.bkt", got)
	}
}

func TestInstallZmetadTagStore_Validation(t *testing.T) {
	probe := func(context.Context, string) (bool, error) { return false, nil }
	cases := []struct {
		name string
		inst func() error
	}{
		{"empty parent", func() error { return InstallZmetadTagStore("", "zfs", "zmetad", "/tmp/db", "/d", probe) }},
		{"unsafe parent", func() error { return InstallZmetadTagStore("pool@snap", "zfs", "zmetad", "/tmp/db", "/d", probe) }},
		{"empty zfs binary", func() error { return InstallZmetadTagStore("p", "", "zmetad", "/tmp/db", "/d", probe) }},
		{"empty zmetad binary", func() error { return InstallZmetadTagStore("p", "zfs", "", "/tmp/db", "/d", probe) }},
		{"empty db path", func() error { return InstallZmetadTagStore("p", "zfs", "zmetad", "", "/d", probe) }},
		{"nil probe", func() error { return InstallZmetadTagStore("p", "zfs", "zmetad", "/tmp/db", "/d", nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.inst(); err == nil {
				t.Fatal("expected install rejection")
			}
			// A rejected install leaves the gate OFF (sidecar everywhere).
			if store := tagStoreFor("/d/bkt"); !isSidecarTagStore(store) {
				t.Fatalf("rejected install must leave sidecar selection, got %T", store)
			}
		})
	}
}

func isSidecarTagStore(s tagStore) bool {
	_, ok := s.(sidecarTagStore)
	return ok
}

func TestUninstallZmetadTagStore_ResetsGate(t *testing.T) {
	installTestTagGate(t, "p", "/d/", func(context.Context, string) (bool, error) {
		return true, nil
	})
	UninstallZmetadTagStore()
	if store := tagStoreFor("/d/bkt"); !isSidecarTagStore(store) {
		t.Fatalf("after uninstall selection must be sidecar, got %T", store)
	}
}

func TestBucketNameFromPath(t *testing.T) {
	cases := []struct {
		path, dir, want string
		wantErr         bool
	}{
		{"/dataDir/bkt", "/dataDir", "bkt", false},
		{"/dataDir/bkt", "/dataDir/", "bkt", false},
		{"/other/bkt", "/dataDir", "", true},
		{"/dataDir", "/dataDir", "", true},     // the dataDir itself is not a bucket
		{"/dataDir/a/b", "/dataDir", "", true}, // nested: not a bucket root
		{"", "/dataDir", "", true},
	}
	for _, tc := range cases {
		got, err := bucketNameFromPath(tc.path, tc.dir)
		if tc.wantErr {
			if err == nil {
				t.Errorf("bucketNameFromPath(%q,%q): expected error, got %q", tc.path, tc.dir, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("bucketNameFromPath(%q,%q) = %q, %v; want %q", tc.path, tc.dir, got, err, tc.want)
		}
	}
}

// TestZmetadTagStore_RealExecFakeBinary exercises the REAL runner path
// (no seam mock) against a fake zmetad script in t.TempDir that records
// its argv to a file and prints a canned --tag-get answer.
func TestZmetadTagStore_RealExecFakeBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "zmetad-fake")
	argvLog := filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + argvLog + "\n" +
		"case \" $* \" in *' --tag-get '*) printf 'k1=v1\\nk2=has=equals\\n' ;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: fake executable needs the exec bit.
		t.Fatalf("write fake zmetad: %v", err)
	}
	// The fake lives outside PATH: pass its absolute path as the binary.
	store := &zmetadTagStore{
		binary:  bin,
		dbPath:  filepath.Join(dir, "unused.db"),
		dataset: "testpool/zval/bkt",
		resolveObjectID: func(context.Context, string, string) (uint64, error) {
			return 7, nil
		},
	}

	tags, err := store.Get("obj.txt")
	if err != nil {
		t.Fatalf("Get via fake binary: %v", err)
	}
	if tags["k1"] != "v1" || tags["k2"] != "has=equals" {
		t.Fatalf("tags = %v", tags)
	}
	raw, err := os.ReadFile(argvLog) //nolint:gosec // G304: test-fixed temp path.
	if err != nil {
		t.Fatalf("argv log missing (binary never exec'd): %v", err)
	}
	if !strings.Contains(string(raw), "--tag-get") || !strings.Contains(string(raw), "--tag-object") || !strings.Contains(string(raw), "7") {
		t.Fatalf("argv log = %s", raw)
	}
}
