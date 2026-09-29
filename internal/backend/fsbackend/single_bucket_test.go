// Package fsbackend — single_bucket_test.go: pins the custom-bucket layout
// (bughunt D1 fix). A single-bucket FS (NewAt) must resolve every object
// operation to <root>/key — the exact path the handler-side getBucketPath
// math reports — with NO <bucket> nesting. The default New backend keeps
// join(root, bucket) unchanged.
package fsbackend

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// putGetDeleteList drives one full CRUD+List round through the Backend
// interface and asserts on-disk placement at wantPath.
func putGetDeleteList(t *testing.T, b *FS, bucket, wantPath string) {
	t.Helper()
	ctx := context.Background()
	const key = "dir/nested/obj.txt"
	const body = "custom-bucket payload"

	obj, err := b.Put(ctx, bucket, key, strings.NewReader(body), int64(len(body)), objectmodel.PutOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if obj.ETag == "" {
		t.Fatal("Put returned empty ETag")
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("object not placed at custom path %s: %v", wantPath, err)
	}

	rc, _, err := b.Get(ctx, bucket, key, objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if string(data) != body {
		t.Fatalf("Get body = %q, want %q", data, body)
	}

	st, err := b.Stat(ctx, bucket, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size != int64(len(body)) {
		t.Fatalf("Stat size = %d, want %d", st.Size, len(body))
	}

	page, err := b.List(ctx, bucket, objectmodel.ListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != key {
		t.Fatalf("List = %+v, want single key %q", page.Objects, key)
	}

	if err := b.Delete(ctx, bucket, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatalf("data file still present after Delete: %v", err)
	}
}

// TestNewAtCustomBucketLayout: single-bucket mode places objects at
// <root>/key with NO bucket nesting, across Put/Get/Stat/List/Delete.
func TestNewAtCustomBucketLayout(t *testing.T) {
	root := t.TempDir()
	b, err := NewAt(root, "photos")
	if err != nil {
		t.Fatal(err)
	}
	putGetDeleteList(t, b, "photos", filepath.Join(root, "dir", "nested", "obj.txt"))
}

// TestNewAtMultipartCompleteLayout: the above-seam multipart-complete flow
// writes the final object at getBucketPath = <custom>/key (flat) with a
// sidecar at <custom>/.metadata/<key>.meta; a following Get must serve it.
// Before the D1 fix this Get resolved nested and 404'd.
func TestNewAtMultipartCompleteLayout(t *testing.T) {
	root := t.TempDir()
	b, err := NewAt(root, "media")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const key = "assembled.bin"
	const body = "assembled-multipart-bytes"

	// Simulate exactly what the handler-side complete does: flat data file
	// under the bucket path + legacy sidecar in .metadata.
	finalPath := filepath.Join(root, key)
	if err := os.WriteFile(finalPath, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	metaDir := filepath.Join(root, metadataDirName)
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"contentType":"application/octet-stream","contentLength":` +
		`25,"eTag":"deadbeef","customMetadata":{},"lastModified":"2026-09-29T00:00:00Z","storagePath":"` +
		finalPath + `"}`
	if err := os.WriteFile(filepath.Join(metaDir, key+".meta"), []byte(sidecar), 0644); err != nil {
		t.Fatal(err)
	}

	rc, got, err := b.Get(ctx, "media", key, objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("Get after flat multipart-complete: %v (D1 split-brain: nested resolution would 404 here)", err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if string(data) != body {
		t.Fatalf("Get body = %q, want %q", data, body)
	}
	if got.Size != int64(len(body)) {
		t.Fatalf("Get size = %d, want %d", got.Size, len(body))
	}
}

// TestNewDefaultKeepsNestedLayout: the dataDir-rooted default backend must
// keep the frozen join(root, bucket) layout byte-for-byte.
func TestNewDefaultKeepsNestedLayout(t *testing.T) {
	root := t.TempDir()
	b, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	putGetDeleteList(t, b, "auto", filepath.Join(root, "auto", "dir", "nested", "obj.txt"))
}

// TestNewAtRejectsEmptyRoot mirrors New's guard.
func TestNewAtRejectsEmptyRoot(t *testing.T) {
	if _, err := NewAt("", "photos"); err == nil {
		t.Fatal(`NewAt("", "photos") = nil error, want error`)
	}
}

// TestFSConstructorSingleBucketOption: the registry-visible "fs" constructor
// switches into single-bucket mode when the option is set — the exact
// backend_lookup.go wiring path for custom buckets.
func TestFSConstructorSingleBucketOption(t *testing.T) {
	root := t.TempDir()
	fn, err := backend.Lookup("fs")
	if err != nil {
		t.Fatalf(`Lookup("fs"): %v`, err)
	}
	b, err := fn(backend.BackendConfig{
		Type:    "fs",
		Root:    root,
		Options: map[string]string{OptSingleBucketBucket: "photos"},
	})
	if err != nil {
		t.Fatalf("fs constructor with single-bucket option: %v", err)
	}
	fs, ok := b.(*FS)
	if !ok {
		t.Fatalf("constructor returned %T, want *FS", b)
	}
	if fs.singleBucket != "photos" {
		t.Fatalf("singleBucket = %q, want photos", fs.singleBucket)
	}
}
