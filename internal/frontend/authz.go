// authz.go — the protocol-neutral authorization helper (pluggable-
// authentication tree leaf 03). THE shared grant-decision entry point for
// HTTP frontends: the S3 dispatch and any future WebDAV/ownCloud frontend
// call AuthorizeRequest; the grant logic itself stays in internal/auth
// (Identity.CanRead/CanWrite). Non-HTTP frontends (FTP/SFTP) call the
// Identity methods directly.
package frontend

import (
	"fmt"

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
// requires CanWrite (which implies read); write=false requires CanRead.
// nil means allowed. A service-level request (bucket == "") is always
// allowed here — ListBuckets filtering by grants is per-frontend policy
// computed from the identity's grants, not a per-request denial.
func AuthorizeRequest(id auth.Identity, bucket string, write bool) error {
	if bucket == "" {
		return nil
	}
	if write {
		if !id.CanWrite(bucket) {
			return &AuthzError{Bucket: bucket, Write: true}
		}
		return nil
	}
	if !id.CanRead(bucket) {
		return &AuthzError{Bucket: bucket, Write: false}
	}
	return nil
}
