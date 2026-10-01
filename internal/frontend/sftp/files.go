// files.go — the small frontend.AuthorizeRequest alias shared by the SFTP
// driver (mirror of the ftp frontend's helper; the decision lives in
// package frontend, this is just the local entry point).
package sftp

import (
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// frontendAuthorize aliases the shared grant helper (leaf 05). Leaf 09:
// the decision is auth.AuthorizeOp — the SFTP adapter maps commands to Op
// (write flag → write|read) and the resolved object key rides through so
// prefix-scoped rich grants apply per object.
func frontendAuthorizeKey(id auth.Identity, bucket, key string, write bool) error {
	op := auth.OpRead
	if write {
		op = auth.OpWrite
	}
	if !auth.AuthorizeOp(id, op, bucket, key, time.Now().UTC()) {
		return &frontend.AuthzError{Bucket: bucket, Write: write}
	}
	return nil
}

// frontendAuthorize is the bucket-level form (no key) kept for the
// list/stat paths whose key is not yet resolved.
func frontendAuthorize(id auth.Identity, bucket string, write bool) error {
	return frontendAuthorizeKey(id, bucket, "", write)
}
