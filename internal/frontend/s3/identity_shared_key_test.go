// identity_shared_key_test.go — the s3 half of the shared-identity
// round-trip pin.
//
// The live ZFS harness found webdav (and h3, which wraps it) publishing
// the authenticated identity under its OWN private context key, so the
// shared s3 batch executor resolved no principal for a ?batch arriving
// over those mounts and stamped owner='unauthenticated'. The key now
// lives in internal/auth as ONE definition (auth.WithIdentity /
// auth.IdentityFromContext) that every frontend publishes and reads.
//
// This pin is the guard against regression to the two-key shape: publish an
// identity through the shared helper and read it back through every s3-side
// reader of that seam — identityOf (grant checks), principalOfRequest
// (breadcrumb stamping) and batchPrincipal (the batch executor). The
// matching webdav-side pin is in internal/frontend/webdav/
// identity_shared_key_test.go, which reads through BOTH frontends.
package s3

import (
	"net/http/httptest"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// TestSharedIdentityKey_S3ReadersRoundTripPublishedIdentity: one publish
// through the shared helper, read by every s3-side reader.
func TestSharedIdentityKey_S3ReadersRoundTripPublishedIdentity(t *testing.T) {
	id := auth.Identity{
		AccessKeyID:  "ak-shared-key",
		BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}},
	}
	req := httptest.NewRequest("GET", "/bkt/key", nil).
		WithContext(auth.WithIdentity(httptest.NewRequest("GET", "/", nil).Context(), id))

	if got := identityOf(req); got.AccessKeyID != id.AccessKeyID {
		t.Errorf("identityOf read %q, want %q — the s3 frontend must read the SHARED identity key", got.AccessKeyID, id.AccessKeyID)
	}
	if got := identityOf(req); !got.CanWrite("any-bucket") {
		t.Errorf("grants lost on the shared-key round-trip: %+v", got.BucketGrants)
	}
	if got := principalOfRequest(req); got != id.AccessKeyID {
		t.Errorf("principalOfRequest read %q, want %q (write breadcrumbs would carry the wrong principal)", got, id.AccessKeyID)
	}
	if got := batchPrincipal(req.Context()); got != id.AccessKeyID {
		t.Errorf("batchPrincipal read %q, want %q (a batch-written object would carry the wrong owner breadcrumb)", got, id.AccessKeyID)
	}
}
