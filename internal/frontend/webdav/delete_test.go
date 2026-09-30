// delete_test.go — leaf 03 Task 4: file/collection delete, recursive
// pagination, root/bucket 403, missing 404.
package webdav

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

func TestDELETE_File204(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("x"))
	req := httptest.NewRequest("DELETE", "/photos/a.txt", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if be.hasKey("photos", "a.txt") {
		t.Fatal("key still present after delete")
	}
}

func TestDELETE_Missing404(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "other.txt", []byte("x"))
	req := httptest.NewRequest("DELETE", "/photos/nope.txt", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDELETE_CollectionRecursive(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "b"})
	be.seed("b", "dir/a.txt", []byte("1"))
	be.seed("b", "dir/sub/b.txt", []byte("2"))
	req := httptest.NewRequest("DELETE", "/dir/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if be.hasKey("b", "dir/a.txt") || be.hasKey("b", "dir/sub/b.txt") {
		t.Fatal("recursive delete left keys behind")
	}
}

func TestDELETE_CollectionFullPagination(t *testing.T) {
	be := newStubBackend()
	// Page 1: truncated with one key; page 2: the rest. Both pages must be
	// consumed before the deletes.
	be.pages["b"] = []objectmodel.ListPage{
		{ // existence probe from resolveKind
			Objects:     []objectmodel.Object{{Key: "dir/p1.txt", Size: 1, LastModified: timeNow()}},
			IsTruncated: false,
		},
		{ // deleteCollection page 1: truncated with one key
			Objects:     []objectmodel.Object{{Key: "dir/p1.txt", Size: 1, LastModified: timeNow()}},
			IsTruncated: true,
			NextToken:   "tok",
		},
		{ // deleteCollection page 2: the rest
			Objects:     []objectmodel.Object{{Key: "dir/p2.txt", Size: 1, LastModified: timeNow()}},
			IsTruncated: false,
		},
		{ // deleteCollection re-list (sweep confirm): empty
			IsTruncated: false,
		},
	}
	f, err := New(be, Config{Bucket: "b"}, WithAuthenticator(newStubAuth()))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "/dir/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("status = %d, want 204; body=%.200s", rec.Code, rec.Body.String())
	}
	deleted := map[string]bool{}
	for _, c := range be.calls {
		if key, found := strings.CutPrefix(c, "delete:"); found {
			deleted[key] = true
		}
	}
	if !deleted["b/dir/p1.txt"] || !deleted["b/dir/p2.txt"] {
		t.Fatalf("pagination dropped keys; deletes = %v", deleted)
	}
}

func TestDELETE_Root403_BothModes(t *testing.T) {
	fA, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("DELETE", "/", nil)
	rec := httptest.NewRecorder()
	fA.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("mode A root: status = %d, want 403", rec.Code)
	}
	fB, _ := newTestFrontend(Config{Bucket: "b"})
	req = httptest.NewRequest("DELETE", "/", nil)
	rec = httptest.NewRecorder()
	fB.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("mode B root: status = %d, want 403", rec.Code)
	}
}

func TestDELETE_BucketModeA403(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	req := httptest.NewRequest("DELETE", "/photos/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (bucket deletion is not expressible over WebDAV)", rec.Code)
	}
}

func TestDELETE_EmptyVirtualCollection404(t *testing.T) {
	// A collection with no keys under the prefix and no marker object is
	// invisible: DELETE misses ⇒ 404.
	f, be := newTestFrontend(Config{})
	be.seed("photos", "elsewhere.txt", []byte("x"))
	req := httptest.NewRequest("DELETE", "/photos/ghost/", nil)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
