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
	// W2: the frontend enforces If-Match itself (the fs backend ignores
	// PutOptions conditionals), but the header still rides to the backend
	// so a conditionally-capable backend enforces it at the seam too.
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("x"))
	req.Header.Set("If-Match", `"stale"`)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if len(be.putCalls) != 0 {
		t.Fatalf("stale If-Match reached the backend: %+v", be.putCalls)
	}
	if rec.Code != 412 {
		t.Fatalf("stale If-Match: status = %d, want 412", rec.Code)
	}
}

func TestPUT_IfNoneMatchStarForwarded(t *testing.T) {
	f, be := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("x"))
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("If-None-Match * on a missing object: status = %d, want 201", rec.Code)
	}
	if len(be.putCalls) != 1 || be.putCalls[0].ifNoneMatch != "*" {
		t.Fatalf("put calls = %+v", be.putCalls)
	}
}

// W2 pins: stale If-Match ⇒ 412 with the previous bytes intact; matching
// If-Match (quoted, unquoted, list) ⇒ 204; If-None-Match * on an existing
// object ⇒ 412; If-None-Match ETag in list ⇒ 412.
func TestPUT_IfMatchStale412BytesIntact(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("old-bytes"))
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("clobber"))
	req.Header.Set("If-Match", `"etag-photos-a.txt-OLD"`)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 412 {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	if len(be.putCalls) != 0 {
		t.Fatalf("stale If-Match PUT reached the backend: %+v", be.putCalls)
	}
	if !be.hasKey("photos", "a.txt") {
		t.Fatal("object vanished on a rejected PUT")
	}
	// Bytes intact (direct store read via GET).
	greq := httptest.NewRequest("GET", "/photos/a.txt", nil)
	grec := httptest.NewRecorder()
	f.Handler().ServeHTTP(grec, greq)
	if !strings.Contains(grec.Body.String(), "old-bytes") {
		t.Fatalf("previous bytes clobbered: %q", grec.Body.String())
	}
}

func TestPUT_IfMatchMatched204(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("old"))
	for _, hdr := range []string{
		`"etag-photos-a.txt"`,         // quoted exact
		`etag-photos-a.txt`,           // unquoted exact
		`"nope", "etag-photos-a.txt"`, // list containing the ETag
		`*`,                           // any-existing wildcard
	} {
		req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("new"))
		req.Header.Set("If-Match", hdr)
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Fatalf("If-Match %q: status = %d, want 204; body=%.200s", hdr, rec.Code, rec.Body.String())
		}
		be.seed("photos", "a.txt", []byte("old")) // restore the original ETag for the next case
	}
}

func TestPUT_IfMatchOnMissing412(t *testing.T) {
	f, be := newTestFrontend(Config{})
	req := httptest.NewRequest("PUT", "/photos/gone.txt", strings.NewReader("x"))
	req.Header.Set("If-Match", `"whatever"`)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 412 {
		t.Fatalf("status = %d, want 412 (If-Match on a missing object)", rec.Code)
	}
	if len(be.putCalls) != 0 {
		t.Fatalf("PUT reached the backend: %+v", be.putCalls)
	}
}

func TestPUT_IfNoneMatchStarOnExisting412(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("old"))
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("clobber"))
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 412 {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	if len(be.putCalls) != 0 {
		t.Fatalf("PUT reached the backend: %+v", be.putCalls)
	}
	if !strings.Contains(string(be.bodies["photos\x00a.txt"]), "old") {
		t.Fatalf("bytes clobbered: %q", be.bodies["photos\x00a.txt"])
	}
}

func TestPUT_IfNoneMatchETagInList412(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("old"))
	req := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("clobber"))
	req.Header.Set("If-None-Match", `"etag-photos-a.txt"`)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 412 {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	// A non-matching list lets the write through (fresh store, same seed).
	f2, be2 := newTestFrontend(Config{})
	be2.seed("photos", "a.txt", []byte("old"))
	req2 := httptest.NewRequest("PUT", "/photos/a.txt", strings.NewReader("new"))
	req2.Header.Set("If-None-Match", `"different"`)
	rec2 := httptest.NewRecorder()
	f2.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != 204 {
		t.Fatalf("non-matching If-None-Match list: status = %d, want 204", rec2.Code)
	}
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
