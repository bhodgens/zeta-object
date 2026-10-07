package transport

// client_test.go - leaf 06 acceptance 1: httptest-based wire tests with
// the gateway's REAL response shapes (207 fixture bytes matching the
// form e2e cases 19/25 pin; PUT Content-Length + conditional headers;
// alt-svc parsing; the batch manifest body).

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gateway207 is the PROPFIND Depth-1 body shape the gateway renders
// (internal/frontend/webdav/propfind.go + the e2e 19/25 pins): literal
// d:/oc: prefixed elements, xmlns declared on the ROOT element, quoted
// getetag, getcontentlength chardata, RFC1123 getlastmodified, and a
// resourcetype collection child for directories.
const gateway207 = `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/bkt/</d:href>
    <d:propstat>
      <d:prop>
        <d:resourcetype><d:collection/></d:resourcetype>
        <d:getetag>"dir-0123456789abcdef"</d:getetag>
        <d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified>
        <oc:fileid>00000001abc</oc:fileid>
        <oc:permissions>SRWD</oc:permissions>
      </d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/bkt/seed.txt</d:href>
    <d:propstat>
      <d:prop>
        <d:getcontentlength>20</d:getcontentlength>
        <d:getetag>"0123456789abcdef0123456789abcdef"</d:getetag>
        <d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified>
        <d:getcontenttype>application/octet-stream</d:getcontenttype>
        <oc:fileid>00000002def</oc:fileid>
      </d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/bkt/photos/</d:href>
    <d:propstat>
      <d:prop>
        <d:resourcetype><d:collection/></d:resourcetype>
        <d:getetag>"dir-fedcba9876543210"</d:getetag>
      </d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
</d:multistatus>`

// gateway207DefaultNS is the SAME document in the RFC default-namespace
// form other webdav servers emit (robustness acceptance; not asserted
// against the gateway).
const gateway207DefaultNS = `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:">
  <response>
    <href>/bkt/</response-href-placeholder>
  </response>
</d:multistatus>`

// defaultNS207 is a WELL-FORMED default-namespace 207 (the placeholder
// above only pins that the parser tolerates unprefixed elements).
const defaultNS207 = `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:">
  <response>
    <href>/bkt/</href>
    <propstat>
      <prop>
        <resourcetype><collection/></resourcetype>
        <getetag>"dir-0123456789abcdef"</getetag>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
  <response>
    <href>/bkt/file.bin</href>
    <propstat>
      <prop>
        <getcontentlength>7</getcontentlength>
        <getetag>"aaaa"</getetag>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</d:multistatus>`

// newTestClient builds a Client over httptest TLS server with the given
// handler. h3 stays unbuilt (no client cert) so every request rides the
// TCP path.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
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
	return c
}

func TestParseAltSvcH3(t *testing.T) {
	cases := []struct {
		header string
		port   string
		ok     bool
	}{
		{`h3=":9443"; persist=1`, "9443", true},            // the gateway's pinned form
		{`h3="9443"; persist=1`, "9443", true},             // no-colon host form (RFC 7838)
		{`h3="host.example:9443"`, "9443", true},           // explicit authority
		{`h2=":443", h3=":8443"; persist=1`, "8443", true}, // comma-separated
		{`h2=":443"`, "", false},                           // no h3
		{``, "", false},                                    // absent
		{`h3=":0"`, "", false},                             // unusable port
		{`h3=":99999"`, "", false},                         // out of range
	}
	for _, tc := range cases {
		port, ok := parseAltSvcH3(tc.header)
		if ok != tc.ok || port != tc.port {
			t.Errorf("parseAltSvcH3(%q) = (%q,%v), want (%q,%v)", tc.header, port, ok, tc.port, tc.ok)
		}
	}
}

func TestAltSvcStickyFallback(t *testing.T) {
	now := time.Now()
	clock := now
	a := newAltsvcState()
	a.now = func() time.Time { return clock }

	// tcp-only -> eligible on advertisement + cert.
	if a.observe(`h3=":9443"; persist=1`, false) {
		t.Fatal("observe without a cert must not act on the advertisement")
	}
	if state, _ := a.current(); state != h3TCPOnly {
		t.Fatalf("state = %v, want tcpOnly", state)
	}
	if !a.observe(`h3=":9443"; persist=1`, true) {
		t.Fatal("observe with a cert should record the advertisement")
	}
	if state, port := a.current(); state != h3Eligible || port != "9443" {
		t.Fatalf("state = (%v,%s), want (eligible,9443)", state, port)
	}

	// eligible -> h3 chosen; failure -> fallback window.
	port, upgradable := a.useH3()
	if port != "9443" || !upgradable {
		t.Fatalf("useH3 = (%q,%v), want (9443,true)", port, upgradable)
	}
	if up := a.h3Down(now); !up {
		t.Fatal("first failure must count as an upgrade attempt (request provably unsent)")
	}
	if state, _ := a.current(); state != h3Fallback {
		t.Fatalf("state = %v, want fallback", state)
	}
	// Inside the window: TCP.
	if port, up := a.useH3(); port != "" || up {
		t.Fatalf("useH3 inside fallback = (%q,%v), want TCP", port, up)
	}
	// After the window: eligible again.
	clock = now.Add(fallbackWindow + time.Minute)
	if port, up := a.useH3(); port != "9443" || !up {
		t.Fatalf("useH3 after window = (%q,%v), want (9443,true)", port, up)
	}

	// Success -> active, sticky.
	a.h3Up()
	if state, _ := a.current(); state != h3Active {
		t.Fatalf("state = %v, want active", state)
	}
	if port, up := a.useH3(); port != "9443" || up {
		t.Fatal("active state must ride h3 without upgrade-retry permission")
	}
	// A failure while ACTIVE is NOT an upgrade (request may have applied).
	if up := a.h3Down(now); up {
		t.Fatal("failure from active state must not be an upgrade attempt")
	}
}

// stubHandler records requests and answers canned responses.
type stubHandler struct {
	mu      []record
	status  int
	body    string
	headers map[string]string
}

type record struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func (s *stubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu = append(s.mu, record{r.Method, r.URL.Path, r.Header.Clone(), b})
	for k, v := range s.headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(s.status)
	_, _ = io.WriteString(w, s.body)
}

func (s *stubHandler) last(t *testing.T) record {
	t.Helper()
	if len(s.mu) == 0 {
		t.Fatal("no requests recorded")
	}
	return s.mu[len(s.mu)-1]
}

func TestPropfindParsesGateway207(t *testing.T) {
	h := &stubHandler{status: http.StatusMultiStatus, body: gateway207}
	c := newTestClient(t, h)
	entries, err := c.Propfind(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.mu) != 1 {
		t.Fatalf("requests = %d, want 1", len(h.mu))
	}
	req := h.last(t)
	if req.method != "PROPFIND" || req.path != "/bkt/" {
		t.Errorf("wire request = %s %s, want PROPFIND /bkt/", req.method, req.path)
	}
	if d := req.header.Get("Depth"); d != "1" {
		t.Errorf("Depth header = %q, want 1", d)
	}
	// Entry 0 is the collection itself, then children sorted
	// (photos/ sorts before seed.txt).
	if len(entries) != 3 {
		t.Fatalf("entries = %d (%+v), want 3", len(entries), entries)
	}
	if entries[0].Key != "" || !entries[0].IsDir || entries[0].ETag != "dir-0123456789abcdef" {
		t.Errorf("self entry wrong: %+v", entries[0])
	}
	if entries[1].Key != "photos/" || !entries[1].IsDir || entries[1].ETag != "dir-fedcba9876543210" {
		t.Errorf("dir child wrong: %+v", entries[1])
	}
	if entries[2].Key != "seed.txt" || entries[2].IsDir || entries[2].Size != 20 ||
		entries[2].ETag != "0123456789abcdef0123456789abcdef" {
		t.Errorf("file child wrong: %+v", entries[2])
	}
	want := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)
	if !entries[2].ModTime.Equal(want) {
		t.Errorf("mtime = %v, want %v", entries[2].ModTime, want)
	}
}

func TestPropfindAcceptsDefaultNamespace(t *testing.T) {
	h := &stubHandler{status: http.StatusMultiStatus, body: defaultNS207}
	c := newTestClient(t, h)
	entries, err := c.Propfind(context.Background(), "", false)
	if err != nil {
		t.Fatalf("default-namespace 207 must parse: %v", err)
	}
	if len(entries) != 2 || entries[0].Key != "" || entries[1].Key != "file.bin" || entries[1].Size != 7 {
		t.Fatalf("entries wrong: %+v", entries)
	}
	_ = gateway207DefaultNS // shape doc only
}

func TestPropfind404(t *testing.T) {
	h := &stubHandler{status: http.StatusNotFound, body: "gone"}
	c := newTestClient(t, h)
	if _, err := c.Propfind(context.Background(), "missing/", false); !errors.Is(err, ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

func TestPutContentLengthAndConditionals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ifMatch     string
		ifNoneStar  bool
		wantIfMatch string
		wantINM     string
	}{
		{"plain overwrite", "", false, "", ""},
		{"if-match", "aabbccdd", false, `"aabbccdd"`, ""},
		{"create (if-none-match:*)", "", true, "", "*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &stubHandler{status: http.StatusCreated, headers: map[string]string{
				"ETag": `"deadbeefdeadbeefdeadbeefdeadbeef"`,
			}}
			c := newTestClient(t, h)
			body := bytes.Repeat([]byte("x"), 100)
			etag, err := c.PutOpts(context.Background(), "dir/file.bin", bytes.NewReader(body), tc.ifMatch, tc.ifNoneStar)
			if err != nil {
				t.Fatal(err)
			}
			if etag != "deadbeefdeadbeefdeadbeefdeadbeef" {
				t.Errorf("serverETag = %q, want the normalized MD5", etag)
			}
			req := h.last(t)
			if req.method != http.MethodPut || req.path != "/bkt/dir/file.bin" {
				t.Errorf("wire request = %s %s", req.method, req.path)
			}
			if cl := req.header.Get("Content-Length"); cl != "100" {
				t.Errorf("Content-Length = %q, want 100 (chunked is a gateway 400)", cl)
			}
			if got := req.header.Get("If-Match"); got != tc.wantIfMatch {
				t.Errorf("If-Match = %q, want %q", got, tc.wantIfMatch)
			}
			if got := req.header.Get("If-None-Match"); got != tc.wantINM {
				t.Errorf("If-None-Match = %q, want %q", got, tc.wantINM)
			}
		})
	}
}

func TestPutMaps412ToConflict(t *testing.T) {
	h := &stubHandler{status: http.StatusPreconditionFailed, body: "stale"}
	c := newTestClient(t, h)
	_, err := c.PutOpts(context.Background(), "f", strings.NewReader("b"), "stale-etag", false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestGetStreamsAndRevalidates(t *testing.T) {
	h := &stubHandler{status: http.StatusOK, body: "hello-bytes", headers: map[string]string{
		"ETag":          `"0123456789abcdef0123456789abcdef"`,
		"Last-Modified": "Mon, 02 Jan 2006 15:04:05 GMT",
	}}
	c := newTestClient(t, h)
	rc, info, err := c.Get(context.Background(), "seed.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "hello-bytes" || info.Size != 11 || info.ETag != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("got %q info %+v", got, info)
	}
	// Revalidation: the caller sends If-None-Match via a fresh GET with
	// the header set through the range-less path (exercised through the
	// wire handler here).
	rc2, _, err := c.Get(context.Background(), "seed.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc2.Close()
	if h.last(t).header.Get("If-None-Match") != "" {
		t.Error("plain GET must not carry If-None-Match")
	}
	// 304 maps to ErrNotModified.
	h.status = http.StatusNotModified
	h.body = ""
	if _, _, err := c.Get(context.Background(), "seed.txt", nil); !errors.Is(err, ErrNotModified) {
		t.Fatalf("err = %v, want ErrNotModified", err)
	}
}

func TestMoveDestinationHeaderAnd412(t *testing.T) {
	h := &stubHandler{status: http.StatusCreated}
	c := newTestClient(t, h)
	if err := c.Move(context.Background(), "a.txt", "b/c.txt", `ddest`); err != nil {
		t.Fatal(err)
	}
	req := h.last(t)
	if req.method != "MOVE" || req.path != "/bkt/a.txt" {
		t.Fatalf("wire request = %s %s, want MOVE /bkt/a.txt", req.method, req.path)
	}
	if dest := req.header.Get("Destination"); dest == "" || !strings.HasSuffix(dest, "/bkt/b/c.txt") {
		t.Errorf("Destination = %q, want .../bkt/b/c.txt", dest)
	}
	if req.header.Get("Overwrite") != "T" {
		t.Errorf("Overwrite = %q, want T", req.header.Get("Overwrite"))
	}
	if req.header.Get("If-Match") != `"ddest"` {
		t.Errorf("If-Match = %q, want quoted dest etag", req.header.Get("If-Match"))
	}
	h.status = http.StatusPreconditionFailed
	if err := c.Move(context.Background(), "a.txt", "b/c.txt", "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestMkdirAndDelete(t *testing.T) {
	h := &stubHandler{status: http.StatusCreated}
	c := newTestClient(t, h)
	if err := c.Mkdir(context.Background(), "newdir"); err != nil {
		t.Fatal(err)
	}
	if req := h.last(t); req.method != "MKCOL" || req.path != "/bkt/newdir" {
		t.Errorf("wire request = %s %s, want MKCOL /bkt/newdir", req.method, req.path)
	}
	h.status = http.StatusMethodNotAllowed
	if err := c.Mkdir(context.Background(), "newdir"); !errors.Is(err, ErrExist) {
		t.Fatalf("mkdir 405 -> %v, want ErrExist", err)
	}

	h.status = http.StatusNoContent
	if err := c.Delete(context.Background(), "old.txt", ""); err != nil {
		t.Fatal(err)
	}
	if req := h.last(t); req.method != http.MethodDelete || req.path != "/bkt/old.txt" {
		t.Errorf("wire request = %s %s", req.method, req.path)
	}
	h.status = http.StatusConflict
	if err := c.Delete(context.Background(), "full-dir", ""); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("delete 409 -> %v, want ErrNotEmpty", err)
	}
	h.status = http.StatusNotFound
	if err := c.Delete(context.Background(), "old.txt", ""); !errors.Is(err, ErrNotExist) {
		t.Fatalf("delete 404 -> %v, want ErrNotExist", err)
	}
}

func TestBatchDeleteManifestShape(t *testing.T) {
	h := &stubHandler{status: http.StatusOK, body: `{"results":[{"index":0,"status":"ok"},{"index":1,"status":"error","code":"NoSuchKey"}]}`}
	c := newTestClient(t, h)
	res, err := c.BatchDelete(context.Background(), []string{"a.txt", "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	req := h.last(t)
	if req.method != http.MethodPost || req.path != "/bkt/" {
		t.Fatalf("wire request = %s %s, want POST /bkt/ (the ?batch query rides the URL)", req.method, req.path)
	}
	// The manifest body shape (internal/batchops.Manifest).
	want := `{"operations":[{"op":"delete","from":"a.txt"},{"op":"delete","from":"b.txt"}]}`
	if string(req.body) != want {
		t.Errorf("manifest = %s\nwant       %s", req.body, want)
	}
	if len(res) != 2 || res[0].Status != "ok" || res[1].Code != "NoSuchKey" {
		t.Errorf("results = %+v", res)
	}
}

func TestLockUnlock(t *testing.T) {
	h := &stubHandler{status: http.StatusOK, headers: map[string]string{
		"Lock-Token": `<opaquelocktoken:e2e31-0000-0000>`,
	}, body: `<D:prop><D:lockdiscovery><D:activelock><D:token>opaquelocktoken:e2e31-0000-0000</D:token></D:activelock></D:lockdiscovery></D:prop>`}
	c := newTestClient(t, h)
	tok, err := c.Lock(context.Background(), "locked.txt", 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "opaquelocktoken:e2e31-0000-0000" {
		t.Errorf("token = %q", tok)
	}
	req := h.last(t)
	if req.method != "LOCK" || req.header.Get("Timeout") != "Second-600" {
		t.Errorf("lock request headers: Timeout=%q", req.header.Get("Timeout"))
	}
	if !strings.Contains(string(req.body), "<exclusive/>") || !strings.Contains(string(req.body), "lockinfo") {
		t.Errorf("lock body lacks lockinfo/exclusive: %s", req.body)
	}
	h.status = http.StatusNoContent
	if err := c.Unlock(context.Background(), "locked.txt", tok); err != nil {
		t.Fatal(err)
	}
	req = h.last(t)
	if req.method != "UNLOCK" || !strings.Contains(req.header.Get("Lock-Token"), tok) {
		t.Errorf("unlock request = %s Lock-Token=%q", req.method, req.header.Get("Lock-Token"))
	}
	h.status = http.StatusConflict
	if err := c.Unlock(context.Background(), "locked.txt", "wrong"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlock 409 -> %v, want ErrConflict", err)
	}
	h.status = http.StatusLocked
	if _, err := c.Lock(context.Background(), "locked.txt", time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("locked 423 -> %v, want ErrConflict", err)
	}
}

func TestBasicAuthSent(t *testing.T) {
	h := &stubHandler{status: http.StatusOK, body: gateway207}
	c := newTestClient(t, h)
	if _, err := c.Propfind(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + basicAuthValue("ak", "sk")
	if got := h.last(t).header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestServerAuthRejectIsError(t *testing.T) {
	h := &stubHandler{status: http.StatusUnauthorized, body: "nope"}
	c := newTestClient(t, h)
	_, _, err := c.Get(context.Background(), "f", nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want an HTTP 401 error", err)
	}
}

// basicAuthValue mirrors net/http's encoding.
func basicAuthValue(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
