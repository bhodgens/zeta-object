package auth

import (
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
)

// cert.go — the mTLS certificate authenticator (quic-h3-2026-10 leaf 02).
// The h3 frontend's auth adapter: the TLS handshake has ALREADY verified the
// peer certificate against the listener's ClientCAs (RequireAndVerifyClientCert),
// so this adapter maps the verified certificate's Subject CN onto the identity
// registry. The CN is the principal (per-device certificates: CN = device name).
//
// Fail-closed: a request without a peer certificate, or with a verified
// certificate whose CN has no registry identity, is a typed rejection — the
// webdav pipeline renders every typed rejection as 401 + WWW-Authenticate.
// (A wrong-CA certificate never reaches this adapter: the handshake rejects it.)

// Sentinel rejections. Callers must not distinguish them from the Basic
// sentinels' treatment: all render as the same client error on the wire.
var (
	ErrCertMissing     = errors.New("auth: no client certificate presented")
	ErrCertUnknownCN   = errors.New("auth: client certificate CN is not a known identity")
	ErrCertRegistryNil = errors.New("auth: registry does not support certificate identities")
)

// IdentityRegistry is the lookup seam CertAuthenticator resolves CNs through.
// It is the frozen v1 registry interface: CN == AccessKey (one principal
// namespace), so an mTLS client's grants are exactly that identity's grants.
type certIdentityRegistry interface {
	LookupByAccessKey(accessKeyID string) (Identity, bool)
}

// CertAuthenticator authenticates a request by mapping its TLS peer
// certificate's Subject CN through the identity registry.
type CertAuthenticator struct {
	registry certIdentityRegistry
}

// compile-time: the adapter satisfies the frozen Authenticator seam.
var _ Authenticator = (*CertAuthenticator)(nil)

// NewCertAuthenticator builds the adapter over reg. A nil registry panics at
// construction (same posture as NewBasicAuthenticator): a nil registry is a
// wiring bug, not a runtime condition.
func NewCertAuthenticator(reg certIdentityRegistry) *CertAuthenticator {
	if reg == nil {
		panic("auth: CertAuthenticator requires a non-nil registry")
	}
	return &CertAuthenticator{registry: reg}
}

// Authenticate implements auth.Authenticator. Rejections are typed:
// no peer certificate → ErrCertMissing; empty CN or unknown CN →
// ErrCertUnknownCN (one sentinel: a probe cannot enumerate device names by
// distinguishing empty from unknown).
func (c *CertAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Identity{}, ErrCertMissing
	}
	cn := r.TLS.PeerCertificates[0].Subject.CommonName
	if strings.TrimSpace(cn) == "" {
		return Identity{}, ErrCertUnknownCN
	}
	id, ok := c.registry.LookupByAccessKey(cn)
	if !ok {
		return Identity{}, ErrCertUnknownCN
	}
	return id, nil
}

// IdentityFromCertificate is the pure CN→identity mapping, exported for the
// h3 frontend's unit table and any future adapter that reasons about a
// certificate outside a request.
func IdentityFromCertificate(reg certIdentityRegistry, cert *x509.Certificate) (Identity, error) {
	if cert == nil {
		return Identity{}, ErrCertMissing
	}
	return identityForCN(reg, cert.Subject.CommonName)
}

func identityForCN(reg certIdentityRegistry, cn string) (Identity, error) {
	if strings.TrimSpace(cn) == "" {
		return Identity{}, ErrCertUnknownCN
	}
	id, ok := reg.LookupByAccessKey(cn)
	if !ok {
		return Identity{}, ErrCertUnknownCN
	}
	return id, nil
}

// CertificateCN returns the peer certificate's Subject CN, or "" when the
// request carries no peer certificate. Diagnostic helper only — never an
// authorization decision (Authenticate owns those).
func CertificateCN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName
}
