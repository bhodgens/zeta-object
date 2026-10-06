// config_store_state_test.go — the config store's STATE invariants
// (bughunt 2026-10-05, M7/M8 + the restart-required, reflinkRetention and
// Persist-permission findings).
//
// The contract pinned here, in one place:
//
//   - a REJECTED patch changes NOTHING observable: not the store, not the
//     live installed seams, not the PROCESS-GLOBAL rich-grant table
//     internal/auth's AuthorizeOp reads (M7 + M8);
//   - a restart-required key is REPORTED and never claimed live: the
//     store's snapshot keeps the RUNNING value while the operator's
//     desired value is persisted for the next boot (MEDIUM);
//   - the per-bucket reflinkRetention survives the object-form decode
//     AND the persisted document (L8);
//   - Persist writes the config file (which carries real, unmasked
//     identity secrets) owner-only (L9).
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// ---- M7: a rejected patch must not touch the live installed seams ----

// TestConfigStoreRejectedPatchLeavesLiveRegionUnchanged is the auditor's EXACT
// M7 execution: Apply({"region":"eu-west-1","zfs_bucket_datasets":true}) must
// fail (the dataset provisioner cannot install from an empty parent dataset)
// and must leave the live SigV4 region alone. Before the fix the region seam
// was installed BEFORE the provisioner step that fails, so a request the API
// reported as a rejected no-op rejected every client signing the region the
// API still advertises.
func TestConfigStoreRejectedPatchLeavesLiveRegionUnchanged(t *testing.T) {
	t.Cleanup(func() {
		SetRegionForTest("")
		s3.UninstallZfsDatasetProvisioner()
	})

	// This test's premise is "no dataset parent is configured", so the
	// rejection fires on the unresolvable parent. Pin the process global
	// (save/restore) instead of inheriting whatever a shuffled earlier test
	// left behind.
	origParent := zfsBucketsParentDataset
	t.Cleanup(func() { zfsBucketsParentDataset = origParent })
	zfsBucketsParentDataset = ""

	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Region = "us-east-1"
	store := NewConfigStore(&cfg)

	// Install the live seams the way startup does (installS3Seams begins
	// with this hot section), so "live region" is a real observation.
	if err := applyHotSeams(&cfg); err != nil {
		t.Fatalf("applyHotSeams (startup shape): %v", err)
	}

	// Baseline: the live region is us-east-1, so a us-east-1 signature
	// authenticates and a eu-west-1 one does not.
	if got := runAuth(signedRequest(t, "us-east-1")); got != http.StatusOK {
		t.Fatalf("baseline us-east-1 auth = %d, want 200", got)
	}
	if got := runAuth(signedRequest(t, "eu-west-1")); got != http.StatusForbidden {
		t.Fatalf("baseline eu-west-1 auth = %d, want 403", got)
	}

	before := store.Snapshot()
	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(
		`{"region":"eu-west-1","zfs_bucket_datasets":true}`)})
	if err == nil {
		t.Fatalf("patch with an unresolvable dataset parent was accepted (applied=%v restart=%v)", applied, restartRequired)
	}
	if !strings.Contains(err.Error(), "parent dataset") {
		t.Errorf("err = %v, want it to name the invalid parent dataset", err)
	}

	// The live seam must be exactly where it was: a us-east-1 signature
	// still authenticates (the auditor's reported symptom) ...
	if got := runAuth(signedRequest(t, "us-east-1")); got != http.StatusOK {
		t.Errorf("a REJECTED patch changed the live SigV4 region: us-east-1 auth = %d, want 200", got)
	}
	// ... and the region the patch asked for was never installed.
	if got := runAuth(signedRequest(t, "eu-west-1")); got != http.StatusForbidden {
		t.Errorf("the rejected region was installed anyway: eu-west-1 auth = %d, want 403", got)
	}
	if s3.ZfsDatasetProvisionerInstalled() {
		t.Error("the rejected patch installed the dataset provisioner")
	}
	// And the store itself is untouched, including what GET /config reports.
	if after := store.Snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("a rejected patch changed the store:\n before=%+v\n after=%+v", before, after)
	}
	if got := store.Snapshot().Region; got != "us-east-1" {
		t.Errorf("GET /config reports region %q after a rejected region patch, want us-east-1", got)
	}
	if len(store.RestartRequired()) != 0 {
		t.Errorf("RestartRequired() = %v after a rejected patch, want empty", store.RestartRequired())
	}
}

// signedRequest builds a SigV4-signed request for the given credential-scope
// region (the auditor's execution signs the region the API advertises).
func signedRequest(t *testing.T, region string) *http.Request {
	t.Helper()
	req, _ := buildSignedRequest(t, map[string]string{"region": region})
	return req
}

// TestConfigStoreRejectedPatchRollsBackHotSeams pins the compensating half of
// the M7 fix: when the hot-apply step still fails after validation (a new
// fallible step in the installer, or an install error validation cannot
// predict), the store re-installs the RUNNING configuration instead of
// leaving a half-applied one.
func TestConfigStoreRejectedPatchRollsBackHotSeams(t *testing.T) {
	t.Cleanup(func() { SetRegionForTest("") })

	// An otherwise-valid patch whose hot apply cannot succeed: the dataset
	// provisioner rejects a parent dataset name that is unsafe but not
	// EMPTY (the empty case is caught by pre-validation, so this exercises
	// the rollback path).
	origParent := zfsBucketsParentDataset
	t.Cleanup(func() { zfsBucketsParentDataset = origParent })
	zfsBucketsParentDataset = "pool/data@bad"

	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Region = "us-east-1"
	store := NewConfigStore(&cfg)
	if err := applyHotSeams(&cfg); err != nil {
		t.Fatalf("applyHotSeams: %v", err)
	}

	if _, _, err := store.Apply(ConfigPatch{JSON: []byte(
		`{"region":"eu-central-1","zfs_bucket_datasets":true}`)}); err == nil {
		t.Fatal("patch with an unsafe dataset parent was accepted")
	}
	if got := runAuth(signedRequest(t, "us-east-1")); got != http.StatusOK {
		t.Errorf("a rejected patch left the live SigV4 region mutated: us-east-1 auth = %d, want 200", got)
	}
	if got := store.Snapshot().Region; got != "us-east-1" {
		t.Errorf("a rejected patch committed region %q to the store, want us-east-1", got)
	}
}

// ---- M8: validation must not publish the process-global grant table ----

// TestConfigStoreRejectedPatchLeavesGrantTableUnchanged is the auditor's EXACT
// M8 execution, in the order that makes it reachable: validateCandidate builds
// a real identity registry, and building one WRITES the process-global rich
// table internal/auth's AuthorizeOp reads. A patch that is widened AND
// invalid therefore granted a privilege the API reported as rejected.
//
// The pinned invariant: after ANY rejected patch the process-global grant
// table is identical to what it was before the call.
func TestConfigStoreRejectedPatchLeavesGrantTableUnchanged(t *testing.T) {
	const accessKey = "AKM8APP"

	// Live configuration: the identity has NO rich grant, so the object-form
	// grant in the patch would widen its authority from nothing to
	// read+write on "secretbkt".
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Region = "us-east-1"
	cfg.Identities = []auth.IdentityConfig{{
		Name: "app", AccessKey: accessKey, SecretKey: "app-secret",
		Grants: map[string]json.RawMessage{"pub": json.RawMessage(`"readonly"`)},
	}}
	store := NewConfigStore(&cfg)

	origReg := identityRegistry
	t.Cleanup(func() { identityRegistry = origReg })
	reg, err := buildRegistryFor(&cfg)
	if err != nil {
		t.Fatalf("buildRegistryFor: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(reg)

	// The effective decision the live registry serves, before the patch.
	beforeDecision := liveAuthorize(t, accessKey, auth.OpWrite, "secretbkt", "k")
	if beforeDecision {
		t.Fatal("precondition: the live identity must NOT be authorized to write secretbkt")
	}
	beforeTable := richTableView(accessKey)

	// The patch widens the identity's grants AND fails to apply (the dataset
	// provisioner cannot install from the resolved parent).
	origParent := zfsBucketsParentDataset
	t.Cleanup(func() { zfsBucketsParentDataset = origParent })
	zfsBucketsParentDataset = "pool/data@bad"

	if _, _, err := store.Apply(ConfigPatch{JSON: []byte(`{"region":"eu-west-1","zfs_bucket_datasets":true,` +
		`"identities":[{"name":"app","accessKey":"` + accessKey + `","secretKey":"app-secret",` +
		`"grants":{"pub":"readonly","secretbkt":{"ops":["read","write"]}}}]}`)}); err == nil {
		t.Fatal("the widening patch was accepted; it must fail on the dataset install")
	}

	if got := liveAuthorize(t, accessKey, auth.OpWrite, "secretbkt", "k"); got != beforeDecision {
		t.Errorf("a REJECTED patch changed the effective grant: AuthorizeOp(write, secretbkt) = %v, want %v",
			got, beforeDecision)
	}
	if after := richTableView(accessKey); !reflect.DeepEqual(beforeTable, after) {
		t.Errorf("a REJECTED patch mutated the process-global rich-grant table:\n before=%+v\n after=%+v",
			beforeTable, after)
	}
	// The read grant the live identity does have is still the whole story.
	if !liveAuthorize(t, accessKey, auth.OpRead, "pub", "k") {
		t.Error("the live read grant on pub was lost")
	}
}

// TestConfigStoreRejectedNewIdentityLeavesNoGrantResidue covers the other half
// of the M8 invariant: a rejected patch that introduces a BRAND NEW identity
// must leave nothing registered under its access key (a candidate-only key is
// cleared, not left behind by the validation build).
func TestConfigStoreRejectedNewIdentityLeavesNoGrantResidue(t *testing.T) {
	const newKey = "AKM8NEW"

	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.Region = "us-east-1"
	cfg.Identities = []auth.IdentityConfig{{
		Name: "app", AccessKey: "AKM8BASE", SecretKey: "s",
		Grants: map[string]json.RawMessage{"pub": json.RawMessage(`"readonly"`)},
	}}
	store := NewConfigStore(&cfg)

	origReg := identityRegistry
	t.Cleanup(func() { identityRegistry = origReg })
	reg, err := buildRegistryFor(&cfg)
	if err != nil {
		t.Fatalf("buildRegistryFor: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(reg)

	origParent := zfsBucketsParentDataset
	t.Cleanup(func() { zfsBucketsParentDataset = origParent })
	zfsBucketsParentDataset = "pool/data@bad"

	if _, _, err := store.Apply(ConfigPatch{JSON: []byte(`{"region":"eu-west-1","zfs_bucket_datasets":true,` +
		`"identities":[{"name":"new","accessKey":"` + newKey + `","secretKey":"s",` +
		`"grants":{"secretbkt":{"ops":["read","write"]}}}]}`)}); err == nil {
		t.Fatal("the patch was accepted; it must fail on the dataset install")
	}
	if got := (auth.Identity{AccessKeyID: newKey}).RichGrants(); len(got) != 0 {
		t.Errorf("a REJECTED patch left rich grants registered under the new access key: %+v", got)
	}
}

// liveAuthorize asks the INSTALLED registry for the identity and runs the one
// decision point (auth.AuthorizeOp) against it — the same call the frontends
// make per request.
func liveAuthorize(t *testing.T, accessKey string, op auth.Op, bucket, key string) bool {
	t.Helper()
	if identityRegistry == nil {
		t.Fatal("identityRegistry is nil")
	}
	id, ok := identityRegistry.LookupByAccessKey(accessKey)
	if !ok {
		t.Fatalf("installed registry no longer resolves %q", accessKey)
	}
	return auth.AuthorizeOp(id, op, bucket, key, time.Now())
}

// richTableView is a comparable snapshot of the process-global rich-grant
// table entries the test's identities can reach.
func richTableView(accessKeys ...string) map[string][]auth.GrantExpr {
	out := make(map[string][]auth.GrantExpr, len(accessKeys))
	for _, k := range accessKeys {
		if exprs := (auth.Identity{AccessKeyID: k}).RichGrants(); len(exprs) > 0 {
			out[k] = exprs
		}
	}
	return out
}

// ---- MEDIUM: a restart-required key is reported, never claimed live ----

// TestConfigStoreRestartRequiredIsReportedNotLive pins the honest shape: the
// store's snapshot (what GET /config reports) keeps the RUNNING value for a
// restart-required key, the key is reported under restartRequired, and the
// operator's desired value is what a subsequent Persist writes so the next
// boot picks it up. Storing a not-yet-running value as live is what made
// GET /config misreport the running configuration.
func TestConfigStoreRestartRequiredIsReportedNotLive(t *testing.T) {
	running := t.TempDir() + "/"
	cfg := defaultServerConfig()
	cfg.DataDir = running
	cfg.Region = "us-east-1"
	store := NewConfigStore(&cfg)

	const desired = "/tmp/zeta-elsewhere/"
	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(`{"dataDir":"` + desired + `"}`)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(applied, "dataDir") {
		t.Errorf("applied = %v must NOT claim dataDir", applied)
	}
	if !contains(restartRequired, "dataDir") || !contains(store.RestartRequired(), "dataDir") {
		t.Errorf("restartRequired = %v / %v, want dataDir in both", restartRequired, store.RestartRequired())
	}
	// GET /config must report the RUNNING data dir, not the pending one.
	if got := store.Snapshot().DataDir; got != running {
		t.Errorf("GET /config reports dataDir %q, want the running %q", got, running)
	}
	// Persist carries the operator's desired value for the next boot.
	out := filepath.Join(t.TempDir(), "config.json")
	if err := store.Persist(out); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), desired) {
		t.Errorf("persisted document does not carry the pending dataDir %q: %s", desired, raw)
	}
}

// TestConfigStoreRestartRequiredComposition pins that a later restart-required
// patch composes with an earlier one instead of dropping it (the desired
// configuration accumulates; the running configuration does not).
func TestConfigStoreRestartRequiredComposition(t *testing.T) {
	running := t.TempDir() + "/"
	cfg := defaultServerConfig()
	cfg.DataDir = running
	store := NewConfigStore(&cfg)

	if _, _, err := store.Apply(ConfigPatch{JSON: []byte(`{"dataDir":"/tmp/zeta-elsewhere/"}`)}); err != nil {
		t.Fatalf("Apply dataDir: %v", err)
	}
	if _, _, err := store.Apply(ConfigPatch{JSON: []byte(`{"listenAddr":":9999"}`)}); err != nil {
		t.Fatalf("Apply listenAddr: %v", err)
	}
	snap := store.Snapshot()
	if snap.DataDir != running || snap.ListenAddr != defaultListenAddr {
		t.Fatalf("running config drifted: dataDir=%q listenAddr=%q", snap.DataDir, snap.ListenAddr)
	}
	got := store.RestartRequired()
	if !contains(got, "dataDir") || !contains(got, "listenAddr") {
		t.Errorf("RestartRequired() = %v, want both keys", got)
	}
	out := filepath.Join(t.TempDir(), "config.json")
	if err := store.Persist(out); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	reloaded := loadConfigForTest(t, out)
	if reloaded.DataDir != "/tmp/zeta-elsewhere/" || reloaded.ListenAddr != ":9999" {
		t.Errorf("persisted desired config = %q/%q, want the pending values", reloaded.DataDir, reloaded.ListenAddr)
	}
}

// ---- L8: the object buckets form must round-trip reflinkRetention ----

// TestConfigStoreBucketsObjectFormRoundTripsReflinkRetention pins that a
// buckets object form carrying reflinkRetention survives the DECODE (it used
// to be dropped there, so the patch reported "applied" while silently
// resetting the per-bucket cap to 0 = keep zero version copies) and is
// written back by Persist.
func TestConfigStoreBucketsObjectFormRoundTripsReflinkRetention(t *testing.T) {
	dir := t.TempDir()
	bucketPath := filepath.Join(dir, "b")
	cfg := defaultServerConfig()
	cfg.DataDir = dir + "/"
	cfg.Buckets = map[string]string{"b": bucketPath}
	store := NewConfigStore(&cfg)

	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(
		`{"buckets":{"b":{"path":"` + bucketPath + `","reflinkRetention":7}}}`)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !contains(applied, "buckets") || len(restartRequired) != 0 {
		t.Fatalf("applied=%v restart=%v, want a hot buckets apply", applied, restartRequired)
	}
	if got := store.Snapshot().BucketReflinkRetention["b"]; got != 7 {
		t.Fatalf("reflinkRetention dropped on decode: live cap = %d, want 7 (0 keeps ZERO version copies)", got)
	}

	out := filepath.Join(t.TempDir(), "config.json")
	if err := store.Persist(out); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reflinkRetention": 7`) {
		t.Errorf("persisted document lost the per-bucket cap: %s", raw)
	}
	if reloaded := loadConfigForTest(t, out); reloaded.BucketReflinkRetention["b"] != 7 {
		t.Errorf("reloaded cap = %d, want 7", reloaded.BucketReflinkRetention["b"])
	}
}

// TestLoadConfigBucketsObjectFormKeepsReflinkRetention pins the decode fix at
// its source (config.go's bucketCfg.UnmarshalJSON), which is also reachable by
// config load, not only by PUT /config.
func TestLoadConfigBucketsObjectFormKeepsReflinkRetention(t *testing.T) {
	dir := t.TempDir()
	cfg := loadConfigForTest(t, writeTempConfig(t,
		`{"dataDir":"`+dir+`/","buckets":{"b":{"path":"`+dir+`/b","auditReads":true,"reflinkRetention":4}}}`))
	if got := cfg.BucketReflinkRetention["b"]; got != 4 {
		t.Fatalf("loaded reflinkRetention = %d, want 4", got)
	}
	if !cfg.BucketAuditReads["b"] {
		t.Error("loaded auditReads = false, want true")
	}
}

// ---- L9: Persist writes the secret-bearing file owner-only ----

// TestConfigStorePersistWritesOwnerOnlyFile pins that the persisted config
// document — which carries REAL (unmasked) identity secrets — is written
// owner-only. Masking is read-only, so the file itself is the secret store.
func TestConfigStorePersistWritesOwnerOnlyFile(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultServerConfig()
	cfg.DataDir = dir + "/"
	cfg.Identities = []auth.IdentityConfig{{
		Name: "app", AccessKey: "AKPERSIST", SecretKey: "real-secret-value",
		Grants: map[string]json.RawMessage{"*": json.RawMessage(`"readonly"`)},
	}}
	store := NewConfigStore(&cfg)
	out := filepath.Join(dir, "config.json")

	if err := store.Persist(out); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("persisted config mode = %04o, want 0600 (the file carries real identity secrets)", perm)
	}
	// Rewriting over an existing WORLD-READABLE file must tighten it: the
	// temp-file+rename path takes the new mode.
	if err := os.Chmod(out, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(out); err != nil {
		t.Fatalf("Persist (2): %v", err)
	}
	info, err = os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("rewritten config mode = %04o, want 0600", perm)
	}
}
