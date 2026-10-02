package fsbackend

// xattr_test.go — principal breadcrumb unit tests (auth extensions leaf
// 10). Uses temp dirs + REAL file xattrs (works on macOS dev and Linux CI,
// same pattern as the rest of the package's tests).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// xget is the test-side raw xattr read (independent of the production
// helpers so the tests pin the contract, not the implementation).
func xget(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	v, err := getXattr(path, name)
	if err != nil {
		if isErrXattrNotFound(err) {
			return "", false
		}
		t.Fatalf("getXattr %s on %s: %v", name, path, err)
	}
	return v, true
}

func putWithPrincipal(t *testing.T, dir, key, body, principal string) string {
	t.Helper()
	f, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Put(context.Background(), "bkt", key, bytes.NewReader([]byte(body)),
		int64(len(body)), objectmodel.PutOptions{Principal: principal})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return filepath.Join(dir, "bkt", key)
}

// TestOwnerSetOnceAtCreate pins design 2a: the creator's AccessKeyID lands
// in user.zeta.owner and a second writer overwriting the object NEVER
// changes it.
func TestOwnerSetOnceAtCreate(t *testing.T) {
	dir := t.TempDir()
	path := putWithPrincipal(t, dir, "obj.txt", "v1", "alice")
	if owner, ok := xget(t, path, xattrOwner); !ok || owner != "alice" {
		t.Fatalf("owner after create = %q ok=%v, want alice", owner, ok)
	}
	// Second writer overwrites the same key.
	f, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(context.Background(), "bkt", "obj.txt",
		strings.NewReader("v2"), 2, objectmodel.PutOptions{Principal: "bob"}); err != nil {
		t.Fatal(err)
	}
	if owner, ok := xget(t, path, xattrOwner); !ok || owner != "alice" {
		t.Fatalf("owner after overwrite = %q ok=%v, want alice (set-once)", owner, ok)
	}
}

// TestWriterBreadcrumbsAccumulate pins the per-principal xattr names: two
// writers produce TWO distinct xattrs (no shared read-modify-write list).
func TestWriterBreadcrumbsAccumulate(t *testing.T) {
	dir := t.TempDir()
	path := putWithPrincipal(t, dir, "obj.txt", "v1", "alice")
	f, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Put(context.Background(), "bkt", "obj.txt",
		strings.NewReader("v2"), 2, objectmodel.PutOptions{Principal: "bob"}); err != nil {
		t.Fatal(err)
	}
	av, ok := xget(t, path, WriterXattrName("alice"))
	if !ok || !strings.HasPrefix(av, xattrOpPut+"@") {
		t.Fatalf("alice writer xattr = %q ok=%v, want put@...", av, ok)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimPrefix(av, xattrOpPut+"@")); err != nil {
		t.Fatalf("writer timestamp not RFC3339: %q (%v)", av, err)
	}
	bv, ok := xget(t, path, WriterXattrName("bob"))
	if !ok || !strings.HasPrefix(bv, xattrOpPut+"@") {
		t.Fatalf("bob writer xattr = %q ok=%v, want put@...", bv, ok)
	}
}

// TestNoPrincipalNoStamps pins the advisory semantics: Principal == ""
// (legacy callers, non-S3 frontends) stamps nothing.
func TestNoPrincipalNoStamps(t *testing.T) {
	dir := t.TempDir()
	path := putWithPrincipal(t, dir, "obj.txt", "v1", "")
	if _, ok := xget(t, path, xattrOwner); ok {
		t.Fatal("owner stamped without a principal")
	}
	names, err := listXattrNames(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "user.zeta.") {
			t.Fatalf("unexpected breadcrumb %q without principal", n)
		}
	}
}

// TestStampFailOpen pins the fail-open contract: a stamp target that cannot
// carry xattrs (a directory) never fails the operation — the stamp helpers
// just log and return.
func TestStampFailOpen(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dir-target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Must not panic; must not return an error path (these helpers return
	// nothing — fail-open is structural — so exercise them directly).
	stampOwnerIfAbsentBreadcrumb(target, "alice")
	stampWriterBreadcrumb(target, "alice", xattrOpPut)
}

// TestOwnerXattrReader covers the enrichment accessor: present when the
// xattr exists, absent-not-fabricated otherwise.
func TestOwnerXattrReader(t *testing.T) {
	dir := t.TempDir()
	path := putWithPrincipal(t, dir, "with.txt", "v", "alice")
	if owner, ok := OwnerXattr(path); !ok || owner != "alice" {
		t.Fatalf("OwnerXattr = %q ok=%v, want alice", owner, ok)
	}
	plain := filepath.Join(dir, "bkt", "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := OwnerXattr(plain); ok {
		t.Fatal("OwnerXattr fabricated an owner for an unstamped object")
	}
}
