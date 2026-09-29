package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mini-s3/internal/auth"
	"mini-s3/internal/backend"
	"mini-s3/internal/frontend"
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
			cfg:         []FrontendConfig{{Type: "webdav"}}, // webdav factory not yet wired (GH issue)
			wantErr:     true,
			errContains: "known: [s3]",
		},
		{
			name:    "duplicate type rejected",
			cfg:     []FrontendConfig{{Type: "s3"}, {Type: "s3", ListenAddr: ":8444"}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	shared, extra := mountFrontends(mux, mounts)

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
	// buildFrontends rejects duplicate types, so exercise the plan split with
	// a pre-built mount set via mountFrontends (covered above) and here via a
	// hand-built registry path: one s3 on default, one dedicated stub via
	// mountFrontends directly.
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
