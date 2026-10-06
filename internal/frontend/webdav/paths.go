// paths.go — the single URL→resource mapping function (webdav-2026-09
// master Contract 3). Every method resolves the request path through
// parseResource; there is no second parser. Pure: no I/O.
package webdav

import (
	"strings"
)

// resource is a parsed WebDAV URL. isCollection is the trailing-slash flag;
// bucket/key carry the storage coordinates (key empty for the root
// collection and for bucket-level collections).
type resource struct {
	bucket       string // mode B: always the configured bucket
	key          string // object key without trailing slash; "" for root/bucket level
	isCollection bool   // trailing slash on the request path
	isRoot       bool   // "/" in either mode
}

// parseResource maps a request path onto (bucket, key, collection) under
// the mode rule:
//
//	mode A (multi-bucket): first segment = bucket; "/" = synthetic root.
//	mode B (single-bucket): everything lives under the configured bucket;
//	  "/" = the configured bucket's root collection.
//
// Segment normalization: duplicate slashes collapse ("//" == "/"); empty
// segments are dropped. Percent-encoding stays as Go's r.URL.Path decoded
// it — the caller passes r.URL.Path (already decoded once) and the href
// renderer re-encodes for output.
//
// Dot-segment safety (W1): the second return value is false when the
// resource would escape the dataDir through the fs backend's unchecked
// root+bucket join — mode A bucket "." or "..", and in BOTH modes a key
// that is "." or ".." or contains a ".." segment. Names that merely
// contain dots ("a..b", "v1.2") stay legal. Callers render 403.
//
// Reserved control segments: keySafe also rejects a ".metadata" or ".zfs"
// key segment (bughunt L1), so a webdav URL can never name the bucket's
// own sidecar area or the ZFS control directory — the same rule the s3 and
// fsbackend key validators apply. These take the SAME 403 rejection path.
func (f *Frontend) parseResource(urlPath string) (resource, bool) {
	cleaned := urlPath
	for strings.Contains(cleaned, "//") {
		cleaned = strings.ReplaceAll(cleaned, "//", "/")
	}
	trimmed := strings.TrimPrefix(cleaned, "/")
	// Trailing slash (before trimming) marks a collection; "/" alone is
	// the root collection in both modes.
	isCollection := strings.HasSuffix(cleaned, "/") || trimmed == ""
	if f.bucket != "" {
		// Mode B: every resource is under the configured bucket.
		if trimmed == "" {
			return resource{bucket: f.bucket, isCollection: true, isRoot: true}, true
		}
		// Strip the trailing slash from the KEY (see mode A comment).
		key := strings.TrimSuffix(trimmed, "/")
		if !keySafe(key) {
			return resource{}, false
		}
		return resource{bucket: f.bucket, key: key, isCollection: isCollection}, true
	}
	// Mode A.
	if trimmed == "" {
		return resource{isCollection: true, isRoot: true}, true
	}
	// Strip the trailing slash from the KEY (the isCollection flag keeps
	// the client's trailing-slash intent): "/photos/2024/" must resolve to
	// prefix key "2024", not "2024/".
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return resource{isCollection: true, isRoot: true}, true
	}
	bucket, key, _ := strings.Cut(trimmed, "/")
	// W1: bucket "." or ".." joins one directory above dataDir in the fs
	// backend; reject before any backend call.
	if bucket == "." || bucket == ".." || !keySafe(key) {
		return resource{}, false
	}
	return resource{bucket: bucket, key: key, isCollection: isCollection}, true
}

// keySafe rejects keys that are, or contain a segment equal to, ".", "..",
// ".metadata" or ".zfs".
//
// The first two are the traversal defense: the fs backend joins
// root+bucket+key with no path cleaning, so a surviving dot-dot segment
// would read or write outside dataDir.
//
// The last two are the RESERVED control segments, and rejecting them is
// what makes this validator AGREE with its two twins (bughunt L1):
// internal/frontend/s3 validateObjectKey and internal/backend/fsbackend
// validateKey both reject ".metadata" and ".zfs". ".metadata" is the
// bucket's own sidecar/locks/versions area — a key segment of that name
// writes into the gateway's control directory. ".zfs" is the ZFS control
// directory present at every dataset mountpoint, so on a dataset-backed
// bucket a key segment of that name collides with it (PUT writes into it,
// DELETE removes it).
//
// The disagreement was invisible in the suite because webdav's batch
// surface (batch.go) deliberately calls the s3 validator for its manifest
// keys: POST ?batch refused ".metadata/x" while a plain PUT of the same
// key accepted it. KeySafe is SEGMENT-exact like both twins — names that
// merely contain dots ("a..b", ".hidden", "v1.2", "x.zfs") stay legal.
func keySafe(key string) bool {
	if key == "" {
		return true
	}
	for seg := range strings.SplitSeq(key, "/") {
		switch seg {
		case ".", "..", ".metadata", ".zfs":
			return false
		}
	}
	return true
}

// collectionPrefix is the List prefix for a collection resource: "" at
// bucket level, "<key>/" for a nested prefix collection.
func (r resource) collectionPrefix() string {
	if r.key == "" {
		return ""
	}
	return r.key + "/"
}

// parentPrefix is the prefix of the collection that contains this resource:
// "" when the parent is the bucket itself, "<dir>/" for a nested parent.
// For a file "a/b.txt" the parent prefix is "a/"; for a collection
// "a/b/" the parent prefix is "a/"; for bucket-level resources it is "".
func (r resource) parentPrefix() string {
	if r.key == "" {
		return ""
	}
	idx := strings.LastIndexByte(r.key, '/')
	if idx < 0 {
		return ""
	}
	return r.key[:idx+1]
}

// davPath renders the resource's request path (what a client would GET):
// mode B re-roots everything under the configured bucket so hrefs keep the
// flat namespace clients mounted at "/" see.
func (f *Frontend) davPath(r resource) string {
	if r.isRoot {
		return "/"
	}
	if f.bucket != "" {
		if r.key == "" {
			return "/"
		}
		p := "/" + r.key
		if r.isCollection {
			p += "/"
		}
		return p
	}
	if r.key == "" {
		return "/" + r.bucket + "/"
	}
	p := "/" + r.bucket + "/" + r.key
	if r.isCollection {
		p += "/"
	}
	return p
}
