// files.go — the small frontend.AuthorizeRequest alias shared by the SFTP
// driver (mirror of the ftp frontend's helper; the decision lives in
// package frontend, this is just the local entry point).
package sftp

import (
	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// frontendAuthorize aliases the shared grant helper (leaf 05).
func frontendAuthorize(id auth.Identity, bucket string, write bool) error {
	return frontend.AuthorizeRequest(id, bucket, write)
}
