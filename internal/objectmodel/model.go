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
func QuotedETag(etag string) string {
	if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 2 {
		return etag
	}
	return `"` + etag + `"`
}

// ETagsMatch compares two ETags quote-insensitively.
func ETagsMatch(a, b string) bool {
	return NormalizeETag(a) == NormalizeETag(b)
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
