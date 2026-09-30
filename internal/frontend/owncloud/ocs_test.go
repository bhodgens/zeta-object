package owncloud

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// golden is a byte-exact expected XML document.
func golden(want string) string { return want }

// TestWriteOCSGolden pins the envelope byte-for-byte (master Contract 1
// wire shape; classic single-line encoding).
func TestWriteOCSGolden(t *testing.T) {
	tests := []struct {
		name       string
		ocsVersion int
		httpStatus int
		statusCode int
		message    string
		data       any
		want       string
	}{
		{
			name:       "v1 ok with payload",
			ocsVersion: 1,
			httpStatus: http.StatusOK,
			statusCode: 200,
			message:    "OK",
			data:       ocUserPayload{Value: "alice"},
			want: golden(`<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>ok</status><statuscode>200</statuscode><message>OK</message></meta><data><id>alice</id></data></ocs>`),
		},
		{
			name:       "v1 error keeps HTTP 200, statuscode in envelope",
			ocsVersion: 1,
			httpStatus: http.StatusNotFound,
			statusCode: 404,
			message:    "Not found",
			data:       nil,
			want: golden(`<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>failure</status><statuscode>404</statuscode><message>Not found</message></meta><data></data></ocs>`),
		},
		{
			name:       "v2 not-found maps to HTTP 404",
			ocsVersion: 2,
			httpStatus: http.StatusNotFound,
			statusCode: 404,
			message:    "Not found",
			data:       nil,
			want: golden(`<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>failure</status><statuscode>404</statuscode><message>Not found</message></meta><data></data></ocs>`),
		},
		{
			name:       "v2 997 unauthorised maps to HTTP 401",
			ocsVersion: 2,
			httpStatus: http.StatusUnauthorized,
			statusCode: 997,
			message:    "Unauthorised",
			data:       nil,
			want: golden(`<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>failure</status><statuscode>997</statuscode><message>Unauthorised</message></meta><data></data></ocs>`),
		},
		{
			name:       "v1 997 keeps HTTP 200",
			ocsVersion: 1,
			httpStatus: http.StatusUnauthorized,
			statusCode: 997,
			message:    "Unauthorised",
			data:       nil,
			want: golden(`<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>failure</status><statuscode>997</statuscode><message>Unauthorised</message></meta><data></data></ocs>`),
		},
		{
			name:       "method not allowed",
			ocsVersion: 2,
			httpStatus: http.StatusMethodNotAllowed,
			statusCode: 405,
			message:    "method not allowed",
			data:       nil,
			want: golden(`<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>failure</status><statuscode>405</statuscode><message>method not allowed</message></meta><data></data></ocs>`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeOCS(rec, tt.ocsVersion, tt.httpStatus, tt.statusCode, tt.message, tt.data)
			if got := rec.Body.String(); got != tt.want {
				t.Fatalf("body =\n%q\nwant\n%q", got, tt.want)
			}
			if ct := rec.Header().Get("Content-Type"); ct != ocsContentType {
				t.Fatalf("Content-Type = %q, want %q", ct, ocsContentType)
			}
			if v := rec.Header().Get("OCS-Version"); v != ocsVersionHeaderValue(tt.ocsVersion) {
				t.Fatalf("OCS-Version = %q, want %q", v, ocsVersionHeaderValue(tt.ocsVersion))
			}
			// The v1/v2 HTTP mapping rule.
			wantHTTP := tt.httpStatus
			if tt.ocsVersion == 1 {
				wantHTTP = http.StatusOK
			}
			if rec.Code != wantHTTP {
				t.Fatalf("HTTP status = %d, want %d (ocsVersion %d)", rec.Code, wantHTTP, tt.ocsVersion)
			}
		})
	}
}

// TestOCSSubpath pins prefix parsing for both versions.
func TestOCSSubpath(t *testing.T) {
	if got := ocsSubpath("/ocs/v1.php/cloud/user", 1); got != "/cloud/user" {
		t.Fatalf("v1 subpath = %q", got)
	}
	if got := ocsSubpath("/ocs/v2.php/config", 2); got != "/config" {
		t.Fatalf("v2 subpath = %q", got)
	}
}

// TestOCSVersionOf pins the exact-prefix rule: only /ocs/v{1,2}.php are
// OCS; near-misses are NOT.
func TestOCSVersionOf(t *testing.T) {
	for _, p := range []string{"/ocs/v1.php/x", "/ocs/v2.php", "/ocs/v1.php"} {
		if _, ok := ocsVersionOf(p); !ok {
			t.Fatalf("ocsVersionOf(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/ocs/", "/ocs/v3.php/x", "/ocsv1.php/x", "/remote.php/webdav/", "/"} {
		if _, ok := ocsVersionOf(p); ok {
			t.Fatalf("ocsVersionOf(%q) = true, want false", p)
		}
	}
}

// TestRouterGolden pins each served endpoint's full document through the
// composed Frontend (authenticated stub), byte-for-byte.
func TestRouterGolden(t *testing.T) {
	f, err := New(newWrappedWebdav(false))
	if err != nil {
		t.Fatal(err)
	}
	const userDoc = `<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>ok</status><statuscode>200</statuscode><message>OK</message></meta><data><id>oc-user</id></data></ocs>`
	const capsDoc = `<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>ok</status><statuscode>200</statuscode><message>OK</message></meta><data><version><major>10</major><minor>11</minor><micro>0</micro><string>10.11.0</string></version><capabilities><files><bigfilechunking>false</bigfilechunking></files></capabilities></data></ocs>`
	const errDoc = `<?xml version="1.0" encoding="UTF-8"?>
<ocs><meta><status>failure</status><statuscode>404</statuscode><message>endpoint not implemented: this server serves the WebDAV+OCS minimal subset only (capabilities, cloud/user); see docs/owncloud-compatibility.md</message></meta><data></data></ocs>`

	tests := []struct {
		name string
		path string
		want string
	}{
		{"/ocs/v1.php/config", "/ocs/v1.php/config", capsDoc},
		{"/ocs/v2.php/config", "/ocs/v2.php/config", capsDoc},
		{"/ocs/v1.php/cloud/capabilities", "/ocs/v1.php/cloud/capabilities", capsDoc},
		{"/ocs/v2.php/cloud/capabilities", "/ocs/v2.php/cloud/capabilities", capsDoc},
		{"/ocs/v1.php/cloud/user", "/ocs/v1.php/cloud/user", userDoc},
		{"/ocs/v2.php/cloud/user", "/ocs/v2.php/cloud/user", userDoc},
		{"unimplemented shares", "/ocs/v2.php/apps/files_sharing/api/v1/shares", errDoc},
		{"unimplemented provisioning", "/ocs/v1.php/cloud/users", errDoc},
	}
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+tt.path, nil)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var b strings.Builder
			buf := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(buf)
				b.Write(buf[:n])
				if err != nil {
					break
				}
			}
			if b.String() != tt.want {
				t.Fatalf("body =\n%q\nwant\n%q", b.String(), tt.want)
			}
		})
	}
}

// TestRouterMethodMismatch: POST on a known GET endpoint → 405 (v2 HTTP
// mapping) with the OCS envelope.
func TestRouterMethodMismatch(t *testing.T) {
	f, err := New(newWrappedWebdav(false))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/ocs/v2.php/config", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /ocs/v2.php/config: status %d, want 405", resp.StatusCode)
	}
	body := make([]byte, 2048)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "<statuscode>405</statuscode>") {
		t.Fatalf("body missing 405 statuscode: %q", string(body[:n]))
	}
}
