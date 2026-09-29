package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// TestBuildBackendLookupCustomBucketPathParity pins the bughunt-D1 fix: a
// custom bucket's seam placement must equal the handler-side getBucketPath
// math exactly — <custom>/key, with NO <bucket> nesting. Before the fix the
// FS backend joined root+bucket, so Put landed nested while multipart
// staging / copy-meta / sweep addressed the flat path (split-brain).
func TestBuildBackendLookupCustomBucketPathParity(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "mydata") // need not pre-exist
	cfg := ServerConfig{
		DataDir: t.TempDir(),
		Buckets: map[string]string{"photos": custom},
	}
	lookup, err := buildBackendLookup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lookup("photos")
	if err != nil {
		t.Fatal(err)
	}

	const key = "prefix/obj.bin"
	const body = "flat-custom-path"
	obj, err := b.Put(context.Background(), "photos", key, strings.NewReader(body), int64(len(body)), objectmodel.PutOptions{})
	if err != nil {
		t.Fatalf("Put via seam: %v", err)
	}
	if obj.ETag == "" {
		t.Fatal("empty ETag")
	}

	// The handler path (getBucketPath semantics) and ONLY that path must
	// hold the object.
	wantPath := filepath.Join(getBucketPathForTest(cfg, "photos"), key)
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("object not at handler path %s: %v", wantPath, err)
	}
	nested := filepath.Join(custom, "photos", key)
	if _, err := os.Stat(nested); !os.IsNotExist(err) {
		t.Fatalf("object nested at %s — split-brain regression", nested)
	}

	// Get/Stat/Delete/List resolve the same flat path.
	rc, _, err := b.Get(context.Background(), "photos", key, objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != body {
		t.Fatalf("Get body = %q, want %q", data, body)
	}
	st, err := b.Stat(context.Background(), "photos", key)
	if err != nil || st.Size != int64(len(body)) {
		t.Fatalf("Stat = (%+v, %v), want size %d", st, err, len(body))
	}
	page, err := b.List(context.Background(), "photos", objectmodel.ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != key {
		t.Fatalf("List = %+v, want [%s]", page.Objects, key)
	}
	if err := b.Delete(context.Background(), "photos", key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatalf("object still present after Delete: %v", err)
	}
}

// TestBuildBackendLookupDefaultBucketNestedLayout pins the OTHER half: the
// dataDir-rooted default (no custom path) keeps join(dataDir, bucket).
func TestBuildBackendLookupDefaultBucketNestedLayout(t *testing.T) {
	dataDir := t.TempDir()
	cfg := ServerConfig{DataDir: dataDir}
	lookup, err := buildBackendLookup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lookup("autobkt")
	if err != nil {
		t.Fatal(err)
	}
	const key = "k.txt"
	if _, err := b.Put(context.Background(), "autobkt", key, strings.NewReader("x"), 1, objectmodel.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dataDir, "autobkt", key)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("default-layout object missing at %s: %v", want, err)
	}
}

// TestGetBucketPathCustomAndDefault pins the handler-side resolver values
// the single-bucket backend must match (wiring-independent math).
func TestGetBucketPathCustomAndDefault(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "mydata")
	cfg := ServerConfig{
		DataDir: t.TempDir(),
		Buckets: map[string]string{"photos": custom},
	}
	if got := getBucketPathForTest(cfg, "photos"); got != custom {
		t.Fatalf("custom getBucketPath = %q, want %q", got, custom)
	}
	if got := getBucketPathForTest(cfg, "other"); got != filepath.Join(cfg.DataDir, "other") {
		t.Fatalf("default getBucketPath = %q, want %q", got, filepath.Join(cfg.DataDir, "other"))
	}
}

// getBucketPathForTest applies getBucketPath's precedence to an explicit
// config (the production getBucketPath reads serverConfig; tests here must
// stay hermetic).
func getBucketPathForTest(cfg ServerConfig, bucket string) string {
	if p, ok := cfg.Buckets[bucket]; ok {
		return p
	}
	return filepath.Join(cfg.DataDir, bucket)
}
