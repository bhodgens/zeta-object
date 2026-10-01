package auth

import "sync"

// reload.go — the hot-reloadable registry wrapper (design-leaf 08,
// docs/plans/auth-2026-09/extensions/08-key-rotation.md, Contract 1).
//
// ReloadableRegistry is an IdentityRegistry wrapper whose inner registry can
// be swapped atomically at runtime (SIGHUP reload in package main). The
// initial inner registry is the startup build, so behavior before the first
// SIGHUP is byte-identical to the pre-leaf behavior. Delegation is per-call
// under a read lock, so a swap between two requests is immediately visible;
// a swap DURING one request is not observed mid-request (the lookup is a
// single delegated call). In-flight requests that already hold an Identity
// value keep its grants for that request — bounded by request duration.
//
// Swap NEVER validates — validation happened in the builder that produced
// the new registry (single-decision rule: NewMultiRegistry remains the only
// validator). Charter: no state is persisted anywhere; the operator's
// config.json on disk is the record of what is and is not valid.

// ReloadableRegistry is an IdentityRegistry whose inner registry can be
// swapped atomically. Construct with NewReloadableRegistry; the zero value
// is not usable (no inner registry to delegate to).
type ReloadableRegistry struct {
	mu    sync.RWMutex
	inner IdentityRegistry // never nil after construction
}

// compile-time: satisfies both frozen seams (IdentityRegistry for every
// frontend; CredentialSource so the s3 adapter's SecretKey path keeps
// working against the wrapper — the env-fallback rotation point).
var (
	_ IdentityRegistry = (*ReloadableRegistry)(nil)
	_ CredentialSource = (*ReloadableRegistry)(nil)
)

// NewReloadableRegistry wraps inner. The wrapper's lookups delegate to
// inner until the first Swap.
func NewReloadableRegistry(inner IdentityRegistry) *ReloadableRegistry {
	return &ReloadableRegistry{inner: inner}
}

// Swap atomically replaces the inner registry and returns the previous one
// (for the caller to log/inspect). newReg must be non-nil; a nil newReg is
// a wiring bug and panics (same posture as a nil registry handed to
// NewBasicAuthenticator) — it can never be the result of a fail-closed
// build, which returns an error and never swaps.
func (r *ReloadableRegistry) Swap(newReg IdentityRegistry) (prev IdentityRegistry) {
	if newReg == nil {
		panic("auth: ReloadableRegistry.Swap called with a nil registry")
	}
	r.mu.Lock()
	prev = r.inner
	r.inner = newReg
	r.mu.Unlock()
	return prev
}

// current returns the inner registry under a read lock — the single read
// path every delegated lookup goes through, so a swap is never observed
// mid-lookup.
func (r *ReloadableRegistry) current() IdentityRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.inner
}

// Registry returns the current inner registry as the narrow password seam
// the sftp factory consumes (design-leaf 08 wiring: package main hands the
// wrapper itself to sftp.ConfigFromOptions; this method adapts it to the
// factory's IdentityRegistries interface).
func (r *ReloadableRegistry) Registry() interface {
	LookupByBasicCredential(username, password string) (Identity, bool)
} {
	return r
}

// Keys returns the wrapper itself as the PublicKeyAuthenticator seam for
// the sftp factory: the wrapper delegates AuthenticatePublicKey through
// Embed to the current inner MultiRegistry.
func (r *ReloadableRegistry) Keys() PublicKeyAuthenticator {
	return reloadablePublicKey{r}
}

// RichGrantsFor re-resolves an identity's rich grant table by access key ID
// through the current inner registry (design-leaf 09: the SFTP session
// handler's one lookup at session start — the CriticalOptions round-trip
// carries only the floor map). A registry whose entries carry no rich
// grants returns nil ⇒ identical pre-leaf behavior.
func (r *ReloadableRegistry) RichGrantsFor(accessKeyID string) []GrantExpr {
	cur, ok := r.current().(interface{ RichGrantsFor(string) []GrantExpr })
	if !ok {
		return nil
	}
	return cur.RichGrantsFor(accessKeyID)
}

// reloadablePublicKey forwards AuthenticatePublicKey through the wrapper's
// single read path, so public-key resolution rotates with the rest.
type reloadablePublicKey struct{ r *ReloadableRegistry }

// AuthenticatePublicKey resolves the presented key against the CURRENT
// inner registry (panic-safe: Swap's non-nil contract guarantees the
// delegation target implements the hook — the only builder is
// NewMultiRegistry).
func (f reloadablePublicKey) AuthenticatePublicKey(presentedKey string) (Identity, error) {
	return f.r.current().(PublicKeyAuthenticator).AuthenticatePublicKey(presentedKey)
}

// LookupByAccessKey resolves a SigV4 access key ID through the current
// inner registry.
func (r *ReloadableRegistry) LookupByAccessKey(accessKeyID string) (Identity, bool) {
	return r.current().LookupByAccessKey(accessKeyID)
}

// LookupByBasicCredential resolves a Basic username+password pair through
// the current inner registry (the constant-time comparison itself lives in
// the inner MultiRegistry — this wrapper adds none of its own).
func (r *ReloadableRegistry) LookupByBasicCredential(username, password string) (Identity, bool) {
	return r.current().LookupByBasicCredential(username, password)
}

// LookupByPublicKey resolves an authorized_keys-format key line through the
// current inner registry.
func (r *ReloadableRegistry) LookupByPublicKey(authorizedKey string) (Identity, bool) {
	return r.current().LookupByPublicKey(authorizedKey)
}

// SecretKey implements the frozen v1 CredentialSource over the current
// inner registry, so the s3 adapter's signing-secret fallback rotates with
// everything else when the wrapper is also installed as the default
// credential source.
func (r *ReloadableRegistry) SecretKey(accessKeyID string) (string, bool) {
	// The recommended wiring (Open Decision 1) builds the inner registry
	// with NewMultiRegistry, which satisfies CredentialSource by
	// construction; an interface-only inner registry (option 2/3 backend)
	// that does not expose secrets makes this a lookup miss, matching the
	// s3 adapter's registrySecretSource posture.
	src, ok := r.current().(CredentialSource)
	if !ok {
		return "", false
	}
	return src.SecretKey(accessKeyID)
}
