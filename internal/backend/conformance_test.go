// External test package (backend_test): an in-package test file cannot
// import internal/backend/conformance (it imports backend — a test-file
// import cycle), so the leaf-pinned entry point lives here instead, where
// both backend and conformance are importable. RunBackendConformance keeps
// the leaf's exact signature and delegates to the single canonical suite.
package backend_test

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/conformance"
)

// RunBackendConformance is the in-package entry point pinned by the leaf:
//
//	RunBackendConformance(t, name string, factory func(t *testing.T) Backend)
//
// The assertion logic lives in internal/backend/conformance (a NON-test
// file, deliberately not a _test.go): Go cannot export test functions across
// package boundaries, and Backend implementations living in other packages
// (internal/backend/fsbackend from leaf 02, future zfsring / s3proxy) run
// the full suite with zero duplicated assertions via
//
//	conformance.Run(t, "fsbackend", factory)
//
// This wrapper serves same-package consumers; it holds no assertions of its
// own, so the contract suite has exactly one source of truth.
func RunBackendConformance(t *testing.T, name string, factory func(t *testing.T) backend.Backend) {
	t.Helper()
	conformance.Run(t, name, factory)
}
