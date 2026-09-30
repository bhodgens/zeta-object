// devmode.go — auth.mode "none" zero-auth dev mode (pluggable-authentication
// tree leaf 05). Opt-in ONLY via config; never the default. Loudness is
// contractual: the constructor emits a multi-line startup banner and every
// Authenticate call emits one request-scoped WARNING line. There is no
// silent mode. Dev mode never masks config validation errors — package main
// validates identities BEFORE consulting auth.mode.
package auth

import (
	"log"
	"net/http"
)

// devAccessKeyID is the fixed anonymous principal name dev mode
// authenticates every request as.
const devAccessKeyID = "anonymous"

// DevAuthenticator authenticates every request as the fixed anonymous
// identity with wildcard readwrite grants. It never fails. The registry
// bypass lives at THIS level (an Authenticator, not a dispatch special-
// case) so every frontend that asks the seam authenticates the same way —
// the protocol-neutrality story stays intact.
type DevAuthenticator struct {
	logger *log.Logger
}

// compile-time: the adapter satisfies the frozen v1 Authenticator seam.
var _ Authenticator = (*DevAuthenticator)(nil)

// NewDevAuthenticator builds the dev-mode authenticator. nil logger falls
// back to log.Default(); tests inject a bytes.Buffer logger.
func NewDevAuthenticator(l *log.Logger) *DevAuthenticator {
	if l == nil {
		l = log.Default()
	}
	return &DevAuthenticator{logger: l}
}

// Banner prints the startup banner: multi-line, impossible to miss, single
// fact per line (leaf 06's e2e case greps "AUTHENTICATION DISABLED").
func (d *DevAuthenticator) Banner() {
	const line = "======================================================================="
	d.logger.Printf("%s", line)
	d.logger.Printf("WARNING: AUTHENTICATION DISABLED — auth.mode is \"none\" (zero-auth dev mode).")
	d.logger.Printf("WARNING: Every request is accepted as the anonymous principal with FULL read-write access to every bucket.")
	d.logger.Printf("WARNING: For local development only. NEVER run this mode on a network-exposed interface.")
	d.logger.Printf("%s", line)
}

// Authenticate returns the anonymous wildcard identity for every request,
// logging exactly one WARNING line per call.
func (d *DevAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	d.logger.Printf("WARNING: dev mode (auth.mode=none): accepting request %s %s as %q without authentication",
		r.Method, r.URL.Path, devAccessKeyID)
	return WildcardIdentity(devAccessKeyID), nil
}
