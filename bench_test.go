package main

// Leaf 4.10 (gap 11 lite): benchmarks for the ListObjectsV2 hot path and the
// atomic-write helper. Bench-only — no assertions (plan convention).

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchKeys builds n synthetic, sorted keys shaped like "dirNNNN/objNNNN" so
// that every key contains a delimiter segment (exercises roll-up) while a
// prefix filter still matches a strict subset.
func benchKeys(n int) []string {
	keys := make([]string, 0, n)
	for i := range n {
		keys = append(keys, fmt.Sprintf("dir%04d/obj%06d", i/100, i))
	}
	return keys
}

// benchMetadata writes minimal .meta files for every key (os.WriteFile — the
// directory is created with MkdirAll; writeFileAtomic deliberately does not
// MkdirAll, a pinned behavior from leaf 4.6) so the bench measures the real
// read+unmarshal path instead of the missing-file skip.
func benchMetadata(b *testing.B, metadataDir string, keys []string) {
	b.Helper()
	meta := []byte("{}")
	for _, k := range keys {
		p := filepath.Join(metadataDir, k+".meta")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatalf("MkdirAll(%s): %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, meta, 0o644); err != nil {
			b.Fatalf("WriteFile(%s): %v", p, err)
		}
	}
}

func benchmarkListObjectsFromKeys(b *testing.B, p listObjectsParams) {
	keys := benchKeys(10000)
	metadataDir := b.TempDir()
	benchMetadata(b, metadataDir, keys)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		// Assert nothing: results are discarded; the bench measures the
		// walk/roll-up/read cost only.
		_, _, _, _, _ = listObjectsFromKeys(keys, p, "bench-bucket", metadataDir)
	}
}

func BenchmarkListObjectsFromKeysPlain(b *testing.B) {
	benchmarkListObjectsFromKeys(b, listObjectsParams{maxKeys: 1000})
}

func BenchmarkListObjectsFromKeysDelimiter(b *testing.B) {
	benchmarkListObjectsFromKeys(b, listObjectsParams{maxKeys: 1000, delimiter: "/"})
}

func BenchmarkListObjectsFromKeysPrefix(b *testing.B) {
	// Only "dir00NN" buckets match — a strict subset of the 10k keys.
	benchmarkListObjectsFromKeys(b, listObjectsParams{maxKeys: 1000, prefix: "dir00"})
}

// BenchmarkWriteFileAtomic measures a 4KB overwrite of an existing file (the
// temp-churn + rename cost) in b.TempDir.
func BenchmarkWriteFileAtomic(b *testing.B) {
	dir := b.TempDir()
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	p := filepath.Join(dir, "bench.dat")
	if err := writeFileAtomic(p, payload, 0o644); err != nil {
		b.Fatalf("seed writeFileAtomic: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := writeFileAtomic(p, payload, 0o644); err != nil {
			b.Fatalf("writeFileAtomic: %v", err)
		}
	}
}
