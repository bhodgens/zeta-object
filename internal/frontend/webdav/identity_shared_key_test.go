// identity_shared_key_test.go — the CROSS-FRONTEND pin for the shared
// identity context key.
//
// The live ZFS harness found a batch copy arriving over h3 (which wraps
// webdav) stamping owner='unauthenticated' while the s3 mount stamped the
// real requester: webdav published the authenticated identity under its
// OWN private context key, so the SHARED s3 batch executor — the code both
// mounts drive — resolved no principal. Every existing test missed it
// because each frontend's tests published and read under the same private
// key.
//
// The fix defines the key ONCE in internal/auth (auth.WithIdentity /
// auth.IdentityFromContext). This pin is the guard against regression to
// the two-key shape: it publishes an identity through the shared helper
// and reads it back through BOTH frontends' read paths. Reintroduce a
// private key in either frontend and the corresponding read returns the
// zero identity — the test fails, naming the frontend.
//
// The mount-level pins (a real Basic-authenticated webdav dispatch, a real
// HTTP/3 request with a client certificate, the s3 mount) live in
// batch_principal_test.go in each package.
package webdav

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// TestSharedIdentityKey_BothFrontendsReadOnePublishedIdentity: publish
// once through the shared helper, read through webdav's own identityOf and
// through the s3 package's identityOf (the same read the batch executor's
// principal resolution and CopyObject's source-grant check use).
func TestSharedIdentityKey_BothFrontendsReadOnePublishedIdentity(t *testing.T) {
	id := auth.Identity{
		AccessKeyID:  "ak-shared-key",
		BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}},
	}
	ctx := auth.WithIdentity(context.Background(), id)
	req := httptest.NewRequest("GET", "/bkt/key", nil).WithContext(ctx)

	if got := identityOf(req); got.AccessKeyID != id.AccessKeyID {
		t.Errorf("webdav identityOf read %q, want %q — the webdav frontend must read the SHARED identity key (a private key here is the bug the live harness found)", got.AccessKeyID, id.AccessKeyID)
	}
	if got := s3.IdentityOfRequestForTest(req); got.AccessKeyID != id.AccessKeyID {
		t.Errorf("s3 identityOf read %q, want %q — the s3 frontend must read the SHARED identity key", got.AccessKeyID, id.AccessKeyID)
	}
	// The batch executor's principal resolution is the reader that produced
	// owner='unauthenticated' on the h3 mount; it must resolve the same
	// published identity.
	if got := s3.BatchPrincipalForTest(ctx); got != id.AccessKeyID {
		t.Errorf("s3 batchPrincipal read %q, want %q — a batch over webdav/h3 must attribute the write to the real requester", got, id.AccessKeyID)
	}
}

// TestSharedIdentityKey_AbsentIdentityIsZeroNotWildcard: the webdav read
// path keeps its existing no-identity behavior — the ZERO Identity (an
// empty AccessKeyID and no grants, so CanWrite is false and every grant
// check denies). The webdav pipeline authenticates BEFORE authorize, so an
// absent identity is a wiring bug, not a client-reachable state; it must
// fail closed, and it must NOT suddenly gain the s3 frontend's wildcard
// fallback (which would grant a bypassed identity every bucket).
func TestSharedIdentityKey_AbsentIdentityIsZeroNotWildcard(t *testing.T) {
	req := httptest.NewRequest("GET", "/bkt/key", nil)
	got := identityOf(req)
	if got.AccessKeyID != "" {
		t.Errorf("webdav identityOf on a bare request = %q, want the zero identity (webdav keeps its fail-closed behavior; the wildcard fallback belongs to the s3 frontend alone)", got.AccessKeyID)
	}
	if got.CanRead("any-bucket") || got.CanWrite("any-bucket") {
		t.Errorf("a zero identity must hold no grants, got %+v", got.BucketGrants)
	}
}
