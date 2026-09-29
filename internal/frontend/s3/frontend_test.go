package s3_test

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// TestS3Frontend_ImplementsFrontend pins the frontend contract surface.
func TestS3Frontend_ImplementsFrontend(t *testing.T) {
	f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	var asFrontend frontend.Frontend = f

	if asFrontend.Name() != "s3" {
		t.Fatalf("Name() = %q, want %q", asFrontend.Name(), "s3")
	}
	if asFrontend.Handler() == nil {
		t.Fatal("Handler() = nil, want non-nil")
	}
	if asFrontend.Authenticator() == nil {
		t.Fatal("Authenticator() = nil, want non-nil")
	}
	caps := asFrontend.Capabilities()
	if !caps.Buckets || !caps.Multipart || !caps.PresignedURLs || !caps.ConditionalReads {
		t.Fatalf("Capabilities() = %+v, want Buckets/Multipart/PresignedURLs/ConditionalReads true", caps)
	}
	if caps.Versioning {
		t.Fatal("Capabilities().Versioning = true, want false (not implemented)")
	}
}

// TestS3Frontend_Conformance runs the reusable conformance suite against
// the s3 frontend (leaf-02 Task 4).
func TestS3Frontend_Conformance(t *testing.T) {
	f := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}
