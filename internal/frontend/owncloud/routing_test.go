package owncloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// recordingWebdav is a webdavFrontend stub that records the path each
// delegated request arrives with. serveHTTP's rewrite branches are the
// unit under test; the real webdav data plane is pinned by the webdav
// package's own tests and the e2e cases.
type recordingWebdav struct {
	lastPath string
	calls    int
}

func (r *recordingWebdav) FrontendName() string                { return "webdav-stub" }
func (r *recordingWebdav) Authenticator() auth.Authenticator   { return &stubAuth{} }
func (r *recordingWebdav) Capabilities() frontend.ProtocolCaps { return frontend.ProtocolCaps{} }
func (r *recordingWebdav) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.lastPath = req.URL.Path
		r.calls++
		w.WriteHeader(http.StatusOK)
	})
}

func newRoutingTestFrontend(t *testing.T, prefix string) (*Frontend, *recordingWebdav) {
	t.Helper()
	rec := &recordingWebdav{}
	f := &Frontend{wrapped: rec, authnr: rec.Authenticator(), pathPrefix: prefix}
	return f, rec
}

func doReq(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestServeHTTPStatusPHP pins the unauthenticated capability ping: any
// path ENDING in /status.php answers 200 JSON with installed=true.
func TestServeHTTPStatusPHP(t *testing.T) {
	for _, path := range []string{"/status.php", "/syncroot/status.php", "/a/b/status.php"} {
		f, rec := newRoutingTestFrontend(t, "")
		w := doReq(t, f.Handler(), http.MethodGet, path)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("GET %s: Content-Type = %q, want application/json", path, ct)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: body is not JSON: %v", path, err)
		}
		if body["installed"] != true {
			t.Fatalf("GET %s: installed = %v, want true", path, body["installed"])
		}
		if rec.calls != 0 {
			t.Fatalf("GET %s: delegated to webdav (%d calls), want status.php answered directly", path, rec.calls)
		}
	}
}

// TestServeHTTPWebdavStrip pins the classic OC10 strip: the FIRST
// /remote.php/webdav occurrence is removed and the remainder re-rooted.
func TestServeHTTPWebdavStrip(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/remote.php/webdav/", "/"},
		{"/remote.php/webdav/dir/file.txt", "/dir/file.txt"},
		{"/remote.php/webdav", "/"},
		{"/remote.php/webdav/remote.php/webdav/x", "/remote.php/webdav/x"}, // first occurrence only
	}
	for _, tc := range tests {
		f, rec := newRoutingTestFrontend(t, "")
		doReq(t, f.Handler(), http.MethodGet, tc.in)
		if rec.lastPath != tc.want {
			t.Fatalf("%s: delegated path = %q, want %q", tc.in, rec.lastPath, tc.want)
		}
	}
}

// TestServeHTTPDavFilesStrip pins the oCIS-style strip:
// /remote.php/dav/files/<user>/<key...> re-roots at /<key...>; the dav
// root itself (no trailing segment) becomes "/".
func TestServeHTTPDavFilesStrip(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/remote.php/dav/files/oc-user/", "/"},
		{"/remote.php/dav/files/oc-user", "/"},
		{"/remote.php/dav/files/oc-user/dir/f.txt", "/dir/f.txt"},
	}
	for _, tc := range tests {
		f, rec := newRoutingTestFrontend(t, "")
		doReq(t, f.Handler(), http.MethodGet, tc.in)
		if rec.lastPath != tc.want {
			t.Fatalf("%s: delegated path = %q, want %q", tc.in, rec.lastPath, tc.want)
		}
	}
}

// TestServeHTTPPathPrefixStrip pins the configured bucket-prefix strip
// (NewWithPathPrefix) COMPOSED with the webdav strip (commit 1f6dd49):
// /remote.php/webdav is stripped first, then the bucket-segment prefix.
func TestServeHTTPPathPrefixStrip(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/remote.php/webdav/oc-bkt", "/"},
		{"/remote.php/webdav/oc-bkt/dir", "/dir"},
		{"/oc-bkt/dir", "/dir"},        // prefix without the webdav segment
		{"/other/path", "/other/path"}, // prefix not present: unchanged
	}
	for _, tc := range tests {
		f, rec := newRoutingTestFrontend(t, "/oc-bkt")
		doReq(t, f.Handler(), http.MethodGet, tc.in)
		if rec.lastPath != tc.want {
			t.Fatalf("%s: delegated path = %q, want %q", tc.in, rec.lastPath, tc.want)
		}
	}
}

// TestServeHTTPPlainDelegation pins the fallthrough: a path with no
// special prefix delegates verbatim.
func TestServeHTTPPlainDelegation(t *testing.T) {
	f, rec := newRoutingTestFrontend(t, "")
	w := doReq(t, f.Handler(), http.MethodGet, "/some/key.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.lastPath != "/some/key.txt" || rec.calls != 1 {
		t.Fatalf("delegated path = %q (%d calls), want /some/key.txt once", rec.lastPath, rec.calls)
	}
}

// TestServeHTTPNestedOCS pins OCS matching at ANY depth (the 6.x client
// resolves OCS relative to its sync root): a path containing /ocs/v2.php
// at a non-root position still routes to the OCS surface, not the data
// plane. Authenticated (stubAuth permits) -> capabilities document.
func TestServeHTTPNestedOCS(t *testing.T) {
	f, rec := newRoutingTestFrontend(t, "")
	w := doReq(t, f.Handler(), http.MethodGet, "/syncroot/ocs/v2.php/cloud/capabilities")
	if rec.calls != 0 {
		t.Fatal("nested OCS path delegated to the webdav data plane; want OCS routing")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "<ocs>") {
		t.Fatalf("body is not an OCS envelope: %s", body)
	}
}

// TestOcsSubpath pins the version-prefix trim (always slash-led).
func TestOcsSubpath(t *testing.T) {
	tests := []struct {
		in      string
		version int
		want    string
	}{
		{"/ocs/v1.php/cloud/user", 1, "/cloud/user"},
		{"/ocs/v2.php/config", 2, "/config"},
		{"/ocs/v2.php", 2, "/"},
	}
	for _, tc := range tests {
		if got := ocsSubpath(tc.in, tc.version); got != tc.want {
			t.Fatalf("ocsSubpath(%q, %d) = %q, want %q", tc.in, tc.version, got, tc.want)
		}
	}
}

// TestRouteOCSUnknownEndpoint pins the documented-degradation 404
// envelope (never a silent empty success). The OCS envelope is XML.
func TestRouteOCSUnknownEndpoint(t *testing.T) {
	f, rec := newRoutingTestFrontend(t, "")
	w := doReq(t, f.Handler(), http.MethodGet, "/ocs/v2.php/cloud/something-new")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if rec.calls != 0 {
		t.Fatal("unknown OCS endpoint delegated to the data plane")
	}
	body := w.Body.String()
	if !strings.Contains(body, "<ocs>") || !strings.Contains(body, "endpoint not implemented") {
		t.Fatalf("body is not the OCS degradation envelope: %s", body)
	}
}

// TestRouteOCSMethodNotAllowed pins the wrong-method envelope on a known
// endpoint (v2 maps the statuscode to HTTP 405).
func TestRouteOCSMethodNotAllowed(t *testing.T) {
	f, _ := newRoutingTestFrontend(t, "")
	w := doReq(t, f.Handler(), http.MethodPost, "/ocs/v2.php/config")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}
