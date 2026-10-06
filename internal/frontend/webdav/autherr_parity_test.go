// autherr_parity_test.go — the webdav 401-vs-500 classification must be the
// SAME decision the owncloud OCS surface makes for the identical error.
//
// dispatch.go used to carry its own inline copy of internal/auth's typed
// rejection sentinels while the owncloud package classified through
// autherr.IsCredentialRejection. Two lists, same adapters, two wire
// protocols — and a sentinel added later lands in exactly one of them, so one
// surface 401s an auth rejection while the other answers 500 for the same
// error. 500 is what every retry and error-budget path reads as a server
// fault: a client holding a good credential but a rejected one gets retried
// and alerted instead of challenged.
//
// These pins assert the DELEGATION, not a re-listing of the sentinels: if
// webdav's predicate ever grows its own list again (or the shared one drops a
// sentinel), the tables below disagree and a test fails.
//
// The cross-frontend half (webdav's answer vs owncloud's OCS answer on one
// error) cannot live in this package — owncloud imports webdav, so importing
// it back would be a cycle. It is pinned in owncloud's own
// TestOCS_AllAuthSentinelsClassifyAsRejections, which drives the OCS dispatcher
// AND the webdav data plane it delegates to over the same sentinel table. The
// half only this package can own is that webdav's verdict IS the shared
// predicate's verdict, which is what makes that cross-frontend assertion
// sound rather than coincidental.
//
// Every sentinel internal/auth declares for an HTTP adapter is in the table.
// auth.ErrKeyMalformed / auth.ErrKeyUnknown are DELIBERATELY excluded: they
// are SFTP-protocol failures with no HTTP wire form, and autherr's
// documented rule keeps them out of the HTTP classification (a frontend
// rendering 401 for them would be wrong). They appear in the negative table
// so that exclusion is pinned rather than assumed.
package webdav

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/autherr"
)

// httpAuthSentinels is the full set of internal/auth rejection sentinels an
// HTTP frontend can be handed, plus the wrapped forms (errors.Is, not ==)
// and the internal faults that must stay 500.
var httpAuthSentinels = []struct {
	name      string
	err       error
	rejection bool
}{
	{name: "basic missing", err: auth.ErrBasicMissing, rejection: true},
	{name: "basic malformed", err: auth.ErrBasicMalformed, rejection: true},
	{name: "bad credentials", err: auth.ErrBadCredentials, rejection: true},
	{name: "basic unsupported", err: auth.ErrBasicUnsupported, rejection: true},
	{name: "cert missing", err: auth.ErrCertMissing, rejection: true},
	{name: "cert unknown CN", err: auth.ErrCertUnknownCN, rejection: true},
	{name: "cert registry nil", err: auth.ErrCertRegistryNil, rejection: true},
	{name: "wrapped basic missing", err: fmt.Errorf("parse header: %w", auth.ErrBasicMissing), rejection: true},
	{name: "wrapped cert unknown CN", err: fmt.Errorf("resolve cn %q: %w", "device-x", auth.ErrCertUnknownCN), rejection: true},
	// Non-sentinels: genuine wiring/lookup faults. The predicate must not
	// widen into "any error" — that would challenge a client for a server bug.
	{name: "bare internal error", err: errors.New("registry: connection reset"), rejection: false},
	{name: "wrapped internal error", err: fmt.Errorf("lookup identity: %w", errors.New("boom")), rejection: false},
	{name: "stub internal failure", err: errStubInternal, rejection: false},
}

// TestAuth_WebdavMatchesSharedPredicate pins that dispatch.go's private
// predicate IS the shared one: same verdict, per sentinel, as
// autherr.IsCredentialRejection. This is the anti-drift pin for the
// delegation — a re-inlined list that happens to agree today fails the moment
// one side gains a sentinel.
func TestAuth_WebdavMatchesSharedPredicate(t *testing.T) {
	for _, s := range httpAuthSentinels {
		t.Run(s.name, func(t *testing.T) {
			got := isCredentialRejection(s.err)
			if got != s.rejection {
				t.Fatalf("isCredentialRejection(%v) = %v, want %v", s.err, got, s.rejection)
			}
			if shared := autherr.IsCredentialRejection(s.err); shared != got {
				t.Fatalf("webdav (%v) and the shared autherr predicate (%v) disagree on %v — "+
					"dispatch.go must delegate, never keep its own sentinel list", got, shared, s.err)
			}
		})
	}
}

// TestAuth_WebdavRejectsSSHKeySentinels pins autherr's documented exclusion:
// the ssh public-key sentinels are SFTP-protocol failures with no HTTP wire
// form, so classifying them as credential rejections here would turn an SFTP
// misconfiguration into a 401 challenge. Reported rather than silently
// tolerated: this is the one place webdav's old inline list and autherr's
// allow-list are EXPECTED to differ in intent, and the difference is
// deliberate on both sides.
func TestAuth_WebdavRejectsSSHKeySentinels(t *testing.T) {
	for _, s := range []struct {
		name string
		err  error
	}{
		{name: "ssh key malformed", err: auth.ErrKeyMalformed},
		{name: "ssh key unknown", err: auth.ErrKeyUnknown},
	} {
		t.Run(s.name, func(t *testing.T) {
			if isCredentialRejection(s.err) {
				t.Fatalf("isCredentialRejection(%v) = true, want false (SFTP-protocol sentinels have no HTTP 401 form)", s.err)
			}
		})
	}
}

// TestAuth_WebdavWireAgreesWithTheSharedPredicate is the wire-level pin: for
// one table of authenticator errors, the verdict the webdav dispatch renders
// on the wire must be the verdict the shared predicate gives — and the two
// protocols' shared shape (a challenge carries no body detail, a 500 carries
// no challenge) must hold.
//
// The cross-frontend half of this pin — webdav and owncloud agreeing on the
// SAME error — cannot live in this package: owncloud imports webdav, so a
// webdav test importing owncloud would be an import cycle. It is pinned on the
// other side instead, by owncloud's own
// TestOCS_AllAuthSentinelsClassifyAsRejections, which drives both the OCS
// dispatcher AND the webdav data plane it delegates to and asserts the same
// table resolves identically. What this file owns is the half only this
// package can see: that webdav's wire answer is the shared predicate's answer,
// which is what makes the two-frontends assertion on the other side sound.
func TestAuth_WebdavWireAgreesWithTheSharedPredicate(t *testing.T) {
	for _, s := range httpAuthSentinels {
		t.Run(s.name, func(t *testing.T) {
			f, err := New(newStubBackend(), Config{}, WithAuthenticator(erroringAuthenticator{err: s.err}))
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, httptest.NewRequest("PROPFIND", "/photos", nil))

			shared := autherr.IsCredentialRejection(s.err)
			isChallenge := rec.Code == http.StatusUnauthorized &&
				rec.Header().Get("WWW-Authenticate") == `Basic realm="zeta-object"`
			if shared != isChallenge {
				t.Fatalf("%s: webdav wire answered %d (challenge=%v) but the shared predicate says %v — "+
					"the frontends that delegate to autherr would now disagree on the same error",
					s.name, rec.Code, isChallenge, shared)
			}
			if isChallenge != s.rejection {
				want := http.StatusInternalServerError
				if s.rejection {
					want = http.StatusUnauthorized
				}
				t.Fatalf("%s: webdav answered %d, want %d for this error class", s.name, rec.Code, want)
			}
			// The two renderings' shared shape.
			if isChallenge {
				if rec.Body.Len() != 0 {
					t.Fatalf("%s: 401 body = %q, want empty", s.name, rec.Body.String())
				}
			} else if rec.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("%s: a %d must not carry a challenge", s.name, rec.Code)
			}
		})
	}
}
