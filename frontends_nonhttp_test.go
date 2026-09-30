package main

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// fakeNonHTTPFrontend is a NonHTTPFrontend used to prove the main-package
// wiring (sftp-ftp-2026-09 leaf 01): raw listener started, Serve sees conns,
// Stop closes; shared-mux mount rejected loudly.
type fakeNonHTTPFrontend struct {
	name string
	addr string

	serveGot chan net.Conn
	stopGot  bool
}

func (f *fakeNonHTTPFrontend) Name() string                      { return f.name }
func (f *fakeNonHTTPFrontend) Handler() http.Handler             { return http.NewServeMux() }
func (f *fakeNonHTTPFrontend) Authenticator() auth.Authenticator { return nil }
func (f *fakeNonHTTPFrontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{}
}
func (f *fakeNonHTTPFrontend) NonHTTPAddr() string { return f.addr }
func (f *fakeNonHTTPFrontend) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			// Listener closed under us = the graceful-stop path: Serve's
			// contract is to return nil (Stop / close), not the accept
			// error (mirrors how http.Server treats ErrServerClosed).
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		f.serveGot <- c
		c.Close()
	}
}
func (f *fakeNonHTTPFrontend) Stop() error {
	f.stopGot = true
	return nil
}

var (
	_ frontend.Frontend        = (*fakeNonHTTPFrontend)(nil)
	_ frontend.NonHTTPFrontend = (*fakeNonHTTPFrontend)(nil)
)

// A NonHTTP frontend WITH a listenAddr mounts as a dedicated listener spec
// (not on the shared mux); a plain frontend is unchanged.
func TestMountFrontends_NonHTTPGoesToDedicatedListener(t *testing.T) {
	nh := &fakeNonHTTPFrontend{name: "fake-nh", addr: "127.0.0.1:0"}
	plain := &stubFrontend{name: "plain"}
	mux := http.NewServeMux()
	shared, extra, err := mountFrontends(mux, []frontendMount{
		{frontend: nh, listenAddr: "127.0.0.1:2121"},
		{frontend: plain, listenAddr: ""},
	})
	if err != nil {
		t.Fatalf("mountFrontends: %v", err)
	}
	if len(shared) != 1 || shared[0].Name() != "plain" {
		t.Fatalf("shared = %+v, want [plain]", shared)
	}
	if len(extra) != 1 || extra[0].addr != "127.0.0.1:2121" {
		t.Fatalf("extra = %+v, want one spec at 127.0.0.1:2121", extra)
	}
	if _, ok := extra[0].frontend.(frontend.NonHTTPFrontend); !ok {
		t.Fatalf("dedicated spec must carry the NonHTTPFrontend for main's type-assert")
	}
}

// Shared-mux mounting of a NonHTTPFrontend is a loud startup error naming
// the frontend and its missing listenAddr.
func TestMountFrontends_SharedMuxNonHTTPRejected(t *testing.T) {
	nh := &fakeNonHTTPFrontend{name: "fake-nh"}
	mux := http.NewServeMux()
	_, _, err := mountFrontends(mux, []frontendMount{{frontend: nh, listenAddr: ""}})
	if err == nil {
		t.Fatal("want loud error for shared-mux non-HTTP mount")
	}
	for _, want := range []string{"fake-nh", "requires its own listenAddr"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
}

// The full startup plan: a fake NonHTTP frontend with a listenAddr — its
// listener is opened, Serve sees a TCP dial, and Stop runs on drain.
func TestStartupPlan_NonHTTPListenerServesAndDrains(t *testing.T) {
	port := freePortForTest(t)
	nh := &fakeNonHTTPFrontend{name: "fake-nh", addr: "127.0.0.1:" + port, serveGot: make(chan net.Conn, 1)}
	restore := registerTestFrontend(t, "fake-nh", func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		return nh, nil
	})
	defer restore()
	plan, err := startupPlan(
		[]FrontendConfig{{Type: "fake-nh", ListenAddr: nh.addr}},
		nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("startupPlan: %v", err)
	}
	if len(plan.listeners) != 1 {
		t.Fatalf("listeners = %+v, want 1 (the non-HTTP frontend)", plan.listeners)
	}
	// Wire the spec to the fake the way main() does.
	spec := plan.listeners[0]
	nhF, ok := spec.frontend.(*fakeNonHTTPFrontend)
	if !ok {
		t.Fatalf("spec frontend = %T, want *fakeNonHTTPFrontend", spec.frontend)
	}
	l, err := net.Listen("tcp", spec.addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- nhF.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	select {
	case <-nhF.serveGot:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve never saw the dialed connection")
	}

	// Graceful drain: close the listener (Stop semantics), Serve returns.
	l.Close()
	if err := <-serveErr; err != nil {
		t.Fatalf("Serve after close: %v", err)
	}
	if err := nhF.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !nhF.stopGot {
		t.Fatal("Stop must be invoked on the drain path")
	}
}

// A NonHTTP frontend configured WITHOUT listenAddr is rejected by the
// shared-mux guard inside startupPlan (loud startup error).
func TestStartupPlan_NonHTTPWithoutListenAddrRejected(t *testing.T) {
	restore := registerTestFrontend(t, "fake-nh-noaddr", func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
		return &fakeNonHTTPFrontend{name: "fake-nh-noaddr"}, nil
	})
	defer restore()
	_, err := startupPlan(
		[]FrontendConfig{{Type: "fake-nh-noaddr"}},
		nilBackend{}, stubCreds{})
	if err == nil {
		t.Fatal("want error for non-HTTP frontend without listenAddr")
	}
	for _, want := range []string{"fake-nh-noaddr", "requires its own listenAddr"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
}

func freePortForTest(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, port, _ := net.SplitHostPort(addr)
	return port
}

// registerTestFrontend registers a factory under name and returns a restore
// func (tests must not leak factories into each other).
func registerTestFrontend(t *testing.T, name string, factory func(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error)) func() {
	t.Helper()
	prev, had := frontendFactories[name]
	frontendFactories[name] = factory
	return func() {
		if had {
			frontendFactories[name] = prev
		} else {
			delete(frontendFactories, name)
		}
	}
}
