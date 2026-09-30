package ftp

import (
	"errors"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// PasswordVerifier is the protocol-neutral USER/PASS seam: verify a login
// pair and resolve it to the identity whose grants authorize storage
// access. The factory wires this to auth.IdentityRegistry's
// LookupByBasicCredential (leaf 05); tests inject fakes.
type PasswordVerifier interface {
	Verify(user, password string) (auth.Identity, bool)
}

// RegistryVerifier adapts the landed auth.IdentityRegistry (password lookup
// IS the Basic-credential lookup: username = access key, password = secret).
type RegistryVerifier struct {
	reg interface {
		LookupByBasicCredential(username, password string) (auth.Identity, bool)
	}
}

// NewRegistryVerifier wires the verifier to an identity registry.
func NewRegistryVerifier(reg interface {
	LookupByBasicCredential(username, password string) (auth.Identity, bool)
}) *RegistryVerifier {
	return &RegistryVerifier{reg: reg}
}

// Verify resolves (user, password) through the registry. No credential
// comparison lives in this package — internal/auth is the only decision
// point (leaf 05 done-when).
func (v *RegistryVerifier) Verify(user, password string) (auth.Identity, bool) {
	if v.reg == nil {
		return auth.Identity{}, false
	}
	return v.reg.LookupByBasicCredential(user, password)
}

// errAuthFailed is the typed auth failure (→ 530 for USER/PASS).
var errAuthFailed = errors.New("authentication failed")
