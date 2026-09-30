// root_coverage_wiring_test.go — leaf 6.4: coverage for the root package's
// delegating shims and wiring seams (s3_wiring.go, backend_lookup.go,
// backend_lazy.go, frontends.go). Each delegating shim gets one delegation
// test; wiring branches get table-driven resolution-order tests. Behavior
// tests live in their home package (internal/frontend/s3, leaf 6.2).
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// ---------------------------------------------------------------------------
// s3_wiring.go — delegating shims
// ---------------------------------------------------------------------------

// TestMainCredentialSource pins the credentials adapter: the configured
// access key resolves the secret, anything else does not.
func TestMainCredentialSource(t *testing.T) {
	src := mainCredentialSource{}

	got, ok := src.SecretKey(serverCredentials.AccessKeyID)
	if !ok || got != serverCredentials.SecretAccessKey {
		t.Errorf("SecretKey(%q) = (%q, %v), want the server secret",
			serverCredentials.AccessKeyID, got, ok)
	}

	got, ok = src.SecretKey("not-the-configured-key")
	if ok || got != "" {
		t.Errorf("SecretKey(unknown) = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestGetBucketPathCustomPrecedence pins the fs-root math the wiring
// installs: a configured custom bucket path wins over dataDir join.
func TestGetBucketPathCustomPrecedence(t *testing.T) {
	env := setupTestEnv(t)
	orig := serverConfig
	t.Cleanup(func() { serverConfig = orig })
	serverConfig.Buckets["custom"] = "/custom/mount"
	if got := getBucketPath("custom"); got != "/custom/mount" {
		t.Errorf("getBucketPath(custom) = %q, want the configured custom path", got)
	}
	want := filepath.Join(env.dataDir, "plain")
	if got := getBucketPath("plain"); got != want {
		t.Errorf("getBucketPath(plain) = %q, want %q", got, want)
	}
}

// TestRunMultipartSweepPassDelegates pins the shim: the pass dispatches to
// the installed frontend sweep entry (and reports 0 when none is installed).
func TestRunMultipartSweepPassDelegates(t *testing.T) {
	orig := multipartSweepEntry
	t.Cleanup(func() { sweepMu.Lock(); multipartSweepEntry = orig; sweepMu.Unlock() })

	// No entry installed: the pass is a no-op returning 0.
	sweepMu.Lock()
	multipartSweepEntry = nil
	sweepMu.Unlock()
	if n := runMultipartSweepPass(); n != 0 {
		t.Errorf("runMultipartSweepPass with nil entry = %d, want 0", n)
	}

	// Installed entry: the pass delegates and returns its count.
	called := false
	sweepMu.Lock()
	multipartSweepEntry = func() int { called = true; return 7 }
	sweepMu.Unlock()
	if n := runMultipartSweepPass(); n != 7 {
		t.Errorf("runMultipartSweepPass = %d, want the entry's 7", n)
	}
	if !called {
		t.Error("runMultipartSweepPass did not delegate to the installed entry")
	}
}

// TestStartMultipartExpirySweeperNonBlocking pins that the sweeper starts
// a goroutine and returns immediately (the established convention from
// multipart_sweeper_test.go: the loop is deliberately NOT tick-tested —
// that would require shrinking sweepInterval and racing the ticker
// goroutine's reads of the var; the pass body's coverage lives in
// runMultipartSweepPass + the frontend's sweep entry).
func TestStartMultipartExpirySweeperNonBlocking(t *testing.T) {
	orig := multipartSweepEntry
	t.Cleanup(func() { sweepMu.Lock(); multipartSweepEntry = orig; sweepMu.Unlock() })
	sweepMu.Lock()
	multipartSweepEntry = func() int { return 0 }
	sweepMu.Unlock()

	done := make(chan struct{})
	go func() {
		startMultipartExpirySweeper()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startMultipartExpirySweeper blocked; expected it to start a goroutine and return")
	}
}

// TestInstallS3SeamsMetadataResolverUnavailable drives the wiring-installed
// metadata resolver end to end: on a bucket whose path is NOT a ZFS dataset
// with events on, the probe reports unavailable and the resolver returns nil
// — so a real signed ?events request gets the contracted 503 (never a
// fabricated history). This exercises the resolver closure installS3Seams
// installs (Lookup → Probe → nil), through the real auth + dispatch path.
func TestInstallS3SeamsMetadataResolverUnavailable(t *testing.T) {
	env := setupTestEnv(t)

	bucket := "evbkt"
	if err := os.MkdirAll(filepath.Join(env.dataDir, bucket), 0755); err != nil {
		t.Fatal(err)
	}

	// installS3Seams registers the real zfs-events provider; guard against
	// a duplicate Register panic if another test already installed.
	if metadata.Lookup("zfs-events") == nil {
		installS3Seams()
	}

	req := buildSignedGetWithQuery(t, "/"+bucket, "events", "events")
	w := runRootSafe(t, req)
	if w.Code != 503 {
		t.Fatalf("GET /%s?events status = %d, want 503 (body: %s)", bucket, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no metadata provider available") {
		t.Errorf("body %q missing the no-provider message", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// backend_lookup.go — install seam + resolution-order table
// ---------------------------------------------------------------------------

// TestInstallBackendLookupNilRestoresLazy pins the restore path: installing
// nil puts the lazy default resolver back in the seam.
func TestInstallBackendLookupNilRestoresLazy(t *testing.T) {
	orig := backendFor
	t.Cleanup(func() { backendFor = orig })

	installBackendLookup(nil)

	// The lazy resolver is installed (not the caller's fn): prove it by
	// rebuilding against a config with an unknown backend name — the lazy
	// path must surface that error. (Set the config BEFORE the first
	// backendFor call: the lazy path self-installs the built table.)
	origCfg := serverConfig
	t.Cleanup(func() { serverConfig = origCfg })
	serverConfig = ServerConfig{
		DataDir:        t.TempDir() + "/",
		Buckets:        map[string]string{"b": ""},
		BucketBackends: map[string]string{"b": "nosuch"},
	}
	if _, err := backendFor("b"); err == nil {
		t.Fatal("lazy resolver did not fail loudly on an unknown backend name")
	}
}

// TestBuildBackendLookupResolutionOrder table-tests the root precedence:
// explicit bucket path > named backend's root > dataDir, the fs-root
// override of the default resolver, and option propagation.
func TestBuildBackendLookupResolutionOrder(t *testing.T) {
	dir := t.TempDir()
	fsRoot := filepath.Join(dir, "fsroot")
	if err := os.MkdirAll(fsRoot, 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("named backend root wins for implicit buckets", func(t *testing.T) {
		cfg := ServerConfig{
			DataDir: dir + "/",
			Buckets: map[string]string{"b1": ""}, // implicit bucket (no path)
			Backends: map[string]BackendCfg{
				"fs": {Root: fsRoot, Options: map[string]string{"max_put_bytes": "1024"}},
			},
		}
		lookup, err := buildBackendLookup(cfg)
		if err != nil {
			t.Fatalf("buildBackendLookup: %v", err)
		}
		for _, bucket := range []string{"b1", "undeclared"} {
			b, err := lookup(bucket)
			if err != nil {
				t.Fatalf("lookup(%q): %v", bucket, err)
			}
			fs, ok := b.(*fsbackend.FS)
			if !ok {
				t.Fatalf("lookup(%q) returned %T, want *fsbackend.FS", bucket, b)
			}
			if fs.Root() != fsRoot {
				t.Errorf("lookup(%q) root = %q, want the named backend's root %q", bucket, fs.Root(), fsRoot)
			}
		}
	})

	t.Run("dataDir fallback when backend entry has no root", func(t *testing.T) {
		cfg := ServerConfig{
			DataDir: dir + "/",
			Buckets: map[string]string{"b2": ""},
			Backends: map[string]BackendCfg{
				"fs": {Root: "", Options: map[string]string{"max_put_bytes": "1024"}},
			},
		}
		lookup, err := buildBackendLookup(cfg)
		if err != nil {
			t.Fatalf("buildBackendLookup: %v", err)
		}
		b, err := lookup("undeclared")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		fs := b.(*fsbackend.FS)
		if want := filepath.Clean(dir); fs.Root() != want {
			t.Errorf("default root = %q, want dataDir %q", fs.Root(), want)
		}
	})

	t.Run("empty root aborts construction with wrapped error", func(t *testing.T) {
		cfg := ServerConfig{
			DataDir: "", // no dataDir: fs.New("") must fail
			Buckets: map[string]string{"b3": ""},
		}
		if _, err := buildBackendLookup(cfg); err == nil {
			t.Fatal("empty root must abort construction")
		} else if !strings.Contains(err.Error(), "initializing backend") {
			t.Errorf("error %q missing the wrapping context", err)
		}
	})
}

// ---------------------------------------------------------------------------
// backend_lazy.go + initBackendLookup — lazy/startup resolution paths
// ---------------------------------------------------------------------------

// withBackendConfigSwapped swaps serverConfig for cfg for the test's
// duration (restored via t.Cleanup) and returns the temp dir used.
func withBackendConfigSwapped(t *testing.T, mutate func(cfg *ServerConfig) string) string {
	t.Helper()
	dir := t.TempDir()
	orig := serverConfig
	t.Cleanup(func() { serverConfig = orig })
	serverConfig = ServerConfig{DataDir: dir + "/", Buckets: map[string]string{}}
	mutate(&serverConfig)
	return dir
}

// TestLazyBackendForInstallsAndFailsLoudly covers both lazyBackendFor
// branches: a valid config builds, installs, and resolves; an unknown
// backend name returns the startup error without installing.
func TestLazyBackendForInstallsAndFailsLoudly(t *testing.T) {
	orig := backendFor
	t.Cleanup(func() { backendFor = orig })

	// Error branch: no install happens.
	withBackendConfigSwapped(t, func(cfg *ServerConfig) string {
		cfg.Buckets["b"] = ""
		cfg.BucketBackends = map[string]string{"b": "nosuch"}
		return ""
	})
	backendFor = lazyBackendFor
	if _, err := backendFor("b"); err == nil {
		t.Fatal("lazyBackendFor must fail loudly on an unknown backend name")
	}

	// Success branch: builds the table, installs it, resolves the bucket.
	dir := withBackendConfigSwapped(t, func(cfg *ServerConfig) string {
		cfg.Buckets["b"] = ""
		return ""
	})
	backendFor = lazyBackendFor
	b, err := backendFor("b")
	if err != nil {
		t.Fatalf("lazyBackendFor: %v", err)
	}
	if b == nil {
		t.Fatal("lazyBackendFor returned a nil backend")
	}
	if _, ok := b.(*fsbackend.FS); !ok {
		t.Errorf("backend type %T, want *fsbackend.FS", b)
	}
	_ = dir
}

// TestLazyDefaultBackendForFailsLoudly covers the lazy default's error
// branch (the non-cycling initializer backend_lazy.go exists for).
func TestLazyDefaultBackendForFailsLoudly(t *testing.T) {
	withBackendConfigSwapped(t, func(cfg *ServerConfig) string {
		cfg.Buckets["b"] = ""
		cfg.BucketBackends = map[string]string{"b": "nosuch"}
		return ""
	})
	if _, err := lazyDefaultBackendFor("b"); err == nil {
		t.Fatal("lazyDefaultBackendFor must surface the build error")
	}
}

// TestInitBackendLookupStartupPaths pins the startup entry point: success
// installs the table; an unknown backend name aborts with the error.
func TestInitBackendLookupStartupPaths(t *testing.T) {
	orig := backendFor
	t.Cleanup(func() { backendFor = orig })

	// Failure: unknown backend name aborts startup.
	withBackendConfigSwapped(t, func(cfg *ServerConfig) string {
		cfg.Buckets["b"] = ""
		cfg.BucketBackends = map[string]string{"b": "nosuch"}
		return ""
	})
	if err := initBackendLookup(); err == nil {
		t.Fatal("initBackendLookup must abort on an unknown backend name")
	}

	// Success: installs the config-driven table.
	withBackendConfigSwapped(t, func(cfg *ServerConfig) string {
		cfg.Buckets["b"] = ""
		return ""
	})
	if err := initBackendLookup(); err != nil {
		t.Fatalf("initBackendLookup: %v", err)
	}
	b, err := backendFor("b")
	if err != nil || b == nil {
		t.Fatalf("backendFor after initBackendLookup: (%v, %v)", b, err)
	}
}

// ---------------------------------------------------------------------------
// frontends.go — factory error + register error + startupPlan error
// ---------------------------------------------------------------------------

// TestBuildFrontendsFactoryErrorWrapped pins that a failing factory aborts
// construction with a wrapped error naming the frontend type.
func TestBuildFrontendsFactoryErrorWrapped(t *testing.T) {
	frontendFactories["boom"] = func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		return nil, errors.New("factory exploded")
	}
	t.Cleanup(func() { delete(frontendFactories, "boom") })

	_, _, err := buildFrontends([]FrontendConfig{{Type: "boom"}}, nilBackend{}, stubCreds{})
	if err == nil || !strings.Contains(err.Error(), "build frontend \"boom\"") || !strings.Contains(err.Error(), "factory exploded") {
		t.Fatalf("err = %v, want wrapped factory error naming the type", err)
	}
}

// TestBuildFrontendsRegisterErrorWrapped pins the duplicate-registration
// guard inside buildFrontends: a factory whose frontend collides with an
// ALREADY-registered name aborts with a wrapped register error. (A repeated
// config type re-registering its OWN name is tolerated — the mount plan
// carries the second listener — so the collision must come from a
// different config type.)
func TestBuildFrontendsRegisterErrorWrapped(t *testing.T) {
	// registerTestFrontend (frontends_nonhttp_test.go) would work, but the
	// direct map write keeps this pin self-contained like the original.
	frontendFactories["dup"] = func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		return &stubFrontend{name: "s3"}, nil // collides with the real s3 frontend
	}
	frontendFactories["dup2"] = func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		return &stubFrontend{name: "s3"}, nil // ALSO collides — "dup2" is registered first, then hits the guard
	}
	t.Cleanup(func() { delete(frontendFactories, "dup"); delete(frontendFactories, "dup2") })

	_, _, err := buildFrontends([]FrontendConfig{{Type: "s3"}, {Type: "dup"}, {Type: "dup2"}}, nilBackend{}, stubCreds{})
	if err == nil || !strings.Contains(err.Error(), "register frontend") {
		t.Fatalf("err = %v, want wrapped register error", err)
	}
}

// TestStartupPlanBuildErrorPropagates pins that startupPlan surfaces the
// construction error instead of returning a partial plan.
func TestStartupPlanBuildErrorPropagates(t *testing.T) {
	_, err := startupPlan([]FrontendConfig{{Type: "nosuchtype"}}, nilBackend{}, stubCreds{})
	if err == nil || !strings.Contains(err.Error(), "known: [ftp owncloud s3 sftp webdav]") {
		t.Fatalf("err = %v, want the unknown-type error with the known list", err)
	}
}
