// config_hot_seams_honesty_test.go — pins for bughunt 2026-10-06 H1, H2, H3.
//
// THE CLASS: one PUT /config patch feeds FOUR authorities that each derived
// their answer independently:
//
//	 1. serverConfig()          — the atomic pointer (config.go)
//	 2. the installed s3 view   — installServerConfigView (seam.go)
//	 3. the fs-root resolver    — a closure wired ONCE at startup
//	 4. the backend lookup      — built ONCE at startup
//
// H1: applyHotSeams installed the CANDIDATE wholesale, so a restart-required
//     key in the same patch reached the live data plane while GET /config
//     (which reads s.live) reported the old value.
// H2: with four authorities, one of them must be wrong; the resolver and the
//     backend lookup stayed frozen at the startup generation.
// H3: the security-relevant consequence — validBucket reads the HOT view while
//     getBucketPath went through the STARTUP-frozen resolver, so the gate
//     authorized one path and the data plane used another.
//
// Every test below drives the REAL Apply path and reads the REAL seams.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// hotSeamEnv builds a config store over a temp dataDir with the s3 seams
// installed the way main() installs them, and restores every global on cleanup.
func hotSeamEnv(t *testing.T) (liveRoot, newRoot string) {
	t.Helper()
	dir := t.TempDir()
	liveRoot = filepath.Join(dir, "live")
	newRoot = filepath.Join(dir, "new")
	for _, p := range []string{liveRoot, newRoot} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	prevCfg := *serverConfig()
	prevStore := configStore
	t.Cleanup(func() {
		setServerConfig(prevCfg)
		configStore = prevStore
		s3.SetRegion("us-east-1")
	})

	base := defaultServerConfig()
	base.DataDir = liveRoot
	setServerConfig(base)
	if initConfigStore() == nil {
		t.Fatal("initConfigStore returned nil")
	}
	// main() wires the seams against the STARTUP config; that is the frozen
	// generation H2/H3 are about.
	installS3Seams(&base)
	return liveRoot, newRoot
}

// TestHotPatchKeepsRestartRequiredKeyOutOfTheLiveView is the H1 pin. A patch
// that co-applies a hot key (region) with a restart-required key (dataDir)
// must install ONLY the hot key into the s3 view: GET /config and the data
// plane must keep agreeing on the RUNNING dataDir.
func TestHotPatchKeepsRestartRequiredKeyOutOfTheLiveView(t *testing.T) {
	liveRoot, newRoot := hotSeamEnv(t)

	// CRITICAL: package main's tests register a config-sync hook
	// (testshim_test.go's init) that re-installs the s3 view from
	// serverConfig() on EVERY consult. serverConfig() is NOT updated by a hot
	// config patch (only the s3 seams are), so with the hook active it
	// overwrites whatever the patch installed and this test passes vacuously
	// on the reverted code. The hook is TEST-ONLY; production never installs
	// one, so a production-semantics pin must clear it.
	s3.SetConfigSyncHook(nil)
	t.Cleanup(installTestConfigSync)

	applied, restart, err := configStore.Apply(ConfigPatch{JSON: []byte(
		`{"dataDir":"` + newRoot + `","region":"eu-central-1"}`)}, )
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) == 0 || len(restart) == 0 {
		t.Fatalf("patch shape changed: applied=%v restartRequired=%v", applied, restart)
	}

	// GET /config reports the RUNNING configuration.
	snap := configStore.Snapshot()
	if snap.DataDir != liveRoot {
		t.Errorf("Snapshot().DataDir = %q, want the RUNNING root %q", snap.DataDir, liveRoot)
	}
	// The hot key DID apply.
	if snap.Region != "eu-central-1" {
		t.Errorf("Snapshot().Region = %q, want the applied eu-central-1", snap.Region)
	}
	// The live s3 view must agree with GET /config on the restart-required key.
	viewDir := s3.InstalledDataDir()
	if viewDir != liveRoot {
		t.Errorf("H1 REGRESSION: the installed s3 view dataDir = %q while GET /config reports %q; "+
			"a restart-required key reached the live data plane", viewDir, snap.DataDir)
	}
}

// TestHotPatchResolverFollowsTheInstalledView is the H3 pin for the
// LAYOUT case. It must move the data root via the REAL hot-apply path and
// assert the resolver follows — a resolver frozen against the startup
// snapshot keeps answering with the old root.
//
// It deliberately uses a CUSTOM bucket, not a plain one: a plain bucket's
// path is dataDir/name under BOTH implementations, so the plain case passes
// on the reverted code (measured) and would be a vacuous pin. The custom case
// is the one that actually distinguishes "reads the live view" from "reads
// the startup snapshot".
func TestHotPatchResolverFollowsTheInstalledView(t *testing.T) {
	liveRoot, _ := hotSeamEnv(t)
	s3.SetConfigSyncHook(nil)
	t.Cleanup(installTestConfigSync)

	customPath := filepath.Join(t.TempDir(), "moved-bucket")
	if err := os.MkdirAll(customPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Declare it custom on the generation both authorities read, then push that
	// generation through the real hot-apply path.
	setServerConfigFieldT(t, func(c *ServerConfig) {
		c.Buckets = map[string]string{"movedbucket": customPath}
	})
	if err := applyHotSeams(serverConfig()); err != nil {
		t.Fatalf("applyHotSeams: %v", err)
	}

	viewPaths := s3.InstalledCustomBucketPaths()
	if got := viewPaths["movedbucket"]; got != customPath {
		t.Fatalf("test premise: the installed view holds %q for movedbucket, want %q", got, customPath)
	}
	got := s3.GetBucketPathShim("movedbucket")
	if got != customPath {
		t.Errorf("H3 REGRESSION: the resolver says %q but the installed view declares %q; "+
			"the resolver is reading a different config generation than the gate", got, customPath)
	}
	// A non-custom bucket still resolves under the live dataDir.
	if want := filepath.Join(liveRoot, "plainbucket"); s3.GetBucketPathShim("plainbucket") != want {
		t.Errorf("plain bucket resolved to %q, want %q", s3.GetBucketPathShim("plainbucket"), want)
	}
}

// TestCustomBucketGateAndResolverAgree is the security-relevant H3 pin: a name
// the gate treats as a config-declared custom bucket must resolve to THAT
// custom path, never to a dataDir default.
//
// The custom mapping is applied through the REAL production authority — the
// atomic serverConfig generation the hot path writes — because package main's
// tests register a config-sync hook (testshim_test.go) that re-installs the
// s3 view from serverConfig() on every consult. Installing a view DIRECTLY is
// therefore immediately overwritten, which is what made the first version of
// this test report a false divergence.
func TestCustomBucketGateAndResolverAgree(t *testing.T) {
	liveRoot, _ := hotSeamEnv(t)

	customPath := filepath.Join(t.TempDir(), "photos-custom")
	if err := os.MkdirAll(customPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Declare the custom bucket on the generation BOTH authorities read.
	setServerConfigFieldT(t, func(c *ServerConfig) {
		c.Buckets = map[string]string{"photos": customPath}
		c.DataDir = liveRoot
	})
	// Push that generation into the s3 seams, exactly as a hot apply does.
	if err := applyHotSeams(serverConfig()); err != nil {
		t.Fatalf("applyHotSeams: %v", err)
	}

	if got := s3.InstalledCustomBucketPaths()["photos"]; got != customPath {
		t.Fatalf("test premise: the installed view holds %q for photos, want %q", got, customPath)
	}
	if got := s3.ValidBucket("photos"); !got {
		t.Fatal("gate rejected a config-declared custom bucket; the test premise is wrong")
	}
	got := s3.GetBucketPathShim("photos")
	if got != customPath {
		t.Errorf("H3 DIVERGENCE: the gate authorizes `photos` as the custom bucket %q "+
			"but the data plane resolves it to %q", customPath, got)
	}
}
