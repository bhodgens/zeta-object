package frontend

import (
	"net/http"
	"testing"

	"mini-s3/internal/auth"
)

type capsStub struct {
	caps ProtocolCaps
}

func (s capsStub) Name() string                      { return "stub" }
func (s capsStub) Handler() http.Handler             { return http.NotFoundHandler() }
func (s capsStub) Authenticator() auth.Authenticator { return nil }
func (s capsStub) Capabilities() ProtocolCaps        { return s.caps }

func TestRunConformanceSuite_StubPasses(t *testing.T) {
	RunConformanceSuite(t, capsStub{caps: ProtocolCaps{Buckets: true}}, ConformanceOptions{})
}

func TestRunConformanceSuite_Reporters(t *testing.T) {
	tests := []struct {
		name       string
		frontend   Frontend
		wantChecks int // stub declares Buckets; suite must run the buckets check
	}{
		{"buckets-capable stub", capsStub{caps: ProtocolCaps{Buckets: true}}, 1},
		{"no-capability stub", capsStub{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			suite := conformanceChecks(tt.frontend)
			calls = len(suite)
			if calls != tt.wantChecks {
				t.Fatalf("conformanceChecks ran %d checks, want %d", calls, tt.wantChecks)
			}
		})
	}
}
