// frontends_coverage_test.go — direct coverage for the frontend-registry
// helpers added by the frontend-interface tree (frontends.go): the option-list
// splitter, the per-bucket Backend delegation (backendResolver +
// perBucketBackend), and the FTP AUTH TLS cert-pair loader. No listeners are
// opened; the delegations target an injected lookup with a recording stub.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// recordingBackendStub records each data-plane call and returns sentinel
// values so the perBucketBackend delegation can be pinned without a real
// filesystem. It implements the full backend.Backend interface.
type recordingBackendStub struct {
	calls []string
}

func (r *recordingBackendStub) Get(_ context.Context, bucket, key string, _ objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	r.calls = append(r.calls, "Get:"+bucket+"/"+key)
	return io.NopCloser(strings.NewReader("data")), objectmodel.Object{Key: key}, nil
}

func (r *recordingBackendStub) Put(_ context.Context, bucket, key string, _ io.Reader, _ int64, _ objectmodel.PutOptions) (objectmodel.Object, error) {
	r.calls = append(r.calls, "Put:"+bucket+"/"+key)
	return objectmodel.Object{Key: key}, nil
}

func (r *recordingBackendStub) Delete(_ context.Context, bucket, key string) error {
	r.calls = append(r.calls, "Delete:"+bucket+"/"+key)
	return nil
}

func (r *recordingBackendStub) Stat(_ context.Context, bucket, key string) (objectmodel.Object, error) {
	r.calls = append(r.calls, "Stat:"+bucket+"/"+key)
	return objectmodel.Object{Key: key}, nil
}

func (r *recordingBackendStub) List(_ context.Context, bucket string, _ objectmodel.ListParams) (objectmodel.ListPage, error) {
	r.calls = append(r.calls, "List:"+bucket)
	return objectmodel.ListPage{}, nil
}

func (r *recordingBackendStub) Buckets(_ context.Context) ([]objectmodel.BucketInfo, error) {
	r.calls = append(r.calls, "Buckets")
	return []objectmodel.BucketInfo{{Name: "b"}}, nil
}

func (r *recordingBackendStub) Capabilities() objectmodel.CapabilitySet {
	r.calls = append(r.calls, "Capabilities")
	return objectmodel.CapabilitySet{}
}

var _ backend.Backend = (*recordingBackendStub)(nil)

// TestSplitOptionList table-tests the comma-separated option splitter used by
// the admin frontend (adminPrincipals): empty/whitespace yields nil, stray
// whitespace and trailing commas are trimmed away.
func TestSplitOptionList(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty string is nil", value: "", want: nil},
		{name: "whitespace only is nil", value: "   \t ", want: nil},
		{name: "single item", value: "alice", want: []string{"alice"}},
		{name: "two items trimmed", value: " alice , bob ", want: []string{"alice", "bob"}},
		{name: "trailing comma dropped", value: "alice,bob,", want: []string{"alice", "bob"}},
		{name: "empty middle segment dropped", value: "alice,,bob", want: []string{"alice", "bob"}},
		{name: "only commas is nil", value: ",,,", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitOptionList(tt.value)
			if !equalStrings(got, tt.want) {
				t.Fatalf("splitOptionList(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestBackendResolverReturnsDelegatingBackend pins that backendResolver builds
// a perBucketBackend (the delegation seat), without invoking the lookup.
func TestBackendResolverReturnsDelegatingBackend(t *testing.T) {
	got := backendResolver()
	if got == nil {
		t.Fatal("backendResolver returned nil")
	}
	if _, ok := got.(*perBucketBackend); !ok {
		t.Fatalf("backendResolver returned %T, want *perBucketBackend", got)
	}
}

// TestPerBucketBackendDelegatesEveryMethod pins that every data-plane method
// routes to the injected lookup's backend with the caller's bucket (and the
// empty bucket for the bucket-agnostic operations).
func TestPerBucketBackendDelegatesEveryMethod(t *testing.T) {
	stub := &recordingBackendStub{}
	var requested []string
	p := &perBucketBackend{lookup: func(bucket string) (backend.Backend, error) {
		requested = append(requested, bucket)
		return stub, nil
	}}
	ctx := context.Background()

	if rc, _, err := p.Get(ctx, "bk", "k", objectmodel.GetOptions{}); err != nil {
		t.Fatalf("Get: %v", err)
	} else {
		_ = rc.Close()
	}
	if _, err := p.Put(ctx, "bk", "k", strings.NewReader("x"), 1, objectmodel.PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.Delete(ctx, "bk", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Stat(ctx, "bk", "k"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if _, err := p.List(ctx, "bk", objectmodel.ListParams{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := p.Buckets(ctx); err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	_ = p.Capabilities()

	wantCalls := []string{"Get:bk/k", "Put:bk/k", "Delete:bk/k", "Stat:bk/k", "List:bk", "Buckets", "Capabilities"}
	if !equalStrings(stub.calls, wantCalls) {
		t.Fatalf("delegated calls = %v, want %v", stub.calls, wantCalls)
	}
	// Bucket-scoped ops pass the caller's bucket; bucket-agnostic ops pass "".
	wantBuckets := []string{"bk", "bk", "bk", "bk", "bk", "", ""}
	if !equalStrings(requested, wantBuckets) {
		t.Fatalf("lookup buckets = %v, want %v", requested, wantBuckets)
	}
}

// TestPerBucketBackendLookupError pins that a failing lookup aborts every
// method with that error (Capabilities degrades to the zero value).
func TestPerBucketBackendLookupError(t *testing.T) {
	sentinel := errors.New("no such backend")
	p := &perBucketBackend{lookup: func(string) (backend.Backend, error) { return nil, sentinel }}
	ctx := context.Background()

	if _, _, err := p.Get(ctx, "bk", "k", objectmodel.GetOptions{}); !errors.Is(err, sentinel) {
		t.Errorf("Get err = %v, want sentinel", err)
	}
	if _, err := p.Put(ctx, "bk", "k", strings.NewReader("x"), 1, objectmodel.PutOptions{}); !errors.Is(err, sentinel) {
		t.Errorf("Put err = %v, want sentinel", err)
	}
	if err := p.Delete(ctx, "bk", "k"); !errors.Is(err, sentinel) {
		t.Errorf("Delete err = %v, want sentinel", err)
	}
	if _, err := p.Stat(ctx, "bk", "k"); !errors.Is(err, sentinel) {
		t.Errorf("Stat err = %v, want sentinel", err)
	}
	if _, err := p.List(ctx, "bk", objectmodel.ListParams{}); !errors.Is(err, sentinel) {
		t.Errorf("List err = %v, want sentinel", err)
	}
	if _, err := p.Buckets(ctx); !errors.Is(err, sentinel) {
		t.Errorf("Buckets err = %v, want sentinel", err)
	}
	if caps := p.Capabilities(); !reflect.DeepEqual(caps, objectmodel.CapabilitySet{}) {
		t.Errorf("Capabilities on error = %+v, want the zero set", caps)
	}
}

// TestPerBucketBackendNotInstalled pins the guard: a nil lookup (or nil
// receiver) fails loudly instead of panicking.
func TestPerBucketBackendNotInstalled(t *testing.T) {
	var nilReceiver *perBucketBackend
	if _, err := nilReceiver.backendForBucket("bk"); err == nil {
		t.Error("nil receiver: backendForBucket returned nil error")
	}
	empty := &perBucketBackend{}
	if _, err := empty.backendForBucket("bk"); err == nil {
		t.Error("nil lookup: backendForBucket returned nil error")
	}
}

// TestLoadServerTLSCertPair drives the FTP AUTH TLS loader: a valid temp cert
// pair yields a TLS 1.2-min config carrying the certificate; a missing file is
// a wrapped error naming the AUTH TLS purpose.
func TestLoadServerTLSCertPair(t *testing.T) {
	t.Run("valid pair loads", func(t *testing.T) {
		certPath, keyPath := writeTestServerCertPair(t)
		prevCfg := *serverConfig()
		t.Cleanup(func() { setServerConfig(prevCfg) })
		setServerConfigField(func(c *ServerConfig) { c.CertFile = certPath })
		setServerConfigField(func(c *ServerConfig) { c.KeyFile = keyPath })

		cfg, err := loadServerTLSCertPair()
		if err != nil {
			t.Fatalf("loadServerTLSCertPair: %v", err)
		}
		if len(cfg.Certificates) != 1 {
			t.Fatalf("Certificates = %d, want 1", len(cfg.Certificates))
		}
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %d, want TLS 1.2", cfg.MinVersion)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		prevCfg := *serverConfig()
		t.Cleanup(func() { setServerConfig(prevCfg) })
		setServerConfigField(func(c *ServerConfig) { c.CertFile = filepath.Join(t.TempDir(), "absent.pem") })
		setServerConfigField(func(c *ServerConfig) { c.KeyFile = filepath.Join(t.TempDir(), "absent.key") })

		_, err := loadServerTLSCertPair()
		if err == nil {
			t.Fatal("loadServerTLSCertPair accepted a missing cert pair")
		}
		if !strings.Contains(err.Error(), "AUTH TLS") {
			t.Errorf("err = %q, want it to name the AUTH TLS purpose", err.Error())
		}
	})
}

// writeTestServerCertPair writes a freshly generated self-signed cert/key pair
// to temp files and returns their paths (no openssl dependency).
func writeTestServerCertPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}
