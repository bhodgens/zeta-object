// seam.go — the exported surface the s3 frontend and package main
// consume for temporary-credential resolution (leaf 02 Task 2).
//
// Dependency direction: package main imports BOTH s3 and sts and wires
// them together; the s3 package imports sts for the prefix constant and
// the TempCredentialSource interface ONLY (no sts construction, no
// crypto here beyond what tempcreds.go owns).
package sts

import (
	"sync/atomic"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// tempCredentialSource is the contract the s3 frontend consults for a
// ZSTSID- key (defined here, referenced by name from s3 through the
// exported alias below — one definition, two consumers).
//
//	Resolve returns the SigV4 signing SECRET (base64url, exactly what the
//	exchange returned) plus the federated identity with grants resolved AT
//	REQUEST TIME. ok=false on any failure (expired, tampered, registry
//	unavailable, grant-shape drift); the s3 path then answers
//	InvalidAccessKeyId, leaking nothing.
type tempCredentialSource interface {
	Resolve(accessKeyID string) (secret string, id auth.Identity, ok bool)
}

// TempCredentialSource is the exported alias the s3 frontend's hook uses.
type TempCredentialSource = tempCredentialSource

// Compile-time: *CredentialResolver is the production implementation.
var _ TempCredentialSource = (*CredentialResolver)(nil)

// NewSigningKeySlot allocates the process signing-key slot package main
// owns and the resolver + frontend share.
func NewSigningKeySlot() *atomic.Pointer[[]byte] { return &atomic.Pointer[[]byte]{} }
