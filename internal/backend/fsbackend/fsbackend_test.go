// Package fsbackend — fsbackend_test.go: constructor, interface assertion,
// registration, capabilities, bucket discovery.
package fsbackend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
)

func TestNewRootedAndInterface(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var _ backend.Backend = f // compile-time assertion (mirrored in fsbackend.go)
	_ = f.Capabilities()      // must not panic; flags are backend-owned
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") = nil error, want error")
	}
}

func TestRegisterFS(t *testing.T) {
	fn, err := backend.Lookup("fs")
	if err != nil {
		t.Fatalf("Lookup(\"fs\"): %v — init() registration missing", err)
	}
	b, err := fn(backend.BackendConfig{Type: "fs", Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fs constructor: %v", err)
	}
	if _, ok := b.(*FS); !ok {
		t.Fatalf("fs constructor returned %T, want *FS", b)
	}
}

func TestBucketsDiscoversDirs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(root, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// A file is NOT a bucket; a hidden dir is NOT a bucket.
	if err := os.WriteFile(filepath.Join(root, "plainfile"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0755); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory IS a bucket (discovery follows symlinks).
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	f, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := f.Buckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	want := []string{"alpha", "beta", "linked"}
	if len(names) != len(want) {
		t.Fatalf("buckets = %v, want %v (sorted, dirs+symlinks only)", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("buckets = %v, want %v", names, want)
		}
	}
}

func TestBucketsEmptyRoot(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := f.Buckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 {
		t.Fatalf("buckets = %v, want empty", buckets)
	}
}
