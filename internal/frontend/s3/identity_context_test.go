// identity_context_test.go — the shared-identity seam's own pins (s3
// side, on the symbols that already exist). The live ZFS harness found
// that webdav (and therefore h3, which wraps it) published the
// authenticated identity into the request context under its OWN private
// key, so the SHARED s3 batch executor resolved no principal and stamped
// owner='unauthenticated' on every batch-written object while the s3 mount
// stamped the requester correctly.
//
// The fix moves the key into internal/auth as ONE definition
// (auth.WithIdentity / auth.IdentityFromContext) that every frontend
// publishes and reads. Two properties must hold afterwards and are pinned
// here — on the surface that already worked, so a regression cannot hide
// behind the new seam:
//
//   - the s3 mount STILL attributes a batch copy to the real requester, and
//   - identityOf keeps its DOCUMENTED degradation: a request that bypassed
//     dispatch (direct handler invocation in unit tests) yields the legacy
//     wildcard principal, not an empty identity and not a panic.
//
// The shared-helper round-trip pins live in identity_shared_key_test.go
// (they reference the new API), the mount-level pins in the webdav and h3
// packages.
package s3

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// TestBatchEndpoint_IdentityOfSurvivesSharedKeyRefactor: the s3 batch
// mount keeps stamping the requester after the context key moved to the
// shared internal/auth definition. A regression here would mean the s3
// publish site stopped using the shared helper — the s3 mount silently
// falling back to the wildcard, exactly the bug the h3 mount had.
func TestBatchEndpoint_IdentityOfSurvivesSharedKeyRefactor(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "shared-key-bkt")
	batchPutObject(t, "shared-key-bkt", "src/plain.txt", "payload", "", nil)

	w := batchPostAs(t, "ak-shared", "shared-key-bkt",
		`{"operations":[{"op":"copy","from":"src/plain.txt","to":"dst/plain.txt"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := batchResults(t, w.Body.String()).Results[0].Status; got != StatusOKWire {
		t.Fatalf("copy item = %q, want ok", got)
	}
	path := filepath.Join(env.dataDir, "shared-key-bkt", "dst/plain.txt")
	if owner, ok := readUserXattr(t, path, "user.zeta.owner"); !ok || owner != "ak-shared" {
		t.Errorf("s3 batch copy user.zeta.owner = %q ok=%v, want ak-shared (the s3 mount must keep stamping the real requester)", owner, ok)
	}
	if _, ok := readUserXattr(t, path, "user.zeta.writer.ak-shared"); !ok {
		t.Errorf("s3 batch copy has no user.zeta.writer.ak-shared breadcrumb")
	}
}

// TestIdentityOf_NoContextIdentityIsWildcard: the documented degradation.
// A request whose context carries no identity (a direct handler invocation
// in a unit test, or any path that bypassed serveHTTP) resolves to the
// legacy wildcard principal — the same synthesis the CredentialSource
// fallback performs. An empty AccessKeyID here would change the behavior
// of every direct-handler test in the package (their writes would lose
// attribution); a panic would be worse.
func TestIdentityOf_NoContextIdentityIsWildcard(t *testing.T) {
	req := httptest.NewRequest("GET", "/bucket/key", nil)
	got := identityOf(req)
	want := auth.WildcardIdentity("unauthenticated")
	if got.AccessKeyID != want.AccessKeyID {
		t.Errorf("identityOf on a bare request = %q, want %q", got.AccessKeyID, want.AccessKeyID)
	}
	if !got.CanRead("any-bucket") || !got.CanWrite("any-bucket") {
		t.Errorf("identityOf on a bare request must keep the wildcard readwrite grants, got %+v", got.BucketGrants)
	}
	// batchPrincipal is the batch executor's reader of the same seam: it
	// must degrade identically, never to an empty principal.
	if p := batchPrincipal(req.Context()); p != want.AccessKeyID {
		t.Errorf("batchPrincipal on a bare context = %q, want the documented wildcard fallback %q", p, want.AccessKeyID)
	}
}
