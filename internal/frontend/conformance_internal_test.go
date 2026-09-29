package frontend

import (
	"net/http"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
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

// TestRunConformanceSuite_CapabilityGatedChecksExecute drives the suite
// end-to-end against stubs declaring each capability, so the check closures
// (httptest mount, conditional GET, capability-error guard) execute their
// happy paths. The stub handler (NotFound) is fine: the checks prove
// mountability and reachability, not protocol-specific status codes.
func TestRunConformanceSuite_CapabilityGatedChecksExecute(t *testing.T) {
	RunConformanceSuite(t, capsStub{caps: ProtocolCaps{Buckets: true, ConditionalReads: true}}, ConformanceOptions{})
}

// TestConformanceChecks_ConditionalReadsGated pins that the suite asks ONLY
// for declared capabilities: a ConditionalReads-only stub yields exactly one
// check (the conditional GET), never the buckets mount check.
func TestConformanceChecks_ConditionalReadsGated(t *testing.T) {
	checks := conformanceChecks(capsStub{caps: ProtocolCaps{ConditionalReads: true}})
	if len(checks) != 1 {
		t.Fatalf("conformanceChecks(ConditionalReads-only) = %d checks, want 1", len(checks))
	}
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
