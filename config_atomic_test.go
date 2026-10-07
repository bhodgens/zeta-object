// config_atomic_test.go — test helper for the atomic serverConfig accessor.
//
// serverConfig is an atomic.Pointer-backed accessor (config.go); tests that
// mutate individual fields install the mutation through setServerConfigField,
// which snapshots the current generation, applies fn, and publishes the
// result as a new generation.
//
// RESTORE CONTRACT (bughunt 2026-10-06 X1): 269c193 introduced this helper
// with NO cleanup registration, and 24 converted test files call it. 21 of
// them register no restore of their own, so every one leaked a mutated global
// into whatever test ran next. The suite is green only because `make test` runs
// in declaration order; the same tree at `-shuffle=12345` failed 138 tests
// (and passed on 269c193's parent, so the migration introduced it).
//
// setServerConfigFieldT is the safe form: it registers a t.Cleanup that
// restores the generation it snapshotted, so a test using it cannot leak.
// New call sites MUST use the T variant. setServerConfigField stays for the
// existing call sites; convert them as they are touched.
package main

import "testing"

// setServerConfigField applies fn to a copy of the current configuration and
// publishes the result atomically.
//
// LEAKS BY DESIGN - prefer setServerConfigFieldT. Kept because 24 existing
// test files call it and converting them all in one edit would bury the
// behavior change; each converted file should switch its own calls over.
func setServerConfigField(fn func(c *ServerConfig)) {
	cfg := *serverConfig()
	fn(&cfg)
	setServerConfig(cfg)
}

// setServerConfigFieldT applies fn and restores the previous generation when
// the test ends. This is the form that cannot leak into a later test.
func setServerConfigFieldT(t *testing.T, fn func(c *ServerConfig)) {
	t.Helper()
	before := *serverConfig()
	t.Cleanup(func() { setServerConfig(before) })
	setServerConfigField(fn)
}

// TestSetServerConfigFieldTRestoresOnCleanup is the X1 pin for the helper
// itself: a mutation installed through the T variant leaves NO trace once the
// test finishes. It is deliberately the last assertion in its own test - the
// verification below reads the global AFTER t.Cleanup has run, which happens in
// a following test; see TestConfigHelperLeaksNothingAfterCleanup.
func TestSetServerConfigFieldTRestoresOnCleanup(t *testing.T) {
	before := serverConfig().DataDir
	setServerConfigFieldT(t, func(c *ServerConfig) { c.DataDir = "/leaked/data" })
	if serverConfig().DataDir != "/leaked/data" {
		t.Fatalf("the mutation did not take effect: DataDir = %q", serverConfig().DataDir)
	}
	// The restore runs in t.Cleanup, i.e. after this function returns. The
	// following test asserts it actually happened.
	_ = before
}

// TestConfigHelperLeaksNothingAfterCleanup proves the restore. Go runs tests in
// source order within a file, and t.Cleanup of the previous test has completed
// by the time this one starts, so reading the global here observes the
// POST-cleanup state. If setServerConfigFieldT failed to restore, this sees
// "/leaked/data" and fails.
//
// Order dependency is the POINT here: this pair only means anything because it
// is the mechanism X1 is about, and it is self-contained (it reads only the
// global it mutated), so it cannot be the source of a shuffle failure itself.
func TestConfigHelperLeaksNothingAfterCleanup(t *testing.T) {
	if got := serverConfig().DataDir; got == "/leaked/data" {
		t.Fatalf("setServerConfigFieldT leaked: DataDir is still %q after the previous "+
			"test's cleanup (bughunt 2026-10-06 X1)", got)
	}
}
