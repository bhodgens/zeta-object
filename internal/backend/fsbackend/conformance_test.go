// Package fsbackend — conformance_test.go: fsbackend runs leaf 01's shared
// Backend contract suite against a fresh temp-root instance per subtest.
// Zero duplicated assertions: the suite is the single source of truth
// (internal/backend/conformance.Run).
package fsbackend

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/conformance"
)

// TestConformanceFS runs the full contract suite against the fs backend.
func TestConformanceFS(t *testing.T) {
	conformance.Run(t, "fsbackend", func(t *testing.T) backend.Backend {
		f, err := New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return f
	})
}
