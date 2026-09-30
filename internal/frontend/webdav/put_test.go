// put_test.go — leaf 03 Task 2: PUT statuses, ETag header, conditionals,
// zero-byte, collection 405, mode-B rooting, chunked rejection.
package webdav

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPUT_Create201(t *testing.T) {
	f, be := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/new.txt", strings.NewReader("data"))
	req.Header.Set("Content-Type", "text/x-custom")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body=%.200s", rec.Code, rec.Body.String())
	}
	if len(be.putCalls) != 1 {
		t.Fatalf("Put calls = %d", len(be.putCalls))
	}
	pc := be.putCalls[0]
	if pc.bucket != "photos" || pc.key != "new.txt" || pc.size != 4 || pc.contentType != "text/x-custom" {
		t.Fatalf("Put call = %+v", pc)
	}
	if string(pc.body) != "data" {
		t.Fatalf("body = %q", pc.body)
	}
	if got := rec.Header().Get("ETag"); got != `"etag-photos-new.txt-new"` {
		t.Fatalf("ETag = %q", got)
	}
}

func TestPUT_Overwrite204(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("old"))
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("new"))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("204 must carry ETag")
	}
}

func TestPUT_EmptyBody201(t *testing.T) {
	f, be := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/empty.txt", strings.NewReader(""))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if len(be.putCalls) != 1 || be.putCalls[0].size != 0 {
		t.Fatalf("Put calls = %+v", be.putCalls)
	}
}

func TestPUT_Collection405(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/dir/", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestPUT_IfMatchForwarded(t *testing.T) {
	f, be := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("x"))
	req.Header.Set("If-Match", `"stale"`)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	// The fs backend ignores conditionals in v1, so the PUT succeeds —
	// the pin is that PutOptions.IfMatch carried the header.
	if len(be.putCalls) != 1 || be.putCalls[0].ifMatch != `"stale"` {
		t.Fatalf("put calls = %+v", be.putCalls)
	}
	_ = rec
}

func TestPUT_IfNoneMatchStarForwarded(t *testing.T) {
	f, be := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("x"))
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if len(be.putCalls) != 1 || be.putCalls[0].ifNoneMatch != "*" {
		t.Fatalf("put calls = %+v", be.putCalls)
	}
	_ = rec
}

func TestPUT_ModeB_Rooting(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "b"})
	req := httptest.NewRequest("PUT", "/dir/f.txt", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if len(be.putCalls) != 1 || be.putCalls[0].bucket != "b" || be.putCalls[0].key != "dir/f.txt" {
		t.Fatalf("put calls = %+v", be.putCalls)
	}
}

func TestPUT_ChunkedWithoutLength400(t *testing.T) {
	f, be := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("chunked body"))
	req.ContentLength = -1 // chunked: no declared length
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 (reject chunked-without-length)", rec.Code)
	}
	if len(be.putCalls) != 0 {
		t.Fatal("chunked PUT must not reach the backend")
	}
}
