// propfind.go — PROPFIND Depth 0/1 (leaf 02 Tasks 2–3): RFC 4918 207
// Multi-Status encoding, allprop/propname/named-prop request bodies,
// collection children via List prefix/delimiter with FULL pagination (a
// truncated listing that silently drops entries is a correctness bug, not
// a degradation).
package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// maxDeleteListPages bounds pagination loops (PROPFIND listing and
// recursive DELETE share the guard) so a misbehaving backend can never hang
// a request: 10000 pages × a generous page size is far beyond any real
// tree; hitting the bound is a 500, never a hang.
// maxListPages bounds pagination AND each page now carries an explicit
// MaxKeys (W4): the fs backend treats MaxKeys<=0 as unbounded, so a Depth:1
// PROPFIND of a huge collection built one giant 207 in memory. Bounded
// pages make the 10000-page cap real.
const maxListPages = 10000

// propfindPageKeys is the per-Page entry bound (objects + common prefixes).
const propfindPageKeys = 1000

// multistatus is the 207 document root. Every element carries
// xml.Name{Space: "DAV:"} so the marshaller emits xmlns="DAV:" scoping —
// RFC 4918 clients accept the unprefixed default-namespace form. The
// ownCloud namespace is declared once on the root (xmlns:oc) and referenced
// by the oc: property elements (issue #5).
type multistatus struct {
	// Elements render with literal "d:"/"oc:" prefixes (the ownCloud
	// client's csync property parser matches prefixed qnames literally);
	// the root declares both bindings. XMLName is empty so Go does not
	// re-declare namespaces per element.
	XmlnsD    string `xml:"xmlns:d,attr"`
	XmlnsOc   string `xml:"xmlns:oc,attr"`
	Responses []response
}

// response is one <D:response>: href + propstat blocks.
type response struct {
	// Local names carry the literal "d:" prefix; the root's xmlns:d
	// declaration binds it (csync matches prefixed qnames literally).
	XMLName   xml.Name   `xml:"d:response"`
	Href      string     `xml:"d:href"`
	Propstats []propstat `xml:"d:propstat"`
}

// propstat groups properties by their status.
type propstat struct {
	Props  []activeProp `xml:"d:prop"`
	Status string       `xml:"d:status"`
}

// propfindRequest is the parsed request body. Go's xml unmarshaller does
// not set bool fields from empty elements like <D:allprop/>, so the body is
// captured as raw inner elements and classified by name; named props are
// re-extracted from the raw body bytes.
type propfindRequest struct {
	Any          []anyXML   `xml:",any"`
	Allprop      bool       `xml:"-"`
	Propname     bool       `xml:"-"`
	hasNamedProp bool       `xml:"-"`
	Named        []xml.Name `xml:"-"`
}

// anyXML is one raw inner element of the propfind body.
type anyXML struct {
	XMLName xml.Name
}

// classify sorts the captured children into the three RFC 4918 body modes.
func (pf *propfindRequest) classify(raw string) {
	for _, c := range pf.Any {
		switch {
		case c.XMLName.Space == Namespace && c.XMLName.Local == "allprop":
			pf.Allprop = true
		case c.XMLName.Space == Namespace && c.XMLName.Local == "propname":
			pf.Propname = true
		case c.XMLName.Space == Namespace && c.XMLName.Local == "prop":
			pf.hasNamedProp = true
		}
	}
	if pf.hasNamedProp {
		pf.Named = parsePropNames(raw)
	}
}

// propInner is the <D:prop> inner capture for named-prop bodies.
type propInner struct {
	Names []xml.Name `xml:",any"`
}

// parsePropNames extracts the requested property names from a named-prop
// propfind body (the <D:prop> children).
func parsePropNames(raw string) []xml.Name {
	var wrapper struct {
		Inner propInner `xml:"DAV: prop"`
	}
	if err := xml.Unmarshal([]byte(raw), &wrapper); err != nil {
		return nil
	}
	return wrapper.Inner.Names
}

// handlePROPFIND implements Depth 0/1 with the Contract-4 status table:
// 207 success; Depth infinity ⇒ 403 propfind-finite-depth; bad XML ⇒ 400;
// missing resource ⇒ 404.
func (f *Frontend) handlePROPFIND(w http.ResponseWriter, r *http.Request, res resource) {
	depth := strings.ToLower(r.Header.Get("Depth"))
	if depth == "" {
		depth = "infinity" // RFC 4918 §9.1: absent Depth = infinity
	}
	if depth != "0" && depth != "1" {
		writeDavError(w, http.StatusForbidden, "propfind-finite-depth")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	pf, err := parsePropfindBody(body)
	if err != nil {
		writeDavError(w, http.StatusBadRequest, "")
		return
	}

	// The identity's write grant on the effective bucket decides the
	// pinned oc:permissions string (readwrite vs readonly variants).
	id := identityOf(r)
	write := f.writeGranted(id, res)

	entries, err := f.propfindEntries(r, res, depth == "1", write)
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}

	ms := multistatus{XmlnsD: "DAV:", XmlnsOc: OCNamespace}
	for _, e := range entries {
		ms.Responses = append(ms.Responses, buildResponse(e, pf))
	}
	writeXMLDocumentFixup(w, http.StatusMultiStatus, ms)
}

// writeXMLDocumentFixup marshals the 207 and repairs Go encoder closing
// tags: elements whose Local name carries a literal "d:"/"oc:" prefix are
// opened prefixed but closed unprefixed by encoding/xml. The ownCloud
// client's strict XML parser rejects mismatched tags.
func writeXMLDocumentFixup(w http.ResponseWriter, status int, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		writeDavError(w, http.StatusInternalServerError, "")
		return
	}
	fixed := fixupPrefixedClosers(body)
	full := append([]byte(xml.Header), fixed...)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", fmtInt(len(full)))
	w.WriteHeader(status)
	_, _ = w.Write(full)
}

// fixupPrefixedClosers rewrites closing tags of prefixed elements: for
// every <pre:name ...> opened with a literal prefix, the matching
// </name> closer becomes </pre:name>. Only the documented property and
// response element names are rewritten.
func fixupPrefixedClosers(body []byte) []byte {
	dNames := []string{
		"multistatus", "response", "href", "propstat", "prop", "status",
		"resourcetype", "collection", "getcontenttype", "getlastmodified",
		"getcontentlength", "getetag",
	}
	out := body
	for _, n := range dNames {
		out = bytes.ReplaceAll(out, []byte("</"+n+">"), []byte("</d:"+n+">"))
	}
	for _, n := range []string{"fileid", "permissions", "size"} {
		out = bytes.ReplaceAll(out, []byte("</"+n+">"), []byte("</oc:"+n+">"))
	}
	return out
}

// propfindEntry is one resource row of the 207 body.
type propfindEntry struct {
	href   string
	bucket string // effective bucket (oc:fileid derivation coordinate)
	obj    objectmodel.Object
	isColl bool
	found  bool  // false ⇒ 404 propstat
	write  bool  // identity's write grant on the bucket ⇒ oc:permissions
	ocSize int64 // collection aggregate size (oc:size); 0 for files
}

// propfindEntries resolves the requested resource (+children at Depth 1)
// into propfindEntry rows. A missing top-level resource ⇒ 404 via
// davStatus (objectmodel.ErrNoSuchKey shape). write is the authenticated
// identity's write grant for the effective bucket (oc:permissions).
func (f *Frontend) propfindEntries(r *http.Request, res resource, depth1, write bool) ([]propfindEntry, error) {
	ctx := r.Context()
	if res.isRoot {
		return f.rootEntries(ctx, depth1, write)
	}
	obj, kind, err := f.resolveKind(ctx, res)
	if err != nil {
		return nil, err
	}
	if kind == kindMissing && !res.isCollection {
		// PROPFIND-only relaxation (ownCloud client interop, F-oc-1):
		// real clients PROPFIND the sync root WITHOUT a trailing slash
		// (owncloudcmd strips it). A slash-less path that exists only as
		// a prefix resolves as a collection here; GET/PUT/DELETE keep
		// the pinned exact-key semantics (resolveKind unchanged).
		exists, cerr := collectionExists(ctx, f.be, res.bucket, res.collectionPrefix())
		if cerr != nil {
			return nil, cerr
		}
		if exists {
			res.isCollection = true
			obj, kind = objectmodel.Object{}, kindCollection
		}
	}
	if kind == kindMissing {
		return nil, objectmodel.ErrNoSuchKey(f.davPath(res))
	}
	out := []propfindEntry{f.entryFor(ctx, res, obj, kind == kindCollection, write)}
	if depth1 {
		children, err := f.childEntries(ctx, res, write)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
	}
	return out, nil
}

// writeGranted reports the identity's write grant for the resource's
// effective bucket (mode B root: the configured bucket). OPTIONS-style
// special cases do not apply — PROPFIND is always authorized before this
// runs, so the identity is present.
func (f *Frontend) writeGranted(id auth.Identity, res resource) bool {
	if res.isRoot && f.bucket != "" {
		return id.CanWrite(f.bucket)
	}
	return id.CanWrite(res.bucket)
}

// rootEntries serves PROPFIND /: mode A lists buckets (via Buckets()), mode
// B lists the configured bucket's top level. write is the identity's write
// grant for the effective bucket (oc:permissions on every row).
func (f *Frontend) rootEntries(ctx context.Context, depth1, write bool) ([]propfindEntry, error) {
	if f.bucket != "" {
		// Mode B: root IS the configured bucket; list its top level.
		res := resource{bucket: f.bucket, isCollection: true, isRoot: true}
		out := []propfindEntry{{
			href:   "/",
			bucket: f.bucket,
			isColl: true,
			found:  true,
			write:  write,
		}}
		if depth1 {
			children, err := f.childEntries(ctx, res, write)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
		}
		return out, nil
	}
	// Mode A: synthetic root; Depth 1 children are buckets.
	out := []propfindEntry{{href: "/", isColl: true, found: true, write: write}}
	if !depth1 {
		return out, nil
	}
	buckets, err := f.be.Buckets(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		child := resource{bucket: b.Name}
		// The bucket row's oc:permissions reflect THIS identity's grant
		// on that bucket (the identity passed authorization for the
		// listing, but may hold read-only on individual buckets).
		id, _ := auth.IdentityFromContext(ctx)
		out = append(out, propfindEntry{
			href: f.davPath(child), bucket: b.Name, isColl: true, found: true,
			write: id.CanWrite(b.Name),
		})
	}
	return out, nil
}

// entryFor builds one row for a resolved resource. Collections get their
// oc:size aggregate computed via a full List under the prefix (derived at
// request time — nothing is cached or persisted).
func (f *Frontend) entryFor(ctx context.Context, res resource, obj objectmodel.Object, isColl, write bool) propfindEntry {
	e := propfindEntry{
		href:   f.davPath(res),
		bucket: res.bucket,
		obj:    obj,
		isColl: isColl,
		found:  true,
		write:  write,
	}
	if isColl {
		// oc:size = aggregate contentLength of everything under the
		// collection prefix (full pagination — discovery is not hot).
		// A size probe error degrades to oc:size 0 rather than failing
		// the whole PROPFIND — the DAV properties remain correct.
		if size, err := collectionSize(ctx, f.be, res.bucket, res.collectionPrefix()); err == nil {
			e.ocSize = size
		}
	}
	return e
}

// childEntries lists the immediate children of a collection: files from
// ListPage.Objects and one-entry collections from ListPage.CommonPrefixes.
// Mode A bucket-level collections (key == "") list with Prefix "".
// write is the identity's write grant for the bucket (oc:permissions).
// Collection children get their oc:size aggregate via a nested full List.
func (f *Frontend) childEntries(ctx context.Context, res resource, write bool) ([]propfindEntry, error) {
	prefix := res.collectionPrefix()
	var out []propfindEntry
	err := f.eachChild(ctx, res.bucket, prefix, func(obj objectmodel.Object) error {
		child := resource{bucket: res.bucket, key: obj.Key}
		out = append(out, propfindEntry{
			href: f.davPath(child), bucket: res.bucket, obj: obj, found: true, write: write,
		})
		return nil
	}, func(cp string) error {
		child := resource{bucket: res.bucket, key: strings.TrimSuffix(cp, "/"), isCollection: true}
		e := propfindEntry{
			href: f.davPath(child), bucket: res.bucket, isColl: true, found: true, write: write,
		}
		// oc:size for the child collection: full List under its prefix.
		// An error degrades to 0 (same policy as entryFor).
		if size, err := collectionSize(ctx, f.be, res.bucket, child.collectionPrefix()); err == nil {
			e.ocSize = size
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Mode A: "/" Depth 1 children are buckets.
	if res.isRoot && f.bucket == "" {
		buckets, err := f.be.Buckets(ctx)
		if err != nil {
			return nil, err
		}
		id, _ := auth.IdentityFromContext(ctx)
		for _, b := range buckets {
			child := resource{bucket: b.Name}
			out = append(out, propfindEntry{
				href: f.davPath(child), bucket: b.Name, isColl: true, found: true,
				write: id.CanWrite(b.Name),
			})
		}
	}
	return out, nil
}

// collectionSize sums the contentLength of every key under the prefix —
// oc:size for collections, derived at PROPFIND time via a fully-paginated
// List (discovery is not a hot path). Nothing is cached or persisted
// (charter: derived, not stored).
func collectionSize(ctx context.Context, be backend.Backend, bucket, prefix string) (int64, error) {
	var total int64
	token := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return 0, objectmodel.ErrInternalError("listing exceeded the page bound")
		}
		p, err := be.List(ctx, bucket, objectmodel.ListParams{
			Prefix:            prefix,
			ContinuationToken: token,
		})
		if err != nil {
			return 0, err
		}
		for _, obj := range p.Objects {
			total += obj.Size
		}
		if !p.IsTruncated || p.NextToken == "" || p.NextToken == token {
			return total, nil
		}
		token = p.NextToken
	}
}

// eachChild pages List(prefix, delimiter) until exhausted (IsTruncated
// false), invoking onObject / onPrefix per entry. Full pagination is a
// correctness requirement (leaf 02 Task 3).
func (f *Frontend) eachChild(ctx context.Context, bucket, prefix string, onObject func(objectmodel.Object) error, onPrefix func(string) error) error {
	token := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return objectmodel.ErrInternalError("listing exceeded the page bound")
		}
		p, err := f.be.List(ctx, bucket, objectmodel.ListParams{
			Prefix:            prefix,
			Delimiter:         "/",
			MaxKeys:           propfindPageKeys,
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range p.Objects {
			if err := onObject(obj); err != nil {
				return err
			}
		}
		for _, cp := range p.CommonPrefixes {
			if err := onPrefix(cp); err != nil {
				return err
			}
		}
		if !p.IsTruncated || p.NextToken == "" || p.NextToken == token {
			return nil
		}
		token = p.NextToken
	}
}

// buildResponse renders one propfindEntry into a <D:response> honoring the
// request body mode: allprop (default), propname, or named props. Unknown
// named properties land in a second propstat with 404 per Contract 5.
func buildResponse(e propfindEntry, pf *propfindRequest) response {
	href := encodeHref(e.href)
	if pf != nil && pf.Propname {
		names := propNames(e)
		var ps propstat
		for _, n := range names {
			ps.Props = append(ps.Props, emptyProp(e, n))
		}
		ps.Status = statusLine(http.StatusOK)
		return response{Href: href, Propstats: []propstat{ps}}
	}
	if pf == nil || pf.Allprop || len(pf.Named) == 0 {
		ps := livePropstat(e)
		return response{Href: href, Propstats: []propstat{ps}}
	}
	ok := propstat{Status: statusLine(http.StatusOK)}
	missing := propstat{Status: statusLine(http.StatusNotFound)}
	known := map[string]bool{}
	for _, p := range propNames(e) {
		known[p] = true
	}
	for _, want := range pf.Named {
		if want.Space != Namespace && want.Space != OCNamespace && want.Space != "" {
			missing.Props = append(missing.Props, activeProp{XMLName: want})
			continue
		}
		if !known[want.Local] {
			missing.Props = append(missing.Props, emptyProp(e, want.Local))
			continue
		}
		ok.Props = append(ok.Props, liveEntry(e, want.Local))
	}
	out := response{Href: href}
	if len(ok.Props) > 0 {
		out.Propstats = append(out.Propstats, ok)
	}
	if len(missing.Props) > 0 {
		out.Propstats = append(out.Propstats, missing)
	}
	return out
}

// emptyProp renders a bare property-name element (propname mode and 404
// propstats) in the right namespace: ownCloud for the oc: discovery names,
// DAV: for everything else.
func emptyProp(_ propfindEntry, name string) activeProp {
	if ocLocalNames[name] {
		return activeProp{XMLName: xml.Name{Local: "oc:" + name}}
	}
	return activeProp{XMLName: xml.Name{Local: "d:" + name}}
}

// ocLocalNames is the set of ownCloud-namespace property local names.
var ocLocalNames = map[string]bool{"fileid": true, "permissions": true, "size": true}

// livePropstat renders the full live property set with 200.
func livePropstat(e propfindEntry) propstat {
	ps := propstat{Status: statusLine(http.StatusOK)}
	for _, p := range propNames(e) {
		ps.Props = append(ps.Props, liveEntry(e, p))
	}
	return ps
}

// propNames lists the live property names for the entry (Contract 5).
func propNames(e propfindEntry) []string {
	if !e.found {
		return nil
	}
	props := ObjectProps(e.obj, e.isColl, e.bucket, e.write, e.ocSize)
	names := make([]string, 0, len(props))
	for _, p := range props {
		names = append(names, p.Name)
	}
	return names
}

// liveEntry renders one named live property for the entry.
func liveEntry(e propfindEntry, name string) activeProp {
	for _, p := range ObjectProps(e.obj, e.isColl, ocBucket(e), e.write, e.ocSize) {
		if p.Name != name {
			continue
		}
		// The ownCloud client's csync property parser matches the
		// PREFIXED qname literally ("oc:fileid", "d:getlastmodified");
		// the root declares both bindings, so render with literal
		// prefixes and no per-element xmlns redeclaration.
		if p.OC {
			if p.Collection {
				return activeProp{XMLName: xml.Name{Local: "oc:" + p.Name}, Inner: collectionInner}
			}
			return activeProp{XMLName: xml.Name{Local: "oc:" + p.Name}, Value: p.Chardata}
		}
		switch {
		case p.Collection:
			return activeProp{XMLName: xml.Name{Local: "d:" + p.Name}, Inner: collectionInner}
		default:
			return activeProp{XMLName: xml.Name{Local: "d:" + p.Name}, Value: p.Chardata}
		}
	}
	return emptyProp(e, name)
}

// ocBucket is the bucket coordinate used for oc:fileid derivation.
func ocBucket(e propfindEntry) string { return e.bucket }

// allPropfind is the parsePropfindBody result for an allprop request (the
// RFC 4918 default when the body is empty).
var allPropfind = &propfindRequest{Allprop: true}

// parsePropfindBody handles allprop, propname, and named prop bodies.
// An EMPTY body is allprop (RFC 4918 §9.1). Malformed XML is an error ⇒ 400.
func parsePropfindBody(body []byte) (*propfindRequest, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return allPropfind, nil // allprop
	}
	var pf propfindRequest
	if err := xml.Unmarshal([]byte(trimmed), &pf); err != nil {
		return nil, err
	}
	pf.classify(trimmed)
	// <D:propfind> with none of the three children is ill-formed.
	if !pf.Allprop && !pf.Propname && !pf.hasNamedProp {
		return nil, errIllFormedPropfind
	}
	return &pf, nil
}

// errIllFormedPropfind marks a propfind body with no recognized child.
var errIllFormedPropfind = &illFormedError{}

type illFormedError struct{}

func (*illFormedError) Error() string { return "webdav: ill-formed propfind body" }

// statusLine renders an HTTP status for <D:status>.
func statusLine(code int) string {
	return "HTTP/1.1 " + fmtInt(code) + " " + http.StatusText(code)
}

// fmtInt renders a small int in decimal.
func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// encodeHref percent-encodes an href path (a space renders %20 — leaf 02
// Task 3 pin).
func encodeHref(path string) string {
	return (&url.URL{Path: path}).EscapedPath()
}
