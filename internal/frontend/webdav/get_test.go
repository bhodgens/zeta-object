// get_test.go — leaf 02 Task 1: GET/HEAD headers, conditionals, collection
// GET, both modes.
package webdav

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

func TestGET_ObjectHeadersAndBody(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("hello"), func(o *objectmodel.Object) {
		o.ContentType = "image/jpeg"
		o.ETag = `"abc123"` // stored quoted form
	})
	req := httptest.NewRequest("GET", "/photos/a.txt", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != `"abc123"` {
		t.Fatalf("ETag = %q, want quoted form", got)
	}
	if got := rec.Header().Get("Last-Modified"); got != "Tue, 29 Sep 2026 12:00:00 GMT" {
		t.Fatalf("Last-Modified = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "5" {
		t.Fatalf("Content-Length = %q", got)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestGET_DefaultContentType(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.bin", []byte("x"), func(o *objectmodel.Object) { o.ContentType = "" })
	req := httptest.NewRequest("GET", "/photos/a.bin", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", got)
	}
}

func TestHEAD_IdenticalHeadersNoBody(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("hello"))
	req := httptest.NewRequest("HEAD", "/photos/a.txt", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("HEAD must carry ETag")
	}
	if rec.Header().Get("Transfer-Encoding") == "chunked" {
		t.Fatal("HEAD must not be chunked")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body = %d bytes, want 0", rec.Body.Len())
	}
}

func TestGET_MissingKeyAndBucket(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("GET", "/photos/missing.txt", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("missing key: status = %d, want 404", rec.Code)
	}
	// Missing bucket: no objects seeded under "nope" — the stub List/Stat
	// miss with NoSuchKey/NoSuchBucket; GET on the bucket collection
	// resolves via List which reports NoSuchBucket ⇒ 404.
	req = httptest.NewRequest("GET", "/nope/", nil)
	rec = httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("missing bucket: status = %d, want 404", rec.Code)
	}
}

func TestGET_CollectionDirectoryType(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "2024/a.txt", []byte("x"))
	req := httptest.NewRequest("GET", "/photos/2024/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "httpd/unix-directory" {
		t.Fatalf("Content-Type = %q, want httpd/unix-directory", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("collection GET body = %d bytes, want 0", rec.Body.Len())
	}
}

func TestGET_PrefixWithoutSlashIs404(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "2024/a.txt", []byte("x"))
	// Pinned decision (get.go): a prefix path without the trailing slash
	// that exists only as a collection is a 404.
	req := httptest.NewRequest("GET", "/photos/2024", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGET_IfNoneMatch304(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("hello"), func(o *objectmodel.Object) { o.ETag = `"abc"` })
	req := httptest.NewRequest("GET", "/photos/a.txt", nil)
	req.Header.Set("If-None-Match", `"abc"`)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 304 {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
}

func TestGET_IfNoneMatchStar304(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("hello"))
	req := httptest.NewRequest("GET", "/photos/a.txt", nil)
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 304 {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
}

func TestGET_ModeB_Rooting(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "b"})
	be.seed("b", "dir/f.txt", []byte("modeb"))
	req := httptest.NewRequest("GET", "/dir/f.txt", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "modeb") {
		t.Fatalf("body = %q", body)
	}
}

func TestGET_StatInternalErrorPassesThrough(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.failStat = true
	req := httptest.NewRequest("GET", "/photos/a.txt", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
