// dispatch_test.go — leaf 01 Task 3 + leaf 02 Task 4: method routing,
// 405+Allow for the unimplemented family, URL→resource mapping table, and
// OPTIONS headers.
package webdav

import (
	"net/http/httptest"
	"testing"
)

func newTestFrontend(cfg Config) (*Frontend, *stubBackend) {
	be := newStubBackend()
	f, err := New(be, cfg, WithAuthenticator(newStubAuth()))
	if err != nil {
		panic(err)
	}
	return f, be
}

func TestDispatch_405Family(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()

	for _, method := range []string{"LOCK", "UNLOCK", "PROPPATCH", "VERSION-CONTROL", "GIBBERISH"} {
		req := httptest.NewRequest(method, "/", nil)
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, req)
		if rec.Code != 405 {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != allowHeader {
			t.Fatalf("%s: Allow = %q, want %q", method, got, allowHeader)
		}
	}
	_ = srv
}

func TestDispatch_AllowHeaderOnReal405(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	// PUT to a collection URL is 405 with Allow.
	req := httptest.NewRequest("PUT", "/photos/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("PUT collection: status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != allowHeader {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestOPTIONS_Headers(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("OPTIONS", "/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("DAV"); got != "1" {
		t.Fatalf("DAV = %q, want 1", got)
	}
	if got := rec.Header().Get("Allow"); got != allowHeader {
		t.Fatalf("Allow = %q", got)
	}
	if got := rec.Header().Get("MS-Author-Via"); got != "DAV" {
		t.Fatalf("MS-Author-Via = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "0" {
		t.Fatalf("Content-Length = %q, want 0", got)
	}
}

func TestParseResource_ModeTable(t *testing.T) {
	tests := []struct {
		name       string
		bucket     string // mode B when non-empty
		url        string
		wantBucket string
		wantKey    string
		wantColl   bool
		wantRoot   bool
	}{
		{"mode A root", "", "/", "", "", true, true},
		{"mode A bucket slash", "", "/photos/", "photos", "", true, false},
		{"mode A bucket no slash", "", "/photos", "photos", "", false, false},
		{"mode A file", "", "/photos/a.txt", "photos", "a.txt", false, false},
		{"mode A nested", "", "/photos/2024/img.png", "photos", "2024/img.png", false, false},
		{"mode A nested coll", "", "/photos/2024/", "photos", "2024", true, false},
		{"double slash", "", "//photos//a.txt", "photos", "a.txt", false, false},
		{"mode B root", "photos", "/", "photos", "", true, true},
		{"mode B flat file", "photos", "/a.txt", "photos", "a.txt", false, false},
		{"mode B dir", "photos", "/2024/", "photos", "2024", true, false},
		{"mode B nested", "photos", "/2024/img.png", "photos", "2024/img.png", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newTestFrontend(Config{Bucket: tt.bucket})
			got := f.parseResource(tt.url)
			if got.bucket != tt.wantBucket || got.key != tt.wantKey || got.isCollection != tt.wantColl || got.isRoot != tt.wantRoot {
				t.Fatalf("parseResource(%q) = %+v, want bucket=%q key=%q coll=%v root=%v",
					tt.url, got, tt.wantBucket, tt.wantKey, tt.wantColl, tt.wantRoot)
			}
		})
	}
}

func TestParseResource_PercentEncodingPinned(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	// Go's httptest.NewRequest decodes %20 into the Path; the parser must
	// keep the decoded space in the key (rendering re-encodes on output).
	req := httptest.NewRequest("GET", "/photos/my%20file.txt", nil)
	got := f.parseResource(req.URL.Path)
	if got.key != "my file.txt" {
		t.Fatalf("key = %q, want %q (decoded once by net/url)", got.key, "my file.txt")
	}
}

func TestDavPath_RoundTrip(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	res := f.parseResource("/photos/my file.txt")
	if got := f.davPath(res); got != "/photos/my file.txt" {
		t.Fatalf("davPath = %q", got)
	}
}

func TestAuth_InternalFailureIs500(t *testing.T) {
	be := newStubBackend()
	stub := newStubAuth()
	stub.fail = true
	f, err := New(be, Config{}, WithAuthenticator(stub))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("internal auth failure: status = %d, want 500 (not 401)", rec.Code)
	}
}
