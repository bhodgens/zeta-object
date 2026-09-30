// props.go — the objectmodel.Object → WebDAV property mapping (master
// Contract 5), ONE exported, side-effect-free function so the docs and the
// future ownCloud tree can reference it.
package webdav

import (
	"encoding/xml"
	"net/http"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// Namespace is the DAV: namespace prefix used in every WebDAV document.
const Namespace = "DAV:"

// activeProp is one live property entry for XML marshaling. Value renders
// inside the element; an omitted value renders the empty element.
type activeProp struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
	Inner   []innerElement
}

// innerElement renders a nested element (resourcetype's <D:collection/>).
type innerElement struct {
	XMLName xml.Name
}

// CollectionInner is the <D:collection/> child of resourcetype for
// collections.
var collectionInner = []innerElement{{XMLName: xml.Name{Space: Namespace, Local: "collection"}}}

// PropEntry is one property for objectProps: name plus whether it is
// present, plus its rendering pieces. Kept exported and side-effect-free
// per the master's ownCloud-reuse note.
type PropEntry struct {
	// Name is the DAV property local name (getetag, getcontentlength, ...).
	Name string
	// Present reports whether the property applies to the resource.
	Present bool
	// Chardata is the text value for simple properties.
	Chardata string
	// Collection marks resourcetype entries for collections.
	Collection bool
}

// ObjectProps maps an object (or collection) onto the five Contract-5
// properties. Collections report httpd/unix-directory, a collection
// resourcetype, and NO getcontentlength/getetag; files report size, quoted
// strong ETag, content type (default application/octet-stream), and an
// empty resourcetype. Zero-byte objects render getcontentlength = 0.
func ObjectProps(o objectmodel.Object, isCollection bool) []PropEntry {
	ct := o.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	if isCollection {
		ct = "httpd/unix-directory"
	}
	lastMod := o.LastModified.UTC().Format(http.TimeFormat)
	if o.LastModified.IsZero() {
		lastMod = time.Unix(0, 0).UTC().Format(http.TimeFormat)
	}
	props := []PropEntry{
		{Name: "resourcetype", Present: true, Collection: isCollection},
		{Name: "getcontenttype", Present: true, Chardata: ct},
		{Name: "getlastmodified", Present: true, Chardata: lastMod},
	}
	if !isCollection {
		props = append(props,
			PropEntry{Name: "getcontentlength", Present: true, Chardata: formatInt(o.Size)},
			PropEntry{Name: "getetag", Present: true, Chardata: objectmodel.QuotedETag(o.ETag)},
		)
	}
	return props
}

// formatInt renders an int64 in decimal.
func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
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
