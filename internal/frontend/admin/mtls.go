package admin

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
)

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

// TLSConfig builds the dedicated listener's TLS configuration: TLS 1.2
// minimum, client certificates REQUIRED and verified against the CA bundle,
// and the process server-certificate pair as this listener's own certificate.
func (f *adminFrontend) TLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.opts.CertFile, f.opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("admin: loading server certificate pair (%s, %s): %w", f.opts.CertFile, f.opts.KeyFile, err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    f.caPool,
		Certificates: []tls.Certificate{cert},
	}, nil
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
// lets the handler reason about the principal and the allow-list.
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
		Roots:         f.caPool,
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
