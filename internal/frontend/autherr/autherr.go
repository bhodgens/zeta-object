// Package autherr classifies an authenticator's error into a client-side
// CREDENTIAL REJECTION (the caller answers 401 / a challenge) or an
// INTERNAL FAULT (the caller answers 500).
//
// It exists because that decision is not per-frontend: webdav's dispatch
// pipeline and the owncloud OCS negotiation surface are different wire
// protocols rendering the same auth adapters, so a sentinel list kept in
// each package goes stale in one of them. The failure mode is silent and
// expensive — a client holding a perfectly good credential but missing the
// frontend's expected one (an mTLS device certificate over an HTTP listener)
// was answered 500, which every retry and error-budget path reads as a
// server fault, so a bad certificate alerted instead of being challenged.
//
// THE PREDICATE IS AN EXPLICIT ALLOW-LIST of internal/auth's typed rejection
// sentinels — never "any error". A non-sentinel authenticator failure is a
// wiring/lookup fault and must stay 500.
//
// errors.Is (not ==) so an adapter that annotates a rejection still matches.
//
// MAINTENANCE RULE: a sentinel declared in internal/auth
// (basic.go: ErrBasicMissing, ErrBasicMalformed, ErrBadCredentials,
// ErrBasicUnsupported; cert.go: ErrCertMissing, ErrCertUnknownCN,
// ErrCertRegistryNil) belongs here in the SAME change, and the new sentinel
// needs a wire test on every frontend that renders 401 from it.
package autherr

import (
	"errors"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// IsCredentialRejection reports whether err is a client-side credential
// rejection rather than an authenticator-internal fault.
//
// Both shipped adapters contribute: the four Basic sentinels and the three
// certificate sentinels (ErrCertMissing with no peer certificate,
// ErrCertUnknownCN for an empty or unregistered CN, ErrCertRegistryNil when
// the adapter cannot consult a registry — all three fail closed).
// auth.ErrKeyMalformed / auth.ErrKeyUnknown (the ssh public-key adapter) are
// deliberately NOT here: they are SFTP-protocol failures, not HTTP credential
// rejections, and this predicate only classifies the HTTP/WebDAV adapters.
func IsCredentialRejection(err error) bool {
	return errors.Is(err, auth.ErrBasicMissing) ||
		errors.Is(err, auth.ErrBasicMalformed) ||
		errors.Is(err, auth.ErrBadCredentials) ||
		errors.Is(err, auth.ErrBasicUnsupported) ||
		errors.Is(err, auth.ErrCertMissing) ||
		errors.Is(err, auth.ErrCertUnknownCN) ||
		errors.Is(err, auth.ErrCertRegistryNil)
}
