// config_store_test.go — management-api-2026-10 leaf 02: the runtime
// configuration store. Covers Task 1 (installer parameter threading), Task 2
// (validate/apply/report), Task 3 (masked read), Task 4 (atomic persistence),
// and Task 5 (startup wiring).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// ---- helpers ----

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

// findIdentity returns the identity with accessKey, or nil.
func findIdentity(cfg ServerConfig, accessKey string) *auth.IdentityConfig {
	for i := range cfg.Identities {
		if cfg.Identities[i].AccessKey == accessKey {
			return &cfg.Identities[i]
		}
	}
	return nil
}

// ---- Task 1: the seam installer takes the configuration ----

// TestInstallS3SeamsUsesPassedConfig proves installS3Seams applies the
// CONFIGURATION PASSED IN, not the package global. Region is the observable:
// an explicit region makes the SigV4 region compare strict, so a request
// signed for a different region is rejected and one signed for the configured
// region authenticates.
func TestInstallS3SeamsUsesPassedConfig(t *testing.T) {
	orig := serverConfig
	t.Cleanup(func() {
		serverConfig = orig
		SetRegionForTest("")
		// Restore the global-backed fs-root resolver the rest of the suite
		// expects (installS3Seams captured a local config above).
		s3.InstallFSRootResolver(func(bucket string) string { return getBucketPath(bucket) })
	})
	// The global holds the default region; the config passed in differs.
	serverConfig = defaultServerConfig()

	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Region = "eu-central-1"
	installS3Seams(&cfg)

	// A request signed for us-east-1 must now be rejected: the seam's region
	// is the passed config's eu-central-1 (strict compare).
	req, _ := buildSignedRequest(t, map[string]string{"region": "us-east-1"})
	if got := runAuth(req); got != http.StatusForbidden {
		t.Fatalf("region from passed config not applied: mismatched-region auth = %d, want 403", got)
	}
	// A request signed for eu-central-1 authenticates.
	req2, _ := buildSignedRequest(t, map[string]string{"region": "eu-central-1"})
	if got := runAuth(req2); got != http.StatusOK {
		t.Fatalf("matching-region auth = %d, want 200", got)
	}
}

// ---- Task 2: the store ----

// TestConfigStoreSnapshotIsDeepCopy pins that Snapshot returns a deep copy:
// mutating the source after construction, or mutating the snapshot, never
// reaches the other side — including every nested map.
func TestConfigStoreSnapshotIsDeepCopy(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Buckets = map[string]string{"b": "/x"}
	cfg.BucketBackends = map[string]string{"b": "fs"}
	cfg.BucketAuditReads = map[string]bool{"b": true}
	cfg.BucketReflinkRetention = map[string]int{"b": 3}
	cfg.Backends = map[string]BackendCfg{"fs": {Root: "/r", Options: map[string]string{"k": "v"}}}
	cfg.Frontends = []FrontendConfig{{Type: "s3", Options: map[string]string{"o": "1"}}}
	cfg.Identities = []auth.IdentityConfig{{
		Name: "app", AccessKey: "AKAPP", SecretKey: "sk",
		Grants: map[string]json.RawMessage{"*": json.RawMessage(`"readonly"`)},
	}}
	cfg.AuditLog = &AuditLogConfig{Path: "/log"}
	store := NewConfigStore(&cfg)

	// Mutating the source must not touch the store.
	cfg.Buckets["b"] = "/mutated"
	cfg.Buckets["new"] = "/new"
	cfg.BucketAuditReads["b"] = false
	cfg.BucketReflinkRetention["b"] = 9
	cfg.Backends["fs"] = BackendCfg{Root: "/other"}
	cfg.Frontends[0].Options["o"] = "2"
	cfg.AuditLog.Path = "/other"
	cfg.Identities[0].SecretKey = "mutated"

	// Mutating a snapshot must not touch the store.
	snap := store.Snapshot()
	snap.Buckets["b"] = "/snap"
	snap.Buckets["x"] = "/x"
	snap.BucketAuditReads["b"] = false
	snap.BucketReflinkRetention["b"] = 42
	snap.Backends["fs"] = BackendCfg{Root: "/snap"}
	snap.Frontends[0].Options["o"] = "3"
	snap.Identities = append(snap.Identities, auth.IdentityConfig{Name: "ghost", AccessKey: "GHOST", SecretKey: "g"})
	snap.AuditLog.Path = "/snap"

	live := store.live
	if live.Buckets["b"] != "/x" || live.Buckets["new"] != "" {
		t.Errorf("store Buckets leaked a source/snapshot mutation: %v", live.Buckets)
	}
	if !live.BucketAuditReads["b"] || live.BucketReflinkRetention["b"] != 3 {
		t.Errorf("store bucket tunables leaked: %v / %v", live.BucketAuditReads, live.BucketReflinkRetention)
	}
	if live.Backends["fs"].Root != "/r" || live.Frontends[0].Options["o"] != "1" {
		t.Errorf("store Backends/Frontends leaked: %+v %+v", live.Backends, live.Frontends)
	}
	if live.Identities[0].SecretKey != "sk" {
		t.Errorf("store identity secret leaked: %q", live.Identities[0].SecretKey)
	}
	if live.AuditLog == nil || live.AuditLog.Path != "/log" {
		t.Errorf("store AuditLog leaked: %+v", live.AuditLog)
	}
}

// TestConfigStoreApplyHotRegion pins that a hot key is applied, reported in
// applied, and observable through the seam (strict region compare).
func TestConfigStoreApplyHotRegion(t *testing.T) {
	t.Cleanup(func() { SetRegionForTest("") })
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	store := NewConfigStore(&cfg)

	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(`{"region":"eu-central-1"}`)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !contains(applied, "region") {
		t.Errorf("applied = %v, want region", applied)
	}
	if len(restartRequired) != 0 {
		t.Errorf("restartRequired = %v, want empty", restartRequired)
	}
	if got := store.Snapshot().Region; got != "eu-central-1" {
		t.Errorf("snapshot region = %q, want eu-central-1", got)
	}

	// Observable through the seam.
	req, _ := buildSignedRequest(t, map[string]string{"region": "us-east-1"})
	if got := runAuth(req); got != http.StatusForbidden {
		t.Fatalf("region hot-apply not visible through the seam: auth = %d, want 403", got)
	}
}

// TestConfigStoreApplyRestartRequired pins that a restart-required key is
// recorded, reported in BOTH return values, and never claimed applied.
func TestConfigStoreApplyRestartRequired(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	store := NewConfigStore(&cfg)

	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(`{"dataDir":"/tmp/zeta-elsewhere/"}`)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(applied, "dataDir") {
		t.Errorf("applied = %v must NOT claim dataDir", applied)
	}
	if !contains(restartRequired, "dataDir") {
		t.Errorf("restartRequired = %v, want dataDir", restartRequired)
	}
	if !contains(store.RestartRequired(), "dataDir") {
		t.Errorf("RestartRequired() = %v, want dataDir", store.RestartRequired())
	}
	// The runtime store accepts the new value (authoritative in memory), but
	// it is reported as needing a restart to take effect.
	if got := store.Snapshot().DataDir; got != "/tmp/zeta-elsewhere/" {
		t.Errorf("snapshot dataDir = %q, want the recorded value", got)
	}
}

// TestConfigStoreApplyRejectsUnknownKey pins fail-loud unknown-key rejection
// with nothing changed.
func TestConfigStoreApplyRejectsUnknownKey(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	store := NewConfigStore(&cfg)
	before := store.Snapshot()

	_, _, err := store.Apply(ConfigPatch{JSON: []byte(`{"dataDirr":"/x"}`)})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want an unknown-field error", err)
	}
	if !reflect.DeepEqual(before, store.Snapshot()) {
		t.Fatal("an invalid patch changed the store")
	}
}

// TestConfigStoreApplyRejectsInvalidValues pins fail-loud rejection of an
// unknown zfs_versioning mode and an unknown backend name, with no change.
func TestConfigStoreApplyRejectsInvalidValues(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	store := NewConfigStore(&cfg)
	before := store.Snapshot()

	for name, patch := range map[string]string{
		"versioning": `{"zfs_versioning":"bogus"}`,
		"backend":    `{"buckets":{"b":{"path":"/tmp/b","backend":"nosuch"}}}`,
		"negative":   `{"zfs_versioning_reflink_retention":-1}`,
	} {
		_, _, err := store.Apply(ConfigPatch{JSON: []byte(patch)})
		if err == nil {
			t.Fatalf("%s: invalid patch was accepted", name)
		}
	}
	if !reflect.DeepEqual(before, store.Snapshot()) {
		t.Fatal("an invalid patch changed the store")
	}
}

// TestConfigStoreConcurrentApplySnapshot exercises Apply and Snapshot
// concurrently under -race.
func TestConfigStoreConcurrentApplySnapshot(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	store := NewConfigStore(&cfg)
	t.Cleanup(func() { SetRegionForTest("") })

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if _, _, err := store.Apply(ConfigPatch{JSON: fmt.Appendf(nil, `{"region":"r%d"}`, i%3)}); err != nil {
				t.Errorf("Apply: %v", err)
			}
		})
	}
	for range 20 {
		wg.Go(func() {
			_ = store.Snapshot()
		})
	}
	wg.Wait()
}

// ---- Task 3: masked read ----

// TestConfigStoreSnapshotMasksSecrets pins the mask constant and that every
// identities[].secretKey AND the env-pair credential are masked on read, while
// the live configuration keeps the real secrets.
func TestConfigStoreSnapshotMasksSecrets(t *testing.T) {
	if maskedSecretValue != "********" {
		t.Fatalf("mask constant changed: %q", maskedSecretValue)
	}
	origEnv := serverCredentials
	t.Cleanup(func() { serverCredentials = origEnv })
	serverCredentials.AccessKeyID = "AKENVMASK"
	serverCredentials.SecretAccessKey = "env-secret"

	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Identities = []auth.IdentityConfig{{
		Name: "app", AccessKey: "AKAPP", SecretKey: "app-secret",
		Grants: map[string]json.RawMessage{"*": json.RawMessage(`"readonly"`)},
	}}
	store := NewConfigStore(&cfg)

	snap := store.Snapshot()
	env := findIdentity(snap, "AKENVMASK")
	if env == nil {
		t.Fatal("snapshot is missing the env-pair identity (should be shown masked)")
	}
	if env.SecretKey != maskedSecretValue {
		t.Errorf("env-pair secret not masked: %q", env.SecretKey)
	}
	app := findIdentity(snap, "AKAPP")
	if app == nil || app.SecretKey != maskedSecretValue {
		t.Fatalf("identity secret not masked: %+v", app)
	}
	if app.SecretKey == "app-secret" {
		t.Fatal("snapshot leaked the real identity secret")
	}
	// The live configuration keeps the real secret (masking is out-only).
	if store.live.Identities[0].SecretKey != "app-secret" {
		t.Fatalf("live secret changed: %q", store.live.Identities[0].SecretKey)
	}
}

// TestConfigStoreMaskedSecretWriteBackIsNoOp pins that echoing the mask back
// never overwrites the real secret, and the installed registry still resolves
// the real credential.
func TestConfigStoreMaskedSecretWriteBackIsNoOp(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Identities = []auth.IdentityConfig{{
		Name: "app", AccessKey: "AKAPP", SecretKey: "app-secret",
		Grants: map[string]json.RawMessage{"*": json.RawMessage(`"readwrite"`)},
	}}
	store := NewConfigStore(&cfg)

	origReg := identityRegistry
	t.Cleanup(func() { identityRegistry = origReg })
	reg, err := buildRegistryFor(&cfg)
	if err != nil {
		t.Fatalf("buildRegistryFor: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(reg)

	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(
		`{"identities":[{"name":"app","accessKey":"AKAPP","secretKey":"********","grants":{"*":"readwrite"}}]}`)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !contains(applied, "identities") || len(restartRequired) != 0 {
		t.Errorf("applied=%v restart=%v", applied, restartRequired)
	}
	if got := store.live.Identities[0].SecretKey; got != "app-secret" {
		t.Fatalf("mask overwrote the real secret: %q", got)
	}
	if _, ok := identityRegistry.LookupByBasicCredential("AKAPP", "app-secret"); !ok {
		t.Fatal("the installed registry no longer resolves the real secret")
	}
}

// ---- Task 4: atomic persistence ----

// TestConfigStorePersistRoundTrip pins that Persist writes JSON loadConfig
// reads back to an equal configuration.
func TestConfigStorePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fixture := `{"dataDir":"` + dir + `/","buckets":{"b":{"path":"` + dir + `/b","backend":"fs","auditReads":true,"reflinkRetention":3}},"region":"eu-west-1","zfs_versioning":"both","identities":[{"name":"app","accessKey":"AKAPP","secretKey":"app-secret","grants":{"*":"readonly"}}]}`
	in := filepath.Join(dir, "in.json")
	if err := os.WriteFile(in, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := serverConfig
	t.Cleanup(func() { serverConfig = orig })
	if err := loadConfig(in); err != nil {
		t.Fatalf("loadConfig fixture: %v", err)
	}
	store := NewConfigStore(&serverConfig)

	out := filepath.Join(dir, "out.json")
	if err := store.Persist(out); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := loadConfig(out); err != nil {
		t.Fatalf("loadConfig (reload): %v", err)
	}
	if !reflect.DeepEqual(*store.live, serverConfig) {
		t.Fatalf("persist round-trip mismatch:\n store=%#v\n reload=%#v", *store.live, serverConfig)
	}
}

// TestConfigStorePersistAtomic pins the temp-file-plus-rename technique (the
// target inode changes) and that no partial temp file is left behind.
func TestConfigStorePersistAtomic(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultServerConfig()
	cfg.DataDir = dir + "/"
	store := NewConfigStore(&cfg)
	target := filepath.Join(dir, "config.json")

	if err := store.Persist(target); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	inoBefore := fileInode(t, target)
	if err := store.Persist(target); err != nil {
		t.Fatalf("Persist (2): %v", err)
	}
	inoAfter := fileInode(t, target)
	if inoBefore == inoAfter {
		t.Error("Persist did not replace the file (temp+rename expected: inode should change)")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// TestConfigStorePersistFailureLeavesPrevious pins that a failed write leaves
// the previous file intact.
func TestConfigStorePersistFailureLeavesPrevious(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultServerConfig()
	cfg.DataDir = dir + "/"
	store := NewConfigStore(&cfg)

	roDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(roDir, "config.json")
	if err := os.WriteFile(target, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	if err := store.Persist(target); err == nil {
		t.Fatal("Persist into a read-only directory should fail")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("previous file unreadable: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("previous file changed on failure: %q", got)
	}
}

func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no syscall.Stat_t for %s", path)
	}
	return st.Ino
}

// ---- Task 5: startup wiring ----

// TestInitConfigStoreStartupWiring pins that the startup wiring creates the
// process store and it returns the loaded configuration.
func TestInitConfigStoreStartupWiring(t *testing.T) {
	orig := serverConfig
	origStore := configStore
	t.Cleanup(func() { serverConfig = orig; configStore = origStore })

	dir := t.TempDir()
	serverConfig = defaultServerConfig()
	serverConfig.DataDir = dir + "/"
	serverConfig.Region = "ap-south-1"

	st := initConfigStore()
	if st == nil || configStore == nil {
		t.Fatal("initConfigStore did not create the process store")
	}
	snap := configStore.Snapshot()
	if snap.DataDir != dir+"/" || snap.Region != "ap-south-1" {
		t.Fatalf("store snapshot = %q/%q, want %q/ap-south-1", snap.DataDir, snap.Region, dir+"/")
	}
}

// TestConfigStoreBucketsTunableVsLayout pins the buckets special case: a
// per-bucket tunable change (auditReads / reflinkRetention) is hot-applied,
// while a bucket path/backend change is restart-required.
func TestConfigStoreBucketsTunableVsLayout(t *testing.T) {
	dir := t.TempDir()
	bucketPath := filepath.Join(dir, "b")
	if err := os.MkdirAll(bucketPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultServerConfig()
	cfg.DataDir = dir + "/"
	cfg.Buckets = map[string]string{"b": bucketPath}
	store := NewConfigStore(&cfg)

	// Tunable-only change: applied.
	tunable := fmt.Sprintf(`{"buckets":{"b":{"path":%q,"auditReads":true}}}`, bucketPath)
	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(tunable)})
	if err != nil {
		t.Fatalf("Apply tunable: %v", err)
	}
	if !contains(applied, "buckets") || len(restartRequired) != 0 {
		t.Fatalf("tunable: applied=%v restart=%v", applied, restartRequired)
	}
	if !store.live.BucketAuditReads["b"] {
		t.Fatal("per-bucket auditReads not applied")
	}

	// Layout change: restart-required, not applied.
	layout := fmt.Sprintf(`{"buckets":{"b":{"path":%q}}}`, filepath.Join(dir, "c"))
	applied, restartRequired, err = store.Apply(ConfigPatch{JSON: []byte(layout)})
	if err != nil {
		t.Fatalf("Apply layout: %v", err)
	}
	if contains(applied, "buckets") || !contains(restartRequired, "buckets") {
		t.Fatalf("layout: applied=%v restart=%v", applied, restartRequired)
	}
}
