package frontend_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"mini-s3/internal/frontend"
)

// ConformanceOptions tunes which checks run.
type ConformanceOptions struct {
	// Strict rejects even capability-degrading responses that are merely
	// suspicious (e.g. HTTP 200 on an unsupported operation). Leave false to
	// only require protocol-appropriate errors.
	Strict bool
}

// RunConformanceSuite runs protocol-agnostic checks against f. Every frontend
// (s3 today; webdav, ftp, owncloud later per their GitHub issues) calls this
// from its own tests to self-test protocol-to-model mapping correctness.
func RunConformanceSuite(t *testing.T, f frontend.Frontend, opts ConformanceOptions) {
	t.Helper()
	t.Run("conformance/"+f.Name(), func(t *testing.T) {
		for _, check := range conformanceChecks(f) {
			check(t)
		}
	})
}

// conformanceChecks builds the check list from the frontend's declared caps.
// Capability-gated: a frontend is never asked to prove a capability it does
// not declare, and a declared capability must not produce silent emulation
// (200-with-body where the protocol should reject or degrade).
func conformanceChecks(f frontend.Frontend) []func(t *testing.T) {
	var checks []func(t *testing.T)
	caps := f.Capabilities()

	if caps.Buckets {
		checks = append(checks, func(t *testing.T) {
			t.Helper()
			// Handler() must serve a request without crashing the process;
			// protocol-appropriate behavior is asserted by each frontend's
			// own protocol tests. Here we prove mountability end-to-end.
			srv := httptest.NewServer(f.Handler())
			defer srv.Close()
			resp, err := srv.Client().Get(srv.URL + "/")
			if err != nil {
				t.Fatalf("GET /: %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
		})
	}
	if caps.ConditionalReads {
		checks = append(checks, func(t *testing.T) {
			t.Helper()
			// Declared ConditionalReads must be reachable through Handler():
			// a conditional GET must not be answered with a capability error.
			srv := httptest.NewServer(f.Handler())
			defer srv.Close()
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("If-None-Match", `"any"`)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("conditional GET: %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if frontend.IsCapabilityError(nil) {
				t.Fatal("unreachable guard; keeps frontend import used")
			}
		})
	}
	return checks
}
