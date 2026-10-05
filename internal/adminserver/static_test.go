package adminserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// assetMapFS is a stand-in for the embedded web/ tree carrying the same
// web/ key prefix as the go:embed directive.
func assetMapFS() fstest.MapFS {
	return fstest.MapFS{
		"web/index.html":     {Data: []byte("<!doctype html><title>shell</title>")},
		"web/login.html":     {Data: []byte("<!doctype html><title>sign in</title>")},
		"web/assets/app.css": {Data: []byte("body{color:red}")},
	}
}

func TestStaticRootServesEmbeddedShell(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / content-type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "zeta-object") {
		t.Fatalf("GET / body does not name the console: %q", rec.Body.String())
	}
}

func TestStaticRootNeedsNoSession(t *testing.T) {
	// No cookie set; the shell must still be served.
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated GET / status = %d, want 200", rec.Code)
	}
}

func TestStaticLoginPageNeedsNoSession(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /login content-type = %q, want text/html", ct)
	}
}

func TestStaticKnownAssetServed(t *testing.T) {
	h := newStaticHandler(assetMapFS())
	req := httptest.NewRequest(http.MethodGet, "/assets/app.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.css status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "body{color:red}" {
		t.Fatalf("asset bytes = %q, want the embedded bytes", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("asset content-type = %q, want text/css", ct)
	}
}

func TestStaticUnknownAssetIs404(t *testing.T) {
	h := newStaticHandler(assetMapFS())
	req := httptest.NewRequest(http.MethodGet, "/assets/missing.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown asset status = %d, want 404", rec.Code)
	}
}

func TestStaticTraversalIs404(t *testing.T) {
	h := newStaticHandler(assetMapFS())
	for _, p := range []string{"/assets/../index.html", "/assets/..%2findex.html", "/assets/"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", p, rec.Code)
		}
	}
}

func TestStaticUnknownRootPathIs404(t *testing.T) {
	h := newStaticHandler(assetMapFS())
	req := httptest.NewRequest(http.MethodGet, "/definitely-not-here", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", rec.Code)
	}
}
