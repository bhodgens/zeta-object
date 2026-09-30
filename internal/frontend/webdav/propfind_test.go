// propfind_test.go — leaf 02 Tasks 2–3: golden-XML PROPFIND tests (Depth
// 0/1), allprop/propname/named-prop bodies, Depth-infinity 403, malformed
// body 400, href encoding, and full pagination.
package webdav

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// propfind issues a PROPFIND and returns status + body.
func propfind(f *Frontend, path, depth, body string) (int, string) {
	req := httptest.NewRequest("PROPFIND", path, strings.NewReader(body))
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	if body == "" {
		req.Body = io.NopCloser(strings.NewReader(""))
		req.ContentLength = 0
	}
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestPROPFIND_Depth0_File_Golden(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("hello"), func(o *objectmodel.Object) {
		o.ETag = "abc123"
		o.ContentType = "text/plain"
	})
	code, body := propfind(f, "/photos/a.txt", "0", "")
	if code != 207 {
		t.Fatalf("status = %d, want 207; body=%.400s", code, body)
	}
	// Golden assertions on the exact element set. Go's marshaller emits
	// the default-namespace form (xmlns="DAV:"), which is RFC 4918
	// compliant — davfs2/Finder accept it (deviation from the leaf's
	// prefixed-form sketch, noted in the implementation record).
	for _, want := range []string{
		`<multistatus xmlns="DAV:">`,
		`<href xmlns="DAV:">/photos/a.txt</href>`,
		`<getcontentlength xmlns="DAV:">5</getcontentlength>`,
		`<getcontenttype xmlns="DAV:">text/plain</getcontenttype>`,
		`<getetag xmlns="DAV:">&#34;abc123&#34;</getetag>`,
		`<getlastmodified xmlns="DAV:">Tue, 29 Sep 2026 12:00:00 GMT</getlastmodified>`,
		`<resourcetype xmlns="DAV:"></resourcetype>`,
		"HTTP/1.1 200 OK",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	// Exactly one response block.
	if n := strings.Count(body, "<response ") + strings.Count(body, "<response>"); n != 1 {
		t.Fatalf("response blocks = %d, want 1", n)
	}
}

func TestPROPFIND_Depth0_Collection(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "2024/a.txt", []byte("x"))
	code, body := propfind(f, "/photos/2024/", "0", "")
	if code != 207 {
		t.Fatalf("status = %d; body=%.400s", code, body)
	}
	if !strings.Contains(body, `<collection xmlns="DAV:"></collection>`) {
		t.Fatalf("missing <collection/> in:\n%s", body)
	}
	if !strings.Contains(body, "httpd/unix-directory") {
		t.Fatalf("missing directory content type in:\n%s", body)
	}
	if strings.Contains(body, "getcontentlength") || strings.Contains(body, "getetag") {
		t.Fatalf("collections must not carry getcontentlength/getetag:\n%s", body)
	}
}

func TestPROPFIND_Depth1_Listing(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("aaa"))
	be.seed("photos", "2024/x.txt", []byte("x"))
	be.seed("photos", "2024/deep/y.txt", []byte("y"))
	code, body := propfind(f, "/photos/", "1", "")
	if code != 207 {
		t.Fatalf("status = %d; body=%.400s", code, body)
	}
	// Parent + a.txt + 2024/ (grandchildren must NOT leak).
	for _, want := range []string{
		`<href xmlns="DAV:">/photos/</href>`,
		`<href xmlns="DAV:">/photos/a.txt</href>`,
		`<href xmlns="DAV:">/photos/2024/</href>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "deep") {
		t.Fatalf("grandchildren leaked:\n%s", body)
	}
	if n := strings.Count(body, "<response ") + strings.Count(body, "<response>"); n != 3 {
		t.Fatalf("response blocks = %d, want 3:\n%s", n, body)
	}
}

func TestPROPFIND_Root_ModeA_Buckets(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("alpha", "f.txt", []byte("x"))
	be.seed("beta", "g.txt", []byte("y"))
	code, body := propfind(f, "/", "1", "")
	if code != 207 {
		t.Fatalf("status = %d; body=%.400s", code, body)
	}
	for _, want := range []string{
		`<href xmlns="DAV:">/</href>`,
		`<href xmlns="DAV:">/alpha/</href>`,
		`<href xmlns="DAV:">/beta/</href>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestPROPFIND_Root_ModeB(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "photos"})
	be.seed("photos", "top.txt", []byte("x"))
	code, body := propfind(f, "/", "1", "")
	if code != 207 {
		t.Fatalf("status = %d; body=%.400s", code, body)
	}
	// The root lists the bucket's CONTENTS, not the bucket itself.
	if !strings.Contains(body, `<href xmlns="DAV:">/top.txt</href>`) {
		t.Fatalf("missing /top.txt in:\n%s", body)
	}
	// No bucket-level hrefs: the bucket name must not appear as a
	// collection child (the stub's auto-ETag contains the bucket name, so
	// scope the check to hrefs).
	for b := range strings.SplitSeq(body, "<href ") {
		if idx := strings.Index(b, ">"); idx >= 0 {
			end := strings.Index(b[idx:], "</href>")
			if end >= 0 && strings.Contains(b[idx:idx+end], "/photos") {
				t.Fatalf("mode-B root exposed the bucket in an href: %s", b[idx:idx+end])
			}
		}
	}
}

func TestPROPFIND_HrefSpaceEncoded(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "my file.txt", []byte("x"))
	code, body := propfind(f, "/photos/", "1", "")
	if code != 207 {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "/photos/my%20file.txt") {
		t.Fatalf("href not percent-encoded:\n%s", body)
	}
}

func TestPROPFIND_MissingResource404(t *testing.T) {
	f, _ := newTestFrontend(Config{})
	code, body := propfind(f, "/photos/nope.txt", "0", "")
	if code != 404 {
		t.Fatalf("status = %d, want 404; body=%.200s", code, body)
	}
}

func TestPROPFIND_DepthInfinity403(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("x"))
	for _, depth := range []string{"infinity", "2", "INFINITY"} {
		code, body := propfind(f, "/photos/a.txt", depth, "")
		if code != 403 {
			t.Fatalf("Depth %q: status = %d, want 403", depth, code)
		}
		if !strings.Contains(body, "propfind-finite-depth") {
			t.Fatalf("Depth %q: missing precondition in:\n%s", depth, body)
		}
	}
	// Absent Depth header defaults to infinity ⇒ 403.
	code, _ := propfind(f, "/photos/a.txt", "", "")
	if code != 403 {
		t.Fatalf("absent Depth: status = %d, want 403", code)
	}
}

func TestPROPFIND_MalformedBody400(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("x"))
	code, _ := propfind(f, "/photos/a.txt", "0", "<broken")
	if code != 400 {
		t.Fatalf("status = %d, want 400", code)
	}
	// A propfind body with no recognized child is ill-formed too.
	code, _ = propfind(f, "/photos/a.txt", "0", `<D:propfind xmlns:D="DAV:"><D:junk/></D:propfind>`)
	if code != 400 {
		t.Fatalf("empty propfind: status = %d, want 400", code)
	}
}

func TestPROPFIND_NamedProp_UnknownGoes404Propstat(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("hello"))
	body := `<D:propfind xmlns:D="DAV:"><D:prop><getetag/><D:quota-available-bytes/></D:prop></D:propfind>`
	code, respBody := propfind(f, "/photos/a.txt", "0", body)
	if code != 207 {
		t.Fatalf("status = %d; body=%.400s", code, respBody)
	}
	if !strings.Contains(respBody, "HTTP/1.1 404 Not Found") {
		t.Fatalf("unknown property must land in a 404 propstat:\n%s", respBody)
	}
	if !strings.Contains(respBody, `<getetag xmlns="DAV:">`) {
		t.Fatalf("known property missing:\n%s", respBody)
	}
}

func TestPROPFIND_Propname(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("hello"))
	body := `<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`
	code, respBody := propfind(f, "/photos/a.txt", "0", body)
	if code != 207 {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(respBody, `<getetag xmlns="DAV:">&#34;`) {
		t.Fatalf("propname must not carry values:\n%s", respBody)
	}
	if !strings.Contains(respBody, `<getetag xmlns="DAV:"></getetag>`) {
		t.Fatalf("propname missing getetag name:\n%s", respBody)
	}
}

func TestPROPFIND_FullPagination(t *testing.T) {
	be := newStubBackend()
	// Script two pages: page 1 truncated with a token, page 2 final.
	be.pages["b"] = []objectmodel.ListPage{
		{
			Objects:     []objectmodel.Object{{Key: "p1.txt", Size: 1, ETag: "e1", LastModified: timeNow()}},
			IsTruncated: true,
			NextToken:   "tok1",
		},
		{
			Objects:     []objectmodel.Object{{Key: "p2.txt", Size: 2, ETag: "e2", LastModified: timeNow()}},
			IsTruncated: false,
		},
	}
	f2, err := New(be, Config{Bucket: "b"}, WithAuthenticator(newStubAuth()))
	if err != nil {
		t.Fatal(err)
	}
	code, body := propfind(f2, "/", "1", "")
	if code != 207 {
		t.Fatalf("status = %d; body=%.400s", code, body)
	}
	if !strings.Contains(body, "p1.txt") || !strings.Contains(body, "p2.txt") {
		t.Fatalf("truncated listing dropped entries:\n%s", body)
	}
}

func TestObjectProps_ZeroByteRendersLength(t *testing.T) {
	props := ObjectProps(objectmodel.Object{Key: "z", ETag: "e", LastModified: timeNow()}, false)
	found := false
	for _, p := range props {
		if p.Name == "getcontentlength" {
			found = true
			if p.Chardata != "0" {
				t.Fatalf("zero-byte getcontentlength = %q, want \"0\"", p.Chardata)
			}
		}
	}
	if !found {
		t.Fatal("getcontentlength missing")
	}
}
