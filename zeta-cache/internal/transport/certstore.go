package transport

// certstore.go - per-device client-certificate storage (leaf 06, auth
// model v1). CertStorage is the seam; FileCertStore is the v1
// implementation: PEM files on disk, permission 0600 for the key.
//
// Tradeoff (documented per the leaf): an OS keychain/keyring
// (macOS Keychain, Linux secret-service) protects the private key with
// the user's session and enables touch-to-use prompts, but pulls in cgo
// or an external daemon dependency - cgo is BANNED repo-wide, so v1
// ships file-based storage. The interface below is what a future
// KeychainCertStore would implement; config keeps pointing at file
// paths (auth.clientCert/auth.clientKey) and the daemon's documented
// hardening step is an os-level file-permission story (0600 key,
// 0700 dir), not an in-process crypto change.

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// CertStorage loads the per-device mTLS certificate. Implementations
// must be safe for concurrent use; the returned tls.Certificate is
// immutable once loaded.
type CertStorage interface {
	// LoadClientCert returns the client certificate pair, or nil when
	// the storage holds none (TCP Basic-only setups).
	LoadClientCert() (*tls.Certificate, error)
	// TrustRoots returns the CA pool the server certificate is verified
	// against, or nil for the system pool.
	TrustRoots() (*x509.CertPool, error)
}

// FileCertStore reads PEM files from disk: the client pair from
// certFile/keyFile, the server-trust roots from caFile.
type FileCertStore struct {
	certFile string // per-device client cert PEM ("" = none)
	keyFile  string // client key PEM
	caFile   string // gateway CA bundle for server verification ("" = system)
}

// NewFileCertStore builds the file-backed store. Any path may be empty.
func NewFileCertStore(certFile, keyFile, caFile string) *FileCertStore {
	return &FileCertStore{certFile: certFile, keyFile: keyFile, caFile: caFile}
}

// ErrNoCert is the sentinel FileCertStore returns when no client pair is
// configured (a valid Basic-only setup, distinct from a LOAD failure).
var ErrNoCert = errors.New("transport: no client certificate configured")

// ErrNoTrustRoots is the sentinel for "use the system pool".
var ErrNoTrustRoots = errors.New("transport: no CA file configured")

// LoadClientCert implements CertStorage. A missing pair returns
// ErrNoCert (Basic-only is a valid configuration); a PAIR half-present
// is an error naming the problem (the config validator already enforces
// this, this is the belt).
func (s *FileCertStore) LoadClientCert() (*tls.Certificate, error) {
	if s.certFile == "" && s.keyFile == "" {
		return nil, ErrNoCert
	}
	if s.certFile == "" || s.keyFile == "" {
		return nil, fmt.Errorf("transport: client cert storage: cert and key must both be set")
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, fmt.Errorf("transport: loading client cert pair (%s, %s): %w", s.certFile, s.keyFile, err)
	}
	return &cert, nil
}

// TrustRoots implements CertStorage: the CA file's PEM certificates, or
// ErrNoTrustRoots when no caFile is configured (system pool).
func (s *FileCertStore) TrustRoots() (*x509.CertPool, error) {
	if s.caFile == "" {
		return nil, ErrNoTrustRoots
	}
	pem, err := os.ReadFile(s.caFile)
	if err != nil {
		return nil, fmt.Errorf("transport: reading caFile %s: %w", s.caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("transport: caFile %s contains no usable PEM certificates", s.caFile)
	}
	return pool, nil
}

// compile-time: FileCertStorage is the CertStorage seam.
var _ CertStorage = (*FileCertStore)(nil)
