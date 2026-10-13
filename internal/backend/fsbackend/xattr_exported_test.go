package fsbackend

// xattr_exported_test.go — covers the exported breadcrumb seams used by the
// above-seam multipart assembly (StampOwnerIfAbsent / StampOwnerCarried /
// CarryWriterBreadcrumbs / StampWriter) plus Root(), the FS.Owner enrichment
// accessor and the atomic write helpers' error paths. Same temp-dir + real
// xattr pattern as xattr_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// TestExportedOwnerStamps pins the exported seam semantics against a REAL
// file: set-once for StampOwnerIfAbsent, carried-else-creator for
// StampOwnerCarried, and the multipart-op breadcrumb from StampWriter.
func TestExportedOwnerStamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obj.bin")
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	// StampOwnerIfAbsent: creator recorded once; a second call never
	// overwrites.
	StampOwnerIfAbsent(path, "alice")
	if owner, ok := xget(t, path, xattrOwner); !ok || owner != "alice" {
		t.Fatalf("owner after StampOwnerIfAbsent = %q ok=%v, want alice", owner, ok)
	}
	StampOwnerIfAbsent(path, "bob")
	if owner, _ := xget(t, path, xattrOwner); owner != "alice" {
		t.Fatalf("owner after second StampOwnerIfAbsent = %q, want alice (set-once)", owner)
	}

	// StampOwnerCarried with no prior owner: the writer becomes the owner.
	path2 := filepath.Join(dir, "fresh.bin")
	if err := os.WriteFile(path2, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	StampOwnerCarried(path2, "carol", "")
	if owner, ok := xget(t, path2, xattrOwner); !ok || owner != "carol" {
		t.Fatalf("owner after carried-create = %q ok=%v, want carol", owner, ok)
	}

	// StampOwnerCarried with a prior owner: the PRIOR owner survives the
	// overwrite even by a different principal.
	StampOwnerCarried(path2, "dave", "carol")
	if owner, _ := xget(t, path2, xattrOwner); owner != "carol" {
		t.Fatalf("owner after carried-overwrite = %q, want carol (carried)", owner)
	}

	// StampWriter records the multipart breadcrumb for the principal.
	StampWriter(path2, "dave")
	v, ok := xget(t, path2, WriterXattrName("dave"))
	if !ok || !strings.HasPrefix(v, xattrOpMultipart+"@") {
		t.Fatalf("dave writer xattr = %q ok=%v, want multipart@...", v, ok)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimPrefix(v, xattrOpMultipart+"@")); err != nil {
		t.Fatalf("multipart timestamp not RFC3339: %q (%v)", v, err)
	}
}

// TestCarryWriterBreadcrumbsRoundTrip pins the snapshot/re-apply pair the
// multipart assembly uses across the atomic rename: CollectBreadcrumbNames
// before the write, write a fresh inode, CarryWriterBreadcrumbs after —
// the prior stamps must survive byte-for-byte.
func TestCarryWriterBreadcrumbsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "obj.bin")
	if err := os.WriteFile(old, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	StampOwnerIfAbsent(old, "alice")
	StampWriter(old, "bob")

	owner, writers := CollectBreadcrumbNames(old)
	if owner != "alice" {
		t.Fatalf("collected owner = %q, want alice", owner)
	}
	if len(writers) != 1 || writers[0].name != WriterXattrName("bob") {
		t.Fatalf("collected writers = %+v, want exactly bob's stamp", writers)
	}

	// Atomic replace with a brand-new inode, then re-apply.
	fresh := filepath.Join(dir, "obj.tmp")
	if err := os.WriteFile(fresh, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	StampOwnerCarried(fresh, "bob", owner)
	CarryWriterBreadcrumbs(fresh, writers)
	if err := os.Rename(fresh, old); err != nil {
		t.Fatal(err)
	}

	if o, _ := xget(t, old, xattrOwner); o != "alice" {
		t.Errorf("owner after carry = %q, want alice", o)
	}
	bv, ok := xget(t, old, WriterXattrName("bob"))
	if !ok || bv != writers[0].value {
		t.Errorf("carried writer stamp = %q ok=%v, want %q", bv, ok, writers[0].value)
	}
}

// TestCollectBreadcrumbNamesEmptyAndForeign covers the snapshot on a plain
// file (nothing collected) and that non-zeta xattrs are never swept up.
func TestCollectBreadcrumbNamesEmptyAndForeign(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.bin")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setXattr(path, "user.comment", "hello"); err != nil {
		t.Fatalf("seed foreign xattr: %v", err)
	}
	owner, writers := CollectBreadcrumbNames(path)
	if owner != "" {
		t.Errorf("owner = %q, want empty on a plain file", owner)
	}
	if len(writers) != 0 {
		t.Errorf("writers = %+v, want none (foreign xattrs are not breadcrumbs)", writers)
	}

	// A writer stamp plus a foreign xattr: only the writer stamp collects.
	StampWriter(path, "zoe")
	_, writers = CollectBreadcrumbNames(path)
	if len(writers) != 1 || writers[0].name != WriterXattrName("zoe") {
		t.Errorf("writers = %+v, want exactly zoe's stamp", writers)
	}
}

// TestCollectBreadcrumbNamesMissingFile covers the absent-file path: no
// panic, empty snapshot.
func TestCollectBreadcrumbNamesMissingFile(t *testing.T) {
	owner, writers := CollectBreadcrumbNames(filepath.Join(t.TempDir(), "absent"))
	if owner != "" || len(writers) != 0 {
		t.Errorf("snapshot on a missing file = (%q, %+v), want empty", owner, writers)
	}
}

// TestFSRootAndOwner pins Root() and the FS.Owner shadow-aware enrichment
// accessor through a real bucket + sidecar.
func TestFSRootAndOwner(t *testing.T) {
	dir := t.TempDir()
	f, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Root(); got != dir {
		// New resolves to an absolute path; on macOS /tmp is a symlink to
		// /private/tmp so compare after resolving both.
		want, rerr := filepath.EvalSymlinks(dir)
		if rerr != nil {
			t.Fatalf("EvalSymlinks: %v", rerr)
		}
		gotEval, gerr := filepath.EvalSymlinks(got)
		if gerr != nil || gotEval != want {
			t.Fatalf("Root() = %q, want %q", got, dir)
		}
	}

	if _, err := f.Put(context.Background(), "bkt", "own.txt",
		strings.NewReader("v"), 1, objectmodel.PutOptions{Principal: "alice"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if owner, ok := f.Owner("bkt", "own.txt"); !ok || owner != "alice" {
		t.Fatalf("FS.Owner = %q ok=%v, want alice", owner, ok)
	}

	// Invalid key: degrade to ("", false), never fabricate.
	if owner, ok := f.Owner("bkt", "../escape"); ok || owner != "" {
		t.Errorf("Owner with an invalid key = %q ok=%v, want \"\",false", owner, ok)
	}
	// Missing object: degrade to ("", false).
	if owner, ok := f.Owner("bkt", "absent.txt"); ok || owner != "" {
		t.Errorf("Owner of a missing object = %q ok=%v, want \"\",false", owner, ok)
	}
}

// TestWriteFileAtomicRoundTripAndFailures covers the happy path plus the
// error branches of the atomic write helper (unwritable directory, rename
// over an occupied directory path).
func TestWriteFileAtomicRoundTripAndFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obj.txt")
	if err := writeFileAtomic(path, []byte("round trip"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "round trip" {
		t.Fatalf("read back = %q err=%v, want %q", got, err, "round trip")
	}
	// No temp residue left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp residue %q left behind", e.Name())
		}
	}

	// Unwritable target directory: the temp file cannot even be created.
	unwritable := filepath.Join(dir, "ro")
	if err := os.Mkdir(unwritable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unwritable, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o755) })
	if err := writeFileAtomic(filepath.Join(unwritable, "x"), []byte("y"), 0o644); err == nil {
		t.Error("writeFileAtomic into an unwritable dir: expected an error, got nil")
	}

	// Rename over an existing DIRECTORY fails on every platform.
	if err := os.Mkdir(filepath.Join(dir, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "occupied"), []byte("y"), 0o644); err == nil {
		t.Error("writeFileAtomic over a directory: expected an error, got nil")
	}

	// writeFileAtomicJSON round-trips through the two-space indent form.
	type doc struct {
		Name string `json:"name"`
	}
	jpath := filepath.Join(dir, "meta.json")
	if err := writeFileAtomicJSON(jpath, doc{Name: "k"}, 0o644); err != nil {
		t.Fatalf("writeFileAtomicJSON: %v", err)
	}
	raw, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{\n  \"name\": \"k\"\n}" {
		t.Errorf("JSON body = %q, want the MarshalIndent two-space form", raw)
	}
}
