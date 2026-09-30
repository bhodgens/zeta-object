// authz_test.go — the shared AuthorizeRequest helper (pluggable-
// authentication tree leaf 03 Task 2): one grant decision, all frontends.
package frontend_test

import (
	"errors"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

func grantIdentity(grants map[string]auth.Grant) auth.Identity {
	return auth.Identity{AccessKeyID: "ak", BucketGrants: grants}
}

func TestAuthorizeRequest(t *testing.T) {
	wildcardRW := grantIdentity(map[string]auth.Grant{"*": {Read: true, Write: true}})
	readonly := grantIdentity(map[string]auth.Grant{"neuro": {Read: true}})
	nilGrants := grantIdentity(nil)

	cases := []struct {
		name   string
		id     auth.Identity
		bucket string
		write  bool
		want   bool // allowed
	}{
		{"wildcard read", wildcardRW, "any", false, true},
		{"wildcard write", wildcardRW, "any", true, true},
		{"readonly read", readonly, "neuro", false, true},
		{"readonly write denied", readonly, "neuro", true, false},
		{"ungranted read denied", readonly, "other", false, false},
		{"ungranted write denied", readonly, "other", true, false},
		{"nil-grants read denied", nilGrants, "any", false, false},
		{"nil-grants write denied", nilGrants, "any", true, false},
		{"service-level always allowed", readonly, "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := frontend.AuthorizeRequest(tc.id, tc.bucket, tc.write)
			if tc.want && err != nil {
				t.Fatalf("denied: %v", err)
			}
			if !tc.want && err == nil {
				t.Fatal("allowed, want denial")
			}
		})
	}
}

// TestAuthzErrorCarriesFacts pins the error's structure: bucket + write flag
// so each frontend can render its protocol-appropriate response.
func TestAuthzErrorCarriesFacts(t *testing.T) {
	id := grantIdentity(map[string]auth.Grant{"neuro": {Read: true}})
	err := frontend.AuthorizeRequest(id, "photos", true)
	if err == nil {
		t.Fatal("want denial")
	}
	var azErr *frontend.AuthzError
	if !errors.As(err, &azErr) {
		t.Fatalf("err type = %T, want *frontend.AuthzError", err)
	}
	if azErr.Bucket != "photos" || !azErr.Write {
		t.Fatalf("AuthzError = %+v", azErr)
	}
	if azErr.Error() == "" {
		t.Fatal("Error() empty")
	}
}
