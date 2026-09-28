package objectmodel

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseMetadataHeaders normalizes raw header names into canonical
// user-metadata keys (lower-case, x-amz-meta- prefix stripped). Parallel
// slices: rawValues[i] holds the value(s) for rawKeys[i]; multiple values
// are joined with ", " to match existing handler behavior.
func ParseMetadataHeaders(rawKeys, rawValues []string) map[string]string {
	meta := make(map[string]string)
	for i, rawKey := range rawKeys {
		key := NormalizeMetadataKey(rawKey)
		if key == "" {
			continue
		}
		if i >= len(rawValues) {
			meta[key] = ""
			continue
		}
		val := rawValues[i]
		// Surplus values (more values than keys) belong to the last key:
		// a multi-valued header contributes all of its values, joined with
		// ", " to match existing handler behavior.
		if i == len(rawKeys)-1 && len(rawValues) > len(rawKeys) {
			val = strings.Join(append([]string{val}, rawValues[len(rawKeys):]...), ", ")
		}
		meta[key] = val
	}
	return meta
}

// headerLookup tries each key variant in turn and returns the first
// non-empty value. Callers pass header getters that may be case-sensitive
// (e.g. plain map lookups in tests); real http.Header.Get is
// case-insensitive and matches on the first variant.
func headerLookup(get func(string) string, key string) string {
	for _, variant := range []string{key, strings.ToLower(key), http.CanonicalHeaderKey(key)} {
		if v := get(variant); v != "" {
			return v
		}
	}
	return ""
}

// ObjectFromHeaders builds an Object's metadata-bearing fields from request
// headers. get is a header lookup (e.g. r.Header.Get); rawMetaKeys enumerates
// raw header names carrying user metadata (e.g. collected by ranging over
// r.Header). User-metadata keys are normalized: lower-case, x-amz-meta-
// prefix stripped. Size and LastModified are caller-supplied (body length
// and server clock are not header concerns).
func ObjectFromHeaders(key string, size int64, lastModified time.Time,
	get func(string) string, rawMetaKeys []string) Object {
	rawValues := make([]string, len(rawMetaKeys))
	for i, k := range rawMetaKeys {
		rawValues[i] = headerLookup(get, k)
	}
	return Object{
		Key:          key,
		Size:         size,
		LastModified: lastModified,
		ContentType:  headerLookup(get, "Content-Type"),
		Metadata:     ParseMetadataHeaders(rawMetaKeys, rawValues),
	}
}

// ObjectToMetadataHeaders renders the canonical S3 metadata surface:
// Content-Type, Content-Length, ETag (quoted), Last-Modified
// (http.TimeFormat), and x-amz-meta-* per Metadata entry (prefix added,
// key lower-cased).
func ObjectToMetadataHeaders(o Object) map[string][]string {
	h := map[string][]string{
		"Content-Type":   {o.ContentType},
		"Content-Length": {strconv.FormatInt(o.Size, 10)},
		"ETag":           {QuotedETag(o.ETag)},
		"Last-Modified":  {o.LastModified.UTC().Format(http.TimeFormat)},
	}
	for k, v := range o.Metadata {
		h[MetadataHeaderName(k)] = []string{v}
	}
	return h
}

// LegacyObjectMetadata mirrors package main's ObjectMetadata (types.go:48)
// without importing it (that would create a main-dependency cycle).
type LegacyObjectMetadata struct {
	ContentType    string
	ContentLength  int64
	ETag           string
	CustomMetadata map[string]string // keys may carry the x-amz-meta- prefix (legacy on-disk form)
	LastModified   time.Time
	StoragePath    string // filesystem-internal; NEVER surfaced on the neutral model
}

// FromLegacy converts a legacy metadata value into the neutral model.
// Mapping: contentType↔ContentType, contentLength↔Size, eTag↔ETag,
// customMetadata↔Metadata, lastModified↔LastModified. StoragePath is
// filesystem-internal and is dropped. ETag passes through unchanged: legacy
// data carries the bare form.
func FromLegacy(key string, m LegacyObjectMetadata) Object {
	meta := make(map[string]string, len(m.CustomMetadata))
	for k, v := range m.CustomMetadata {
		meta[NormalizeMetadataKey(k)] = v
	}
	return Object{
		Key:          key,
		Size:         m.ContentLength,
		ETag:         m.ETag,
		LastModified: m.LastModified,
		ContentType:  m.ContentType,
		Metadata:     meta,
	}
}

// ToLegacy reconstructs the legacy value. ETag keeps whatever form the
// Object carries (callers quote for the wire). CustomMetadata keys get the
// x-amz-meta- prefix re-added (legacy on-disk form). StoragePath is left
// zero: the backend adapter sets it.
func ToLegacy(o Object) LegacyObjectMetadata {
	custom := make(map[string]string, len(o.Metadata))
	for k, v := range o.Metadata {
		custom[MetadataHeaderName(k)] = v
	}
	return LegacyObjectMetadata{
		ContentType:    o.ContentType,
		ContentLength:  o.Size,
		ETag:           o.ETag,
		CustomMetadata: custom,
		LastModified:   o.LastModified,
	}
}
