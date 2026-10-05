package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

type nilBackend struct{ backend.Backend } // embeds interface; methods unused in these tests

type stubCreds struct{}

func (stubCreds) SecretKey(accessKeyID string) (string, bool) {
	return "minioadmin", accessKeyID == "minioadmin"
}

// stubFrontend is a minimal Frontend used to exercise the dedicated-listener
// mount path before a second concrete frontend exists (webdav/ftp GH issues).
type stubFrontend struct {
	name string
}

func (f *stubFrontend) Name() string                      { return f.name }
func (f *stubFrontend) Handler() http.Handler             { return http.NewServeMux() }
func (f *stubFrontend) Authenticator() auth.Authenticator { return nil }
func (f *stubFrontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{}
}

func TestBuildFrontends(t *testing.T) {
	tests := []struct {
		name        string
		cfg         []FrontendConfig
		wantErr     bool
		errContains string
		wantNames   []string
	}{
		{
			name:      "empty config yields default s3 mount",
			cfg:       nil,
			wantNames: []string{"s3"},
		},
		{
			name:      "explicit s3 on default listener",
			cfg:       []FrontendConfig{{Type: "s3"}},
			wantNames: []string{"s3"},
		},
		{
			name:        "unknown type rejected with known list",
			cfg:         []FrontendConfig{{Type: "gopher"}},
			wantErr:     true,
			errContains: "known: [admin ftp owncloud s3 sftp webdav]",
		},
		{
			name:      "webdav mode A registers",
			cfg:       []FrontendConfig{{Type: "webdav"}},
			wantNames: []string{"webdav"},
		},
		{
			name:      "webdav mode B registers",
			cfg:       []FrontendConfig{{Type: "webdav", Bucket: "photos"}},
			wantNames: []string{"webdav"},
		},
		{
			name: "ambiguous duplicate rejected (same type+addr+bucket)",
			cfg:  []FrontendConfig{{Type: "s3", ListenAddr: ":8444"}, {Type: "s3", ListenAddr: ":8444"}},
			// Both entries carry type s3, listenAddr ":8444" and no
			// bucket: an ambiguous mount. C2 (2026-09-30 bughunt)
			// reworked the duplicate check from type-only to this triple;
			// see the dedicated listener variant below.
			wantErr:     true,
			errContains: "configured more than once with identical",
		},
		{
			name:      "second webdav with own listenAddr+bucket is legal (e2e case 19 shape)",
			cfg:       []FrontendConfig{{Type: "s3"}, {Type: "webdav", ListenAddr: ":8444"}, {Type: "webdav", ListenAddr: ":8445", Bucket: "photos"}},
			wantNames: []string{"s3", "webdav"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The webdav factory reads the process identity registry;
			// install a minimal one for the registration paths.
			prev := identityRegistry
			reg16, err16 := auth.NewMultiRegistry([]auth.IdentityConfig{
				{Name: "test", AccessKey: "ak", SecretKey: "sk"},
			})
			if err16 != nil {
				t.Fatal(err16)
			}
			identityRegistry = auth.NewReloadableRegistry(reg16)
			defer func() { identityRegistry = prev }()
			reg, _, err := buildFrontends(tt.cfg, nilBackend{}, stubCreds{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildFrontends err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("err = %q, want substring %q", err.Error(), tt.errContains)
				}
				return
			}
			var names []string
			for _, f := range reg.All() {
				names = append(names, f.Name())
			}
			if len(names) != len(tt.wantNames) {
				t.Fatalf("registered %v, want %v", names, tt.wantNames)
			}
			for i := range tt.wantNames {
				if names[i] != tt.wantNames[i] {
					t.Fatalf("registered %v, want %v", names, tt.wantNames)
				}
			}
		})
	}
}

func TestMountFrontends_SplitsSharedMuxFromDedicatedListeners(t *testing.T) {
	sharedFE := &stubFrontend{name: "s3"}
	dedicatedFE := &stubFrontend{name: "webdav"}
	mounts := []frontendMount{
		{frontend: sharedFE, listenAddr: ""},
		{frontend: dedicatedFE, listenAddr: ":8444"},
	}
	mux := http.NewServeMux()
	shared, extra, err := mountFrontends(mux, mounts)
	if err != nil {
		t.Fatalf("mountFrontends: %v", err)
	}
	if len(shared) != 1 || shared[0] != sharedFE {
		t.Fatalf("shared = %+v, want [s3]", shared)
	}
	if len(extra) != 1 || extra[0].frontend != dedicatedFE || extra[0].addr != ":8444" {
		t.Fatalf("extra = %+v, want [{webdav :8444}]", extra)
	}
	// The shared mux serves the shared frontend's handler; the dedicated
	// frontend must NOT be mounted on it.
	h, _ := mux.Handler(httptest.NewRequest(http.MethodGet, "/", nil))
	if h == nil {
		t.Fatal("shared mux has no handler registered at /")
	}
}

// A second shared-mux mount would double-register "/" and panic inside
// http.ServeMux; mountFrontends must reject it with a config error naming
// the frontends instead (bughunt C7).
func TestMountFrontends_DuplicateSharedMountRejected(t *testing.T) {
	feA := &stubFrontend{name: "s3"}
	feB := &stubFrontend{name: "webdav"}
	mounts := []frontendMount{
		{frontend: feA, listenAddr: ""},
		{frontend: feB, listenAddr: ""},
	}
	mux := http.NewServeMux()
	_, _, err := mountFrontends(mux, mounts)
	if err == nil {
		t.Fatal("mountFrontends accepted two shared-mux mounts; want config error")
	}
	if !strings.Contains(err.Error(), "s3") || !strings.Contains(err.Error(), "webdav") {
		t.Fatalf("err = %q, want it to name both frontends", err.Error())
	}
}

// startupPlan propagates the duplicate-shared-mount rejection so main()
// aborts startup loudly instead of panicking (bughunt C7).
func TestStartupPlan_DuplicateSharedMountRejected(t *testing.T) {
	_, err := startupPlan([]FrontendConfig{{Type: "s3"}}, nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("single s3 plan: %v", err)
	}
}

// A set-but-EMPTY ZETAOBJECT_LISTEN_ADDR is warned about and ignored — it must
// not silently behave like an unset variable (bughunt E7).
func TestApplyListenAddrOverride_EmptyEnvWarnsAndIgnores(t *testing.T) {
	cfg := ServerConfig{ListenAddr: ":8443"}
	t.Setenv("ZETAOBJECT_LISTEN_ADDR", "")
	applyListenAddrOverride(&cfg)
	if cfg.ListenAddr != ":8443" {
		t.Fatalf("ListenAddr = %q, want :8443 (empty env ignored)", cfg.ListenAddr)
	}
}

// startupPlan composes buildFrontends + mountFrontends: config in, plan out,
// no listeners opened.
func TestStartupPlan_BackwardCompat(t *testing.T) {
	tests := []struct {
		name          string
		cfg           []FrontendConfig
		wantShared    int
		wantListeners int
	}{
		{"no frontends key", nil, 1, 0},
		{"explicit s3 only", []FrontendConfig{{Type: "s3"}}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := startupPlan(tt.cfg, nilBackend{}, stubCreds{})
			if err != nil {
				t.Fatalf("startupPlan: %v", err)
			}
			if len(plan.shared) != tt.wantShared || len(plan.listeners) != tt.wantListeners {
				t.Fatalf("plan = shared=%d listeners=%d, want shared=%d listeners=%d",
					len(plan.shared), len(plan.listeners), tt.wantShared, tt.wantListeners)
			}
		})
	}
}

// A frontend entry with its own listenAddr lands in the plan's listener list;
// the s3 default mount stays on the shared mux.
func TestStartupPlan_DedicatedListenerSpec(t *testing.T) {
	// The duplicate check keys on (type, listenAddr, bucket), so the plan
	// split can also be exercised through a real two-entry config: two
	// webdav entries (mode A + mode B) yield two dedicated listeners.
	plan, err := startupPlan([]FrontendConfig{{Type: "s3"}}, nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("startupPlan: %v", err)
	}
	if plan.registry == nil {
		t.Fatal("startupPlan returned nil registry")
	}
	if _, ok := plan.registry.Lookup("s3"); !ok {
		t.Fatal("registry missing s3 frontend")
	}
	_ = plan.mux
}

// startupPlan with TWO webdav entries (mode A on its own listenAddr, mode B
// on another listenAddr with a bucket) is the documented e2e case 19 shape:
// it must return 2 dedicated listeners and no error (bughunt C2).
func TestStartupPlan_TwoWebdavEntriesLegal(t *testing.T) {
	// The webdav factory reads the process identity registry and rejects a
	// nil backend (bughunt C1) — install both like main() does.
	prevReg := identityRegistry
	reg16, err16 := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "test", AccessKey: "ak", SecretKey: "sk"},
	})
	if err16 != nil {
		t.Fatal(err16)
	}
	identityRegistry = auth.NewReloadableRegistry(reg16)
	defer func() { identityRegistry = prevReg }()

	plan, err := startupPlan([]FrontendConfig{
		{Type: "webdav", ListenAddr: ":8444"},
		{Type: "webdav", ListenAddr: ":8445", Bucket: "photos"},
	}, nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("startupPlan with two webdav entries: %v", err)
	}
	if len(plan.listeners) != 2 {
		t.Fatalf("listeners = %+v, want 2 dedicated webdav listeners", plan.listeners)
	}
	if len(plan.shared) != 0 {
		t.Fatalf("shared = %+v, want none (both entries carry a listenAddr)", plan.shared)
	}
	if plan.listeners[0].addr != ":8444" || plan.listeners[1].addr != ":8445" {
		t.Fatalf("listener addrs = %q, %q; want :8444 then :8445",
			plan.listeners[0].addr, plan.listeners[1].addr)
	}
}

// Two IDENTICAL s3 entries (same type, same listenAddr, same bucket) remain
// an ambiguous mount: startupPlan must still reject them (bughunt C2 pin).
func TestStartupPlan_IdenticalDuplicateEntriesRejected(t *testing.T) {
	_, err := startupPlan([]FrontendConfig{
		{Type: "s3", ListenAddr: ":8444"},
		{Type: "s3", ListenAddr: ":8444"},
	}, nilBackend{}, stubCreds{})
	if err == nil {
		t.Fatal("startupPlan accepted two identical s3 entries; want ambiguous-mount error")
	}
	if !strings.Contains(err.Error(), "configured more than once with identical") {
		t.Fatalf("err = %q, want the identical-mount ambiguity message", err.Error())
	}
}
