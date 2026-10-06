// autherr_test.go — the shared 401-vs-500 predicate, pinned against
// internal/auth's declared rejection sentinels.
//
// The frontend-level wire tests (webdav dispatch_certauth_test.go, owncloud
// certauth_predicate_test.go) prove each surface renders what it should. This
// file pins the LIST itself: every rejection sentinel internal/auth declares
// must classify as a credential rejection, and a non-sentinel error must not.
// A sentinel added in internal/auth without a line here re-creates the bug
// this package exists to prevent - silently, because each frontend keeps
// passing its own narrower expectations.
package autherr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/autherr"
)

// TestIsCredentialRejection_Sentinels pins every declared rejection sentinel,
// bare and WRAPPED (errors.Is, not ==: an adapter that annotates a rejection
// with context must still classify as a client error).
func TestIsCredentialRejection_Sentinels(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrBasicMissing", auth.ErrBasicMissing},
		{"ErrBasicMalformed", auth.ErrBasicMalformed},
		{"ErrBadCredentials", auth.ErrBadCredentials},
		{"ErrBasicUnsupported", auth.ErrBasicUnsupported},
		{"ErrCertMissing", auth.ErrCertMissing},
		{"ErrCertUnknownCN", auth.ErrCertUnknownCN},
		{"ErrCertRegistryNil", auth.ErrCertRegistryNil},
		{"wrapped ErrBasicMissing", fmt.Errorf("parse Authorization: %w", auth.ErrBasicMissing)},
		{"wrapped ErrBadCredentials", fmt.Errorf("lookup %q: %w", "AK", auth.ErrBadCredentials)},
		{"wrapped ErrCertUnknownCN", fmt.Errorf("resolve cn %q: %w", "device-x", auth.ErrCertUnknownCN)},
		{"wrapped ErrCertMissing", fmt.Errorf("peer state: %w", auth.ErrCertMissing)},
		{"wrapped ErrBasicMalformed", fmt.Errorf("header: %w", auth.ErrBasicMalformed)},
		{"wrapped ErrBasicUnsupported", fmt.Errorf("registry: %w", auth.ErrBasicUnsupported)},
		{"wrapped ErrCertRegistryNil", fmt.Errorf("registry: %w", auth.ErrCertRegistryNil)},
	}
	for _, s := range sentinels {
		t.Run(s.name, func(t *testing.T) {
			if !autherr.IsCredentialRejection(s.err) {
				t.Fatalf("IsCredentialRejection(%v) = false, want true (a client credential rejection, not a server fault)", s.err)
			}
		})
	}
}

// TestIsCredentialRejection_InternalFaults is the other half: the predicate
// must NOT widen into "any error". A registry lookup that panicked, a
// connection that reset, an ssh public-key adapter failure - none are HTTP
// credential rejections, and answering 401 for them turns a server fault into
// a silent client re-auth loop.
func TestIsCredentialRejection_InternalFaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New("registry: connection reset")},
		{"wrapped plain error", fmt.Errorf("lookup identity: %w", errors.New("boom"))},
		{"ssh key adapter sentinel", auth.ErrKeyMalformed},
		{"ssh key unknown sentinel", auth.ErrKeyUnknown},
		{"io error", errors.New("read tcp: i/o timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if autherr.IsCredentialRejection(tc.err) {
				t.Fatalf("IsCredentialRejection(%v) = true, want false (only auth's typed rejection sentinels are 401)", tc.err)
			}
		})
	}
}
