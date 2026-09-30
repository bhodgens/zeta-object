// basic.go — the HTTP Basic authenticator (pluggable-authentication tree
// leaf 03). One adapter, shared by every HTTP frontend that does not speak
// SigV4 (WebDAV/ownCloud/FTP-over-HTTP shapes later): parse the Basic
// header, resolve through the IdentityRegistry, return the identity with
// grants. The server is TLS-only (certFile/keyFile are required), so
// credentials cross the wire once, encrypted; the adapter itself does not
// check the scheme.
package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

// Typed rejections. Frontends render them however their wire protocol
// speaks (S3: 403 AccessDenied; WebDAV later: 401 + WWW-Authenticate:
// Basic). Unknown-user and wrong-password failures are the SAME sentinel —
// callers must never be able to distinguish them.
var (
	ErrBasicMissing     = errors.New("auth: missing Basic Authorization header")
	ErrBasicMalformed   = errors.New("auth: malformed Basic Authorization header")
	ErrBadCredentials   = errors.New("auth: invalid credentials")
	ErrBasicUnsupported = errors.New("auth: registry does not support Basic credentials")
)

// BasicAuthenticator authenticates HTTP Basic credentials against the
// registry. Username is the access key; password is the secret key
// (one credential namespace, one registry).
type BasicAuthenticator struct {
	registry IdentityRegistry
}

// compile-time: the adapter satisfies the frozen v1 Authenticator seam.
var _ Authenticator = (*BasicAuthenticator)(nil)

// NewBasicAuthenticator builds the adapter over reg (never nil-checked:
// a nil registry is a wiring bug that must panic at construction).
func NewBasicAuthenticator(reg IdentityRegistry) *BasicAuthenticator {
	return &BasicAuthenticator{registry: reg}
}

// Authenticate implements auth.Authenticator. Rejections are typed:
// missing header → ErrBasicMissing; non-Basic scheme or bad base64 or no
// colon in the decoded pair → ErrBasicMalformed; unknown user / wrong
// password → ErrBadCredentials (same sentinel for both).
func (b *BasicAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return Identity{}, ErrBasicMissing
	}
	scheme, encoded, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "Basic") {
		return Identity{}, ErrBasicMalformed
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		// Accept unpadded input too (clients occasionally strip padding).
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return Identity{}, ErrBasicMalformed
		}
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		// Split on the FIRST colon only (passwords may contain colons);
		// a missing colon is structurally malformed.
		return Identity{}, ErrBasicMalformed
	}
	id, found := b.registry.LookupByBasicCredential(username, password)
	if !found {
		return Identity{}, ErrBadCredentials
	}
	return id, nil
}
