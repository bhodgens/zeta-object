// resolver.go — the SigV4-side resolution of ZSTSID- temporary
// credentials (accessstor appliance tree leaf 02, Task 2). Package main
// installs ONE resolver over the live FederationRegistry and the shared
// signing key; the s3 frontend's credentialSecret branches on the
// ZSTSID- prefix and consults it. Per-request resolution: grants are
// re-derived from the credential's claims EVERY request — no stale grant
// caching across the TTL (revocation = grant removal is visible on the
// next request; expiry is a hard stop).
//
// Charter: pure derivation and mapping, zero stored state.
package sts

import (
	"encoding/base64"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// RegistryFunc returns the LIVE federation registry (it is rebuilt on
// SIGHUP / POST /auth/reload, so the resolver must read through the
// getter, never hold a stale pointer). A nil return fails closed.
type RegistryFunc func() *auth.FederationRegistry

// CredentialResolver resolves ZSTSID- access keys for the SigV4 path.
// The zero value is not usable; NewCredentialResolver wires it.
type CredentialResolver struct {
	registry  RegistryFunc
	serverKey *atomic.Pointer[[]byte]
	now       func() time.Time // test seam
}

// NewCredentialResolver builds the resolver. serverKey is the process
// signing-key slot (package main re-reads the key file on reload and
// Stores a fresh value; every resolution sees the current key, so a
// rotation invalidates outstanding credentials fail-closed).
func NewCredentialResolver(reg RegistryFunc, serverKey *atomic.Pointer[[]byte]) *CredentialResolver {
	return &CredentialResolver{registry: reg, serverKey: serverKey, now: time.Now}
}

// setNow replaces the clock (tests only). Exported surface reserved for
// the exchange's own test suite; unexported so production wiring cannot
// reach it.
//
//nolint:unused // reserved test seam (see above); keeping it beside the field it controls
//lint:ignore U1000 reserved clock-injection test seam - `now` is load-bearing in Resolve (r.now().Before(expiry)); no exchange test references it yet, so staticcheck's U1000 would otherwise flag the setter
func (r *CredentialResolver) setNow(f func() time.Time) { r.now = f }

// Resolve validates one ZSTSID- key id and returns the SigV4 signing
// secret plus the federated identity (grants resolved NOW). ok=false on
// ANY failure — the s3 path then answers InvalidAccessKeyId exactly as it
// would for an unknown access key, leaking nothing about which stage
// failed.
func (r *CredentialResolver) Resolve(accessKeyID string) (secret string, id auth.Identity, ok bool) {
	if r == nil || r.registry == nil || r.serverKey == nil {
		return "", auth.Identity{}, false
	}
	keyPtr := r.serverKey.Load()
	if keyPtr == nil || len(*keyPtr) == 0 {
		return "", auth.Identity{}, false
	}
	serverKey := *keyPtr
	fed := r.registry()
	if fed == nil {
		return "", auth.Identity{}, false
	}
	principal, expiry, _, groups, err := ParseAccessKey(accessKeyID)
	if err != nil {
		return "", auth.Identity{}, false
	}
	// Expiry is a hard stop, checked before any secret material is
	// computed (the expiry is inside the HMAC payload regardless, so a
	// tampered expiry fails the signature check even without this gate).
	if !r.now().Before(expiry) {
		return "", auth.Identity{}, false
	}
	// Re-derive the secret from the key id alone (stateless). The
	// request's SigV4 signature is the authenticity proof: a tampered
	// payload re-derives a DIFFERENT secret and the signature comparison
	// (timing-safe, in the s3 path) fails.
	payload, err := payloadOf(accessKeyID)
	if err != nil {
		return "", auth.Identity{}, false
	}
	mac := DeriveSecret(serverKey, payload)
	derived := base64.RawURLEncoding.EncodeToString(mac)
	// Grants re-resolved AT REQUEST TIME from the live registry. The
	// claim names here are the federation DEFAULTS (ClaimUsername /
	// ClaimGroups mirror leaf 01's defaults); a credential minted under
	// customized claim names yields a prefix/grant count mismatch below
	// and fails closed rather than serving wrong grants.
	username, err := UsernameFromPrincipal(auth.Principal(principal))
	if err != nil {
		return "", auth.Identity{}, false
	}
	claims := map[string]any{ClaimUsername: username}
	if len(groups) > 0 {
		// []any, not []string: leaf 01's GrantsFor asserts the JSON
		// decode shape ([]any) for the groups claim.
		anyGroups := make([]any, len(groups))
		for i, g := range groups {
			anyGroups[i] = g
		}
		claims[ClaimGroups] = anyGroups
	}
	values := fed.GrantsFor(auth.Principal(principal), claims)
	prefixes := GrantPrefixes(username, groups)
	if len(prefixes) != len(values) || len(prefixes) == 0 {
		// Drift between the stateless reconstruction and the registry's
		// deterministic rule (e.g. customized claim names, empty claims):
		// fail closed, never guess.
		return "", auth.Identity{}, false
	}
	grants := make(map[string]auth.Grant, len(prefixes))
	for i := range prefixes {
		grants[BucketForPrefix(prefixes[i])] = values[i]
	}
	// AccessKeyID stays the full ZSTSID- key id: the writer xattr
	// breadcrumb (user.zeta.writer.<keyID>) records it as-is, so a
	// temporary credential's writes attribute to the credential, not to
	// a static key.
	return derived, auth.Identity{AccessKeyID: accessKeyID, BucketGrants: grants}, true
}

// SecretStringFor is the mint-side check helper: derives the secret a
// PRESENTED key id should carry under the CURRENT server key. Exported
// for tests that assert mint/verify consistency without the registry.
//
// Timing discipline note (review checklist): this derivation produces the
// expected secret; the AUTHENTICITY decision is the SigV4 signature
// comparison in the s3 frontend, which runs hmacEqual — a fixed-length
// timing-safe compare. No secret material is ever compared by this
// package.
func SecretStringFor(serverKey []byte, keyID string) (string, error) {
	if len(serverKey) == 0 {
		return "", fmt.Errorf("sts: signing key is empty")
	}
	return SecretString(serverKey, keyID)
}
