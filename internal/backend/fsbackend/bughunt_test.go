// Package fsbackend — bughunt_test.go: pins for bughunt findings B5, B6,
// B7, B9, and B11 (B3's open-under-locks is exercised by the conformance
// suite's ConcurrentSameKey pin under -race; a targeted TOCTOU repro would
// be inherently flaky, so the fix is documented in object.go instead).
package fsbackend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// mustPut is a Put helper that fails the test on error.
func mustPut(t *testing.T, f *FS, bucket, key, body string) objectmodel.Object {
	t.Helper()
	obj, err := f.Put(context.Background(), bucket, key, strings.NewReader(body), int64(len(body)), objectmodel.PutOptions{})
	if err != nil {
		t.Fatalf("Put %q: %v", key, err)
	}
	return obj
}

// --- B5: key aliasing — non-canonical keys are rejected ----------------------

func TestValidateKeyRejectsNonCanonicalForms(t *testing.T) {
	rejects := []string{
		"a//b",     // doubled slash aliases a/b
		"a/./b",    // dot segment aliases a/b
		"/abs",     // leading slash aliases abs
		"a/",       // trailing slash aliases a
		"a/b/",     // trailing slash
		"a//b//c",  // multiple doubled slashes
		"./a",      // leading dot segment
		"a/b/../c", // Clean collapses (also caught by the ".." rule)
	}
	for _, key := range rejects {
		if err := validateKey(key); err == nil {
			t.Errorf("validateKey(%q) = nil, want rejection (aliases a Clean-collapsed path)", key)
		}
	}
	accepts := []string{"a", "a/b", "a/b/c", "x.txt", "dir/file.tar.gz", "a b/c d"}
	for _, key := range accepts {
		if err := validateKey(key); err != nil {
			t.Errorf("validateKey(%q) = %v, want nil", key, err)
		}
	}
}

func TestPutRejectsAliasingKeys(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustPut(t, f, "bkt", "a/b", "canonical")
	for _, key := range []string{"a//b", "a/./b", "/a/b", "a/b/"} {
		if _, err := f.Put(ctx, "bkt", key, strings.NewReader("x"), 1, objectmodel.PutOptions{}); err == nil {
			t.Errorf("Put(%q) = nil error, want InvalidArgument (would alias a/b)", key)
		}
	}
	// The canonical object is untouched and readable.
	if _, err := f.Stat(ctx, "bkt", "a/b"); err != nil {
		t.Errorf("Stat a/b after alias-rejections: %v", err)
	}
}

// --- B6: Delete ENOTDIR is idempotent (204-class nil) ------------------------

func TestDeleteENOTDIRIsIdempotentNoop(t *testing.T) {
	root := t.TempDir()
	f, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	bucketDir := filepath.Join(root, "bkt")
	if err := os.MkdirAll(filepath.Join(bucketDir, ".metadata", "a"), 0755); err != nil {
		t.Fatal(err)
	}
	// A FILE occupies the "a" prefix of the bucket: os.Remove(<bucket>/a/b)
	// on macOS/Linux yields ENOTDIR, not ENOENT.
	if err := os.WriteFile(filepath.Join(bucketDir, "a"), []byte("prefix-file"), 0644); err != nil {
		t.Fatal(err)
	}
	// Missing sidecar path: Delete must still be a nil no-op (B6), not 500.
	if err := f.Delete(context.Background(), "bkt", "a/b"); err != nil {
		t.Errorf("Delete with ENOTDIR ancestor = %v, want nil (S3 idempotent 204)", err)
	}
}

// --- B7: Get/Stat ENOTDIR maps to NoSuchKey, not 500 -------------------------

func TestStatGetENOTDIRIsNoSuchKey(t *testing.T) {
	root := t.TempDir()
	f, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustPut(t, f, "bkt", "a/b", "data") // sidecar exists at .metadata/a/b.meta
	// Now drop a FILE at <bucket>/a, shadowing the directory: data stat
	// and open hit ENOTDIR.
	bucketDir := filepath.Join(root, "bkt")
	if err := os.RemoveAll(filepath.Join(bucketDir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketDir, "a"), []byte("prefix-file"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat(ctx, "bkt", "a/b"); err == nil {
		t.Error("Stat with ENOTDIR data path = nil error, want NoSuchKey")
	} else if omErr := (*objectmodel.Error)(nil); !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
		t.Errorf("Stat ENOTDIR error = %v, want code NoSuchKey", err)
	}
	if _, _, err := f.Get(ctx, "bkt", "a/b", objectmodel.GetOptions{}); err == nil {
		t.Error("Get with ENOTDIR data path = nil error, want NoSuchKey")
	} else if omErr := (*objectmodel.Error)(nil); !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
		t.Errorf("Get ENOTDIR error = %v, want code NoSuchKey", err)
	}
}

// --- B9: sidecar storagePath outside the bucket root is ignored --------------

func TestResolveDataPathIgnoresEscapingSidecarPath(t *testing.T) {
	root := t.TempDir()
	f, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustPut(t, f, "bkt", "k", "payload")

	// Craft a sidecar whose storagePath is an absolute path OUTSIDE the
	// bucket root. Get/Stat must fall back to the canonical layout and
	// serve the canonical file — never read (or delete) the foreign path.
	outside := filepath.Join(t.TempDir(), "evil.bin")
	if err := os.WriteFile(outside, []byte("DO-NOT-SERVE"), 0644); err != nil {
		t.Fatal(err)
	}
	metaPath := sidecarPath(filepath.Join(root, "bkt"), "k")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta legacyMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	meta.StoragePath = outside
	if err := writeFileAtomicJSON(metaPath, meta, 0644); err != nil {
		t.Fatal(err)
	}

	obj, err := f.Stat(ctx, "bkt", "k")
	if err != nil {
		t.Fatalf("Stat with escaping storagePath: %v", err)
	}
	if obj.Size != int64(len("payload")) {
		t.Errorf("Stat size = %d, want %d (canonical file served, not the outside path)", obj.Size, len("payload"))
	}
	rc, _, err := f.Get(ctx, "bkt", "k", objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("Get with escaping storagePath: %v", err)
	}
	buf := make([]byte, 32)
	n, _ := rc.Read(buf)
	_ = rc.Close()
	if string(buf[:n]) != "payload" {
		t.Errorf("Get body = %q, want %q (canonical fallback)", buf[:n], "payload")
	}

	// Delete with the same crafted sidecar must remove the canonical data
	// file and leave the outside path untouched.
	if err := f.Delete(ctx, "bkt", "k"); err != nil {
		t.Fatalf("Delete with escaping storagePath: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bkt", "k")); !os.IsNotExist(err) {
		t.Errorf("canonical data file after Delete: %v, want gone", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside file was touched by Delete: %v", err)
	}
}

// --- B11: Put size cap --------------------------------------------------------

func TestPutRejectsBodyOverCap(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.maxPutBytes = 8 // shrink the cap for the test
	ctx := context.Background()
	// Declared size over the cap: rejected before reading.
	if _, err := f.Put(ctx, "bkt", "big", strings.NewReader("0123456789"), 10, objectmodel.PutOptions{}); err == nil {
		t.Error("Put with size 10 > cap 8 = nil error, want InvalidArgument")
	}
	// Undeclared size (size < 0): rejected after reading through the cap.
	if _, err := f.Put(ctx, "bkt", "big", strings.NewReader("0123456789"), -1, objectmodel.PutOptions{}); err == nil {
		t.Error("Put with 10-byte body > cap 8 = nil error, want InvalidArgument")
	}
	// Exactly at the cap: accepted.
	mustPut(t, f, "bkt", "ok", "01234567")
}

func TestPutCapConfigurableViaOption(t *testing.T) {
	root := t.TempDir()
	fn, err := backend.Lookup("fs")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fn(backend.BackendConfig{Root: root, Options: map[string]string{optMaxPutBytes: "16"}})
	if err != nil {
		t.Fatal(err)
	}
	f := b.(*FS)
	if f.maxPutBytes != 16 {
		t.Errorf("maxPutBytes = %d, want 16 (from max_put_bytes option)", f.maxPutBytes)
	}
	// Invalid/zero option values fall back to the default.
	b2, err := fn(backend.BackendConfig{Root: root, Options: map[string]string{optMaxPutBytes: "not-a-number"}})
	if err != nil {
		t.Fatal(err)
	}
	if b2.(*FS).maxPutBytes != maxPutBytesDefault {
		t.Errorf("maxPutBytes = %d, want default %d for unparseable option", b2.(*FS).maxPutBytes, maxPutBytesDefault)
	}
	b3, err := fn(backend.BackendConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if b3.(*FS).maxPutBytes != maxPutBytesDefault {
		t.Errorf("maxPutBytes = %d, want default %d when option unset", b3.(*FS).maxPutBytes, maxPutBytesDefault)
	}
}

// TestValidateKeyDotKeyAllowed pins the regression-review fix: the bare
// S3-legal key "." must not be rejected by the canonical-form rule
// (Clean folds "/." to "/"). It cannot alias any other canonical key.
func TestValidateKeyDotKeyAllowed(t *testing.T) {
	if err := validateKey("."); err != nil {
		t.Fatalf(`validateKey(".") = %v, want nil`, err)
	}
	// The reject set must still hold.
	for _, k := range []string{"a//b", "a/./b", "/abs", "a/", "./a", "..", "a/../b"} {
		if err := validateKey(k); err == nil {
			t.Fatalf("validateKey(%q) = nil, want rejection", k)
		}
	}
}
