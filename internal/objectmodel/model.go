// Package objectmodel defines the backend-neutral object model shared by all
// mini-s3 Backend implementations, Frontend protocol plugins, and
// MetadataProviders. It carries no HTTP, XML, or storage dependencies.
package objectmodel

import (
	"strings"
	"time"
)

// PINNED — field names, types, and order must match exactly.

type Object struct {
	Key          string
	Size         int64
	ETag         string // opaque strong validator, S3 quoted form
	LastModified time.Time
	ContentType  string
	Metadata     map[string]string // user metadata; lower-case keys, NO x-amz-meta- prefix
}

type BucketInfo struct {
	Name      string
	CreatedAt time.Time
}

type ListPage struct {
	Objects        []Object
	CommonPrefixes []string
	IsTruncated    bool
	NextToken      string
}

type ListParams struct {
	Prefix            string
	Delimiter         string
	StartAfter        string
	ContinuationToken string
	MaxKeys           int
}

type GetOptions struct {
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   time.Time
	IfUnmodifiedSince time.Time
	Range             string
}

type PutOptions struct {
	ContentType string
	Metadata    map[string]string
	IfMatch     string
	IfNoneMatch string
	// Principal is the authenticated principal (AccessKeyID) performing
	// the write. Empty = unattributed write (backends skip breadcrumb
	// stamping). Advisory per the design (zfs-principal-metadata.md
	// section 3): backends that cannot persist it ignore it; fsbackend
	// stamps best-effort user.zeta.* xattrs with it.
	Principal string
}

type CapabilitySet struct {
	Multipart         bool
	Versioning        bool
	Immutable         bool
	MetadataProviders []string
}

// NormalizeETag strips a surrounding pair of double quotes, returning the
// bare validator form. Input without quotes is returned unchanged.
func NormalizeETag(etag string) string {
	if len(etag) >= 2 && strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) {
		return etag[1 : len(etag)-1]
	}
	return etag
}

// QuotedETag returns the S3 quoted wire form, adding quotes if absent.
// Malformed-input behavior (pinned, do not "fix" silently): the already-
// quoted check is a naive prefix/suffix test, so a value like `"abc` (open
// quote, no close) does NOT count as quoted and gets wrapped a second time
// (`"\"abc"`), and an empty input returns `""` (two quote characters).
// Callers pass validator strings this package produced (NormalizeETag /
// storage round-trip); arbitrary header input should be normalized through
// NormalizeETag first.
func QuotedETag(etag string) string {
	if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 2 {
		return etag
	}
	return `"` + etag + `"`
}

// ETagsMatch compares two ETags quote-insensitively and
// weak-validator-insensitively: a leading W/ prefix is stripped from both
// sides before comparison, matching the S3 frontend's etagMatches — so
// W/"abc" matches "abc" and W/"abc" matches W/"abc". Strong-vs-weak
// semantics (RFC 7232 §2.3: weak validators must not be used for
// If-(None-)Match range preconditions) are deliberately NOT enforced here;
// this is a surface-parity/matching helper, not a conditional-request
// evaluator.
func ETagsMatch(a, b string) bool {
	return NormalizeETag(stripWeakPrefix(a)) == NormalizeETag(stripWeakPrefix(b))
}

// stripWeakPrefix removes a leading RFC 7232 weak-validator prefix "W/"
// (case-sensitive per the RFC ABNF, which pins the octets W and /).
func stripWeakPrefix(etag string) string {
	if strings.HasPrefix(etag, "W/") {
		return etag[2:]
	}
	return etag
}

// metaHeaderPrefix is the S3 user-metadata header prefix, lower-case.
const metaHeaderPrefix = "x-amz-meta-"

// NormalizeMetadataKey lower-cases the key and strips a leading
// x-amz-meta- prefix (case-insensitive), yielding the canonical in-memory
// form used by Object.Metadata.
func NormalizeMetadataKey(k string) string {
	lowered := strings.ToLower(k)
	return strings.TrimPrefix(lowered, metaHeaderPrefix)
}

// MetadataHeaderName returns the wire header name for a canonical key.
func MetadataHeaderName(key string) string {
	return metaHeaderPrefix + strings.ToLower(key)
}
