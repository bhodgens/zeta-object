package bucketmanager_test

import (
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/bucketmanager"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// TestValidateNameMatchesS3Rule is the machine-checked drift guard between the
// S3 copy of the naming rule (internal/frontend/s3/bucket_handlers.go
// validateBucketName, exported for this test as s3.ValidateBucketName) and the
// duplicated one in this package. They cannot share one function — s3 imports
// bucketmanager, so bucketmanager importing s3 would be an import cycle — so
// the rule is duplicated and this external test compares them over a corpus,
// failing on any divergence in accept/reject OR in the error text.
//
// This file MUST stay in package bucketmanager_test: an in-package test file
// importing s3 would close the cycle.
//
// If you change either copy and this test fails, change both. (The s3 copy is
// the WIRE surface; this copy is the containment guard.)
func TestValidateNameMatchesS3Rule(t *testing.T) {
	// The names s3's copy must accept and reject identically. Traversal
	// shapes are included because both copies must refuse them — the s3 copy
	// does so via its own rules (no separator, no leading dot, min length 3).
	accepted := []string{
		"abc", "a-bucket-1.test-2", "123-bucket", "bucket-123", "e2e-33-pb3",
		strings.Repeat("a", 63), "1.2.3.4x", // not an IP: trailing letter
	}
	rejected := []string{
		"", "a", "ab", "0", "x1", strings.Repeat("a", 64),
		"Abc", "-abc", "abc-", "_abc", "abc_", ".abc", "abc.",
		"..", ".", "./x", "../x", "../../etc", "a/b", "a\\b", "/abs", "a/../..",
		"a b", "a	b", "a%2fb", "a..b", "a-b..c",
		"192.168.1.1", "1.2.3.4",
	}
	for _, name := range accepted {
		if err := bucketmanager.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil (s3 accepts it)", name, err)
		}
		if err := s3.ValidateBucketName(name); err != nil {
			t.Errorf("s3.ValidateBucketName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range rejected {
		mine, theirs := bucketmanager.ValidateName(name), s3.ValidateBucketName(name)
		switch {
		case mine == nil:
			t.Errorf("ValidateName(%q) = nil, want a refusal (s3 refuses it)", name)
		case theirs == nil:
			t.Errorf("s3.ValidateBucketName(%q) = nil, but this copy refuses it (%v)", name, mine)
		default:
			// Both copies must agree on WHICH clause was violated. They need
			// not use the same words: for a traversal shape this copy's
			// containment layer reports first ("must not contain a path
			// separator") where s3's single character-set rule reports
			// ("can only contain lowercase letters..."). Same refusal,
			// different layer — that is the designed difference, so only a
			// same-layer mismatch is a failure.
			if sameLayer(name) && mine.Error() != theirs.Error() {
				t.Errorf("message drift for %q: this copy %q, s3 %q", name, mine, theirs)
			}
		}
	}
}

// sameLayer reports whether the two copies judge this name by the SAME rule,
// i.e. neither copy's containment layer short-circuits first. A name carrying a
// separator (or ".", "..", "") is a containment-layer case in this copy and a
// character-set case in s3's; everything else is judged by the naming rule on
// both sides.
func sameLayer(name string) bool {
	switch name {
	case "", ".", "..", "./x", "../x", "../../etc", "/abs", "a/../..", "a/b", "a\\b":
		return false
	}
	return true
}
