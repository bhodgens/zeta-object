package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// tempLitter reports any leftover *.tmp-* files in dir.
func tempLitter(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var litter []string
	for _, e := range entries {
		if bytes.Contains([]byte(e.Name()), []byte(".tmp-")) {
			litter = append(litter, e.Name())
		}
	}
	return litter
}

func TestStorageAtomic_OverwriteIsByteExactAndClean(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	old := bytes.Repeat([]byte("A"), 4096)
	new := bytes.Repeat([]byte("B"), 1000) // different length on purpose
	if err := os.WriteFile(p, old, 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeFileAtomic(p, new, 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, new) {
		t.Fatalf("overwrite not byte-exact: got %d bytes, want %d", len(got), len(new))
	}
	if litter := tempLitter(t, dir); len(litter) != 0 {
		t.Fatalf("temp litter after overwrite: %v", litter)
	}
}

func TestStorageAtomic_NewFileInMissingDir_Pinned(t *testing.T) {
	dir := t.TempDir()
	// PINNED BEHAVIOR: writeFileAtomic does NOT MkdirAll. The temp file is
	// created directly in the target's parent, so a missing parent directory
	// makes OpenFile fail and the helper returns an error without creating
	// anything. If this test ever goes RED, the helper grew an implicit
	// MkdirAll — update this pin deliberately, not silently.
	p := filepath.Join(dir, "missing", "obj")
	err := writeFileAtomic(p, []byte("x"), 0644)
	if err == nil {
		t.Fatal("expected error writing into a non-existent parent dir")
	}
	if _, statErr := os.Stat(filepath.Dir(p)); !os.IsNotExist(statErr) {
		t.Fatalf("parent dir should still not exist, stat err = %v", statErr)
	}
}

func TestStorageAtomic_RenameFailureCleansTemp_NoRootSkip(t *testing.T) {
	// Replaces the euid-0 skip in multipart_handlers_test.go: renaming a FILE
	// over a non-empty DIRECTORY fails for every euid (EISDIR/ENOTEMPTY), so
	// no root guard is needed here.
	dir := t.TempDir()
	target := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(target, "inner"), 0755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := writeFileAtomic(target, []byte("x"), 0644); err == nil {
		t.Fatal("expected rename over a non-empty directory to fail")
	}
	if litter := tempLitter(t, dir); len(litter) != 0 {
		t.Fatalf("temp file not cleaned up after failed rename: %v", litter)
	}
}

func TestStorageAtomic_ReadOnlyParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) }) // allow TempDir cleanup
	p := filepath.Join(dir, "obj")
	if err := writeFileAtomic(p, []byte("x"), 0644); err == nil {
		t.Fatal("expected error writing into a read-only directory")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("no partial target may exist, stat err = %v", err)
	}
}

func TestStorageAtomic_ConcurrentSamePathNeverTorn(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	const writers = 10
	want := make([][]byte, writers)
	for i := range want {
		want[i] = bytes.Repeat([]byte{byte('0' + i)}, 512)
	}
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			errs[i] = writeFileAtomic(p, want[i], 0644)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("final file is empty (torn write)")
	}
	ok := false
	for _, w := range want {
		if bytes.Equal(got, w) {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("final content is torn (matches none of the %d writers): %q", writers, got[:min(32, len(got))])
	}
	if litter := tempLitter(t, dir); len(litter) != 0 {
		t.Fatalf("temp litter after concurrent writes: %v", litter)
	}
}

func TestStorageAtomic_PermMasked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	if err := writeFileAtomic(p, []byte("x"), 0600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := fi.Mode().Perm()
	if mode&0o077 != 0 {
		t.Fatalf("perm = %#o, want group/other bits masked by umask (0600 request)", mode)
	}
	if mode&0o400 == 0 {
		t.Fatalf("perm = %#o, owner read bit missing", mode)
	}
}

func TestStorageAtomicJSON_ExactMarshalIndentBytes(t *testing.T) {
	type payload struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	if err := writeFileAtomicJSON(p, payload{A: 42, B: "hello"}, 0644); err != nil {
		t.Fatalf("writeFileAtomicJSON: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want, err := json.MarshalIndent(payload{A: 42, B: "hello"}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestStorageAtomicJSON_UnmarshalableTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	original := []byte(`{"intact": true}`)
	if err := os.WriteFile(p, original, 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeFileAtomicJSON(p, map[string]any{"ch": make(chan int)}, 0644); err == nil {
		t.Fatal("expected error for unmarshalable input")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("target was modified: %q, want %q", got, original)
	}
	if litter := tempLitter(t, dir); len(litter) != 0 {
		t.Fatalf("temp litter after marshal failure: %v", litter)
	}
}

func TestLockObject_ReLockable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	unlock := lockObject(p)
	unlock()
	unlock2 := lockObject(p) // must not deadlock
	unlock2()
}

func TestLockObject_ConcurrentIncrements(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obj")
	var n int
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 100 {
				unlock := lockObject(p)
				n++
				unlock()
			}
		})
	}
	wg.Wait()
	if n != 1000 {
		t.Fatalf("n = %d, want 1000 (lost updates under lockObject)", n)
	}
}

func TestLockObject_DifferentPathsIndependent(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, "a")
	unlock := lockObject(held)
	// NOTE: no defer here — the explicit unlock below must run exactly once
	// before re-acquiring at the end. If the goroutines deadlocked on a
	// shared mutex, wg.Wait() would hang and the test would fail by timeout.

	var wg sync.WaitGroup
	for g := range 10 {
		wg.Go(func() {
			other := filepath.Join(dir, fmt.Sprintf("b-%d", g))
			u := lockObject(other)
			u()
		})
	}
	wg.Wait()
	// All 10 goroutines acquired and released locks on DIFFERENT paths while
	// `held` was still locked. If lockObject shared one global mutex, they
	// would have deadlocked; wg.Wait() returning is the assertion. Belt and
	// braces: verify `held` can still be re-acquired afterwards.
	unlock()
	u := lockObject(held)
	u()
}
