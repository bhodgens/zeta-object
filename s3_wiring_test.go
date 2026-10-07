package main

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/metadata"
)

// TestS3WiringRegistersZFSEventsProvider pins the C1 fix: installS3Seams
// (called from main() before the listener opens) registers the built-in
// "zfs-events" MetadataProvider, so production ?events requests resolve a
// provider instead of always 503ing. The s3 frontend test binary does NOT
// run installS3Seams, so its own stub registration is unaffected.
func TestS3WiringRegistersZFSEventsProvider(t *testing.T) {
	if metadata.Lookup("zfs-events") == nil {
		// Guarded: Register panics on duplicates if another test in this
		// package already ran installS3Seams.
		installS3Seams(serverConfig())
	}
	p := metadata.Lookup("zfs-events")
	if p == nil {
		t.Fatal("installS3Seams did not register the zfs-events provider")
	}
	if p.Name() != "zfs-events" {
		t.Fatalf("registered provider name = %q, want zfs-events", p.Name())
	}
}
