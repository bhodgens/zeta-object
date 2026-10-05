package admin

import (
	"fmt"
	"net"
	"strings"
)

// validateListenAddr enforces the loopback-only default at CONSTRUCTION time
// (never at request time): the host embedded in addr must resolve entirely to
// loopback addresses (127.0.0.1, ::1, localhost, ...) unless allowNonLoopback
// is set. Port 0 is accepted so tests and ephemeral operators can bind freely.
//
// An empty host (":9000") and 0.0.0.0 bind every interface and are rejected;
// an unresolvable host is rejected. The error names the address.
func validateListenAddr(addr string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin: invalid listenAddr %q: %w", addr, err)
	}
	if allowNonLoopback {
		return nil
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("admin: listenAddr %q is not loopback (it binds every interface); set allowNonLoopback to true to permit a non-loopback bind", addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("admin: cannot resolve listenAddr host %q in %q: %w", host, addr, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("admin: listenAddr %q resolved to no addresses", addr)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("admin: listenAddr %q is not loopback (resolved %s); set allowNonLoopback to true to permit a non-loopback bind", addr, ip)
		}
	}
	return nil
}
