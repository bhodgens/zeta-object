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

// OCNamespace is the ownCloud namespace for the discovery properties the
// real desktop client's DiscoverySingleDirectoryJob requires (issue #5).
const OCNamespace = "http://owncloud.org/ns"

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

// CollectionInner is the <d:collection/> child of resourcetype for
// collections (literal prefixed name — the root declares xmlns:d; the
// ownCloud client's parser matches prefixed qnames literally).
var collectionInner = []innerElement{{XMLName: xml.Name{Local: "d:collection"}}}

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
	// OC marks entries in the ownCloud namespace (oc:fileid, oc:permissions,
	// oc:size) — rendered with xmlns="http://owncloud.org/ns".
	OC bool
}

// ObjectProps maps a resource onto the five Contract-5
// properties PLUS the ownCloud-namespace discovery properties (issue #5):
// oc:fileid, oc:permissions on every resource, and oc:size on collections
// (files already carry getcontentlength). Collections report
// httpd/unix-directory, a collection resourcetype, and NO
// getcontentlength; files report size, quoted strong ETag, content
// type (default application/octet-stream), and an empty resourcetype.
// Zero-byte objects render getcontentlength = 0.
//
// etag is the resource's ETag value: the stored object ETag for files, the
// derived immediate-children token ("dir-<hex>", colltoken.go) for
// collections with a usable derivation, or "" when neither exists (the
// historical quoted-empty rendering).
//
// The permission strings are a pinned, documented SIMPLIFICATION of the real
// ownCloud grammar (docs/owncloud-compatibility.md): readwrite file = "RW",
// readwrite collection = "RDNVCK", readonly file = "R", readonly collection
// = "RG". The server's grant model has only read/write, so the richer
// letters (N/C/K/G/D per-resource) are not representable.
//
// oc:fileid is DERIVED at request time from a stable hash of bucket+key
// (FNV-1a, top bit masked to uint63). It is NEVER persisted: no sidecar
// field, no database, no server-owned index (project charter) — the same
// bucket+key hashes to the same fileid across requests and restarts, and
// the id has no meaning outside this hash.
func ObjectProps(o objectmodel.Object, isCollection bool, bucket string, write bool, ocSize int64, etag string) []PropEntry {
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
		)
	}
	// getetag on EVERY resource (real OC10 collections return one; the
	// ownCloud 6.x discovery job treats its absence as an invalid reply).
	// For collections the value is the DERIVED change token (colltoken.go),
	// not a stored object ETag.
	props = append(props,
		PropEntry{Name: "getetag", Present: true, Chardata: objectmodel.QuotedETag(etag)},
	)
	// ownCloud namespace (issue #5): the real client's discovery job
	// requires fileid + permissions on EVERY resource, and size on
	// collections.
	fileid := OCFileID(bucket, o.Key)
	perm := ocPermissions(isCollection, write)
	props = append(props,
		PropEntry{Name: "fileid", Present: true, Chardata: fileid, OC: true},
		PropEntry{Name: "permissions", Present: true, Chardata: perm, OC: true},
	)
	if isCollection {
		props = append(props, PropEntry{Name: "size", Present: true, Chardata: formatInt(ocSize), OC: true})
	}
	return props
}

// OCFileID derives the oc:fileid for a resource: the FNV-1a hash of
// bucket + "\x00" + key, masked to uint63 (the real client treats it as a
// numeric id). Pure and stateless — the value is computed fresh on every
// PROPFIND and never stored (charter: derived, not persisted). Hash
// collisions are accepted; the id is a discovery hint, not a lookup key.
func OCFileID(bucket, key string) string {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for _, b := range []byte(bucket + "\x00" + key) {
		h ^= uint64(b)
		h *= prime64
	}
	return formatInt(int64(h &^ (1 << 63))) //nolint:gosec // G115: the mask clears the sign bit, so the value fits int64 by construction.
}

// ocPermissions pins the ownCloud permission string for the resource under
// the simplified grammar: R=read, W=write(file) / N=mkdir+move+rename
// (collection dir), D=delete file (collection dir), C=create file
// (collection), K=delete (collection), V/G=version letters we do not emit.
// The grant model is read/write only, so the mapping is:
//
//	readwrite  file = "RW"        collection = "RDNVCK"
//	readonly   file = "R"         collection = "RG"
func ocPermissions(isCollection, write bool) string {
	if write {
		if isCollection {
			return "RDNVCK"
		}
		return "RW"
	}
	if isCollection {
		return "RG"
	}
	return "R"
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
