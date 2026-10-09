package transport

// msparse.go - the 207 Multi-Status parser (leaf 06). The gateway's
// form (pinned by server e2e cases 19/25, internal/frontend/webdav/
// propfind.go) is the ownCloud-compat shape: LITERAL d:/oc: prefixed
// element names with the namespace bindings declared on the ROOT
// element (xmlns:d="DAV:" xmlns:oc="..."). encoding/xml resolves those
// prefixes through the xmlns declarations, so matching fields with
// namespace-qualified tags (`xml:"DAV: getetag"`) decodes it; the SAME
// tags also decode the RFC default-namespace form (xmlns="DAV:",
// unprefixed elements) for robustness against other webdav servers.
// Only the gateway's form is asserted in tests.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// repairRootCloser fixes the gateway's one literal tag quirk: an
// unprefixed root opener <multistatus ...> paired with the prefixed
// closer </d:multistatus> (fixupPrefixedClosers rewrites every
// "multistatus" closer, including the root's). When the opener is
// unprefixed, the closer is unprefixed to match; a prefixed root opener
// is left alone. Anything else passes through unchanged.
func repairRootCloser(body []byte) []byte {
	if !bytes.Contains(body, []byte("<multistatus")) {
		return body // already prefixed (or not a 207): nothing to do
	}
	return bytes.Replace(body, []byte("</d:multistatus>"), []byte("</multistatus>"), 1)
}

// msRow is one flattened <d:response> row.
type msRow struct {
	href    string
	isDir   bool
	size    int64
	etag    string
	modTime time.Time
}

// msProps captures the DAV: properties the client consumes.
type msProps struct {
	ContentLength string `xml:"DAV: getcontentlength"`
	ETag          string `xml:"DAV: getetag"`
	LastModified  string `xml:"DAV: getlastmodified"`
	ResourceType  struct {
		// <d:collection/> (any binding) marks a directory row.
		Collection []xml.Name `xml:"collection"`
	} `xml:"DAV: resourcetype"`
}

// msPSRaw is one propstat block. The gateway emits the live props as
// DIRECT children of propstat (its serializer renders each activeProp
// under its own name - no <d:prop> wrapper, pinned by the wire tests);
// the RFC-wrapped <DAV: prop> container is decoded too (other servers).
type msPSRaw struct {
	Props  msProps `xml:"DAV: prop"`
	Status string  `xml:"DAV: status"`
	// Gateway direct-form: same props, one level up.
	// RawPropsFallback mirrors Props for the gateway's UNWRAPPED form:
	// its serializer renders each prop directly under propstat (no
	// <d:prop> container), and a field tagged DAV: on the propstat
	// itself cannot express that - decode via the raw inner bytes in
	// decodeProps (below) instead.
	RawPropsFallback []byte `xml:",innerxml"`
}

// msRespRaw is one <d:response>: href + propstat blocks.
type msRespRaw struct {
	Href     string    `xml:"DAV: href"`
	Propstat []msPSRaw `xml:"DAV: propstat"`
}

// msRaw is the 207 document root. The gateway's root element is
// UNPREFIXED (<multistatus> with xmlns:d/xmlns:oc declared on it) while
// every child carries the literal d:/oc: prefixes (propfind.go's
// multistatus struct has an empty XMLName); match the root by local name
// only. Nested elements keep the DAV:-qualified tags, which decode both
// the prefixed (gateway) and default-namespace (RFC) forms.
type msRaw struct {
	Responses []msRespRaw `xml:"DAV: response"`
}

// parseMultistatus decodes a 207 body into rows. Only 2xx propstat
// blocks contribute (a 404 propstat names ABSENT props); unknown
// properties and namespaces are skipped. A structurally invalid
// document is an error (the caller wraps it with the key).
//
// The gateway's pinned wire form has ONE literal tag mismatch: an
// unprefixed root <multistatus> closed by </d:multistatus> (its
// fixupPrefixedClosers rewrites "multistatus" closers unconditionally
// while the root opener is emitted unprefixed). The ownCloud client's
// prefix-literal parser tolerates it; encoding/xml does not - the
// repairNormalizesRootCloser pass below fixes exactly that pairing
// before the strict parse.
func parseMultistatus(body []byte) ([]msRow, error) {
	var raw msRaw
	if err := xml.Unmarshal(repairRootCloser(body), &raw); err != nil {
		return nil, fmt.Errorf("parsing 207 multistatus: %w", err)
	}
	rows := make([]msRow, 0, len(raw.Responses))
	for _, rr := range raw.Responses {
		row := msRow{href: strings.TrimSpace(rr.Href)}
		for _, ps := range rr.Propstat {
			if !strings.Contains(ps.Status, " 2") {
				continue // 404 propstat: absent props, no values
			}
			// The gateway renders props as DIRECT propstat children
			// (no <d:prop> wrapper - see the struct comment): when the
			// wrapped form decoded empty, re-decode the inner bytes as
			// if they were the prop container.
			if ps.Props.ETag == "" && ps.Props.ContentLength == "" && len(ps.RawPropsFallback) > 0 {
				// The fragment carries literal d: prefixes with no
				// xmlns declaration of its own - re-home it inside a
				// root that declares the binding before decoding.
				wrapped := append([]byte("<root xmlns:d=\"DAV:\" xmlns:oc=\"http://owncloud.org/ns\">"), ps.RawPropsFallback...)
				wrapped = append(wrapped, []byte("</root>")...)
				var alt msProps
				if err := xml.Unmarshal(wrapped, &alt); err == nil {
					ps.Props = alt
				}
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.Props.ContentLength), 10, 64); err == nil {
				row.size = n
			}
			if row.etag == "" {
				row.etag = normalizeETag(ps.Props.ETag)
			}
			if row.modTime.IsZero() {
				row.modTime = parseHTTPTime(ps.Props.LastModified)
			}
			if len(ps.Props.ResourceType.Collection) > 0 {
				row.isDir = true
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
