// authz.go — the protocol-neutral authorization helper (pluggable-
// authentication tree leaf 03). THE shared grant-decision entry point for
// HTTP frontends: the S3 dispatch and any future WebDAV/ownCloud frontend
// call AuthorizeRequest; the grant logic itself stays in internal/auth
// (Identity.CanRead/CanWrite). Non-HTTP frontends (FTP/SFTP) call the
// Identity methods directly.
package frontend

import (
	"fmt"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// AuthzError is the protocol-neutral authorization rejection. Frontends
// render it however their wire protocol speaks (S3 AccessDenied XML 403,
// WebDAV 401/403, ...). The error carries the bucket and whether the
// rejected access was a write so each protocol can shape its response;
// it deliberately does NOT distinguish "bucket outside grants" from
// "read-only on a writable op" on the wire — both are simple denials that
// must not leak which grants the identity holds.
type AuthzError struct {
	Bucket string
	Write  bool
}

// Error satisfies the error interface.
func (e *AuthzError) Error() string {
	action := "read"
	if e.Write {
		action = "write"
	}
	return fmt.Sprintf("access denied: identity may not %s bucket %q", action, e.Bucket)
}

// AuthorizeRequest checks id's grants for bucket access. write=true
// requires write; write=false requires read. nil means allowed. A
// service-level request (bucket == "") is always allowed here — ListBuckets
// filtering by grants is per-frontend policy computed from the identity's
// grants, not a per-request denial.
//
// Leaf 09: this is now a thin special case of auth.AuthorizeOp (THE single
// rich-grant decision) with op = read|write, key = "", now = time.Now() —
// the delegation keeps all HTTP frontends coherent with prefix/op/time-
// scoped grants without touching their call sites.
func AuthorizeRequest(id auth.Identity, bucket string, write bool) error {
	if bucket == "" {
		return nil
	}
	op := auth.OpRead
	if write {
		op = auth.OpWrite
	}
	if !auth.AuthorizeOp(id, op, bucket, "", time.Now().UTC()) {
		return &AuthzError{Bucket: bucket, Write: write}
	}
	return nil
}
