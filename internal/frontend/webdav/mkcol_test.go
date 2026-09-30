// mkcol_test.go — leaf 03 Task 3: MKCOL semantics and the no-marker rule.
package webdav

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMKCOL_NestedParentExists201(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "2024/a.txt", []byte("x")) // parent /photos/2024/ exists
	req := httptest.NewRequest("MKCOL", "/photos/2024/newdir/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body=%.200s", rec.Code, rec.Body.String())
	}
	// No marker object: zero Puts (virtual collections).
	if len(be.putCalls) != 0 {
		t.Fatalf("MKCOL wrote %d marker objects", len(be.putCalls))
	}
}

func TestMKCOL_BucketLevelParentRoot201(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("x")) // bucket exists
	req := httptest.NewRequest("MKCOL", "/photos/newdir/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if len(be.putCalls) != 0 {
		t.Fatalf("MKCOL wrote marker objects")
	}
}

func TestMKCOL_MissingParent409(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("x"))
	req := httptest.NewRequest("MKCOL", "/photos/nope/sub/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409 (RFC 4918 §9.3.1, no auto-vivify)", rec.Code)
	}
	_ = be
}

func TestMKCOL_ExistingCollection405(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "2024/a.txt", []byte("x"))
	req := httptest.NewRequest("MKCOL", "/photos/2024/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestMKCOL_ExistingFile405(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("x"))
	req := httptest.NewRequest("MKCOL", "/photos/a.txt/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestMKCOL_Root_ModeA405_ModeB403(t *testing.T) {
	fA, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("MKCOL", "/", nil)
	rec := httptest.NewRecorder()
	fA.Handler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("mode A root: status = %d, want 405", rec.Code)
	}

	fB, _ := newTestFrontend(Config{Bucket: "photos"})
	req = httptest.NewRequest("MKCOL", "/", nil)
	rec = httptest.NewRecorder()
	fB.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("mode B root: status = %d, want 403", rec.Code)
	}
}

func TestMKCOL_TopLevelModeA403_NoCreateBucketSeam(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("MKCOL", "/newbucket/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (no create-bucket method on the Backend seam)", rec.Code)
	}
}

func TestMKCOL_WithBody415(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("MKCOL", "/photos/newdir/", strings.NewReader("<xml/>"))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}
