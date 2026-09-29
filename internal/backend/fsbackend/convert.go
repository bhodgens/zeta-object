// Package fsbackend — convert.go: the frozen sidecar struct and the
// legacy↔neutral conversions. The legacyMeta JSON keys ARE the on-disk
// format (identical to package main's types.go ObjectMetadata tags) — do
// not rename them.
package fsbackend

import (
	"strings"
	"time"

	"mini-s3/internal/objectmodel"
)

// legacyMeta mirrors package main's ObjectMetadata (types.go) WITHOUT
// importing it. Its JSON tags are the frozen on-disk sidecar format.
type legacyMeta struct {
	ContentType    string            `json:"contentType"`
	ContentLength  int64             `json:"contentLength"`
	ETag           string            `json:"eTag"`
	CustomMetadata map[string]string `json:"customMetadata"` // x-amz-meta- prefixed keys (legacy on-disk form)
	LastModified   time.Time         `json:"lastModified"`
	StoragePath    string            `json:"storagePath"` // actual path to the object data on disk
}

// prefixedMetadata converts user-metadata keys into the legacy on-disk
// form. Keys already carrying the x-amz-meta- prefix pass through VERBATIM
// (original casing preserved — the pre-seam handlers stored the raw
// request-header name); canonical (prefix-less) keys get the lower-case
// prefix re-added (the neutral-model form used by non-S3 frontends).
// Never returns nil: the pre-seam handlers always wrote a (possibly empty)
// customMetadata object.
func prefixedMetadata(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			out[k] = v
			continue
		}
		out[objectmodel.MetadataHeaderName(k)] = v
	}
	return out
}

// canonicalMetadata converts legacy prefixed keys into canonical
// (lower-case, prefix-stripped) form for the neutral model.
func canonicalMetadata(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[objectmodel.NormalizeMetadataKey(k)] = v
	}
	return out
}

// objectFromLegacy converts a sidecar value into the neutral model. size is
// the caller-resolved ACTUAL data size (never the sidecar's possibly-stale
// contentLength — pre-seam leaf-2.4 fix 5 serves truth, not the sidecar).
// ETag passes through in the bare (unquoted) form the sidecar stores.
// StoragePath never surfaces on the neutral model.
func objectFromLegacy(key string, m legacyMeta, size int64) objectmodel.Object {
	return objectmodel.Object{
		Key:          key,
		Size:         size,
		ETag:         m.ETag,
		LastModified: m.LastModified,
		ContentType:  m.ContentType,
		Metadata:     canonicalMetadata(m.CustomMetadata),
	}
}
