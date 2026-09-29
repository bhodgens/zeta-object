package frontend_test

import (
	"errors"
	"fmt"
	"testing"

	"mini-s3/internal/frontend"
)

func TestErrCapability(t *testing.T) {
	tests := []struct {
		name    string
		cap     string
		wantMsg string
	}{
		{"versioning unsupported", "Versioning", `frontend does not support capability "Versioning"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := frontend.ErrCapability(tt.cap)
			if err == nil || err.Error() != tt.wantMsg {
				t.Fatalf("ErrCapability(%q) = %v, want %q", tt.cap, err, tt.wantMsg)
			}
			if !frontend.IsCapabilityError(err) {
				t.Fatal("IsCapabilityError = false, want true")
			}
			if frontend.IsCapabilityError(errors.New("other")) {
				t.Fatal("IsCapabilityError(other) = true, want false")
			}
			if !frontend.IsCapabilityError(fmt.Errorf("wrapped: %w", err)) {
				t.Fatal("IsCapabilityError must unwrap wrapped capability errors")
			}
		})
	}
}
