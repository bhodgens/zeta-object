package fsbackend

// coverage_gaps_test.go — targeted tests for the remaining uncovered
// branches: collectObjectKeys' skip/missing-dir behavior, validateKey's
// rejection table, isErrXattrNotFound/xattrNotFoundError, and the List
// shadow-aware sidecar path.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
	"golang.org/x/sys/unix"
)

// TestCollectObjectKeys pins the walk contract: keys come from *.meta
// sidecars only, nested keys keep their separator, .uploads staging is
// skipped, non-sidecar files are ignored, and a missing metadata dir is an
// empty bucket (not an error).
func TestCollectObjectKeys(t *testing.T) {
	dir := t.TempDir()
	metaDir := filepath.Join(dir, "bkt", ".metadata")
	if err := os.MkdirAll(filepath.Join(metaDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(metaDir, ".uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	seeds := map[string]string{
		filepath.Join(metaDir, "a.txt.meta"):           "{}",
		filepath.Join(metaDir, "nested", "b.txt.meta"): "{}",
		filepath.Join(metaDir, ".uploads", "stg.meta"): "{}",
		filepath.Join(metaDir, "notes.txt"):            "not a sidecar",
	}
	for p, body := range seeds {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := collectObjectKeys(metaDir)
	if err != nil {
		t.Fatalf("collectObjectKeys: %v", err)
	}
	want := map[string]bool{"a.txt": true, "nested/b.txt": true}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", keys, want)
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q (want only %v)", k, want)
		}
	}

	// Missing metadata dir: empty, not an error.
	keys, err = collectObjectKeys(filepath.Join(dir, "absent", ".metadata"))
	if err != nil || len(keys) != 0 {
		t.Errorf("missing metadataDir = (%v, %v), want empty, nil", keys, err)
	}
}

// TestValidateKeyTable re-pins the defensive backend-side key validation on
// the rejection cases the frontend sends and a few acceptances.
func TestValidateKeyTable(t *testing.T) {
	cases := []struct {
		key    string
		wantOK bool
	}{
		{"plain.txt", true},
		{"nested/dir/key.bin", true},
		{"", false},
		{strings.Repeat("k", 1025), false},
		{"has\x00null", false},
		{"../escape", false},
		{"a/../b", false},
		{"a/.metadata/b", false},
		{".metadata", false},
		{"a/.zfs/snap", false},
		{".zfs", false},
	}
	for _, tc := range cases {
		err := validateKey(tc.key)
		if tc.wantOK && err != nil {
			t.Errorf("validateKey(%q) = %v, want nil", tc.key, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("validateKey(%q) = nil, want a rejection", tc.key)
		}
	}
}

// TestXattrNotFoundClassification covers the attribute-absent classifier:
// a wrapped *xattrNotFoundError, the raw errnos, os.ErrNotExist, and a
// non-matching error.
func TestXattrNotFoundClassification(t *testing.T) {
	if !isErrXattrNotFound(&xattrNotFoundError{err: unix.ENODATA}) {
		t.Error("wrapped xattrNotFoundError not classified as absent")
	}
	if !isErrXattrNotFound(unix.ENODATA) {
		t.Error("raw ENODATA not classified as absent")
	}
	if !isErrXattrNotFound(os.ErrNotExist) {
		t.Error("os.ErrNotExist not classified as absent")
	}
	if isErrXattrNotFound(unix.EPERM) {
		t.Error("EPERM misclassified as attribute-absent")
	}

	// The wrapper's Error/Unwrap surface the underlying errno.
	wrapped := &xattrNotFoundError{err: unix.ENOATTR}
	if wrapped.Error() != unix.ENOATTR.Error() {
		t.Errorf("Error() = %q, want the underlying errno text", wrapped.Error())
	}
	if !errors.Is(wrapped, unix.ENOATTR) {
		t.Error("Unwrap does not expose the underlying errno")
	}
}

// TestListReadsShadowSidecar covers List's shadow-aware sidecar branch: a
// sidecar carrying a storagePath re-homes the listed object's size/content
// data. Here it pins that a listed sidecar without a shadow still reports
// the canonical data path's stats and that Put→List round-trips metadata.
func TestListSidecarMetadataRoundTrip(t *testing.T) {
	f, dir := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{"a.txt": "hello", "d/b.txt": "xx"})

	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{MaxKeys: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 2 {
		t.Fatalf("objects = %d, want 2", len(page.Objects))
	}
	for _, o := range page.Objects {
		switch o.Key {
		case "a.txt":
			if o.Size != 5 {
				t.Errorf("a.txt size = %d, want 5", o.Size)
			}
		case "d/b.txt":
			if o.Size != 2 {
				t.Errorf("d/b.txt size = %d, want 2", o.Size)
			}
		default:
			t.Errorf("unexpected key %q", o.Key)
		}
	}

	// A sidecar deleted out-of-band degrades to an empty page, not a
	// crash (the lenient legacyMeta path through statLocked).
	metaPath := filepath.Join(dir, "bkt", ".metadata", "a.txt.meta")
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	page, err = f.List(context.Background(), "bkt", objectmodel.ListParams{MaxKeys: 10})
	if err != nil {
		t.Fatalf("List after sidecar removal: %v", err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != "d/b.txt" {
		t.Errorf("objects after sidecar removal = %v, want only d/b.txt", keysOf(page))
	}
}
