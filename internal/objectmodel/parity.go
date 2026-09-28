package objectmodel

import (
	"fmt"
	"strings"
)

// HeaderSnapshot is a protocol-neutral capture of the canonical metadata
// surface of one response, comparable with == after normalization.
type HeaderSnapshot struct {
	ContentType   string
	ContentLength string
	ETag          string
	LastModified  string
	UserMetadata  map[string]string // canonical keys (lower-case, no prefix)
}

// SnapshotHeaders builds a HeaderSnapshot via a case-insensitive header
// getter. metaKeys enumerates the raw user-metadata header names to capture
// (e.g. collected by ranging over a recorded response's Header map) — a
// getter cannot enumerate names on its own.
func SnapshotHeaders(get func(string) string, metaKeys []string) HeaderSnapshot {
	meta := ParseMetadataHeaders(metaKeys, valuesFor(get, metaKeys))
	return HeaderSnapshot{
		ContentType:   get("Content-Type"),
		ContentLength: get("Content-Length"),
		ETag:          get("ETag"),
		LastModified:  get("Last-Modified"),
		UserMetadata:  meta,
	}
}

func valuesFor(get func(string) string, keys []string) []string {
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = get(k)
	}
	return vals
}

// AssertHeaderParity compares two snapshots field-by-field across exactly
// the canonical surface (Content-Type, Content-Length, ETag, Last-Modified,
// x-amz-meta-*). Returns an empty string on parity, otherwise a
// "\n"-joined description of every difference. This is the drift gate for
// sibling trees: same requests against FS-backed vs alternate-backend
// buckets must yield equal snapshots.
func AssertHeaderParity(a, b HeaderSnapshot) string {
	var diffs []string
	if a.ContentType != b.ContentType {
		diffs = append(diffs, fmt.Sprintf("Content-Type: %q vs %q", a.ContentType, b.ContentType))
	}
	if a.ContentLength != b.ContentLength {
		diffs = append(diffs, fmt.Sprintf("Content-Length: %q vs %q", a.ContentLength, b.ContentLength))
	}
	if a.ETag != b.ETag {
		diffs = append(diffs, fmt.Sprintf("ETag: %q vs %q", a.ETag, b.ETag))
	}
	if a.LastModified != b.LastModified {
		diffs = append(diffs, fmt.Sprintf("Last-Modified: %q vs %q", a.LastModified, b.LastModified))
	}
	for k, v := range a.UserMetadata {
		if b.UserMetadata[k] != v {
			diffs = append(diffs, fmt.Sprintf("x-amz-meta-%s: %q vs %q", k, v, b.UserMetadata[k]))
		}
	}
	for k, v := range b.UserMetadata {
		if _, ok := a.UserMetadata[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("x-amz-meta-%s: absent vs %q", k, v))
		}
	}
	return strings.Join(diffs, "\n")
}
