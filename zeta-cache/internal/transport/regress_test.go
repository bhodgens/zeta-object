package transport

// regress_test.go - H2 / L46 / L47 regression tests on the REAL http
// test server (httptest TLS, the client_test.go pattern), not memfs.
//
//   - H2: Put with an EMPTY etag sends If-None-Match:* on the wire and
//     an existing object yields the 412 -> ErrConflict create-race path.
//   - L46: do() sets req.GetBody from the ReadSeeker, so an h3->TCP
//     fallback can rebuild a consumed body; classifyH3Err buckets by
//     TYPE, never by error-string sniffing.
//   - L47: TCP 401/403 map onto ErrAuthFailed (errors.Is), the same
//     sentinel the h3 mTLS handshake failure wraps.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/quic-go/quic-go"
)

// TestPutEmptyETagSendsIfNoneMatchStar (H2, wire pin): the REAL client
// sends If-None-Match:* when Put is called with an empty etag.
func TestPutEmptyETagSendsIfNoneMatchStar(t *testing.T) {
	h := &stubHandler{status: http.StatusCreated, headers: map[string]string{
		"ETag": `"deadbeefdeadbeefdeadbeefdeadbeef"`,
	}}
	c := newTestClient(t, h)
	body := bytes.Repeat([]byte("x"), 50)
	if _, err := c.Put(context.Background(), "new.bin", bytes.NewReader(body), ""); err != nil {
		t.Fatal(err)
	}
	req := h.last(t)
	if req.method != http.MethodPut || req.path != "/bkt/new.bin" {
		t.Fatalf("wire request = %s %s, want PUT /bkt/new.bin", req.method, req.path)
	}
	if got := req.header.Get("If-None-Match"); got != "*" {
		t.Fatalf("If-None-Match = %q, want * (empty etag = create-only, H2)", got)
	}
	if got := req.header.Get("If-Match"); got != "" {
		t.Fatalf("If-Match = %q, want empty", got)
	}
	if cl := req.header.Get("Content-Length"); cl != "50" {
		t.Fatalf("Content-Length = %q, want 50", cl)
	}
}

// TestPutEmptyETagExistingObject412Conflict (H2, the create-race path):
// an existing object answers 412 to If-None-Match:*, and the REAL client
// maps it to ErrConflict - the exact signal diff.go's matrix-8 upload
// relies on to leave the row dirty instead of clobbering.
func TestPutEmptyETagExistingObject412Conflict(t *testing.T) {
	h := &stubHandler{status: http.StatusPreconditionFailed, body: "exists"}
	c := newTestClient(t, h)
	_, err := c.Put(context.Background(), "raced.bin", strings.NewReader("local"), "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict (412 on If-None-Match:*)", err)
	}
	if got := h.last(t).header.Get("If-None-Match"); got != "*" {
		t.Fatalf("the racing PUT sent If-None-Match = %q, want *", got)
	}
}

// TestPutNonEmptyETagSendsIfMatch (H2 contract, the other half): a
// non-empty etag still sends If-Match - the FUSE overwrite path.
func TestPutNonEmptyETagSendsIfMatch(t *testing.T) {
	h := &stubHandler{status: http.StatusNoContent}
	c := newTestClient(t, h)
	if _, err := c.Put(context.Background(), "f", strings.NewReader("b"), "cafebabecafebabecafebabecafebabe"); err != nil {
		t.Fatal(err)
	}
	req := h.last(t)
	if got := req.header.Get("If-Match"); got != `"cafebabecafebabecafebabecafebabe"` {
		t.Fatalf("If-Match = %q, want the quoted etag", got)
	}
	if got := req.header.Get("If-None-Match"); got != "" {
		t.Fatalf("If-None-Match = %q, want empty on an If-Match put", got)
	}
}

// TestPutBodyIntactThroughReconnect (L46): with GetBody wired from the
// ReadSeeker, a request whose connection died before the server read
// the body can be re-driven with a REBUILT body - the server reads the
// exact bytes. Drives a real PUT through the real HTTP stack.
func TestPutBodyIntactThroughReconnect(t *testing.T) {
	var bodies []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("ETag", `"00000000000000000000000000000000"`)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(Options{
		ServerURL:          srv.URL,
		Bucket:             "bkt",
		BasicAuthUser:      "ak",
		BasicAuthPass:      "sk",
		InsecureSkipVerify: true, // httptest's self-signed pair
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	const want = "body-bytes-that-must-survive"
	if _, err := c.Put(context.Background(), "g.bin", strings.NewReader(want), ""); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("server read bodies = %q, want one intact %q (GetBody must allow a rebuild)", bodies, want)
	}
}

// TestClassifyH3ErrTypeBased (L46): the buckets are TYPE-based; an
// error whose TEXT mentions network/handshake words but carries no
// dial-class TYPE must be classSent (never re-driven - a consumed PUT
// body could double-apply).
func TestClassifyH3ErrTypeBased(t *testing.T) {
	class, auth := classifyH3Err(errors.New("network handshake udp connection refused"))
	if class != classSent || auth {
		t.Fatalf("string-only error classified (%v,%v), want (classSent,false) - no string sniffing", class, auth)
	}

	// Typed dial-class shapes stay dial.
	class, auth = classifyH3Err(&quic.TransportError{ErrorCode: quic.ConnectionRefused})
	if class != classDial || auth {
		t.Fatalf("TransportError classified (%v,%v), want (classDial,false)", class, auth)
	}

	// Crypto TransportError = dial + auth failure (L47 h3 shape).
	class, auth = classifyH3Err(&quic.TransportError{ErrorCode: 0x100}) // first crypto code
	if class != classDial || !auth {
		t.Fatalf("crypto TransportError classified (%v,%v), want (classDial,true)", class, auth)
	}

	// Stream error = sent.
	class, _ = classifyH3Err(&quic.StreamError{StreamID: 4})
	if class != classSent {
		t.Fatalf("StreamError classified %v, want classSent", class)
	}

	// Typed net.Error = dial.
	class, _ = classifyH3Err(&net.DNSError{Err: "no such host", Name: "h.example"})
	if class != classDial {
		t.Fatalf("net.Error classified %v, want classDial", class)
	}

	// Unknown error type (e.g. a body-reader failure mid-flight) =
	// sent, the conservative side.
	class, _ = classifyH3Err(errors.New("body read failed mid-flight"))
	if class != classSent {
		t.Fatalf("unknown error classified %v, want classSent", class)
	}
}

// TestAuthFailedSentinelBothShapes (L47): TCP 401/403 map onto
// ErrAuthFailed via errors.Is, matching the h3 mTLS shape.
func TestAuthFailedSentinelBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"401", http.StatusUnauthorized},
		{"403", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &stubHandler{status: tc.status, body: "nope"}
			c := newTestClient(t, h)
			_, _, err := c.Get(context.Background(), "f", nil)
			if !errors.Is(err, ErrAuthFailed) {
				t.Fatalf("statusErr(%s) = %v, want errors.Is(err, ErrAuthFailed)", tc.name, err)
			}
		})
	}

	// The h3 mTLS shape wraps the same sentinel (asAuthFailed).
	err := asAuthFailed(&quic.TransportError{ErrorCode: 0x152})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("asAuthFailed(crypto) = %v, want ErrAuthFailed wrap", err)
	}
	// Non-crypto transport errors pass through untouched.
	if err := asAuthFailed(&quic.TransportError{ErrorCode: quic.ConnectionRefused}); errors.Is(err, ErrAuthFailed) {
		t.Fatal("non-crypto transport error must not become ErrAuthFailed")
	}

	// Other statuses keep their vocabulary.
	h := &stubHandler{status: http.StatusInternalServerError, body: "boom"}
	c := newTestClient(t, h)
	_, _, err = c.Get(context.Background(), "f", nil)
	if err == nil || errors.Is(err, ErrAuthFailed) {
		t.Fatalf("500 = %v, want a generic (non-auth) error", err)
	}
}

// TestDeleteSendsNoPrecondition (M14 doc-honest pin): Delete carries NO
// If-Match on the wire - the sync engine's listing check is the guard.
func TestDeleteSendsNoPrecondition(t *testing.T) {
	h := &stubHandler{status: http.StatusNoContent}
	c := newTestClient(t, h)
	if err := c.Delete(context.Background(), "old.txt", "deadbeefdeadbeefdeadbeefdeadbeef"); err != nil {
		t.Fatal(err)
	}
	req := h.last(t)
	if req.method != http.MethodDelete {
		t.Fatalf("wire request = %s, want DELETE", req.method)
	}
	if got := req.header.Get("If-Match"); got != "" {
		t.Fatalf("If-Match = %q, want empty (Delete has no wire precondition, M14)", got)
	}
}
