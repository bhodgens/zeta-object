package main

// w2_m6_sweep_wiring_test.go - pins that the staging sweep is wired in
// main: the daemon-start helper delegates to fusefs.SweepStaging over
// cfg.CacheDir and reports count/bytes (W2-M6 - fs.go's old promise of a
// daemon-start reconcile that never existed).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestW2M6SweepStagingAtStartWired(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "orphan"), []byte("12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, b, err := sweepStagingAtStart(dir)
	if err != nil {
		t.Fatalf("sweepStagingAtStart: %v", err)
	}
	if n != 1 || b != 8 {
		t.Fatalf("sweepStagingAtStart = (%d, %d), want (1, 8)", n, b)
	}
	if _, err := os.Stat(filepath.Join(staging, "orphan")); !os.IsNotExist(err) {
		t.Fatal("orphaned staging file survived the startup sweep")
	}
}
