// grants_test.go — the Identity.CanRead/CanWrite grant math (pluggable-
// authentication tree leaf 01 Task 1): wildcard, per-bucket, write⊃read,
// nil/empty grants deny everything. Table-driven.
package auth_test

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

func TestIdentityCanRead(t *testing.T) {
	cases := []struct {
		name   string
		grants map[string]auth.Grant
		bucket string
		want   bool
	}{
		{"nil grants deny", nil, "any", false},
		{"empty grants deny", map[string]auth.Grant{}, "any", false},
		{"wildcard readwrite reads", map[string]auth.Grant{"*": {Read: true, Write: true}}, "any", true},
		{"wildcard readonly reads", map[string]auth.Grant{"*": {Read: true}}, "any", true},
		{"wildcard write-only does not read", map[string]auth.Grant{"*": {Write: true}}, "any", false},
		{"specific readonly reads", map[string]auth.Grant{"photos": {Read: true}}, "photos", true},
		{"specific readwrite reads", map[string]auth.Grant{"photos": {Read: true, Write: true}}, "photos", true},
		{"unknown bucket denied", map[string]auth.Grant{"photos": {Read: true, Write: true}}, "other", false},
		{"other wildcard bucket denied", map[string]auth.Grant{"photos": {Read: true}}, "*", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := auth.Identity{AccessKeyID: "ak", BucketGrants: tc.grants}
			if got := id.CanRead(tc.bucket); got != tc.want {
				t.Fatalf("CanRead(%q) = %v, want %v", tc.bucket, got, tc.want)
			}
		})
	}
}

func TestIdentityCanWrite(t *testing.T) {
	cases := []struct {
		name   string
		grants map[string]auth.Grant
		bucket string
		want   bool
	}{
		{"nil grants deny", nil, "any", false},
		{"wildcard readwrite writes", map[string]auth.Grant{"*": {Read: true, Write: true}}, "any", true},
		{"wildcard readonly does not write", map[string]auth.Grant{"*": {Read: true}}, "any", false},
		{"specific readwrite writes", map[string]auth.Grant{"photos": {Read: true, Write: true}}, "photos", true},
		{"specific readonly does not write", map[string]auth.Grant{"photos": {Read: true}}, "photos", false},
		{"unknown bucket denied", map[string]auth.Grant{"photos": {Read: true, Write: true}}, "other", false},
		{"wildcard covers all buckets", map[string]auth.Grant{"*": {Read: true, Write: true}}, "some-other", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := auth.Identity{AccessKeyID: "ak", BucketGrants: tc.grants}
			if got := id.CanWrite(tc.bucket); got != tc.want {
				t.Fatalf("CanWrite(%q) = %v, want %v", tc.bucket, got, tc.want)
			}
		})
	}
}

// TestIdentityWriteImpliesRead pins the write⊃read rule as the registry
// translates it: a readwrite grant always satisfies CanRead.
func TestIdentityWriteImpliesRead(t *testing.T) {
	id := auth.Identity{BucketGrants: map[string]auth.Grant{
		"*": {Read: true, Write: true},
	}}
	for _, bucket := range []string{"a", "b", "photos"} {
		if !id.CanRead(bucket) || !id.CanWrite(bucket) {
			t.Fatalf("wildcard readwrite: bucket %q read=%v write=%v", bucket, id.CanRead(bucket), id.CanWrite(bucket))
		}
	}
}

// TestPerBucketGrantBeatsWildcard pins the A1 fix: a PRESENT per-bucket entry
// is authoritative for that bucket — it overrides the "*" wildcard in BOTH
// directions (readonly per-bucket denies write; readwrite per-bucket allows
// write even under a readonly wildcard). Only an ABSENT entry falls through.
// This is the README.md shape: {"*":"readwrite", "photos":"readonly"}.
func TestPerBucketGrantBeatsWildcard(t *testing.T) {
	id := auth.Identity{
		AccessKeyID: "ak",
		BucketGrants: map[string]auth.Grant{
			"*":      {Read: true, Write: true},
			"photos": {Read: true}, // readonly
		},
	}
	if id.CanRead("photos") != true {
		t.Error("CanRead(photos) = false, want true (per-bucket readonly reads)")
	}
	if id.CanWrite("photos") != false {
		t.Error("CanWrite(photos) = true, want false (per-bucket readonly must beat wildcard readwrite)")
	}
	if !id.CanRead("other") || !id.CanWrite("other") {
		t.Error("absent per-bucket entry must fall through to wildcard readwrite")
	}
	// Reverse direction: per-bucket readwrite beats a readonly wildcard.
	id2 := auth.Identity{BucketGrants: map[string]auth.Grant{
		"*":      {Read: true},
		"photos": {Read: true, Write: true},
	}}
	if !id2.CanWrite("photos") {
		t.Error("CanWrite(photos) = false, want true (per-bucket readwrite beats readonly wildcard)")
	}
	if id2.CanWrite("other") {
		t.Error("CanWrite(other) = true, want false (readonly wildcard)")
	}
	// A per-bucket write-only entry is authoritative and does NOT grant read.
	id3 := auth.Identity{BucketGrants: map[string]auth.Grant{
		"*":      {Read: true, Write: true},
		"photos": {Write: true},
	}}
	if !id3.CanWrite("photos") {
		t.Error("CanWrite(photos) = false, want true")
	}
	if id3.CanRead("photos") {
		t.Error("CanRead(photos) = true, want false (per-bucket entry authoritative, no wildcard read rescue)")
	}
}

func TestWildcardIdentity(t *testing.T) {
	id := auth.WildcardIdentity("env-ak")
	if id.AccessKeyID != "env-ak" {
		t.Fatalf("AccessKeyID = %q", id.AccessKeyID)
	}
	for _, bucket := range []string{"a", "photos", "*"} {
		if !id.CanRead(bucket) || !id.CanWrite(bucket) {
			t.Fatalf("wildcard identity denied on %q", bucket)
		}
	}
}
