// admin_wiring_coverage_test.go — direct behaviour coverage for the
// management-console status assembly and the error-mapping seam added by the
// admin-server tree (admin_wiring.go). These functions only need stubs: a
// controllable metadata provider (nil / probe-error / available), listener /
// frontend / backend sets, and a table of error kinds.
package main

import (
	"context"
	"errors"
	"testing"

	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// stubMetaProvider is a controllable MetadataProvider for the status probe
// branches (the shared recordingProvider in admin_wiring_test.go has a fixed
// Probe result).
type stubMetaProvider struct {
	probe    metadata.ProbeResult
	probeErr error
}

func (p *stubMetaProvider) Name() string { return zfsEventsProviderName }
func (p *stubMetaProvider) Probe(context.Context, string) (metadata.ProbeResult, error) {
	return p.probe, p.probeErr
}
func (p *stubMetaProvider) History(context.Context, string, string, metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	return nil, nil
}
func (p *stubMetaProvider) Purge(context.Context, string) error { return nil }

// withMetadataLookup swaps the package-level provider lookup for the test and
// restores the previous one on cleanup.
func withMetadataLookup(t *testing.T, p metadata.MetadataProvider) {
	t.Helper()
	prev := adminMetadataLookup
	adminMetadataLookup = func(string) metadata.MetadataProvider { return p }
	t.Cleanup(func() { adminMetadataLookup = prev })
}

// TestAdminStatusServiceAssemblesLiveState drives GET /status end to end: the
// listener/frontend/backend sets and the honest metadata-provider probe are
// assembled from the installed ConfigStore.
func TestAdminStatusServiceAssemblesLiveState(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.ListenAddr = ":8443"
	cfg.Frontends = []FrontendConfig{
		{Type: "webdav", ListenAddr: ":8444"},
		{Type: "webdav", ListenAddr: ":8444"}, // duplicate addr is collapsed
		{Type: "sftp"},                        // no dedicated listener
	}
	cfg.Backends = map[string]BackendCfg{"zb": {}, "fs": {}}
	installTestConfigStore(t, cfg)
	withMetadataLookup(t, &stubMetaProvider{probe: metadata.ProbeResult{Available: true, Reason: "events on"}})

	rep, err := adminStatusService(context.Background())
	if err != nil {
		t.Fatalf("adminStatusService: %v", err)
	}
	if rep.Version == "" || rep.Version == "(devel)" {
		t.Errorf("Version = %q, want a non-empty non-(devel) value", rep.Version)
	}
	if rep.Uptime == "" {
		t.Error("Uptime is empty")
	}
	if want := []string{":8443", ":8444"}; !equalStrings(rep.Listeners, want) {
		t.Errorf("Listeners = %v, want %v", rep.Listeners, want)
	}
	if want := []string{"webdav", "webdav", "sftp"}; !equalStrings(rep.Frontends, want) {
		t.Errorf("Frontends = %v, want %v", rep.Frontends, want)
	}
	if want := []string{"fs", "zb"}; !equalStrings(rep.Backends, want) {
		t.Errorf("Backends = %v, want %v", rep.Backends, want)
	}
	if !rep.MetadataProvider.Available || rep.MetadataProvider.Reason != "events on" {
		t.Errorf("MetadataProvider = %+v, want available with the probe reason", rep.MetadataProvider)
	}
}

// TestAdminMetadataStatusBranches pins the three probe outcomes: unregistered
// provider, provider probe error, and an honest probe result.
func TestAdminMetadataStatusBranches(t *testing.T) {
	tests := []struct {
		name       string
		provider   metadata.MetadataProvider
		wantAvail  bool
		wantReason string
	}{
		{
			name:       "nil provider is explicitly unavailable",
			provider:   nil,
			wantAvail:  false,
			wantReason: "zfs-events provider is not registered",
		},
		{
			name:       "probe error surfaces the error text",
			provider:   &stubMetaProvider{probeErr: errors.New("zpool busy")},
			wantAvail:  false,
			wantReason: "zpool busy",
		},
		{
			name:       "available probe passes through",
			provider:   &stubMetaProvider{probe: metadata.ProbeResult{Available: true, Reason: "ok"}},
			wantAvail:  true,
			wantReason: "ok",
		},
		{
			name:       "unavailable probe passes through",
			provider:   &stubMetaProvider{probe: metadata.ProbeResult{Available: false, Reason: "events off"}},
			wantAvail:  false,
			wantReason: "events off",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMetadataLookup(t, tt.provider)
			got := adminMetadataStatus(context.Background(), "/data")
			if got.Available != tt.wantAvail || got.Reason != tt.wantReason {
				t.Fatalf("adminMetadataStatus = %+v, want {Available:%v Reason:%q}", got, tt.wantAvail, tt.wantReason)
			}
		})
	}
}

// TestAdminListenerAddrs table-tests the default-plus-dedicated listener set:
// an empty address is skipped, duplicates across frontends collapse, and the
// default listener is always first.
func TestAdminListenerAddrs(t *testing.T) {
	tests := []struct {
		name string
		snap ServerConfig
		want []string
	}{
		{
			name: "empty config yields no listeners",
			snap: ServerConfig{},
			want: nil,
		},
		{
			name: "default listener only",
			snap: ServerConfig{ListenAddr: ":9000"},
			want: []string{":9000"},
		},
		{
			name: "frontend addr equal to default is not repeated",
			snap: ServerConfig{
				ListenAddr: ":9000",
				Frontends:  []FrontendConfig{{Type: "s3", ListenAddr: ":9000"}},
			},
			want: []string{":9000"},
		},
		{
			name: "empty frontend addr skipped, distinct deduped",
			snap: ServerConfig{
				ListenAddr: ":9000",
				Frontends: []FrontendConfig{
					{Type: "sftp"},
					{Type: "webdav", ListenAddr: ":9001"},
					{Type: "webdav", ListenAddr: ":9001"},
					{Type: "ftp", ListenAddr: ":9002"},
				},
			},
			want: []string{":9000", ":9001", ":9002"},
		},
		{
			name: "no default listener still lists frontend addrs",
			snap: ServerConfig{Frontends: []FrontendConfig{{Type: "webdav", ListenAddr: ":9443"}}},
			want: []string{":9443"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adminListenerAddrs(tt.snap)
			if len(got) != len(tt.want) {
				t.Fatalf("adminListenerAddrs = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("adminListenerAddrs = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestAdminFrontendNames table-tests the default-s3 fallback and the
// configured type list.
func TestAdminFrontendNames(t *testing.T) {
	tests := []struct {
		name string
		snap ServerConfig
		want []string
	}{
		{name: "no frontends defaults to s3", snap: ServerConfig{}, want: []string{"s3"}},
		{
			name: "configured types reported in order",
			snap: ServerConfig{Frontends: []FrontendConfig{{Type: "webdav"}, {Type: "s3"}}},
			want: []string{"webdav", "s3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := adminFrontendNames(tt.snap); !equalStrings(got, tt.want) {
				t.Fatalf("adminFrontendNames = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAdminBackendNames table-tests the default-fs fallback and the sorted
// configured backend-name list.
func TestAdminBackendNames(t *testing.T) {
	tests := []struct {
		name string
		snap ServerConfig
		want []string
	}{
		{name: "no backends defaults to fs", snap: ServerConfig{}, want: []string{defaultBackendName}},
		{
			name: "configured names sorted",
			snap: ServerConfig{Backends: map[string]BackendCfg{"zeta": {}, "alpha": {}, "fs": {}}},
			want: []string{"alpha", "fs", "zeta"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := adminBackendNames(tt.snap); !equalStrings(got, tt.want) {
				t.Fatalf("adminBackendNames = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAdminBucketRetention pins the unset->nil vs configured->pointer contract
// (nil means the server-wide default applies).
func TestAdminBucketRetention(t *testing.T) {
	snap := ServerConfig{BucketReflinkRetention: map[string]int{"capped": 5}}
	if got := adminBucketRetention(snap, "capped"); got == nil || *got != 5 {
		t.Errorf("adminBucketRetention(capped) = %v, want pointer to 5", got)
	}
	if got := adminBucketRetention(snap, "unset"); got != nil {
		t.Errorf("adminBucketRetention(unset) = %v, want nil", got)
	}
	if got := adminBucketRetention(ServerConfig{}, "anything"); got != nil {
		t.Errorf("adminBucketRetention with nil map = %v, want nil", got)
	}
}

// TestAdminServiceErrorMapping table-tests the error taxonomy mapping onto the
// admin envelope: nil stays nil, an *objectmodel.Error keeps its code and
// canonical status (0 defaults to 500), and anything else is a generic 500.
func TestAdminServiceErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantNil    bool
		wantStatus int
		wantCode   string
	}{
		{name: "nil stays nil", err: nil, wantNil: true},
		{
			name:       "taxonomy error keeps code and status",
			err:        &objectmodel.Error{Code: "NoSuchBucket", Message: "gone", HTTPStatus: 404},
			wantStatus: 404,
			wantCode:   "NoSuchBucket",
		},
		{
			name:       "taxonomy error with zero status defaults to 500",
			err:        &objectmodel.Error{Code: "InternalError", Message: "boom", HTTPStatus: 0},
			wantStatus: 500,
			wantCode:   "InternalError",
		},
		{
			name:       "non-taxonomy error is a generic 500",
			err:        errors.New("disk on fire"),
			wantStatus: 500,
			wantCode:   "InternalError",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adminServiceError(tt.err)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("adminServiceError(nil) = %v, want nil", got)
				}
				return
			}
			se, ok := errors.AsType[*admin.ServiceError](got)
			if !ok {
				t.Fatalf("adminServiceError = %T, want *admin.ServiceError", got)
			}
			if se.Status != tt.wantStatus || se.Code != tt.wantCode {
				t.Fatalf("ServiceError = {Status:%d Code:%q}, want {Status:%d Code:%q}", se.Status, se.Code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

// TestBuildVersionNeverEmpty pins the version fallback: a test binary carries
// no release version, so buildVersion must report "devel" (never an empty or
// "(devel)" literal leaked from build info).
func TestBuildVersionNeverEmpty(t *testing.T) {
	if got := buildVersion(); got != "devel" {
		t.Fatalf("buildVersion() = %q, want %q for a (devel) test build", got, "devel")
	}
	// It must never return the raw "(devel)" build-info sentinel.
	if buildVersion() == "(devel)" {
		t.Fatal("buildVersion returned the raw (devel) sentinel")
	}
}

// equalStrings compares two string slices element-wise (nil and empty are
// treated as equal).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
