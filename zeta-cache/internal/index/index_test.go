package index

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesPlaceholderV0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open = %v, want nil", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Errorf("user_version = %d, want 0", v)
	}
}

func TestOpenExistingV0Again(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open = %v, want nil", err)
	}
	defer db2.Close()
}
