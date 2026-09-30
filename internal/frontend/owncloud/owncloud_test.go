package owncloud

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/frontend/webdav"
)

// The webdav Frontend is accepted via the concrete type; compile-time
// proof that the wrapper really wraps it lives in New's signature.
var _ = webdav.New

func newFrontend(t *testing.T) (*Frontend, *webdav.Frontend) {
	t.Helper()
	wd := newWrappedWebdav(false)
	f, err := New(wd)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f, wd
}

// TestFrontendName pins master Contract 3: Name() == "owncloud".
func TestFrontendName(t *testing.T) {
	f, _ := newFrontend(t)
	if got := f.Name(); got != "owncloud" {
		t.Fatalf("Name() = %q, want %q", got, "owncloud")
	}
}

// TestFrontendAuthenticator delegates to the wrapped webdav adapter.
func TestFrontendAuthenticator(t *testing.T) {
	f, _ := newFrontend(t)
	if f.Authenticator() == nil {
		t.Fatal("Authenticator() = nil, want the wrapped webdav adapter")
	}
}

// TestFrontendCapabilitiesAreComposed: composed caps == wrapped webdav's
// caps (Versioning stays false — no provider in v1).
func TestFrontendCapabilitiesAreComposed(t *testing.T) {
	f, wd := newFrontend(t)
	want := wd.Capabilities()
	got := f.Capabilities()
	if got != want {
		t.Fatalf("Capabilities() = %+v, want wrapped webdav's %+v", got, want)
	}
	if got.Versioning {
		t.Fatal("Versioning = true, want false (no metadata provider in v1)")
	}
}

// TestNewRejectsNilWebdav: fail-loud construction.
func TestNewRejectsNilWebdav(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil): got nil error, want failure")
	}
}

// TestOCSPrefixesRouteToOCS: /ocs/v1.php and /ocs/v2.php are answered by
// the OCS surface, everything else delegates to the wrapped webdav
// handler — exercised against the REAL webdav 401 pipeline (denied
// credentials → 401, not an OCS envelope).
func TestOCSPrefixesRouteToOCS(t *testing.T) {
	denied := newWrappedWebdav(true)
	f, err := New(denied)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()

	// v2 maps the 997 statuscode to HTTP 401; v1 answers HTTP 200 with
	// the statuscode in the envelope (the v1/v2 mapping rule, xml.go).
	resp2, err := srv.Client().Get(srv.URL + "/ocs/v2.php/config")
	if err != nil {
		t.Fatalf("GET v2 config denied: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /ocs/v2.php/config (denied): status %d, want 401", resp2.StatusCode)
	}

	// Non-OCS path delegates to webdav: same anonymous request gets the
	// webdav 401 with WWW-Authenticate (a shape OCS responses never carry).
	resp, err := srv.Client().Get(srv.URL + "/remote.php/webdav/")
	if err != nil {
		t.Fatalf("GET /remote.php/webdav/: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /remote.php/webdav/ (no creds): status %d, want webdav 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("webdav delegation lost the WWW-Authenticate challenge")
	}
}

// TestConformanceSmoke: the frontend satisfies the shared frontend seam
// (leaf 02 Task 5 smoke; leaf 05 extends with data-plane instantiation).
func TestConformanceSmoke(t *testing.T) {
	f, _ := newFrontend(t)
	if _, ok := any(f).(frontend.Frontend); !ok {
		t.Fatal("owncloud.Frontend does not satisfy frontend.Frontend")
	}
	frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}
