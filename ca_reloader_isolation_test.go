package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	h3 "github.com/bhodgens/zeta-object/internal/frontend/h3"
)

// ca_reloader_isolation_test.go — the PROCESS-GLOBAL client-CA reloader
// registry's test-isolation contract, pinned deterministically instead of
// hoping a shuffled run happens to pass.
//
// The registry (admin_wiring.go) is keyed by frontend Name() and lives for the
// process lifetime, but every CONSTRUCTION of a registering frontend writes
// to it (frontends.go). A test that builds an h3 frontend therefore captures
// that instance's clientCAFile — a path inside its own t.TempDir() — into a
// global that outlives the test. Any LATER test that calls the reload fan then
// invokes a registrant whose CA file is gone, and because commit c0df3a8's fan
// runs every registrant without short-circuiting, the dead one is LOUD:
// "h3: reading clientCAFile \".../TestStartQUICFrontend_.../ca.pem\": no such
// file or directory". That is order dependence, not a flaky assertion.
//
// Two properties are pinned here:
//
//   - ISOLATION. A helper that constructs a registering frontend must leave no
//     registrant behind, so the next test's fan cannot see a dead CA path.
//     TestClientCAReloaderIsolation_HelperCleanupPreventsCrossTestLeak drives
//     this DETERMINISTICALLY with subtests (a leaking helper always fails it;
//     -shuffle is not needed to expose it).
//   - IDEMPOTENT REGISTRATION. Registering on every construction is safe
//     because the registry replaces by Name(): building the same frontend
//     twice leaves ONE entry, and it is the live instance reloads reach.

// fsbackendForTest builds the backend argument the frontend factories take,
// over the test's dataDir — the same construction
// buildH3FrontendThroughFactory performs (fsbackend.New over
// serverConfig().DataDir), factored out so both call sites share one shape.
func fsbackendForTest(t *testing.T) (backend.Backend, error) {
	t.Helper()
	return fsbackend.New(strings.TrimSuffix(serverConfig().DataDir, "/"))
}

// isolateClientCAReloaders empties the process-global client-CA reloader
// registry NOW and again on cleanup, so every caller that inherits it both
// starts from a known state and leaves none behind.
//
// This is the shared-helper-level form of the fix. A reset in ONE test is
// what the suite did before and it is what failed: the leak is created by
// every frontend construction, so the cleanup belongs on the construction
// helper that all its callers inherit (buildH3FrontendThroughFactory), not on
// one test that happened to notice.
func isolateClientCAReloaders(t *testing.T) {
	t.Helper()
	resetClientCAReloaders()
	t.Cleanup(resetClientCAReloaders)
}

// TestClientCAReloaderIsolation_HelperCleanupPreventsCrossTestLeak is the
// deterministic pin for the order-dependence bug: two subtests in sequence,
// where the first constructs a registering frontend over a CA file that DIES
// with its own t.TempDir(), and the second runs the reload fan.
//
// The second subtest is exactly the shape that failed under -shuffle in
// TestAdminWiringReloadAuthReloadsIdentitiesAndCA. If the construction helper
// leaks its registrant, the fan reaches the first subtest's deleted CA file and
// this test fails on every run — no shuffle required. That is what makes the
// fix verifiable rather than lucky: the shuffle gate is probabilistic, this is
// not.
func TestClientCAReloaderIsolation_HelperCleanupPreventsCrossTestLeak(t *testing.T) {
	isolateClientCAReloaders(t)

	t.Run("constructs a frontend over its own temp CA", func(t *testing.T) {
		pki := installCAPKITestWiring(t)
		buildH3FrontendThroughFactory(t, pki)
		if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"h3"}) {
			t.Fatalf("registrants = %v, want the freshly built h3 frontend", got)
		}
		// t.TempDir() is removed when THIS subtest ends, taking pki.caPath
		// with it. A registrant still registered past this point is a
		// closure over a deleted path — the leak the next subtest detects.
	})

	// A second, unrelated test in the same process. It registers its own
	// live registrant and reloads; the fan must reach only that one. The
	// leaked h3 registrant would fail here on the missing file.
	t.Run("a later reload must not reach the finished test's CA path", func(t *testing.T) {
		called := false
		registerClientCAReloader("admin", func() error { called = true; return nil })

		if err := reloadRegisteredClientCAs(); err != nil {
			t.Fatalf("reload reached a registrant left behind by a finished test: %v", err)
		}
		if !called {
			t.Fatal("reload did not reach the live registrant it was given")
		}
		if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"admin"}) {
			t.Fatalf("registrants = %v, want only this subtest's own admin registrant", got)
		}
	})
}

// TestFactory_H3ReconstructionReplacesTheSameNameRegistrant pins the
// registration INVARIANT frontends.go relies on: ONE registrant per NAMED
// consumer, and re-registration REPLACES (admin_wiring.go's
// registerClientCAReloader overwrites a Name()-matching entry instead of
// appending).
//
// Registering on every construction is therefore idempotent, not a leak:
// constructing the h3 frontend TWICE leaves the registry at length 1, not 2.
// The stronger half of the assertion is WHICH instance survives — the live one.
// The first instance's CA file is deleted before the reload; a registry that
// kept both entries (a slice append) would fail on the missing file, and a
// registry that kept only the FIRST would swap the wrong listener's pool.
func TestFactory_H3ReconstructionReplacesTheSameNameRegistrant(t *testing.T) {
	pki := installCAPKITestWiring(t)
	isolateClientCAReloaders(t)

	// Instance 1, over pki.caPath.
	first := buildH3FrontendThroughFactory(t, pki)

	// Instance 2, over a DIFFERENT CA file, so "which instance is registered"
	// is observable rather than assumed.
	secondCA := filepath.Join(pki.dir, "client-ca-2.pem")
	if err := os.WriteFile(secondCA, newTestCA(t, "client-ca-reload-root-2").pemData, 0o600); err != nil {
		t.Fatalf("write second client CA: %v", err)
	}
	be, err := fsbackendForTest(t)
	if err != nil {
		t.Fatalf("backend for second construction: %v", err)
	}
	fe, err := frontendFactories["h3"](
		FrontendConfig{Type: "h3", ListenAddr: "127.0.0.1:0", Bucket: "photos",
			Options: map[string]string{"clientCAFile": secondCA}},
		be, stubCreds{})
	if err != nil {
		t.Fatalf("second h3 factory: %v", err)
	}
	second, ok := fe.(*h3.Frontend)
	if !ok {
		t.Fatalf("factory returned %T, want *h3.Frontend", fe)
	}
	if first == second {
		t.Fatal("the factory returned the same instance twice; the two constructions must be independent")
	}

	// The pin: length 1, not 2.
	if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"h3"}) {
		t.Fatalf("registrants = %v, want exactly one h3 entry after constructing the frontend twice (re-registration replaces, it does not append)", got)
	}

	// Which instance is live: instance 1's CA file is GONE, instance 2's is
	// intact. A reload that reaches instance 1 fails on the missing file; one
	// that reaches instance 2 succeeds.
	if err := os.Remove(pki.caPath); err != nil {
		t.Fatalf("remove the superseded instance's CA file: %v", err)
	}
	if err := reloadRegisteredClientCAs(); err != nil {
		t.Fatalf("reload reached the SUPERSEDED h3 instance (registry length 1 must hold the live one): %v", err)
	}

	// And the live instance is genuinely the one wired up: corrupting ITS
	// bundle is what a reload now reports.
	if err := os.WriteFile(secondCA, []byte("not a pem bundle"), 0o600); err != nil {
		t.Fatalf("corrupt the live instance's CA file: %v", err)
	}
	err = reloadRegisteredClientCAs()
	if err == nil {
		t.Fatal("reload accepted a bundle with no certificates on the live h3 instance")
	}
	if !strings.Contains(err.Error(), "h3 client CA reload") {
		t.Fatalf("error %q must name the h3 listener", err.Error())
	}
	_ = second
}

// TestFactory_AdminReconstructionReplacesTheSameNameRegistrant is the admin
// half of the same invariant: the admin factory registers on every
// construction too (frontends.go), and must behave identically — one entry per
// name, replaced in place.
func TestFactory_AdminReconstructionReplacesTheSameNameRegistrant(t *testing.T) {
	pki := installCAPKITestWiring(t)
	isolateClientCAReloaders(t)
	installTestConfigStore(t, defaultServerConfig())

	caPath := filepath.Join(pki.dir, "admin-ca.pem")
	if err := os.WriteFile(caPath, newTestCA(t, "admin-ca-root").pemData, 0o600); err != nil {
		t.Fatalf("write admin CA: %v", err)
	}
	entry := FrontendConfig{Type: "admin", ListenAddr: "127.0.0.1:0",
		Options: map[string]string{"clientCAFile": caPath}}

	if _, err := frontendFactories["admin"](entry, nilBackend{}, stubCreds{}); err != nil {
		t.Fatalf("first admin factory: %v", err)
	}
	if _, err := frontendFactories["admin"](entry, nilBackend{}, stubCreds{}); err != nil {
		t.Fatalf("second admin factory: %v", err)
	}

	if got := registeredClientCAReloaders(); !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("registrants = %v, want exactly one admin entry after constructing the frontend twice", got)
	}
}
