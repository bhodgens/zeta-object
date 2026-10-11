// listen.go — the sts frontend's construction-time listener validation and
// TLS material loading (copied discipline from the admin frontend:
// loopback-only by default, explicit non-loopback opt-out, fail-loud CA
// bundle).
package sts

import (
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
)

// parseSeconds parses a bare positive decimal integer (seconds).
func parseSeconds(raw string) (int64, error) {
	var sec int64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &sec); err != nil {
		return 0, fmt.Errorf("not an integer: %w", err)
	}
	return sec, nil
}

// validateListenAddr enforces the loopback-only default at CONSTRUCTION
// time (never at request time): the host embedded in addr must resolve
// entirely to loopback addresses unless allowNonLoopback is set. An empty
// host (":8444") and 0.0.0.0 bind every interface and are rejected; an
// unresolvable host is rejected. The error names the address.
func validateListenAddr(addr string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("sts: invalid listenAddr %q: %w", addr, err)
	}
	if allowNonLoopback {
		return nil
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("sts: listenAddr %q is not loopback (it binds every interface); set allowNonLoopback to true to permit a non-loopback bind", addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("sts: cannot resolve listenAddr host %q in %q: %w", host, addr, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("sts: listenAddr %q resolved to no addresses", addr)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("sts: listenAddr %q is not loopback (resolved %s); set allowNonLoopback to true to permit a non-loopback bind", addr, ip)
		}
	}
	return nil
}

// loadClientCAs reads a PEM bundle and returns the trusted-client-CA
// pool. The file is REQUIRED when mTLS mode is on: a missing or unreadable
// file, or a bundle containing no usable certificate, is a loud error
// naming the path.
func loadClientCAs(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sts: reading clientCAFile %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("sts: clientCAFile %q contains no usable PEM certificates", path)
	}
	return pool, nil
}
