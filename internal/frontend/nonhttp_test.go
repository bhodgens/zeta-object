package frontend

import (
	"net"
	"net/http"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// fakeBase is a minimal Frontend used by the compliance tests.
type fakeBase struct{ name string }

func (f *fakeBase) Name() string                      { return f.name }
func (f *fakeBase) Handler() http.Handler             { return http.NewServeMux() }
func (f *fakeBase) Authenticator() auth.Authenticator { return nil }
func (f *fakeBase) Capabilities() ProtocolCaps        { return ProtocolCaps{} }

// fakeNonHTTP embeds Frontend plus the three NonHTTPFrontend methods.
type fakeNonHTTP struct {
	fakeBase
}

func (f *fakeNonHTTP) NonHTTPAddr() string        { return "127.0.0.1:0" }
func (f *fakeNonHTTP) Serve(l net.Listener) error { return l.Close() }
func (f *fakeNonHTTP) Stop() error                { return nil }

// Compile-time asserts (leaf 01 test-first step 1): a type embedding
// Frontend + the three extra methods satisfies NonHTTPFrontend; a plain
// frontend does not.
var (
	_ NonHTTPFrontend = (*fakeNonHTTP)(nil)
	_ Frontend        = (*fakeBase)(nil)
)

func TestNonHTTPFrontendInterfaceCompliance(t *testing.T) {
	nh := &fakeNonHTTP{fakeBase{"nonhttp"}}
	var i any = nh
	if _, ok := i.(NonHTTPFrontend); !ok {
		t.Fatalf("*fakeNonHTTP must satisfy NonHTTPFrontend")
	}
	plain := &fakeBase{"plain"}
	if _, ok := any(plain).(NonHTTPFrontend); ok {
		t.Fatalf("a plain Frontend must NOT satisfy NonHTTPFrontend (optional interface)")
	}
	// The frozen Frontend interface is untouched: NonHTTPFrontend embeds it.
	var f Frontend = nh
	if f.Name() != "nonhttp" {
		t.Fatalf("Name() = %q, want nonhttp", f.Name())
	}
}

// Serve must be a total function: accepting a listener, returning on close.
func TestNonHTTPFrontendServeContract(t *testing.T) {
	nh := &fakeNonHTTP{fakeBase{"nonhttp"}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// fakeNonHTTP.Serve closes the listener and returns nil.
	if err := nh.Serve(l); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if err := nh.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
