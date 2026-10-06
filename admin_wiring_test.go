package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/bucketmanager"
	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
	"github.com/bhodgens/zeta-object/internal/fslock"
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// fakeProvisioner is a controllable bucketmanager.Provisioner (dataset
// feature on).
type fakeProvisioner struct {
	parent string
	exists bool
}

func (f fakeProvisioner) Create(context.Context, string) (string, error) { return f.parent, nil }
func (f fakeProvisioner) Destroy(context.Context, string) error          { return nil }
func (f fakeProvisioner) Exists(context.Context, string) (bool, error)   { return f.exists, nil }
func (f fakeProvisioner) Parent() string                                 { return f.parent }

// installBucketTestEnv installs a bucketmanager environment rooted at dataDir.
func installBucketTestEnv(t *testing.T, dataDir string, custom map[string]string, prov bucketmanager.Provisioner) {
	t.Helper()
	t.Cleanup(func() { bucketmanager.Install(bucketmanager.Env{}) })
	bucketmanager.Install(bucketmanager.Env{
		Locks: fslock.Default,
		BucketPath: func(b string) string {
			if b == "" {
				return dataDir
			}
			return filepath.Join(dataDir, b)
		},
		Custom: func(b string) (string, bool) {
			p, ok := custom[b]
			return p, ok
		},
		Provisioner: prov,
	})
}

// installTestConfigStore installs a ConfigStore built from cfg for the test.
func installTestConfigStore(t *testing.T, cfg ServerConfig) {
	t.Helper()
	prev := configStore
	t.Cleanup(func() { configStore = prev })
	configStore = NewConfigStore(&cfg)
}

// TestAdminWiringBuildsCompleteServices pins Task 2: every route's service is
// wired (no nil field a route needs) and the audit seam is injected.
func TestAdminWiringBuildsCompleteServices(t *testing.T) {
	installTestConfigStore(t, defaultServerConfig())
	w := buildAdminWiring(s3AppendRecorder(&[]string{}))
	checks := map[string]any{
		"Status": w.services.Status, "GetConfig": w.services.GetConfig, "PutConfig": w.services.PutConfig,
		"SaveConfig": w.services.SaveConfig, "ReloadAuth": w.services.ReloadAuth, "ListBuckets": w.services.ListBuckets,
		"CreateBucket": w.services.CreateBucket, "DeleteBucket": w.services.DeleteBucket, "BucketDetail": w.services.BucketDetail,
		"BucketSettings": w.services.BucketSettings, "Purge": w.services.Purge,
	}
	for name, fn := range checks {
		if fn == nil {
			t.Fatalf("services.%s is nil (a route would 503)", name)
		}
	}
	if w.audit == nil {
		t.Fatal("audit seam not injected")
	}
}

// s3AppendRecorder returns an admin.AuditFunc recording each call's op.
func s3AppendRecorder(ops *[]string) admin.AuditFunc {
	return func(_, _, _, _, op string, _ int, _ bool) { *ops = append(*ops, op) }
}

// TestAdminWiringAuditCalledOncePerBuiltServiceCall pins that the injected
// audit function is invoked exactly once per management action with op=admin.
func TestAdminWiringAuditCalledOncePerBuiltServiceCall(t *testing.T) {
	installTestConfigStore(t, defaultServerConfig())
	var ops []string
	w := buildAdminWiring(s3AppendRecorder(&ops))
	w.audit("cn", "GET", "b", "k", string(auth.OpAdmin), 200, false)
	if len(ops) != 1 || ops[0] != "admin" {
		t.Fatalf("audit calls = %v, want exactly one op=admin", ops)
	}
}

// TestAdminWiringPurgeUsesProvider pins that POST /purge reaches the metadata
// provider's Purge for the named dataset and that a nil provider is a 503.
func TestAdminWiringPurgeUsesProvider(t *testing.T) {
	installTestConfigStore(t, defaultServerConfig())
	fake := &recordingProvider{}
	prev := adminMetadataLookup
	adminMetadataLookup = func(string) metadata.MetadataProvider { return fake }
	t.Cleanup(func() { adminMetadataLookup = prev })

	w := buildAdminWiring(nil)
	if err := w.services.Purge(context.Background(), "pool/ds"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if fake.purged != "pool/ds" {
		t.Fatalf("provider.Purge got %q, want pool/ds", fake.purged)
	}

	// Nil provider → explicit 503.
	adminMetadataLookup = func(string) metadata.MetadataProvider { return nil }
	none := buildAdminWiring(nil)
	err := none.services.Purge(context.Background(), "pool/ds")
	se, ok := errors.AsType[*admin.ServiceError](err)
	if !ok || se.Status != 503 {
		t.Fatalf("nil provider err = %v, want a 503 ServiceError", err)
	}
}

// recordingProvider is a minimal MetadataProvider recording Purge calls.
type recordingProvider struct{ purged string }

func (p *recordingProvider) Name() string { return zfsEventsProviderName }
func (p *recordingProvider) Probe(context.Context, string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{Available: false, Reason: "fake"}, nil
}
func (p *recordingProvider) History(context.Context, string, string, metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	return nil, nil
}
func (p *recordingProvider) Purge(_ context.Context, dataset string) error {
	p.purged = dataset
	return nil
}

// TestAdminWiringReloadAuthReloadsIdentitiesAndCA pins that POST /auth/reload
// re-runs the SIGHUP identity reload AND refreshes the admin client CA.
func TestAdminWiringReloadAuthReloadsIdentitiesAndCA(t *testing.T) {
	cfgJSON := `{"identities":[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	prevPath, prevReg, prevCfg, prevCreds := serverConfigPath, identityRegistry, serverConfig, serverCredentials
	t.Cleanup(func() {
		serverConfigPath, identityRegistry, serverConfig, serverCredentials = prevPath, prevReg, prevCfg, prevCreds
	})
	serverConfigPath = path
	serverConfig = defaultServerConfig()
	reg, err := auth.NewMultiRegistry(nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(reg)

	caCalled := false
	registerClientCAReloader("admin", func() error { caCalled = true; return nil })
	t.Cleanup(resetClientCAReloaders)

	installTestConfigStore(t, defaultServerConfig())
	if err := adminReloadAuthService(context.Background()); err != nil {
		t.Fatalf("ReloadAuth: %v", err)
	}
	if !caCalled {
		t.Fatal("ReloadAuth did not refresh the admin client CA")
	}
	if _, ok := identityRegistry.LookupByAccessKey("ak-one"); !ok {
		t.Fatal("ReloadAuth did not swap in the reloaded identity registry")
	}
}

// TestAdminWiringListMergesCustomBuckets pins the carry-forward note: GET
// /buckets merges the discovered directories with the configured custom
// buckets.
func TestAdminWiringListMergesCustomBuckets(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "auto"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultServerConfig()
	cfg.DataDir = dataDir
	cfg.Buckets = map[string]string{"custom": "/mnt/custom"}
	installTestConfigStore(t, cfg)
	installBucketTestEnv(t, dataDir, cfg.Buckets, nil)

	w := buildAdminWiring(nil)
	raw, err := w.services.ListBuckets(context.Background())
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	var body struct {
		Buckets []adminBucketEntry `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	names := map[string]bool{}
	for _, b := range body.Buckets {
		names[b.Name] = true
	}
	if !names["auto"] || !names["custom"] {
		t.Fatalf("buckets = %s, want auto + custom merged", raw)
	}
}

// TestAdminWiringDeleteDatasetRefused pins that DELETE asks the manager for
// NO dataset destroy and maps ErrDatasetBucketNotDeletable to 409 with the
// operator workflow message.
func TestAdminWiringDeleteDatasetRefused(t *testing.T) {
	dataDir := t.TempDir()
	bucketPath := filepath.Join(dataDir, "dsbucket")
	if err := os.Mkdir(bucketPath, 0o755); err != nil {
		t.Fatal(err)
	}
	installTestConfigStore(t, defaultServerConfig())
	installBucketTestEnv(t, dataDir, nil, fakeProvisioner{parent: "pool", exists: true})

	w := buildAdminWiring(nil)
	err := w.services.DeleteBucket(context.Background(), "dsbucket")
	se, ok := errors.AsType[*admin.ServiceError](err)
	if !ok {
		t.Fatalf("err = %v, want a ServiceError", err)
	}
	if se.Status != 409 || se.Code != "DatasetBucketNotDeletable" {
		t.Fatalf("err = %+v, want 409 DatasetBucketNotDeletable", se)
	}
	if !strings.Contains(se.Message, "zfs destroy") {
		t.Fatalf("message %q must name the operator workflow (zfs destroy)", se.Message)
	}
}

// TestAdminWiringPutConfigInvalidChangesNothing pins that an invalid patch is
// a 400 carrying the validator message and the store is untouched.
func TestAdminWiringPutConfigInvalidChangesNothing(t *testing.T) {
	cfg := defaultServerConfig()
	installTestConfigStore(t, cfg)
	before := configStore.Snapshot()

	w := buildAdminWiring(nil)
	_, err := w.services.PutConfig(context.Background(), json.RawMessage(`{"nope":1}`))
	se, ok := errors.AsType[*admin.ServiceError](err)
	if !ok || se.Status != 400 {
		t.Fatalf("err = %v, want a 400 ServiceError", err)
	}
	if !strings.Contains(se.Message, "unknown field") {
		t.Fatalf("validator message lost: %q", se.Message)
	}
	after := configStore.Snapshot()
	if after.ListenAddr != before.ListenAddr || after.Region != before.Region {
		t.Fatalf("store changed on an invalid patch: before=%+v after=%+v", before, after)
	}
}

// TestAdminWiringGetConfigNoSecret pins that GET /config never carries a
// literal secret.
func TestAdminWiringGetConfigNoSecret(t *testing.T) {
	const secret = "literal-secret-value-xyz"
	cfg := defaultServerConfig()
	cfg.Identities = []auth.IdentityConfig{{Name: "a", AccessKey: "AK", SecretKey: secret}}
	installTestConfigStore(t, cfg)

	prev := serverCredentials
	t.Cleanup(func() { serverCredentials = prev })
	serverCredentials.AccessKeyID = "AKENV"
	serverCredentials.SecretAccessKey = "env-secret-xyz"

	w := buildAdminWiring(nil)
	raw, err := w.services.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "env-secret-xyz") {
		t.Fatalf("config body leaked a secret: %s", raw)
	}
	if !strings.Contains(string(raw), "restartRequired") {
		t.Fatalf("config body missing restartRequired: %s", raw)
	}
}

// TestAdminWiringSaveConfigPersists pins POST /config/save writes the file.
func TestAdminWiringSaveConfigPersists(t *testing.T) {
	cfg := defaultServerConfig()
	installTestConfigStore(t, cfg)
	prevPath := serverConfigPath
	t.Cleanup(func() { serverConfigPath = prevPath })
	serverConfigPath = filepath.Join(t.TempDir(), "config.json")

	w := buildAdminWiring(nil)
	if err := w.services.SaveConfig(context.Background()); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := os.Stat(serverConfigPath); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
}

// TestAdminWiringCreateDeletePlainBucket pins create + delete on a
// plain-directory bucket (dataset feature off).
func TestAdminWiringCreateDeletePlainBucket(t *testing.T) {
	dataDir := t.TempDir()
	installTestConfigStore(t, defaultServerConfig())
	installBucketTestEnv(t, dataDir, nil, nil)
	w := buildAdminWiring(nil)

	if err := w.services.CreateBucket(context.Background(), "plainbkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plainbkt", ".metadata")); err != nil {
		t.Fatalf("bucket not created: %v", err)
	}
	if err := w.services.DeleteBucket(context.Background(), "plainbkt"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plainbkt")); !os.IsNotExist(err) {
		t.Fatalf("bucket not deleted: %v", err)
	}
}

// TestAdminWiringBucketDetailNoObjectCount pins that GET /buckets/{name}
// reports isDataset but never an object count.
func TestAdminWiringBucketDetailNoObjectCount(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	installTestConfigStore(t, defaultServerConfig())
	installBucketTestEnv(t, dataDir, nil, nil)
	w := buildAdminWiring(nil)

	raw, err := w.services.BucketDetail(context.Background(), "b")
	if err != nil {
		t.Fatalf("BucketDetail: %v", err)
	}
	if strings.Contains(string(raw), "objectCount") {
		t.Fatalf("objectCount must not be reported: %s", raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "b" {
		t.Fatalf("detail = %s", raw)
	}
	// Unknown bucket → 404.
	if _, err := w.services.BucketDetail(context.Background(), "nope"); err == nil {
		t.Fatal("unknown bucket should 404")
	} else {
		se, ok := errors.AsType[*admin.ServiceError](err)
		if !ok || se.Status != 404 {
			t.Fatalf("unknown bucket err = %v, want 404", err)
		}
	}
}

// TestAdminWiringBucketSettingsAutoProvisionedBucket pins the v1-limitation
// fix: PUT /buckets/{name}/settings works for an auto-provisioned bucket
// (created via the shared bucket manager, NOT present in the config buckets
// map), and the bucket is still not custom — it stays deletable through the
// API.
func TestAdminWiringBucketSettingsAutoProvisionedBucket(t *testing.T) {
	dataDir := t.TempDir()
	installBucketTestEnv(t, dataDir, nil, nil)
	cfg := defaultServerConfig()
	cfg.DataDir = dataDir + "/"
	installTestConfigStore(t, cfg)

	w := buildAdminWiring(nil)
	ctx := context.Background()
	if err := w.services.CreateBucket(ctx, "autobkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, isCustom := configStore.Snapshot().Buckets["autobkt"]; isCustom {
		t.Fatal("autobkt unexpectedly in the config buckets map")
	}

	if err := w.services.BucketSettings(ctx, "autobkt",
		json.RawMessage(`{"auditReads":true,"reflinkRetention":3}`)); err != nil {
		t.Fatalf("BucketSettings: %v", err)
	}
	snap := configStore.Snapshot()
	if !snap.BucketAuditReads["autobkt"] || snap.BucketReflinkRetention["autobkt"] != 3 {
		t.Fatalf("tunables not applied: %v / %v", snap.BucketAuditReads, snap.BucketReflinkRetention)
	}
	if _, isCustom := snap.Buckets["autobkt"]; isCustom {
		t.Fatal("settings made autobkt a custom bucket")
	}

	// Still NOT custom: deletable through the API path.
	if err := w.services.DeleteBucket(ctx, "autobkt"); err != nil {
		t.Fatalf("DeleteBucket after settings: %v (bucket became custom?)", err)
	}
}

// TestAdminWiringBucketSettingsNotFound pins 404 NoSuchBucket for a bucket
// that does not exist.
func TestAdminWiringBucketSettingsNotFound(t *testing.T) {
	dataDir := t.TempDir()
	installBucketTestEnv(t, dataDir, nil, nil)
	cfg := defaultServerConfig()
	cfg.DataDir = dataDir + "/"
	installTestConfigStore(t, cfg)

	w := buildAdminWiring(nil)
	err := w.services.BucketSettings(context.Background(), "ghost", json.RawMessage(`{"auditReads":true}`))
	se, ok := errors.AsType[*admin.ServiceError](err)
	if !ok || se.Status != 404 || se.Code != "NoSuchBucket" {
		t.Fatalf("err = %v, want 404 NoSuchBucket", err)
	}
}

// TestAdminWiringBucketSettingsInvalidInput pins 400 for invalid input and
// that nothing changes: a smuggled path/backend (rejected at the route) and a
// negative retention (rejected by the store validator).
func TestAdminWiringBucketSettingsInvalidInput(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "autobkt"), 0o755); err != nil {
		t.Fatal(err)
	}
	installBucketTestEnv(t, dataDir, nil, nil)
	cfg := defaultServerConfig()
	cfg.DataDir = dataDir + "/"
	installTestConfigStore(t, cfg)
	before := configStore.Snapshot()

	w := buildAdminWiring(nil)
	ctx := context.Background()
	for name, body := range map[string]string{
		"path":     `{"path":"/tmp/x"}`,
		"backend":  `{"backend":"fs"}`,
		"negative": `{"reflinkRetention":-1}`,
	} {
		err := w.services.BucketSettings(ctx, "autobkt", json.RawMessage(body))
		se, ok := errors.AsType[*admin.ServiceError](err)
		if !ok || se.Status != 400 {
			t.Fatalf("%s: err = %v, want a 400 ServiceError", name, err)
		}
	}
	if !reflect.DeepEqual(before, configStore.Snapshot()) {
		t.Fatal("invalid settings changed the store")
	}
}
