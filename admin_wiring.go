// admin_wiring.go — package main's wiring for the management API
// (management-api-2026-10 leaf 04, Task 2). It assembles the admin frontend's
// injected route surface from the live ConfigStore (leaf 02), the shared
// bucketmanager (leaf 03), the metadata provider and the reload path, and
// injects the exported audit seam (internal/frontend/s3.AppendAudit) so the
// admin package never imports package s3.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/bucketmanager"
	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// zfsEventsProviderName is the registry name of the zfs-events metadata
// provider (unchanged); the purge route resolves it by name.
const zfsEventsProviderName = "zfs-events"

// adminProcessStart is the process start time, the baseline for the status
// uptime report.
var adminProcessStart = time.Now()

// adminMetadataLookup is the provider lookup the status and purge routes use.
// It is a package var so tests can substitute a controlled provider.
var adminMetadataLookup = metadata.Lookup

// adminWiring is the assembled management surface: the injected route services
// and the audit append injected into the admin frontend.
type adminWiring struct {
	services admin.Services
	audit    admin.AuditFunc
}

// buildAdminWiring assembles the management route surface. audit is the
// write-only audit append (main passes s3.AppendAudit; tests pass a recorder).
func buildAdminWiring(audit admin.AuditFunc) adminWiring {
	provider := adminMetadataLookup(zfsEventsProviderName)
	if provider != nil {
		// Apply the config-driven purge binary at WIRING time (leaf 04
		// Task 2): the zmetad binary for `zmetad --purge`.
		metadata.SetZmetadBinary(provider, serverConfig.ZmetadBinary)
	}
	svc := admin.Services{
		Status:         adminStatusService,
		GetConfig:      adminGetConfigService,
		PutConfig:      adminPutConfigService,
		SaveConfig:     adminSaveConfigService,
		ReloadAuth:     adminReloadAuthService,
		ListBuckets:    adminListBucketsService,
		CreateBucket:   adminCreateBucketService,
		DeleteBucket:   adminDeleteBucketService,
		BucketDetail:   adminBucketDetailService,
		BucketSettings: adminBucketSettingsService,
		Purge:          adminPurgeService(provider),
	}
	return adminWiring{services: svc, audit: audit}
}

// adminStatusService is GET /status: version, uptime, listeners, frontends,
// backends, the restart-required keys, and an HONEST metadata-provider
// availability probe (never an invented value).
func adminStatusService(ctx context.Context) (admin.StatusReport, error) {
	snap := configStore.Snapshot()
	return admin.StatusReport{
		Version:          buildVersion(),
		Uptime:           time.Since(adminProcessStart).Round(time.Second).String(),
		Listeners:        adminListenerAddrs(snap),
		Frontends:        adminFrontendNames(snap),
		Backends:         adminBackendNames(snap),
		RestartRequired:  configStore.RestartRequired(),
		MetadataProvider: adminMetadataStatus(ctx, snap.DataDir),
	}, nil
}

// adminMetadataStatus probes the zfs-events provider for the data root and
// reports {available, reason}. An unregistered provider is reported as an
// explicit unavailable with a reason, never silently omitted.
func adminMetadataStatus(ctx context.Context, dataDir string) admin.MetadataProviderStatus {
	p := adminMetadataLookup(zfsEventsProviderName)
	if p == nil {
		return admin.MetadataProviderStatus{Available: false, Reason: "zfs-events provider is not registered"}
	}
	res, err := p.Probe(ctx, dataDir)
	if err != nil {
		return admin.MetadataProviderStatus{Available: false, Reason: err.Error()}
	}
	return admin.MetadataProviderStatus{Available: res.Available, Reason: res.Reason}
}

// adminListenerAddrs lists the default listener plus every dedicated-frontend
// listener.
func adminListenerAddrs(snap ServerConfig) []string {
	out := []string{}
	if snap.ListenAddr != "" {
		out = append(out, snap.ListenAddr)
	}
	seen := map[string]bool{}
	for _, a := range out {
		seen[a] = true
	}
	for _, fe := range snap.Frontends {
		if fe.ListenAddr == "" || seen[fe.ListenAddr] {
			continue
		}
		seen[fe.ListenAddr] = true
		out = append(out, fe.ListenAddr)
	}
	return out
}

// adminFrontendNames reports the configured frontend type names (default s3).
func adminFrontendNames(snap ServerConfig) []string {
	if len(snap.Frontends) == 0 {
		return []string{"s3"}
	}
	names := make([]string, 0, len(snap.Frontends))
	for _, fe := range snap.Frontends {
		names = append(names, fe.Type)
	}
	return names
}

// adminBackendNames reports the configured backend type names (default fs).
func adminBackendNames(snap ServerConfig) []string {
	if len(snap.Backends) == 0 {
		return []string{defaultBackendName}
	}
	names := make([]string, 0, len(snap.Backends))
	for name := range snap.Backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// adminGetConfigService is GET /config: the masked effective configuration
// plus the restart-required list. The store's Snapshot masks every secret.
func adminGetConfigService(_ context.Context) (json.RawMessage, error) {
	snap := configStore.Snapshot()
	doc, err := marshalConfigDocument(&snap)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(doc, &obj); err != nil {
		return nil, fmt.Errorf("admin: re-reading config document: %w", err)
	}
	restart, err := json.Marshal(configStore.RestartRequired())
	if err != nil {
		return nil, err
	}
	obj["restartRequired"] = restart
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// adminPutConfigService is PUT /config: a partial update through the store.
// An invalid patch is a 400 carrying the validator message and changes
// nothing (the store validates before mutating).
func adminPutConfigService(_ context.Context, patch json.RawMessage) (admin.ConfigApplyResult, error) {
	applied, restartRequired, err := configStore.Apply(ConfigPatch{JSON: patch})
	if err != nil {
		return admin.ConfigApplyResult{}, &admin.ServiceError{Status: 400, Code: "InvalidConfiguration", Message: err.Error()}
	}
	return admin.ConfigApplyResult{Applied: applied, RestartRequired: restartRequired}, nil
}

// adminSaveConfigService is POST /config/save: persist the live configuration
// to the config file atomically.
func adminSaveConfigService(_ context.Context) error {
	if err := configStore.Persist(serverConfigPath); err != nil {
		return &admin.ServiceError{Status: 500, Code: "PersistFailed", Message: err.Error()}
	}
	return nil
}

// adminReloadAuthService is POST /auth/reload: re-run the SIGHUP identity
// reload AND refresh the admin listener's trusted client CA (so a replaced CA
// file revokes old certificates without a restart).
func adminReloadAuthService(_ context.Context) error {
	idErr := reloadIdentityRegistry()
	caErr := reloadAdminClientCA()
	if idErr != nil {
		return &admin.ServiceError{Status: 500, Code: "ReloadFailed", Message: idErr.Error()}
	}
	if caErr != nil {
		return &admin.ServiceError{Status: 500, Code: "ReloadFailed", Message: caErr.Error()}
	}
	return nil
}

// adminBucketEntry is one row of GET /buckets / GET /buckets/{name}.
type adminBucketEntry struct {
	Name             string `json:"name"`
	Backend          string `json:"backend"`
	AuditReads       bool   `json:"auditReads"`
	ReflinkRetention *int   `json:"reflinkRetention,omitempty"`
	IsDataset        bool   `json:"isDataset,omitempty"`
	CreatedAt        string `json:"createdAt,omitempty"`
}

// adminListBucketsService is GET /buckets: the discovered buckets MERGED with
// the configured custom buckets (bucketmanager.List cannot see configured
// custom paths, which may live outside dataDir).
func adminListBucketsService(ctx context.Context) (json.RawMessage, error) {
	snap := configStore.Snapshot()
	infos, err := bucketmanager.List(ctx)
	if err != nil {
		return nil, adminServiceError(err)
	}
	entries := make([]adminBucketEntry, 0, len(infos)+len(snap.Buckets))
	seen := make(map[string]bool, len(infos))
	for _, in := range infos {
		e := adminBucketEntry{
			Name:             in.Name,
			Backend:          adminBucketBackend(snap, in.Name),
			AuditReads:       snap.BucketAuditReads[in.Name],
			ReflinkRetention: adminBucketRetention(snap, in.Name),
			CreatedAt:        in.CreatedAt.UTC().Format(time.RFC3339),
		}
		entries = append(entries, e)
		seen[in.Name] = true
	}
	custom := make([]string, 0, len(snap.Buckets))
	for name := range snap.Buckets {
		if !seen[name] {
			custom = append(custom, name)
		}
	}
	sort.Strings(custom)
	for _, name := range custom {
		entries = append(entries, adminBucketEntry{
			Name:             name,
			Backend:          adminBucketBackend(snap, name),
			AuditReads:       snap.BucketAuditReads[name],
			ReflinkRetention: adminBucketRetention(snap, name),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	out, err := json.Marshal(map[string]any{"buckets": entries})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// adminBucketBackend resolves a bucket's backend name (default fs).
func adminBucketBackend(snap ServerConfig, name string) string {
	if b := snap.BucketBackends[name]; b != "" {
		return b
	}
	return defaultBackendName
}

// adminBucketRetention returns a bucket's reflinkRetention cap, or nil when
// unset (server-wide default applies).
func adminBucketRetention(snap ServerConfig, name string) *int {
	if r, ok := snap.BucketReflinkRetention[name]; ok {
		v := r
		return &v
	}
	return nil
}

// adminCreateBucketService is POST /buckets: create via bucketmanager.Create.
func adminCreateBucketService(ctx context.Context, name string) error {
	return adminServiceError(bucketmanager.Create(ctx, name))
}

// adminDeleteBucketService is DELETE /buckets/{name}: plain-directory buckets
// only. A dataset-backed bucket is refused with 409 DatasetBucketNotDeletable
// naming the operator workflow; the manager is NEVER asked to destroy a
// dataset (AllowDatasetDestroy false, user decision 5).
func adminDeleteBucketService(ctx context.Context, name string) error {
	err := bucketmanager.Delete(ctx, name, bucketmanager.DeleteOptions{AllowDatasetDestroy: false})
	if err == nil {
		return nil
	}
	if errors.Is(err, bucketmanager.ErrDatasetBucketNotDeletable) {
		dataset := adminDatasetName(name)
		return &admin.ServiceError{
			Status: 409, Code: "DatasetBucketNotDeletable",
			Message: fmt.Sprintf("bucket %q is a ZFS dataset (%s); the management API does not destroy datasets. Remove it on the host with `zfs destroy %s`.", name, dataset, dataset),
		}
	}
	return adminServiceError(err)
}

// adminDatasetName renders the dataset name for a bucket when the dataset
// feature is on (parent/name), else just the bucket name.
func adminDatasetName(name string) string {
	if p := s3.NewDatasetProvisioner(); p != nil && p.Parent() != "" {
		return p.Parent() + "/" + name
	}
	return name
}

// adminBucketDetailService is GET /buckets/{name}. It reports the backend,
// tunables and whether the bucket is a dataset. It NEVER reports an object
// count: no index exists (Contract 5).
func adminBucketDetailService(ctx context.Context, name string) (json.RawMessage, error) {
	snap := configStore.Snapshot()
	exists, err := bucketmanager.Exists(ctx, name)
	if err != nil {
		return nil, adminServiceError(err)
	}
	isDataset := adminBucketIsDataset(ctx, name)
	if !exists && !isDataset {
		return nil, &admin.ServiceError{Status: 404, Code: "NoSuchBucket",
			Message: fmt.Sprintf("The specified bucket does not exist: %s", name)}
	}
	entry := adminBucketEntry{
		Name:             name,
		Backend:          adminBucketBackend(snap, name),
		AuditReads:       snap.BucketAuditReads[name],
		ReflinkRetention: adminBucketRetention(snap, name),
		IsDataset:        isDataset,
	}
	out, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// adminBucketIsDataset reports whether the bucket is a provisioned ZFS
// dataset (best-effort: the dataset feature must be on).
func adminBucketIsDataset(ctx context.Context, name string) bool {
	p := s3.NewDatasetProvisioner()
	if p == nil || p.Parent() == "" {
		return false
	}
	ok, err := p.Exists(ctx, p.Parent()+"/"+name)
	if err != nil {
		return false
	}
	return ok
}

// adminBucketSettingsService is PUT /buckets/{name}/settings: per-bucket
// auditReads and reflinkRetention through the store's hot-apply path.
func adminBucketSettingsService(_ context.Context, name string, patch json.RawMessage) error {
	snap := configStore.Snapshot()
	if _, ok := snap.Buckets[name]; !ok {
		return &admin.ServiceError{Status: 400, Code: "InvalidConfiguration",
			Message: fmt.Sprintf("bucket %q is not declared in the config buckets map; per-bucket tunables apply to configured buckets", name)}
	}
	fullPatch, err := buildBucketSettingsPatch(snap, name, patch)
	if err != nil {
		return err
	}
	if _, _, err := configStore.Apply(ConfigPatch{JSON: fullPatch}); err != nil {
		return &admin.ServiceError{Status: 400, Code: "InvalidConfiguration", Message: err.Error()}
	}
	return nil
}

// buildBucketSettingsPatch rewrites the FULL configured buckets object (so the
// store sees no layout change and hot-applies only the tunables) with the
// named bucket's new auditReads/reflinkRetention applied.
func buildBucketSettingsPatch(snap ServerConfig, name string, patch json.RawMessage) (json.RawMessage, error) {
	var p struct {
		AuditReads       *bool `json:"auditReads"`
		ReflinkRetention *int  `json:"reflinkRetention"`
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, &admin.ServiceError{Status: 400, Code: "InvalidArgument", Message: "settings body must be a JSON object: " + err.Error()}
	}
	buckets := make(map[string]map[string]any, len(snap.Buckets))
	for bn, path := range snap.Buckets {
		b := map[string]any{"path": path}
		if be := snap.BucketBackends[bn]; be != "" {
			b["backend"] = be
		}
		if snap.BucketAuditReads[bn] {
			b["auditReads"] = true
		}
		if r, ok := snap.BucketReflinkRetention[bn]; ok {
			b["reflinkRetention"] = r
		}
		buckets[bn] = b
	}
	target := buckets[name]
	if p.AuditReads != nil {
		target["auditReads"] = *p.AuditReads
	}
	if p.ReflinkRetention != nil {
		target["reflinkRetention"] = *p.ReflinkRetention
	}
	buckets[name] = target
	out, err := json.Marshal(map[string]any{"buckets": buckets})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// adminPurgeService is POST /purge: the metadata history purge for the named
// dataset. It is the only irreversibly destructive route (it clears the event
// history AND the permanent gap/loss record). The provider seam takes a bucket
// path, so the named dataset is passed through unchanged to provider.Purge.
func adminPurgeService(provider metadata.MetadataProvider) func(ctx context.Context, dataset string) error {
	return func(ctx context.Context, dataset string) error {
		if provider == nil {
			return &admin.ServiceError{Status: 503, Code: "MetadataProviderUnavailable",
				Message: "the zfs-events metadata provider is not registered"}
		}
		metadata.SetZmetadBinary(provider, serverConfig.ZmetadBinary)
		if err := provider.Purge(ctx, dataset); err != nil {
			return &admin.ServiceError{Status: 500, Code: "PurgeFailed", Message: err.Error()}
		}
		return nil
	}
}

// adminServiceError maps a bucketmanager/objectmodel error onto the admin JSON
// envelope (its code and canonical status); a non-taxonomy error is a generic
// 500.
func adminServiceError(err error) error {
	if err == nil {
		return nil
	}
	if oe, ok := errors.AsType[*objectmodel.Error](err); ok {
		status := oe.HTTPStatus
		if status == 0 {
			status = 500
		}
		return &admin.ServiceError{Status: status, Code: oe.Code, Message: oe.Message}
	}
	return &admin.ServiceError{Status: 500, Code: "InternalError", Message: err.Error()}
}

// buildVersion reports the module version from build info (devel when built
// from a worktree).
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "devel"
}

// admin client-CA reload registration: the admin frontend is constructed
// inside the frontends factory, so the factory registers its CA-reload entry
// point here for the /auth/reload service to reach.
var (
	adminCAMu     sync.Mutex
	adminCAReload func() error
)

// setAdminClientCAReloader records the constructed admin frontend's CA-reload
// entry point (nil-safe).
func setAdminClientCAReloader(fn func() error) {
	adminCAMu.Lock()
	defer adminCAMu.Unlock()
	adminCAReload = fn
}

// reloadAdminClientCA refreshes the admin listener's trusted client CA (a
// no-op when no admin frontend is built).
func reloadAdminClientCA() error {
	adminCAMu.Lock()
	fn := adminCAReload
	adminCAMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}
