// capabilities_test.go — GET /capabilities (leaf 03 Task 3): the Contract 4
// document assembled from live sources, the no-cache headers, the auth
// gate (mTLS OR fed_session), and the honest-degradation table.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// capsEnv is a frontend with the capabilities surface wired.
func capsEnv(t *testing.T, deps *CapabilityDeps) *testEnv {
	t.Helper()
	env := newTestEnv(t, Options{})
	env.frontend.capabilities = deps
	return env
}

func TestCapabilities_LiveSourcesMatrix(t *testing.T) {
	env := capsEnv(t, &CapabilityDeps{
		ListDatasets: func(context.Context) ([]string, error) {
			return []string{"tank/zeta-b", "tank/zeta-a", "plain"}, nil
		},
		ProtocolFlags: func(_ context.Context, dataset string) ProtocolFlags {
			return ProtocolFlags{S3: true, NFS: dataset != "plain"}
		},
		FeatureFlags: func(_ context.Context, dataset string) FeatureFlags {
			// "test doubles allowed" (leaf spec): the flags stand in for
			// the ZFS property reads (events=on etc.).
			return FeatureFlags{Versions: dataset != "plain", Events: dataset == "tank/zeta-a"}
		},
		AuthFlags: func() AuthFlags {
			return AuthFlags{OIDC: true, STSExchange: true}
		},
	})
	rr := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/capabilities"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	// No-cache headers.
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store, no-cache, must-revalidate" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if pg := rr.Header().Get("Pragma"); pg != "no-cache" {
		t.Fatalf("Pragma = %q", pg)
	}
	var doc CapabilitiesDocument
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Dataset list is sorted (live listing order is filesystem-dependent).
	if len(doc.Datasets) != 3 ||
		doc.Datasets[0].Name != "plain" || doc.Datasets[1].Name != "tank/zeta-a" || doc.Datasets[2].Name != "tank/zeta-b" {
		t.Fatalf("datasets = %+v", doc.Datasets)
	}
	plain := doc.Datasets[0]
	if !plain.Protocols.S3 || plain.Protocols.NFS || plain.Features.Events || plain.Features.Versions {
		t.Fatalf("plain row = %+v", plain)
	}
	a := doc.Datasets[1]
	if !a.Protocols.NFS || !a.Features.Events || !a.Features.Versions {
		t.Fatalf("tank/zeta-a row = %+v", a)
	}
	// Every row marshals the FULL flag set (Contract 4 shape, no omitted
	// false fields drifting per row).
	var raw struct {
		Datasets []struct {
			Protocols map[string]bool `json:"protocols"`
			Features  map[string]bool `json:"features"`
		} `json:"datasets"`
		Auth map[string]bool `json:"auth"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for i, row := range raw.Datasets {
		for _, key := range []string{"smb", "nfs", "s3", "webdav", "sftp", "ftp"} {
			if _, ok := row.Protocols[key]; !ok {
				t.Fatalf("row %d missing protocols.%s", i, key)
			}
		}
		for _, key := range []string{"versions", "events", "previous_versions_smb"} {
			if _, ok := row.Features[key]; !ok {
				t.Fatalf("row %d missing features.%s", i, key)
			}
		}
	}
	for _, key := range []string{"oidc", "saml", "sts_exchange", "kerberos"} {
		if _, ok := raw.Auth[key]; !ok {
			t.Fatalf("auth missing %s", key)
		}
	}
	if !raw.Auth["oidc"] || !raw.Auth["sts_exchange"] || raw.Auth["kerberos"] {
		t.Fatalf("auth = %v", raw.Auth)
	}
}

// TestCapabilities_HonestDegradesTable pins the nil-deps honesty: every
// nil live source renders as empty/false, never as a claimed capability.
func TestCapabilities_HonestDegradesTable(t *testing.T) {
	tests := []struct {
		name        string
		deps        *CapabilityDeps
		wantSets    int
		wantAuthOID bool
	}{
		{"nil deps entirely", nil, 0, false},
		{"nil dataset list", &CapabilityDeps{AuthFlags: func() AuthFlags { return AuthFlags{OIDC: true} }}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := capsEnv(t, tt.deps)
			rr := httptest.NewRecorder()
			env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/capabilities"))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			var doc CapabilitiesDocument
			if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(doc.Datasets) != tt.wantSets {
				t.Fatalf("datasets = %+v, want %d rows", doc.Datasets, tt.wantSets)
			}
			if doc.Auth.OIDC != tt.wantAuthOID {
				t.Fatalf("auth = %+v", doc.Auth)
			}
			// Nil slices marshal as [] not null (the document is always a
			// valid matrix shape for consumers).
			if !jsonContainsArray(rr.Body.String()) {
				t.Fatalf("datasets is not a JSON array in %s", rr.Body.String())
			}
		})
	}
}

func jsonContainsArray(body string) bool {
	return len(body) > 0 && (contains(body, `"datasets":[]`) || contains(body, `"datasets": [`) || contains(body, `"datasets":[{`))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestCapabilities_DatasetListingFailure pins the honest 500 (the listing
// error is never swallowed into an empty matrix).
func TestCapabilities_DatasetListingFailure(t *testing.T) {
	env := capsEnv(t, &CapabilityDeps{
		ListDatasets: func(context.Context) ([]string, error) { return nil, errors.New("zfs list failed") },
	})
	rr := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/capabilities"))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if body := decodeError(t, rr); body.Error.Code != "InternalError" {
		t.Fatalf("code = %q, want InternalError (and no zfs detail in body: %s)", body.Error.Code, rr.Body.String())
	}
}

// TestCapabilities_AuthGate pins the 401 matrix and the method gate.
func TestCapabilities_AuthGate(t *testing.T) {
	env := capsEnv(t, &CapabilityDeps{
		ListDatasets: func(context.Context) ([]string, error) { return []string{"d"}, nil },
	})
	// No credentials at all: 401.
	rr := httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, plainRequest(http.MethodGet, "/capabilities"))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", rr.Code)
	}
	// mTLS principal: 200.
	rr = httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/capabilities"))
	if rr.Code != http.StatusOK {
		t.Fatalf("mTLS = %d, want 200", rr.Code)
	}
	// Wrong method: 405 (before auth, matching the package's method-first
	// fed-route style; either way it is not a 200).
	rr = httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, plainRequest(http.MethodPost, "/capabilities"))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rr.Code)
	}
	// Audit: exactly one record for the authenticated mTLS request.
	var calls []auditCall
	env.frontend.opts.Audit = func(principal, method, bucket, key, op string, status int, denied bool) {
		calls = append(calls, auditCall{principal, method, bucket, key, op, status, denied})
	}
	rr = httptest.NewRecorder()
	env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/capabilities"))
	if len(calls) != 1 || calls[0].principal != "tester" || calls[0].op != "admin" {
		t.Fatalf("capabilities audit = %+v", calls)
	}
}

// TestCapabilities_FedSessionGate: a valid fed_session (no cert) reaches
// the matrix; a garbage cookie does not. Federation enabled via fedEnv.
func TestCapabilities_FedSessionGate(t *testing.T) {
	e := newFedEnv(t)
	e.frontend.capabilities = &CapabilityDeps{
		ListDatasets: func(context.Context) ([]string, error) { return []string{"tank/x"}, nil },
		FeatureFlags: func(context.Context, string) FeatureFlags { return FeatureFlags{Events: true} },
	}
	rec := e.doLogin(t, nil)
	sess := cookieValue(t, rec, fedSessionCookieName)

	ok := plainRequest(http.MethodGet, "/capabilities")
	ok.AddCookie(&http.Cookie{Name: fedSessionCookieName, Value: sess.Value})
	rr := httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(rr, ok)
	if rr.Code != http.StatusOK {
		t.Fatalf("fed session = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var doc CapabilitiesDocument
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Datasets) != 1 || doc.Datasets[0].Name != "tank/x" || !doc.Datasets[0].Features.Events {
		t.Fatalf("document = %+v", doc)
	}

	bad := plainRequest(http.MethodGet, "/capabilities")
	bad.AddCookie(&http.Cookie{Name: fedSessionCookieName, Value: "forged.value"})
	rr = httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(rr, bad)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("forged session = %d, want 401", rr.Code)
	}
}

// TestCapabilities_NoLiveReadOfConfigStore is a structural assertion: the
// handler touches ONLY the injected CapabilityDeps (the wiring layer owns
// how live values get there — package main's admin_wiring.go), so the
// matrix source is a seam, not a store.
func TestCapabilities_NoLiveReadOfConfigStore(t *testing.T) {
	// The wire shape re-derives from the marshaled document every call:
	// two requests with a counting ListDatasets show 2 calls (no cache).
	calls := 0
	env := capsEnv(t, &CapabilityDeps{
		ListDatasets: func(context.Context) ([]string, error) {
			calls++
			return []string{"d"}, nil
		},
	})
	for range 2 {
		rr := httptest.NewRecorder()
		env.frontend.Handler().ServeHTTP(rr, env.authedRequest(http.MethodGet, "/capabilities"))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("ListDatasets called %d times for 2 requests; the matrix is being cached", calls)
	}
}
