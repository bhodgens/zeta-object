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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// IdentityConfig is the config.json "identities" entry shape (leaf 01,
// frozen JSON keys). Name is for logs only; AccessKey is also the Basic-auth
// username (one credential namespace, one registry); SSHPublicKeys are
// authorized_keys-format lines consumed by the future SFTP frontend.
//
// Grants is the DUAL-FORM grant map (leaf 09): a string value is the
// legacy frozen shorthand ("readonly"/"readwrite"); an object value is a
// rich grant expression (prefix/op/time-scoped — see richgrants.go). The
// Go field type is the raw-JSON carrier so both forms decode; every
// legacy document parses to identical semantics (golden-tested).
type IdentityConfig struct {
	Name          string                     `json:"name"`
	AccessKey     string                     `json:"accessKey"`
	SecretKey     string                     `json:"secretKey"`
	Grants        map[string]json.RawMessage `json:"grants,omitempty"`
	SSHPublicKeys []string                   `json:"sshPublicKeys,omitempty"`
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

// dummySecretHash is the fixed digest the unknown-user path compares against,
// so its timing depends only on the presented password, not on stored state.
var dummySecretHash = secretHash("zeta-object-registry-constant-shape-miss")

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
		// Principal breadcrumb boundary (design zfs-principal-metadata.md
		// 2a: "validate at config load that access keys fit the prefix
		// budget"): xattr names cap at 255 bytes, and the longest
		// breadcrumb this build stamps is user.zeta.writer.<AccessKeyID>.
		// The arithmetic pairs with fsbackend's xattr.go constants (kept
		// local to avoid an auth→fsbackend import; a comment there pins
		// the pairing). Trivially true for real keys — the check
		// documents the boundary and fails loud naming the offender.
		if n := len("user.zeta.writer.") + len(cfg.AccessKey); n >= 255 {
			return nil, fmt.Errorf("identity %q: accessKey %d bytes long; user.zeta.writer breadcrumb name would reach the 255-byte xattr-name cap", cfg.Name, len(cfg.AccessKey))
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
		grants, rich, err := parseGrants(cfg)
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
		// Rich grants (leaf 09): register the parsed expressions beside the
		// frozen floor map. Registered unconditionally — even when empty — so
		// a reload fully replaces any table a previous build left under this
		// access key (a downgraded/revoked identity never keeps rich grants).
		st.identity = st.identity.WithRichGrants(rich)
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

// parseGrants translates the DUAL-FORM grants map into the frozen floor
// map plus the rich expressions (leaf 09). Absent grants ⇒ wildcard
// readwrite (the historical single-pair semantic) — but ONLY when there are
// no rich entries either.
//
// Floor rules (leaf 09 Contract 3):
//   - legacy string entries build the frozen map exactly as before;
//   - every rich entry is parsed fail-loud and derives its implied v1 floor
//     via applyFloorUnion (read/write bits for unbounded entries; a
//     zero-bit marker for time-scoped entries; nothing for non-read/write
//     entries).
func parseGrants(cfg IdentityConfig) (map[string]Grant, []GrantExpr, error) {
	if len(cfg.Grants) == 0 {
		return map[string]Grant{"*": {Read: true, Write: true}}, nil, nil
	}
	// Pass 1: parse every entry fail-loud (pattern grammar + value form).
	type parsedEntry struct {
		legacy  *Grant
		rich    GrantExpr
		hasRich bool
	}
	parsed := make(map[string]parsedEntry, len(cfg.Grants))
	richOrder := make([]string, 0, len(cfg.Grants))
	for bucket, value := range cfg.Grants {
		if bucket == "" {
			return nil, nil, fmt.Errorf("identity %q: grant bucket name must not be empty", cfg.Name)
		}
		// The SFTP frontend serializes grants into CriticalOptions as
		// comma-separated key=value pairs; a bucket name containing "," or
		// "=" would round-trip as a wider grant. Reject at config load.
		// (The BUCKET COMPONENT only — the full pattern key, including any
		// prefix form, stays intact for ParseGrantValue and the rich table.)
		bucketComp := bucket
		if bucketComp != "*" {
			if i := strings.IndexByte(bucketComp, '/'); i >= 0 {
				bucketComp = bucketComp[:i]
			}
		}
		if strings.ContainsAny(bucketComp, ",=") {
			return nil, nil, fmt.Errorf("identity %q: grant bucket name %q must not contain %q or %q",
				cfg.Name, bucketComp, ",", "=")
		}
		expr, err := ParseGrantValue(cfg.Name, bucket, value)
		if err != nil {
			return nil, nil, err
		}
		entry := parsedEntry{}
		trimmed := strings.TrimSpace(string(value))
		if strings.HasPrefix(trimmed, "\"") {
			// Legacy string form: translate through the frozen two-bit map
			// by reading the parsed expression's ops (readonly = read,
			// readwrite = read+write) — same frozen vocabulary, same error
			// messages, same map values.
			g := Grant{Read: expr.Allows(OpRead), Write: expr.Allows(OpWrite)}
			entry.legacy = &g
		} else {
			entry.rich, entry.hasRich = expr, true
			richOrder = append(richOrder, bucket)
		}
		parsed[bucket] = entry
	}
	// Pass 2: build the frozen floor. Legacy entries first (exact v1
	// values), then the rich entries' floor union.
	grants := make(map[string]Grant, len(parsed))
	var rich []GrantExpr
	for bucket, entry := range parsed {
		if entry.legacy != nil {
			grants[bucket] = *entry.legacy
		}
	}
	for _, bucket := range richOrder {
		entry := parsed[bucket]
		rich = append(rich, entry.rich)
		applyFloorUnion(grants, entry.rich)
	}
	return grants, rich, nil
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
// username is the access key. The PRESENTED password alone is hashed and
// compared against the precomputed stored digest (constant-time). Both miss
// paths (unknown user, wrong password) take the same hash-then-compare work
// against a fixed dummy digest so timing never distinguishes them.
func (r *MultiRegistry) LookupByBasicCredential(username, password string) (Identity, bool) {
	presented := secretHash(password)
	st, ok := r.byAccessKey[username]
	if !ok {
		// Unknown user: hash the presented password and compare against a
		// fixed dummy digest so timing depends only on the presented input.
		subtle.ConstantTimeCompare(presented[:], dummySecretHash[:])
		return Identity{}, false
	}
	if subtle.ConstantTimeCompare(presented[:], st.secretHash[:]) != 1 {
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

// RichGrantsFor returns the parsed rich expressions registered under
// accessKeyID (leaf 09 — the SFTP session handler's one re-resolution
// lookup at session start). Unknown keys and legacy-only identities return
// nil ⇒ pure v1 behavior.
func (r *MultiRegistry) RichGrantsFor(accessKeyID string) []GrantExpr {
	st, ok := r.byAccessKey[accessKeyID]
	if !ok {
		return nil
	}
	return st.identity.RichGrants()
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
