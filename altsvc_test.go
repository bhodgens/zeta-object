package main

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// altsvc_test.go — the Alt-Svc advertisement middleware table (quic-h3-2026-10
// leaf 02 Task 3): header present on HTTP frontend responses when an h3
// frontend is mounted, absent when not, absent on the h3 frontend's own
// responses, and an existing Alt-Svc value preserved via append-with-comma
// (pinned merge rule).

// stubQUICFrontend is a minimal QUICListenerFrontend (no real QUIC).
type stubQUICFrontend struct {
	name string
	addr string
}

func (f *stubQUICFrontend) Name() string                      { return f.name }
func (f *stubQUICFrontend) Handler() http.Handler             { return http.NewServeMux() }
func (f *stubQUICFrontend) Authenticator() auth.Authenticator { return nil }
func (f *stubQUICFrontend) Capabilities() frontend.ProtocolCaps {
	return frontend.ProtocolCaps{}
}
func (f *stubQUICFrontend) Addr() string         { return f.addr }
func (f *stubQUICFrontend) IsQUICListener() bool { return true }
func (f *stubQUICFrontend) TLSConfig() (*tls.Config, error) {
	return nil, errors.New("not exercised by the middleware table")
}

var (
	_ frontend.Frontend             = (*stubQUICFrontend)(nil)
	_ frontend.QUICListenerFrontend = (*stubQUICFrontend)(nil)
)

// altSvcProbeHandler is a handler the middleware wraps; it optionally
// pre-sets an Alt-Svc value to exercise the merge rule.
func altSvcProbeHandler(existing string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if existing != "" {
			w.Header().Set("Alt-Svc", existing)
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestAltSvcHeaderValue(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		{name: "port-only form", addr: ":9443", want: `h3=":9443"; persist=1`},
		{name: "host:port form", addr: "0.0.0.0:9443", want: `h3=":9443"; persist=1`},
		{name: "loopback form", addr: "127.0.0.1:9443", want: `h3=":9443"; persist=1`},
		{name: "no port", addr: "0.0.0.0", wantErr: true},
		{name: "garbage port", addr: "0.0.0.0:notaport", wantErr: true},
		{name: "port zero", addr: ":0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := altSvcHeaderValue(&stubQUICFrontend{name: "h3", addr: tt.addr})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("altSvcHeaderValue(%q) = %q, want error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("altSvcHeaderValue(%q): %v", tt.addr, err)
			}
			if got != tt.want {
				t.Fatalf("altSvcHeaderValue(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestAltSvcMiddleware(t *testing.T) {
	const adv = `h3=":9443"; persist=1`
	tests := []struct {
		name       string
		existing   string
		wantHeader string
	}{
		{
			name:       "no prior value: header set",
			existing:   "",
			wantHeader: adv,
		},
		{
			name:       "existing value preserved via append-with-comma (pinned)",
			existing:   `h3-29=":8443"; ma=3600`,
			wantHeader: `h3-29=":8443"; ma=3600, ` + adv,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := wrapWithAltSvc(altSvcProbeHandler(tt.existing), adv)
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			got := rec.Header().Get("Alt-Svc")
			if got != tt.wantHeader {
				t.Fatalf("Alt-Svc = %q, want %q", got, tt.wantHeader)
			}
		})
	}
}

// TestAltSvc_ResponseControllerCapabilitiesSurviveWrapper pins that the
// advertisement wrapper is TRANSPARENT to http.ResponseController. The
// wrapper embeds the http.ResponseWriter INTERFACE, so nothing promotes
// through it: without an explicit Unwrap() the controller cannot walk down
// to the real *http.response and every capability (Flush, SetWriteDeadline,
// Hijack) degrades to ErrNotSupported for EVERY wrapped frontend whenever an
// h3 frontend is mounted. The concrete victim is
// internal/frontend/s3/rangemulti.go, whose `_ = rc.Flush()` on a
// multi-range 206 silently stops flushing.
//
// Asserted against a REAL httptest server, not a recorder, because the point
// is that the walk reaches the live connection's *http.response.
func TestAltSvc_ResponseControllerCapabilitiesSurviveWrapper(t *testing.T) {
	const adv = `h3=":9443"; persist=1`
	// The handler asks the controller for the capabilities a ResponseWriter
	// hides behind the wrapper. The results travel over a side channel, not
	// headers: rc.Flush() COMMITS the header block, so anything set after
	// the call is (correctly) dropped by net/http.
	var mu sync.Mutex
	var flushErr, deadlineErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		fErr := rc.Flush()
		dErr := rc.SetWriteDeadline(time.Time{})
		mu.Lock()
		flushErr, deadlineErr = fErr, dErr
		mu.Unlock()
		_, _ = w.Write([]byte("body after flush"))
	})
	srv := httptest.NewServer(wrapWithAltSvc(inner, adv))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/multi-range")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := resp.Header.Get("Alt-Svc"); got != adv {
		t.Fatalf("Alt-Svc = %q, want %q (the wrapper must still advertise)", got, adv)
	}
	if string(body) != "body after flush" {
		t.Fatalf("body = %q, want %q", body, "body after flush")
	}
	mu.Lock()
	defer mu.Unlock()
	// The controller must have reached the underlying response writer: a
	// real connection's *http.response supports both.
	if flushErr != nil {
		t.Fatalf("ResponseController.Flush through the Alt-Svc wrapper = %v, want nil: the wrapper must expose Unwrap()", flushErr)
	}
	if deadlineErr != nil {
		t.Fatalf("ResponseController.SetWriteDeadline through the Alt-Svc wrapper = %v, want nil", deadlineErr)
	}
}

// TestAltSvc_WrapperUnwrapsToUnderlyingWriter pins the wrapper's own
// unwrap contract directly: Unwrap must return the writer it was handed, so
// the ResponseController's rwUnwrapper walk terminates on a writer that
// actually implements the capabilities.
func TestAltSvc_WrapperUnwrapsToUnderlyingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &altSvcWriter{ResponseWriter: rec, value: `h3=":9443"; persist=1`}
	u, ok := any(w).(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		t.Fatal("altSvcWriter must implement Unwrap() http.ResponseWriter (ResponseController capabilities are otherwise lost)")
	}
	if got := u.Unwrap(); got != http.ResponseWriter(rec) {
		t.Fatalf("Unwrap = %#v, want the wrapped ResponseWriter", got)
	}
}

// TestAltSvc_AbsentWithoutQUICFrontend pins findQUICFrontend's nil result
// (no advertisement when no h3 frontend is mounted).
func TestAltSvc_AbsentWithoutQUICFrontend(t *testing.T) {
	if q := findQUICFrontend([]frontendMount{{frontend: &stubFrontend{name: "s3"}}}); q != nil {
		t.Fatalf("findQUICFrontend = %v, want nil", q)
	}
	if q := findQUICFrontend(nil); q != nil {
		t.Fatalf("findQUICFrontend(nil) = %v, want nil", q)
	}
	// And the middleware wiring is a no-op: no handler rewritten.
	mux := http.NewServeMux()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get("Alt-Svc") != "" {
			t.Error("Alt-Svc set without a QUIC frontend mounted")
		}
	})}
	applyAltSvcAdvertisement(mux, nil, []frontendMount{{frontend: &stubFrontend{name: "s3"}}}, []*http.Server{srv})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Alt-Svc") != "" {
		t.Fatalf("Alt-Svc = %q, want empty", rec.Header().Get("Alt-Svc"))
	}
}

// TestAltSvc_WiringThroughApply pins the main-side wiring: with an h3 mount,
// the dedicated-listener server's handler carries the header.
func TestAltSvc_WiringThroughApply(t *testing.T) {
	q := &stubQUICFrontend{name: "h3", addr: "0.0.0.0:9443"}
	mux := http.NewServeMux()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	mux = applyAltSvcAdvertisement(mux, nil, []frontendMount{{frontend: q}}, []*http.Server{srv})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Alt-Svc"); got != `h3=":9443"; persist=1` {
		t.Fatalf("Alt-Svc = %q, want h3=\":9443\"; persist=1", got)
	}
	// Shared-mux frontends get the wrapped handler re-registered.
	shared := []frontend.Frontend{&stubFrontend{name: "webdav"}}
	mux2 := http.NewServeMux()
	srv2 := &http.Server{}
	mux2 = applyAltSvcAdvertisement(mux2, shared, []frontendMount{{frontend: q}}, []*http.Server{srv2})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, req)
	if got := rec2.Header().Get("Alt-Svc"); !strings.Contains(got, `h3=":9443"`) {
		t.Fatalf("shared-mux Alt-Svc = %q, want h3=\":9443\" present", got)
	}
}
