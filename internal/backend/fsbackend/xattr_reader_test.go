package fsbackend

// xattr_reader_test.go — auditReads read-path breadcrumb tests (auth
// extensions leaf 10): first-read-per-principal-only stamping, fail-open.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReaderStampFirstReadOnly pins design 2a: the FIRST read stamps
// user.zeta.reader.<principal>; subsequent reads see it present and skip —
// one xattr write per reader per object, not per read.
func TestReaderStampFirstReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obj")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	StampReaderFirstRead(path, "carol")
	v1, ok := xget(t, path, ReaderXattrName("carol"))
	if !ok {
		t.Fatal("first read did not stamp the reader breadcrumb")
	}
	// Second read: value must be unchanged (skip, not re-stamp).
	StampReaderFirstRead(path, "carol")
	v2, _ := xget(t, path, ReaderXattrName("carol"))
	if v1 != v2 {
		t.Fatalf("second read re-stamped: %q -> %q", v1, v2)
	}
	// A different principal stamps independently.
	StampReaderFirstRead(path, "dave")
	if _, ok := xget(t, path, ReaderXattrName("dave")); !ok {
		t.Fatal("second principal was not stamped")
	}
	if _, ok := xget(t, path, ReaderXattrName("carol")); !ok {
		t.Fatal("first principal's stamp vanished")
	}
}

// TestReaderStampFailOpen pins fail-open: an unstampable target (here: a
// missing file — the read-only-mount stand-in) must not panic or write
// anything; the GET keeps succeeding because the helper returns silently.
func TestReaderStampFailOpen(t *testing.T) {
	StampReaderFirstRead(filepath.Join(t.TempDir(), "nope"), "carol")
	StampReaderFirstRead(filepath.Join(t.TempDir(), "nope"), "")
}

// TestNoPrincipalNoReaderStamp covers the empty-principal guard.
func TestNoPrincipalNoReaderStamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obj")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	StampReaderFirstRead(path, "")
	if _, ok := xget(t, path, xattrReaderPrefix); ok {
		t.Fatal("empty principal produced a stamp")
	}
}
