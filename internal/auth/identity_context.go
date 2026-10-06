// identity_context.go — the ONE request-context key for the
// authenticated identity.
//
// Every frontend publishes the identity it resolved in its auth stage into
// the request context so handlers reached through a dispatch switch (which
// does not thread the identity as a parameter) can read it: grant checks
// beyond the URL-path bucket, the principal behind every write's
// user.zeta.owner / user.zeta.writer.* attribution breadcrumbs, and the
// SHARED batch executor the s3 and webdav/h3 mounts both drive.
//
// Why the key lives HERE rather than per frontend: a per-frontend key type
// silently breaks every cross-frontend consumer. The JSON ?batch surface is
// one implementation in internal/batchops with one executor in the s3
// package, mounted by BOTH the s3 frontend and the webdav frontend (h3
// inherits it by wrapping webdav); when webdav published the identity
// under its own private key, the shared executor saw no principal and
// stamped owner='unauthenticated' on every batch-written object that
// arrived over webdav or h3, while every unit test stayed green because
// each frontend's own tests published and read under the same private key.
// Only the live ZFS harness (s15's breadcrumb check) exercised the other
// mount.
//
// Define the key ONCE and have every frontend go through this pair. A
// frontend that needs a second context seam (a per-request flag, say) still
// gets its own key — but never a second one for the identity.
package auth

import "context"

// identityContextKey is the unexported context key for the authenticated
// Identity. Unexported and package-owned: no other package can construct
// it, so the only way to publish or read the identity is this pair.
type identityContextKey struct{}

// WithIdentity returns a context carrying id as the authenticated
// identity. A frontend's dispatch calls this once, after authentication
// and before method dispatch, on the context it hands its handlers.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// IdentityFromContext returns the identity WithIdentity published, and
// whether one was present. Absent means the request never passed through a
// frontend auth stage that published — a direct handler invocation in a
// test, or a code path that bypassed dispatch — so callers decide their
// own degradation (the s3 frontend synthesizes the legacy wildcard
// principal; webdav keeps the zero identity and fails closed).
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(Identity)
	return id, ok
}
