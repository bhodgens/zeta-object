package main

import (
	"os"
	"strings"
	"testing"

	"mini-s3/internal/backend"
)

// TestBuildBackendLookupInstallsInMain pins the wiring order: main() must
// install the config-driven backendFor BEFORE the listener opens, so no
// request can hit the lazy default and an unknown backend name aborts
// startup. We cannot run main() itself in-process, so we pin the
// installBackendLookup seam main() calls and that it replaced the
// leaf-02 lazy default var target.
func TestBuildBackendLookupInstallsInMain(t *testing.T) {
	orig := backendFor
	t.Cleanup(func() { backendFor = orig })
	installed := false
	installBackendLookup(func(bucket string) (backend.Backend, error) {
		installed = true
		return nil, nil
	})
	// The package-level backendFor must now dispatch to the installed fn.
	if _, err := backendFor("any"); err != nil {
		t.Fatalf("backendFor should delegate to installed lookup: %v", err)
	}
	if !installed {
		t.Fatal("installBackendLookup did not install the lookup function")
	}
}

// TestLoadConfigUnknownBucketBackendAbortsStartup verifies the leaf's
// fail-loud rule end-to-end at the config layer: a bucket selecting an
// unregistered backend name produces a startup error naming the type and
// the registered names (no silent fs fallback).
func TestLoadConfigUnknownBucketBackendAbortsStartup(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	content := `{"dataDir":"` + dir + `","buckets":{"photos":{"path":"` + dir + `","backend":"nosuch"}}}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(path); err != nil {
		t.Fatalf("config parse should succeed: %v", err)
	}
	if _, err := buildBackendLookup(serverConfig); err == nil {
		t.Fatal("unknown backend name must abort startup, not fall back to fs")
	} else if !containsAll(err.Error(), []string{"nosuch", "fs"}) {
		t.Fatalf("error should name the type and registered names, got: %v", err)
	}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
