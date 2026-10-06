// paths_reserved_test.go — bughunt L1: the webdav key validator
// (paths.go keySafe) rejected only "." and "..", while BOTH other key
// validators in the tree reject the reserved control segments ".metadata"
// and ".zfs" too:
//
//   - internal/frontend/s3/object_handlers.go validateObjectKey
//   - internal/backend/fsbackend/paths.go validateKey
//
// The disagreement was invisible in the unit suite because webdav's batch
// surface (batch.go:66) deliberately calls the s3 validator for its
// manifest keys — so POST ?batch rejected ".metadata/x" while a plain PUT
// of the same key accepted it and wrote into the bucket's control
// directory. On a dataset-backed bucket ".zfs" is the ZFS snapshot control
// directory at the mountpoint (zfs-bucket-datasets-2026-10): a PUT there
// writes into it, a DELETE os.Remove's it.
//
// The rule is SEGMENT-exact, like the twins: only whole segments named
// ".metadata" or ".zfs" are reserved. Keys that merely CONTAIN dots stay
// legal ("a..b", ".hidden", "v1.2", "x.zfs").
package webdav

import (
	"testing"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// TestKeySafe_ReservedControlSegments pins keySafe's table against the
// s3/fsbackend validator twins.
func TestKeySafe_ReservedControlSegments(t *testing.T) {
	rejected := []struct {
		name string
		key  string
	}{
		{"whole key .metadata", ".metadata"},
		{"whole key .zfs", ".zfs"},
		{"leading segment .metadata", ".metadata/x"},
		{"trailing segment .metadata", "x/.metadata"},
		{"mid-path segment .metadata", "a/.metadata/b.txt"},
		{"mid-path segment .zfs", "a/.zfs/snapshot"},
		{"leading segment .zfs", ".zfs/x"},
		{"trailing segment .zfs", "a/b/.zfs"},
		{"both reserved", ".metadata/.zfs"},
		{"dot dot still rejected", ".."},
		{"dot segment still rejected", "a/./b"},
	}
	for _, tt := range rejected {
		t.Run("reject/"+tt.name, func(t *testing.T) {
			if keySafe(tt.key) {
				t.Errorf("keySafe(%q) = true, want false (reserved control segment)", tt.key)
			}
		})
	}

	accepted := []struct {
		name string
		key  string
	}{
		{"empty key", ""},
		{"plain name", "doc.txt"},
		{"dotted name", "v1.2"},
		{"double dot inside a name", "a..b"},
		{"dotfile", ".hidden"},
		{"zfs suffix is not a segment", "x.zfs"},
		{"metadata suffix is not a segment", "x.metadata"},
		{"dotfile in a path", "dir/.hidden/file"},
		{"double dot in a path", "dir/a..b/file"},
		{"dot in a path segment", "v1.2/x..y"},
		{"nested reserved-looking prefix", "my.metadata.dir/x"},
	}
	for _, tt := range accepted {
		t.Run("accept/"+tt.name, func(t *testing.T) {
			if !keySafe(tt.key) {
				t.Errorf("keySafe(%q) = false, want true (legit dotted name)", tt.key)
			}
		})
	}
}

// TestParseResource_RejectsReservedControlSegments pins the WIRE surface:
// parseResource must refuse a URL carrying a reserved control segment in
// either mode, so the handler answers 403 before any backend call (the same
// rejection path a dot-segment key takes).
func TestParseResource_RejectsReservedControlSegments(t *testing.T) {
	cases := []struct {
		name string
		bkt  string // mode B when non-empty
		url  string
	}{
		{"mode A whole key .metadata", "", "/photos/.metadata"},
		{"mode A nested .metadata", "", "/photos/a/.metadata/b"},
		{"mode A whole key .zfs", "", "/photos/.zfs"},
		{"mode A nested .zfs", "", "/photos/a/.zfs/b"},
		{"mode B whole key .metadata", "photos", "/.metadata"},
		{"mode B nested .metadata", "photos", "/a/.metadata/b"},
		{"mode B whole key .zfs", "photos", "/.zfs"},
		{"mode B nested .zfs", "photos", "/a/.zfs/b"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newTestFrontend(Config{Bucket: tt.bkt})
			if res, ok := f.parseResource(tt.url); ok {
				t.Fatalf("parseResource(%q) = %+v ok, want rejection", tt.url, res)
			}
		})
	}
	// Legit dotted names keep working over the wire.
	f, _ := newTestFrontend(Config{})
	for _, url := range []string{"/photos/a..b", "/photos/.hidden", "/photos/v1.2/x..y", "/photos/x.zfs"} {
		if _, ok := f.parseResource(url); !ok {
			t.Errorf("parseResource(%q) rejected, want accepted (legit dotted name)", url)
		}
	}
}

// TestParseResource_MatchesS3ValidatorParity is the disagreement-closing
// pin: for a table of keys, webdav's keySafe decision must agree with the
// s3 validateObjectKey the batch surface already delegates to. Any future
// divergence between the three validators (webdav, s3, fsbackend) fails
// here instead of surviving behind a green suite.
func TestParseResource_MatchesS3ValidatorParity(t *testing.T) {
	keys := []string{
		".metadata", ".zfs", "a/.metadata", "a/.zfs", "a/.metadata/b",
		"a/.zfs/b", ".metadata/x", "x/.metadata", ".metadata/.zfs",
		"..", "a/../b", "a..b", ".hidden", "v1.2", "x.zfs",
		"x.metadata", "my.metadata.dir/x", "dir/.hidden/f",
	}
	// SCOPE: the RESERVED control segments only. Two deliberate
	// divergences stay outside this pin and are NOT bugs:
	//   - "." as a whole segment: webdav's keySafe rejects it (it aliases
	//     a directory entry), s3's validator does not list it;
	//   - the empty key: the bucket root collection on webdav (a legal
	//     resource), an invalid key on s3.
	for _, key := range keys {
		t.Run("key="+key, func(t *testing.T) {
			// s3.ValidateObjectKey is the SAME function webdav's batch
			// surface passes to the s3 batch handler (batch.go:66) — the
			// authority the single-op webdav path must match.
			wantSafe := s3.ValidateObjectKey(key) == nil
			if got := keySafe(key); got != wantSafe {
				t.Fatalf("keySafe(%q) = %v, s3 ValidateObjectKey accepts = %v — the validators must agree", key, got, wantSafe)
			}
		})
	}
}
