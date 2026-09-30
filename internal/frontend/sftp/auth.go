package sftp

import (
	"errors"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// PasswordVerifier is the protocol-neutral password seam: verify a login
// pair and resolve it to the identity whose grants authorize storage
// access. The factory wires this to auth.IdentityRegistry's
// LookupByBasicCredential (leaf 05); tests inject fakes.
type PasswordVerifier interface {
	Verify(user, password string) (auth.Identity, bool)
}

// PublicKeyChecker is the public-key seam: Accept reports the identity
// (AccessKeyID + BucketGrants) authorized for key. The factory wires this
// to auth.PublicKeyAuthenticator (the auth tree's landed association hook).
type PublicKeyChecker interface {
	Accept(pubKey authorizedKey) (auth.Identity, bool)
}

// authorizedKey is the narrow view of an ssh.PublicKey this package needs
// (keeps x/crypto/ssh out of the seam's type signature).
type authorizedKey interface {
	Type() string
	Marshal() []byte
}

// RegistryVerifier adapts the landed auth.IdentityRegistry.
type RegistryVerifier struct {
	reg interface {
		LookupByBasicCredential(username, password string) (auth.Identity, bool)
	}
}

// NewRegistryVerifier wires the password verifier to an identity registry.
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

// RegistryKeyChecker adapts auth.PublicKeyAuthenticator (the auth tree's
// landed public-key→identity association) to the seam. The presented
// ssh.PublicKey is rendered authorized_keys-style ("type blob") and
// canonicalized INSIDE internal/auth — this package never compares keys.
type RegistryKeyChecker struct {
	pka auth.PublicKeyAuthenticator
}

// NewRegistryKeyChecker wires the key checker to the registry's
// public-key authenticator.
func NewRegistryKeyChecker(pka auth.PublicKeyAuthenticator) *RegistryKeyChecker {
	return &RegistryKeyChecker{pka: pka}
}

// Accept renders the presented key as "<type> <base64-blob>" and resolves
// it through the registry. Malformed/unknown → false (SSH auth failure).
func (c *RegistryKeyChecker) Accept(pubKey authorizedKey) (auth.Identity, bool) {
	if c.pka == nil || pubKey == nil {
		return auth.Identity{}, false
	}
	line := pubKey.Type() + " " + marshalKeyBlob(pubKey)
	id, err := c.pka.AuthenticatePublicKey(line)
	if err != nil {
		return auth.Identity{}, false
	}
	return id, true
}

// marshalKeyBlob base64-encodes the wire form of the public key.
func marshalKeyBlob(pubKey interface {
	Marshal() []byte
}) string {
	return base64Encode(pubKey.Marshal())
}

// errAuthFailed is the typed auth failure (SSH disconnect, no subsystem).
var errAuthFailed = errors.New("sftp: authentication failed")
