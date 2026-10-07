package transport

// altsvc.go - alt-Svc discovery and the sticky-fallback state machine
// (leaf 06). The gateway stamps `alt-svc: h3="<port>"; persist=1` on every
// HTTP response (repo-root altsvc.go); this side parses the header and
// decides, per request, whether the next request rides TCP or h3.
//
// State machine (one mutex-protected struct; no timers running, just
// deadline comparisons against time.Now):
//
//	tcpOnly --(alt-svc seen AND client cert configured)--> eligible
//	eligible --(h3 dial+handshake+response OK)----------> active
//	active   --(h3 dial/handshake failure, req unsent)--> fallback(until)
//	fallback --(deadline passes)------------------------> eligible
//	eligible --(h3 fails again)-------------------------> fallback(until)
//
// TCP responses never move the state backwards: once active, a working
// h3 path stays sticky. Errors AFTER a request was sent (HTTP status,
// stream reset, body error) NEVER touch the state and are never retried
// on the other transport - the no-double-apply rule (see client.go).

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fallbackWindow is how long a failed h3 endpoint stays blacklisted
// before the next request retries the upgrade. UDP is often blocked by
// captive networks for minutes; hammering the handshake on every request
// would add a UDP round-trip timeout to each one.
const fallbackWindow = 5 * time.Minute

// h3State enumerates the sticky-fallback states.
type h3State int

const (
	h3TCPOnly  h3State = iota // no advertisement (yet) or no client cert
	h3Eligible                // advertisement + cert: next request may try h3
	h3Active                  // h3 works: sticky, all requests ride it
	h3Fallback                // h3 failed: TCP until the fallback deadline
)

// altsvcState is the alt-Svc / h3 stickiness for one Client. Transport
// is stateless except this and the auth material (leaf-06 constraint).
type altsvcState struct {
	mu sync.Mutex

	state         h3State
	port          string           // h3 UDP port from the alt-svc header ("" = none)
	fallbackUntil time.Time        // valid in h3Fallback
	now           func() time.Time // injectable clock (tests)
}

// newAltsvcState builds the state with the wall clock.
func newAltsvcState() *altsvcState {
	return &altsvcState{now: time.Now}
}

// observe parses the alt-svc header of a TCP response. When it advertises
// h3 and the client carries a client certificate, the state becomes
// eligible (the upgrade itself happens lazily on a later request).
// A false return means "no usable advertisement"; the state is unchanged.
func (a *altsvcState) observe(header string, haveCert bool) bool {
	port, ok := parseAltSvcH3(header)
	if !ok || !haveCert {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.port = port
	if a.state == h3TCPOnly || a.state == h3Eligible {
		a.state = h3Eligible
	}
	return true
}

// useH3 decides the transport for the NEXT request. It reports the UDP
// port to dial and whether the caller may blacklists h3 on a dial-class
// failure (true only on an upgrade attempt from eligible - the only
// state where the failure is provably pre-send).
func (a *altsvcState) useH3() (port string, upgradable bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.state {
	case h3Eligible:
		return a.port, true
	case h3Active:
		return a.port, false
	case h3Fallback:
		if a.now().After(a.fallbackUntil) {
			a.state = h3Eligible // deadline passed: try the upgrade again
			return a.port, true
		}
		return "", false
	default: // h3TCPOnly
		return "", false
	}
}

// h3Up marks a successful h3 request: the state goes (and stays) active.
func (a *altsvcState) h3Up() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = h3Active
}

// h3Down records a dial/handshake-class h3 failure: TCP for the fallback
// window, then one retry (back to eligible). upgradable reports whether
// THIS failure was an upgrade attempt (request provably not sent).
func (a *altsvcState) h3Down(now time.Time) (upgradable bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	upgradable = a.state == h3Eligible
	a.state = h3Fallback
	a.fallbackUntil = now.Add(fallbackWindow)
	return upgradable
}

// current reports the state and port (tests + logging).
func (a *altsvcState) current() (h3State, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, a.port
}

// parseAltSvcH3 extracts the h3 UDP port from an alt-svc header value:
// the gateway's pinned form is `h3=":9443"; persist=1` (repo-root
// altsvc.go), RFC 7838 also allows `h3="host:port"` and comma-separated
// alternatives. Returns the port string (no colon) and false when the
// header advertises no usable h3 endpoint.
func parseAltSvcH3(header string) (string, bool) {
	for alt := range strings.SplitSeq(header, ",") {
		alt = strings.TrimSpace(alt)
		// The value may carry parameters ("; persist=1") after the
		// quoted authority: take the FIRST "=" split of the h3 token's
		// leading segment.
		if !strings.HasPrefix(alt, "h3=") && !strings.HasPrefix(alt, `h3="`) {
			continue
		}
		val := strings.TrimPrefix(alt, "h3=")
		// Parameters ride after the quoted authority or a bare port.
		if i := strings.IndexByte(val, ';'); i >= 0 {
			val = val[:i]
		}
		authority := strings.Trim(strings.TrimSpace(val), `"`)
		_, port, err := net.SplitHostPort(authority)
		if err != nil {
			// Bare "port" form: SplitHostPort rejects a missing colon.
			if !strings.Contains(authority, ":") {
				if p, aerr := strconv.Atoi(authority); aerr == nil && p > 0 && p <= 65535 {
					return authority, true
				}
			}
			continue
		}
		if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
			continue
		}
		return port, true
	}
	return "", false
}
