// propfind.go — PROPFIND Depth 0/1 (leaf 02 Tasks 2–3): RFC 4918 207
// Multi-Status encoding, allprop/propname/named-prop request bodies,
// collection children via List prefix/delimiter with FULL pagination (a
// truncated listing that silently drops entries is a correctness bug, not
// a degradation).
package webdav

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// maxDeleteListPages bounds pagination loops (PROPFIND listing and
// recursive DELETE share the guard) so a misbehaving backend can never hang
// a request: 10000 pages × a generous page size is far beyond any real
// tree; hitting the bound is a 500, never a hang.
const maxListPages = 10000

// multistatus is the 207 document root. Every element carries
// xml.Name{Space: "DAV:"} so the marshaller emits xmlns="DAV:" scoping —
// RFC 4918 clients accept the unprefixed default-namespace form.
type multistatus struct {
	XMLName   xml.Name `xml:"DAV: multistatus"`
	Responses []response
}

// response is one <D:response>: href + propstat blocks.
type response struct {
	XMLName   xml.Name   `xml:"DAV: response"`
	Href      string     `xml:"DAV: href"`
	Propstats []propstat `xml:"DAV: propstat"`
}

// propstat groups properties by their status.
type propstat struct {
	Props  []activeProp `xml:"DAV: prop"`
	Status string       `xml:"DAV: status"`
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

	entries, err := f.propfindEntries(r, res, depth == "1")
	if err != nil {
		writeDavErrorFrom(w, err)
		return
	}

	ms := multistatus{}
	for _, e := range entries {
		ms.Responses = append(ms.Responses, buildResponse(e, pf))
	}
	writeXMLDocument(w, http.StatusMultiStatus, ms)
}

// propfindEntry is one resource row of the 207 body.
type propfindEntry struct {
	href   string
	obj    objectmodel.Object
	isColl bool
	found  bool // false ⇒ 404 propstat
}

// propfindEntries resolves the requested resource (+children at Depth 1)
// into propfindEntry rows. A missing top-level resource ⇒ 404 via
// davStatus (objectmodel.ErrNoSuchKey shape).
func (f *Frontend) propfindEntries(r *http.Request, res resource, depth1 bool) ([]propfindEntry, error) {
	ctx := r.Context()
	if res.isRoot {
		return f.rootEntries(ctx, depth1)
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
	out := []propfindEntry{f.entryFor(res, obj, kind == kindCollection)}
	if depth1 {
		children, err := f.childEntries(ctx, res)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
	}
	return out, nil
}

// rootEntries serves PROPFIND /: mode A lists buckets (via Buckets()),
// mode B lists the configured bucket's top level.
func (f *Frontend) rootEntries(ctx context.Context, depth1 bool) ([]propfindEntry, error) {
	if f.bucket != "" {
		// Mode B: root IS the configured bucket; list its top level.
		res := resource{bucket: f.bucket, isCollection: true, isRoot: true}
		out := []propfindEntry{{
			href:   "/",
			isColl: true,
			found:  true,
		}}
		if depth1 {
			children, err := f.childEntries(ctx, res)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
		}
		return out, nil
	}
	// Mode A: synthetic root; Depth 1 children are buckets.
	out := []propfindEntry{{href: "/", isColl: true, found: true}}
	if !depth1 {
		return out, nil
	}
	buckets, err := f.be.Buckets(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		child := resource{bucket: b.Name}
		out = append(out, propfindEntry{href: f.davPath(child), isColl: true, found: true})
	}
	return out, nil
}

// entryFor builds one row for a resolved resource.
func (f *Frontend) entryFor(res resource, obj objectmodel.Object, isColl bool) propfindEntry {
	return propfindEntry{
		href:   f.davPath(res),
		obj:    obj,
		isColl: isColl,
		found:  true,
	}
}

// childEntries lists the immediate children of a collection: files from
// ListPage.Objects and one-entry collections from ListPage.CommonPrefixes.
// Mode A bucket-level collections (key == "") list with Prefix "".
func (f *Frontend) childEntries(ctx context.Context, res resource) ([]propfindEntry, error) {
	prefix := res.collectionPrefix()
	var out []propfindEntry
	err := f.eachChild(ctx, res.bucket, prefix, func(obj objectmodel.Object) error {
		child := resource{bucket: res.bucket, key: obj.Key}
		out = append(out, propfindEntry{href: f.davPath(child), obj: obj, found: true})
		return nil
	}, func(cp string) error {
		child := resource{bucket: res.bucket, key: strings.TrimSuffix(cp, "/"), isCollection: true}
		out = append(out, propfindEntry{href: f.davPath(child), isColl: true, found: true})
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
		for _, b := range buckets {
			child := resource{bucket: b.Name}
			out = append(out, propfindEntry{href: f.davPath(child), isColl: true, found: true})
		}
	}
	return out, nil
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
			ps.Props = append(ps.Props, activeProp{XMLName: xml.Name{Space: Namespace, Local: n}})
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
		if want.Space != Namespace && want.Space != "" {
			missing.Props = append(missing.Props, activeProp{XMLName: want})
			continue
		}
		if !known[want.Local] {
			missing.Props = append(missing.Props, activeProp{XMLName: xml.Name{Space: Namespace, Local: want.Local}})
			continue
		}
		entry := liveEntry(e, want.Local)
		ok.Props = append(ok.Props, entry)
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
	props := ObjectProps(e.obj, e.isColl)
	names := make([]string, 0, len(props))
	for _, p := range props {
		names = append(names, p.Name)
	}
	return names
}

// liveEntry renders one named live property for the entry.
func liveEntry(e propfindEntry, name string) activeProp {
	for _, p := range ObjectProps(e.obj, e.isColl) {
		if p.Name != name {
			continue
		}
		switch {
		case p.Collection:
			return activeProp{XMLName: xml.Name{Space: Namespace, Local: p.Name}, Inner: collectionInner}
		default:
			return activeProp{XMLName: xml.Name{Space: Namespace, Local: p.Name}, Value: p.Chardata}
		}
	}
	return activeProp{XMLName: xml.Name{Space: Namespace, Local: name}}
}

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

// writeXMLDocument marshals v with the xml.Header prolog and writes it.
func writeXMLDocument(w http.ResponseWriter, status int, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		writeDavError(w, http.StatusInternalServerError, "")
		return
	}
	full := append([]byte(xml.Header), body...)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", fmtInt(len(full)))
	w.WriteHeader(status)
	_, _ = w.Write(full)
}
