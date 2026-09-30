// registry.go — the multi-identity registry (pluggable-authentication tree
// leaf 01). This is THE credential→identity decision point: wire adapters
// (SigV4 in the s3 frontend, Basic in basic.go, public keys in publickey.go)
// translate their wire formats into lookups here; no frontend holds its own
// credential table.
//
// Secrets are hashed with SHA-256 at build time and compared with
// crypto/subtle.ConstantTimeCompare over the fixed-length digests, so
// neither content nor length of a secret leaks through timing.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"sort"
)

// IdentityConfig is the config.json "identities" entry shape (leaf 01,
// frozen JSON keys). Name is for logs only; AccessKey is also the Basic-auth
// username (one credential namespace, one registry); SSHPublicKeys are
// authorized_keys-format lines consumed by the future SFTP frontend.
type IdentityConfig struct {
	Name          string            `json:"name"`
	AccessKey     string            `json:"accessKey"`
	SecretKey     string            `json:"secretKey"`
	Grants        map[string]string `json:"grants,omitempty"`
	SSHPublicKeys []string          `json:"sshPublicKeys,omitempty"`
}

// IdentityRegistry resolves wire credentials to identities. An OAuth/OIDC
// (option 2) or subprocess (option 3) backend implements this same
// interface; the static config-backed MultiRegistry ships with option 1.
type IdentityRegistry interface {
	// LookupByAccessKey: SigV4 access key ID → identity.
	LookupByAccessKey(accessKeyID string) (Identity, bool)
	// LookupByBasicCredential: Basic username+password → identity. The
	// username IS the access key (one namespace, one registry); the
	// password is the secret key. Constant-time comparison.
	LookupByBasicCredential(username, password string) (Identity, bool)
	// LookupByPublicKey: authorized_keys-format key line → identity
	// (consumed by the future SFTP frontend via PublicKeyAuthenticator).
	LookupByPublicKey(authorizedKey string) (Identity, bool)
}

// compile-time: MultiRegistry still satisfies the frozen v1 CredentialSource
// so the existing S3 adapter keeps working against it.
var _ CredentialSource = (*MultiRegistry)(nil)

// grant value vocabulary (frozen JSON values).
const (
	GrantReadOnly  = "readonly"
	GrantReadWrite = "readwrite"
)

// secretHash hashes a secret for constant-time comparison. Fixed-length
// SHA-256 digests mean comparison time never depends on input length.
func secretHash(secret string) [sha256.Size]byte { return sha256.Sum256([]byte(secret)) }

// secretsEqual compares two secrets in constant time via their digests.
func secretsEqual(a, b string) bool {
	h1, h2 := secretHash(a), secretHash(b)
	return subtle.ConstantTimeCompare(h1[:], h2[:]) == 1
}

// storedIdentity is MultiRegistry's internal per-identity record. The raw
// secret is retained because SigV4 needs it for the signing-key derivation;
// it is never logged. Basic credential checks compare SHA-256 digests
// (constant-time), not the raw bytes.
type storedIdentity struct {
	identity   Identity // BucketGrants populated; no secret material
	secret     string   // raw secret key (SigV4 signing; never logged)
	secretHash [sha256.Size]byte
	publicKeys map[string]string // canonical key → SHA-256 fingerprint (log-safe)
}

// MultiRegistry is the static, config-backed IdentityRegistry: N identities,
// each with optional grants, wildcard support, constant-time secret
// comparison, and authorized_keys-format public-key matching.
type MultiRegistry struct {
	byAccessKey    map[string]*storedIdentity
	byName         map[string]*storedIdentity
	publicKeyOwner map[string]*storedIdentity // canonical key → identity
}

// NewMultiRegistry validates the identity set and builds the lookup tables.
// All validation is fail-loud (the error names the offender):
//   - name, accessKey, secretKey must be non-empty
//   - names, access keys, and canonical public keys must be unique
//   - grant values must be "readonly" or "readwrite"; a grant key must be
//     non-empty ("*" is the wildcard bucket); duplicate grant keys cannot
//     occur (map), but "*" and a bucket may coexist (per-bucket beats "*"
//     for that bucket only in effect)
func NewMultiRegistry(identities []IdentityConfig) (*MultiRegistry, error) {
	reg := &MultiRegistry{
		byAccessKey:    make(map[string]*storedIdentity, len(identities)),
		byName:         make(map[string]*storedIdentity, len(identities)),
		publicKeyOwner: make(map[string]*storedIdentity),
	}
	for i, cfg := range identities {
		if cfg.Name == "" {
			return nil, fmt.Errorf("identity %d: name is required", i)
		}
		if cfg.AccessKey == "" {
			return nil, fmt.Errorf("identity %q: accessKey is required", cfg.Name)
		}
		if cfg.SecretKey == "" {
			return nil, fmt.Errorf("identity %q: secretKey is required", cfg.Name)
		}
		if _, dup := reg.byName[cfg.Name]; dup {
			return nil, fmt.Errorf("identity name %q is configured more than once", cfg.Name)
		}
		if _, dup := reg.byAccessKey[cfg.AccessKey]; dup {
			return nil, fmt.Errorf("access key of identity %q is configured more than once", cfg.Name)
		}
		grants, err := parseGrants(cfg)
		if err != nil {
			return nil, err
		}
		st := &storedIdentity{
			identity: Identity{
				AccessKeyID:  cfg.AccessKey,
				BucketGrants: grants,
			},
			secret:     cfg.SecretKey,
			secretHash: secretHash(cfg.SecretKey),
			publicKeys: make(map[string]string),
		}
		// Public keys: canonicalized (leaf 04) and deduplicated across the
		// whole registry — one key must resolve to exactly one identity.
		for _, keyLine := range cfg.SSHPublicKeys {
			canonical, fingerprint, err := CanonicalizePublicKey(keyLine)
			if err != nil {
				return nil, fmt.Errorf("identity %q: sshPublicKeys: %w", cfg.Name, err)
			}
			if _, dup := reg.publicKeyOwner[canonical]; dup {
				return nil, fmt.Errorf("ssh public key of identity %q is configured more than once", cfg.Name)
			}
			st.publicKeys[canonical] = fingerprint
			reg.publicKeyOwner[canonical] = st
		}
		reg.byName[cfg.Name] = st
		reg.byAccessKey[cfg.AccessKey] = st
	}
	return reg, nil
}

// parseGrants translates the JSON string grants into Grant values. Absent
// grants ⇒ wildcard readwrite (the historical single-pair semantic).
func parseGrants(cfg IdentityConfig) (map[string]Grant, error) {
	if len(cfg.Grants) == 0 {
		return map[string]Grant{"*": {Read: true, Write: true}}, nil
	}
	grants := make(map[string]Grant, len(cfg.Grants))
	for bucket, value := range cfg.Grants {
		if bucket == "" {
			return nil, fmt.Errorf("identity %q: grant bucket name must not be empty", cfg.Name)
		}
		switch value {
		case GrantReadOnly:
			grants[bucket] = Grant{Read: true}
		case GrantReadWrite:
			grants[bucket] = Grant{Read: true, Write: true}
		default:
			return nil, fmt.Errorf("identity %q: unknown grant value %q for bucket %q (want %q or %q)",
				cfg.Name, value, bucket, GrantReadOnly, GrantReadWrite)
		}
	}
	return grants, nil
}

// LookupByAccessKey resolves a SigV4 access key ID.
func (r *MultiRegistry) LookupByAccessKey(accessKeyID string) (Identity, bool) {
	st, ok := r.byAccessKey[accessKeyID]
	if !ok {
		return Identity{}, false
	}
	return st.identity, true
}

// LookupByBasicCredential resolves a Basic username+password pair. The
// username is the access key. Both miss paths (unknown user, wrong
// password) take the same hash-then-compare work so timing never
// distinguishes them.
func (r *MultiRegistry) LookupByBasicCredential(username, password string) (Identity, bool) {
	st, ok := r.byAccessKey[username]
	if !ok {
		// Unknown user: burn the same hash+compare work as the hit path
		// so miss timing matches hit timing.
		secretsEqual(password, "zeta-object-registry-constant-shape-miss")
		return Identity{}, false
	}
	if !secretsEqual(password, st.secret) {
		return Identity{}, false
	}
	return st.identity, true
}

// LookupByPublicKey resolves an authorized_keys-format key line. Malformed
// input is an error (never a silent miss); valid-but-unregistered keys
// return ErrKeyUnknown.
func (r *MultiRegistry) LookupByPublicKey(authorizedKey string) (Identity, bool) {
	canonical, _, err := CanonicalizePublicKey(authorizedKey)
	if err != nil {
		return Identity{}, false
	}
	st, ok := r.publicKeyOwner[canonical]
	if !ok {
		return Identity{}, false
	}
	return st.identity, true
}

// SecretKey implements the frozen v1 CredentialSource so the existing S3
// adapter keeps compiling against a MultiRegistry (master Contract 3).
func (r *MultiRegistry) SecretKey(accessKeyID string) (string, bool) {
	st, ok := r.byAccessKey[accessKeyID]
	if !ok {
		return "", false
	}
	return st.secret, true
}

// Names returns the configured identity names in sorted order (log-safe
// diagnostics; never secret material).
func (r *MultiRegistry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
