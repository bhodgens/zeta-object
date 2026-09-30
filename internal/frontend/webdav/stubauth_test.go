// stubauth_test.go — the permissive stub authenticator for non-auth tests.
package webdav

import (
	"net/http"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// stubAuthenticator permits everything with a wildcard-readwrite identity
// (auth behavior is leaf 04's matrix in auth_test.go with the REAL
// BasicAuthenticator).
type stubAuthenticator struct {
	identity auth.Identity
	fail     bool // internal-failure path (⇒ 500)
}

func newStubAuth() *stubAuthenticator {
	return &stubAuthenticator{identity: auth.WildcardIdentity("stub")}
}

func (s *stubAuthenticator) Authenticate(_ *http.Request) (auth.Identity, error) {
	if s.fail {
		return auth.Identity{}, errStubInternal
	}
	return s.identity, nil
}

// errStubInternal simulates an authenticator-internal failure (a non-
// credential error — must map to 500, not 401).
var errStubInternal = &stubInternalError{}

type stubInternalError struct{}

func (*stubInternalError) Error() string { return "stub: internal failure" }
