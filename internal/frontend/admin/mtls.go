package admin

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// ClientCAReloader is implemented by the admin frontend: package main holds
// one and re-reads the trust bundle from disk on POST /auth/reload. It is a
// separate interface so the reload path stays off the frozen frontend seams.
type ClientCAReloader interface {
	// ReloadClientCA re-reads ClientCAFile and swaps the trusted pool. A
	// missing/unreadable/invalid bundle returns an error and leaves the
	// previous pool in place (fail-closed: the running listener keeps
	// trusting the old CA rather than trusting nothing or everything).
	ReloadClientCA() error
}

// loadClientCAs reads a PEM bundle and returns the trusted-client-CA pool.
// The file is REQUIRED: a missing or unreadable file, or a bundle containing
// no usable certificate, is a loud error naming the path.
func loadClientCAs(path string) (*x509.CertPool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("admin: clientCAFile is required (PEM bundle of trusted client CAs)")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("admin: reading clientCAFile %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("admin: clientCAFile %q contains no usable PEM certificates", path)
	}
	return pool, nil
}

// currentPool returns the currently-trusted client CA pool. It is an atomic
// load so the handshake path observes a reload without a lock.
func (f *adminFrontend) currentPool() *x509.CertPool {
	return f.caPool.Load()
}

// ReloadClientCA re-reads the configured clientCAFile and swaps the trusted
// pool (management-api-2026-10 leaf 04, Task 4). A replaced CA file therefore
// revokes client certificates signed by the OLD CA without a restart. It is
// the implementation behind ClientCAReloader.
func (f *adminFrontend) ReloadClientCA() error {
	pool, err := loadClientCAs(f.opts.ClientCAFile)
	if err != nil {
		return err
	}
	f.caPool.Store(pool)
	return nil
}

// TLSConfig builds the dedicated listener's TLS configuration: TLS 1.2
// minimum, client certificates REQUIRED and verified against the CA bundle,
// and the process server-certificate pair as this listener's own certificate.
//
// A tls.Config must not be mutated after first use, so a refreshed CA pool is
// surfaced through GetConfigForClient rather than by writing ClientCAs on the
// live config: the returned base config is built ONCE with the initial pool,
// and every handshake asks for a fresh per-connection config pinned to the
// CURRENT cached pool (f.currentPool). Replacing the CA file and calling
// ReloadClientCA therefore takes effect on the next handshake with no data
// race on the served config.
func (f *adminFrontend) TLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.opts.CertFile, f.opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("admin: loading server certificate pair (%s, %s): %w", f.opts.CertFile, f.opts.KeyFile, err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    f.currentPool(),
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return f.connectionTLSConfig(cert), nil
		},
	}, nil
}

// connectionTLSConfig builds the per-connection configuration the handshake
// uses, pinned to the CURRENT cached CA pool. It carries the same certificate
// and client-auth posture as the base config.
func (f *adminFrontend) connectionTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    f.currentPool(),
		Certificates: []tls.Certificate{cert},
	}
}

// authenticate extracts and verifies the request's client certificate. On
// success it returns the certificate Subject Common Name and a zero status;
// otherwise it returns an HTTP status and a generic code that leaks NOTHING
// about the failure (a missing certificate, a wrong issuer and an expired
// certificate are all indistinguishable 401s). A verified principal outside a
// configured allow-list is a 403.
//
// TLSConfig already sets RequireAndVerifyClientCert, so the handshake rejects
// bad certificates; this check is the in-process defence in depth that also
// lets the handler reason about the principal and the allow-list. It verifies
// against the CURRENT cached pool, so it follows a CA reload.
func (f *adminFrontend) authenticate(r *http.Request) (principal string, status int, code string) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", http.StatusUnauthorized, "Unauthorized"
	}
	leaf := r.TLS.PeerCertificates[0]
	intermediates := x509.NewCertPool()
	for _, ic := range r.TLS.PeerCertificates[1:] {
		intermediates.AddCert(ic)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         f.currentPool(),
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return "", http.StatusUnauthorized, "Unauthorized"
	}
	cn := leaf.Subject.CommonName
	if cn == "" {
		// Fail closed: a certificate with no Subject CN has no principal.
		return "", http.StatusUnauthorized, "Unauthorized"
	}
	if len(f.principals) > 0 && !f.principals[cn] {
		return "", http.StatusForbidden, "Forbidden"
	}
	return cn, 0, ""
}
