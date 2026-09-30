package ftp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// fakeBackend records Backend calls (test-first driver assertions).
type fakeBackend struct {
	backend.Backend // embeds interface; unimplemented methods panic loudly

	buckets []objectmodel.BucketInfo
	puts    []putCall
	gets    []getCall
	deletes []string
	stats   []string
	lists   []objectmodel.ListParams
	putErr  error
}

type putCall struct {
	bucket, key string
	data        []byte
	size        int64
}

type getCall struct {
	bucket, key string
}

func (f *fakeBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (interface {
	Read([]byte) (int, error)
	Close() error
}, objectmodel.Object, error) {
	return nil, objectmodel.Object{}, errNotImpl
}

func (f *fakeBackend) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	return f.buckets, nil
}

var errNotImpl = &fakeTestError{"fakeBackend: method not configured"}

// fakeTestError is the test-fake sentinel error type.
type fakeTestError struct{ s string }

func (e *fakeTestError) Error() string { return e.s }

// staticVerifier is the PasswordVerifier fake.
type staticVerifier struct {
	idents map[string]auth.Identity
	passes map[string]string
}

func (v *staticVerifier) Verify(user, password string) (auth.Identity, bool) {
	if v.passes[user] != password {
		return auth.Identity{}, false
	}
	id, ok := v.idents[user]
	return id, ok
}

// testTLS generates a throwaway self-signed cert for FTPS tests.
func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}
