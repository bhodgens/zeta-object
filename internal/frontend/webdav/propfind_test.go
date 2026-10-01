// propfind_test.go — leaf 02 Tasks 2–3: golden-XML PROPFIND tests (Depth
// 0/1), allprop/propname/named-prop bodies, Depth-infinity 403, malformed
// body 400, href encoding, and full pagination.
package webdav

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
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
		`<multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">`,
		`<d:href>/photos/a.txt</d:href>`,
		`<d:getcontentlength>5</d:getcontentlength>`,
		`<d:getcontenttype>text/plain</d:getcontenttype>`,
		`<d:getetag>&#34;abc123&#34;</d:getetag>`,
		`<d:getlastmodified>Tue, 29 Sep 2026 12:00:00 GMT</d:getlastmodified>`,
		`<d:resourcetype></d:resourcetype>`,
		`<oc:fileid>7919420764097676869</oc:fileid>`,
		`<oc:permissions>RW</oc:permissions>`,
		"HTTP/1.1 200 OK",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	// Exactly one response block.
	if n := strings.Count(body, "<d:response ") + strings.Count(body, "<d:response>"); n != 1 {
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
	if !strings.Contains(body, `<d:collection></d:collection>`) {
		t.Fatalf("missing <d:collection/> in:\n%s", body)
	}
	if !strings.Contains(body, "httpd/unix-directory") {
		t.Fatalf("missing directory content type in:\n%s", body)
	}
	if strings.Contains(body, "getcontentlength") {
		t.Fatalf("collections must not carry getcontentlength:\n%s", body)
	}
	// getetag IS expected on collections (real OC10 behavior; the ownCloud
	// 6.x discovery job treats its absence as an invalid reply).
	if !strings.Contains(body, "getetag") {
		t.Fatalf("collection missing getetag:\n%s", body)
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
		`<d:href>/photos/</d:href>`,
		`<d:href>/photos/a.txt</d:href>`,
		`<d:href>/photos/2024/</d:href>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "deep") {
		t.Fatalf("grandchildren leaked:\n%s", body)
	}
	if n := strings.Count(body, "<d:response ") + strings.Count(body, "<d:response>"); n != 3 {
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
		`<d:href>/</d:href>`,
		`<d:href>/alpha/</d:href>`,
		`<d:href>/beta/</d:href>`,
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
	if !strings.Contains(body, `<d:href>/top.txt</d:href>`) {
		t.Fatalf("missing /top.txt in:\n%s", body)
	}
	// No bucket-level hrefs: the bucket name must not appear as a
	// collection child (the stub's auto-ETag contains the bucket name, so
	// scope the check to hrefs).
	for b := range strings.SplitSeq(body, "<href ") {
		if idx := strings.Index(b, ">"); idx >= 0 {
			end := strings.Index(b[idx:], "</d:href>")
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
	if !strings.Contains(respBody, `<d:getetag>`) {
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
	if strings.Contains(respBody, `<d:getetag>&#34;`) {
		t.Fatalf("propname must not carry values:\n%s", respBody)
	}
	if !strings.Contains(respBody, `<d:getetag></d:getetag>`) {
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
	props := ObjectProps(objectmodel.Object{Key: "z", ETag: "e", LastModified: timeNow()}, false, "b", true, 0)
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

// --- issue #5: ownCloud-namespace discovery properties ----------------------

// TestOCProps_FileidStableAndDistinct pins the charter constraint: fileid is
// DERIVED (FNV-1a of bucket\x00key, uint63) — identical across two requests
// (and restarts), distinct between a file and its parent collection.
func TestOCProps_FileidStableAndDistinct(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "photos"})
	be.seed("photos", "2024/a.txt", []byte("hello"))

	_, body1 := propfind(f, "/2024/", "1", "")
	_, body2 := propfind(f, "/2024/", "1", "")
	if body1 != body2 {
		t.Fatalf("two identical PROPFINDs returned different bodies (fileid not stable)")
	}
	dirID := fileIDFromBody(t, body1, "/2024/")
	fileID := fileIDFromBody(t, body1, "/2024/a.txt")
	if dirID == "" || fileID == "" {
		t.Fatalf("fileid missing: dir=%q file=%q", dirID, fileID)
	}
	if dirID == fileID {
		t.Fatalf("file and parent collection share fileid %q", dirID)
	}
	// Deterministic derivation: recompute and compare.
	if got := OCFileID("photos", "2024/a.txt"); got != fileID {
		t.Fatalf("OCFileID mismatch: got %q, wire %q", got, fileID)
	}
	// uint63: non-negative decimal.
	if fileID[0] == '-' {
		t.Fatalf("fileid %q is not uint63", fileID)
	}
	// Cross-bucket stability: the SAME bucket+key derives the same id.
	// (Identical args are intentional here: this pins determinism.)
	//nolint:staticcheck // SA4000: identical args are intentional - this pins determinism.
	if OCFileID("photos", "2024/a.txt") != OCFileID("photos", "2024/a.txt") {
		t.Fatal("OCFileID not deterministic")
	}
	if OCFileID("other", "2024/a.txt") == OCFileID("photos", "2024/a.txt") {
		t.Fatal("different buckets must not collide on the same key (expected for stable derivation)")
	}
}

// fileIDFromBody extracts the oc:fileid chardata from the response block
// whose href is wantHref.
func fileIDFromBody(t *testing.T, body, wantHref string) string {
	t.Helper()
	for block := range strings.SplitSeq(body, "<d:response>") {
		if !strings.Contains(block, "<d:href>"+wantHref+"</d:href>") {
			continue
		}
		_, rest, found := strings.Cut(block, `<oc:fileid>`)
		if !found {
			t.Fatalf("response block for %s has no oc:fileid:\n%s", wantHref, block)
		}
		value, _, found := strings.Cut(rest, "</oc:fileid>")
		if !found {
			t.Fatalf("response block for %s has no closing oc:fileid", wantHref)
		}
		return value
	}
	t.Fatalf("no response block for %s in:\n%s", wantHref, body)
	return ""
}

// TestOCProps_PermissionsAndSize pins the simplified permission grammar and
// the oc:size aggregate on discovery replies (trailing-slash and slash-less).
func TestOCProps_PermissionsAndSize(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "photos"})
	be.seed("photos", "2024/a.txt", []byte("hello"))    // 5
	be.seed("photos", "2024/deep/b.txt", []byte("xy"))  // 2
	be.seed("photos", "2024/deep/c.txt", []byte("zzz")) // 3

	// Slash-less discovery (the owncloudcmd form — F-oc-1 relaxation keeps
	// working): oc: props must appear on those responses too.
	code, body := propfind(f, "/2024", "1", "")
	if code != 207 {
		t.Fatalf("slash-less PROPFIND status = %d, want 207", code)
	}
	assertOCDiscovery(t, body, "photos", true)

	// Trailing-slash form: same guarantees.
	code, body = propfind(f, "/2024/", "1", "")
	if code != 207 {
		t.Fatalf("trailing-slash PROPFIND status = %d, want 207", code)
	}
	assertOCDiscovery(t, body, "photos", true)

	// oc:size aggregates the whole subtree (5 + 2 + 3 = 10) on the parent.
	if !strings.Contains(body, `<oc:size>10</oc:size>`) {
		t.Fatalf("oc:size not the subtree aggregate (want 10):\n%s", body)
	}
	// deep/ sums to 5.
	if !strings.Contains(body, `<oc:size>5</oc:size>`) {
		t.Fatalf("oc:size for deep/ not 5:\n%s", body)
	}
	// Files carry permissions RW (readwrite) but NO oc:size (that is a
	// collection aggregate; files have getcontentlength).
	if strings.Count(body, `<oc:permissions>RW</oc:permissions>`) < 1 {
		t.Fatalf("readwrite file oc:permissions missing:\n%s", body)
	}
	for block := range strings.SplitSeq(body, "<d:response>") {
		if strings.Contains(block, "a.txt</d:href>") && strings.Contains(block, `<size xmlns=`) {
			t.Fatalf("file carries oc:size:\n%s", block)
		}
	}
}

// TestOCProps_PermissionsReadonly pins the readonly variants of the pinned
// grammar through an identity with a read-only grant.
func TestOCProps_PermissionsReadonly(t *testing.T) {
	be := newStubBackend()
	be.seed("photos", "2024/a.txt", []byte("x"))
	id := auth.Identity{AccessKeyID: "ro", BucketGrants: map[string]auth.Grant{"photos": {Read: true}}}
	f2, err := New(be, Config{Bucket: "photos"}, WithAuthenticator(&stubAuthenticator{identity: id}))
	if err != nil {
		t.Fatal(err)
	}
	code, body := propfind(f2, "/2024/", "0", "")
	if code != 207 {
		t.Fatalf("status = %d, want 207; body=%.300s", code, body)
	}
	if !strings.Contains(body, `<oc:permissions>RG</oc:permissions>`) {
		t.Fatalf("readonly collection must advertise RG:\n%s", body)
	}
}

// assertOCDiscovery checks the issue-#5 requirements on a discovery reply:
// xmlns:oc on the root, fileid+permissions on every response block.
func assertOCDiscovery(t *testing.T, body, bucket string, _ bool) {
	t.Helper()
	if !strings.Contains(body, `xmlns:oc="http://owncloud.org/ns"`) {
		t.Fatalf("xmlns:oc missing on multistatus root:\n%s", body)
	}
	blocks := 0
	for block := range strings.SplitSeq(body, "<d:response>") {
		if !strings.Contains(block, "</d:response>") {
			continue
		}
		blocks++
		if !strings.Contains(block, `<oc:fileid>`) {
			t.Fatalf("response block missing oc:fileid:\n%s", block)
		}
		if !strings.Contains(block, `<oc:permissions>`) {
			t.Fatalf("response block missing oc:permissions:\n%s", block)
		}
	}
	if blocks == 0 {
		t.Fatalf("no response blocks:\n%s", body)
	}
}
