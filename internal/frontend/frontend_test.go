package frontend_test

import (
	"net/http"
	"testing"

	"mini-s3/internal/auth"
	"mini-s3/internal/frontend"
)

type fakeFrontend struct{}

func (fakeFrontend) Name() string                        { return "fake" }
func (fakeFrontend) Handler() http.Handler               { return http.NotFoundHandler() }
func (fakeFrontend) Authenticator() auth.Authenticator   { return nil }
func (fakeFrontend) Capabilities() frontend.ProtocolCaps { return frontend.ProtocolCaps{Buckets: true} }

func TestFrontend_InterfaceSatisfaction(t *testing.T) {
	tests := []struct {
		name     string
		f        frontend.Frontend
		wantName string
		wantCaps frontend.ProtocolCaps
	}{
		{"fake frontend", fakeFrontend{}, "fake", frontend.ProtocolCaps{Buckets: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Name(); got != tt.wantName {
				t.Fatalf("Name() = %q, want %q", got, tt.wantName)
			}
			if got := tt.f.Capabilities(); got != tt.wantCaps {
				t.Fatalf("Capabilities() = %+v, want %+v", got, tt.wantCaps)
			}
		})
	}
}

func TestProtocolCaps_ZeroValue(t *testing.T) {
	var caps frontend.ProtocolCaps
	if caps.Buckets || caps.Versioning || caps.ConditionalReads || caps.Multipart || caps.PresignedURLs {
		t.Fatalf("zero ProtocolCaps must be all-false, got %+v", caps)
	}
}
