// zfssurface_bridge.go — the PRODUCTION bridge the webdav frontend
// drives for the ZFS enrichment READ surfaces (quic-h3-2026-10 leaf 06),
// following leaf 05's versioning_bridge.go pattern: the response
// builders, provider resolution, and derivation rules stay unexported
// where they exist (capability_endpoints.go); this file re-exports the
// same call sites the s3 capability endpoints use and adds the file
// ?versions JSON form — the SAME derived listing (versionsFromEvents,
// the pinned derivation rules) rendered per-key as JSON, so the wire
// shapes are NEVER duplicated across frontends. Byte-identical parity is
// structural: both protocols run this one code.
package s3

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/metadata"
)

// ObjectVersionsJSON is the file ?versions JSON envelope (webdav GET
// <file>?versions). It carries the SAME envelope fields the s3 derived
// listing (ListObjectVersionsExt) exposes — name, IsLossy, RecordsLost,
// RingSwaps — and the key's entries in the SAME newest-first stream
// order with identical entry shapes (the XML field per JSON field).
// Versions stays nil (JSON null) for a key with no version events —
// never fabricated into an empty array.
type ObjectVersionsJSON struct {
	Name        string                 `json:"name"`
	IsLossy     bool                   `json:"isLossy"`
	RecordsLost uint64                 `json:"recordsLost"`
	RingSwaps   uint64                 `json:"ringSwaps"`
	Versions    []ObjectVersionExtJSON `json:"versions"`
}

// ObjectVersionExtJSON is one derived version entry (or delete marker)
// in the file ?versions JSON form — field-for-field the XML
// ObjectVersionExt shape (Key/VersionId/IsLatest/LastModified/Size/Op/
// IsDeleteMarker), same values, same derivation rules.
type ObjectVersionExtJSON struct {
	Key            string `json:"key"`
	VersionId      string `json:"versionId"` // "txg-<txg>-<index>"; response-scoped, not durable
	IsLatest       bool   `json:"isLatest"`
	LastModified   string `json:"lastModified"` // empty: upstream wire has no time field yet
	Size           int64  `json:"size"`
	Op             string `json:"op"` // originating event op: create|truncate|rename|remove
	IsDeleteMarker bool   `json:"isDeleteMarker"`
}

// HandleBucketEventsForBucket is the webdav entry for GET
// <collection>?events and ?events&versions (the combined query resolves
// to the events extension, same dispatch precedence as the s3 bucket
// level — the events extension wins over the plain versions check). It
// runs the same pipeline as the s3 GET /{bucket}?events handler:
// bucket validation (404), provider resolution (the contracted 503
// NotImplemented when none), the JSON envelope (application/json) or
// the derived XML version listing — byte-identical to the s3 wire.
//
// bucketPath comes from the caller (the webdav frontend's own
// bucket-path resolver — production wires the SAME getBucketPath value
// the s3 frontend uses; no second config view exists).
func HandleBucketEventsForBucket(w http.ResponseWriter, r *http.Request, bucketName, bucketPath string) {
	// resolveEventsContext's validation, against the caller-resolved
	// provider root (same precedence: 404 over provider resolution).
	if !validBucket(bucketName) || !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for ?events", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}
	p := metadataProviderFor(bucketPath)
	if p == nil {
		writeNoProviderError(w)
		return
	}
	q := metadata.HistoryQuery{MaxEvents: maxEventsFromQuery(r)}
	events, err := p.History(r.Context(), bucketPath, "", q)
	if err != nil {
		// NO provider/exec detail reaches the client — server log only.
		log.Printf("metadata: events for %s: %v", strconv.Quote(bucketName), err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		writeS3Error(w, "InternalError", "unable to read event history", http.StatusInternalServerError)
		return
	}
	detail := historyDetailFor(p)
	if _, ok := r.URL.Query()["versions"]; ok {
		ext := versionsFromEvents(events, detail, r.URL.Query().Get("prefix"))
		ext.Name = bucketName // S3 Name convention: the bucket name
		writeXML(w, http.StatusOK, ext)
		return
	}
	writeEventsJSON(w, ObjectEventHistory{
		Dataset:     detail.Dataset,
		RecordsLost: detail.RecordsLost,
		RingSwaps:   detail.RingSwaps,
		Events:      toEventJSON(events, eventsOwnerLookup(bucketName)),
	})
}

// HandleObjectEventsForBucket is the webdav entry for GET
// <file>?events: the key-scoped JSON ObjectEventHistory, byte-identical
// to the s3 GET /{bucket}/{key}?events response for the same key.
func HandleObjectEventsForBucket(w http.ResponseWriter, r *http.Request, bucketName, objectName, bucketPath string) {
	if !validBucket(bucketName) || !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for ?events", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}
	p := metadataProviderFor(bucketPath)
	if p == nil {
		writeNoProviderError(w)
		return
	}
	q := metadata.HistoryQuery{MaxEvents: maxEventsFromQuery(r)}
	events, err := p.History(r.Context(), bucketPath, objectName, q)
	if err != nil {
		log.Printf("metadata: events for %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		writeS3Error(w, "InternalError", "unable to read event history", http.StatusInternalServerError)
		return
	}
	detail := historyDetailFor(p)
	writeEventsJSON(w, ObjectEventHistory{
		Dataset:     detail.Dataset,
		RecordsLost: detail.RecordsLost,
		RingSwaps:   detail.RingSwaps,
		Events:      toEventJSON(events, eventsOwnerLookup(bucketName)),
	})
}

// HandleObjectVersionsForBucket is the webdav entry for GET
// <file>?versions: the file's version listing as JSON — the SAME data
// the s3 ?events&versions derived listing carries for the key (identical
// entry shapes, newest-first stream order, honest lossy envelope),
// derived by the SAME versionsFromEvents rules (one key filter = the
// prefix filter with the exact key). Provider resolution identical to
// the events surface: the contracted 503 NotImplemented when none is
// attached, 404 for an unknown bucket.
func HandleObjectVersionsForBucket(w http.ResponseWriter, r *http.Request, bucketName, objectName, bucketPath string) {
	if !validBucket(bucketName) || !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for ?versions", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}
	p := metadataProviderFor(bucketPath)
	if p == nil {
		writeNoProviderError(w)
		return
	}
	q := metadata.HistoryQuery{MaxEvents: maxEventsFromQuery(r)}
	events, err := p.History(r.Context(), bucketPath, "", q)
	if err != nil {
		log.Printf("metadata: versions for %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		writeS3Error(w, "InternalError", "unable to read event history", http.StatusInternalServerError)
		return
	}
	detail := historyDetailFor(p)
	// The key filter IS the prefix filter with the exact key: identical
	// derivation, identical VersionId indices (index = position in THIS
	// event slice), identical IsLatest/lossy semantics.
	ext := versionsFromEvents(events, detail, objectName)
	out := ObjectVersionsJSON{
		Name:        bucketName,
		IsLossy:     ext.IsLossy,
		RecordsLost: ext.RecordsLost,
		RingSwaps:   ext.RingSwaps,
	}
	for _, v := range ext.Version {
		if v.Key != objectName {
			continue // prefix match is necessary, not sufficient (e.g. "doc" matches "doc.txt")
		}
		out.Versions = append(out.Versions, ObjectVersionExtJSON(v))
	}
	data, err := json.Marshal(out)
	if err != nil {
		log.Printf("Error marshalling %T to JSON: %v", out, err)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
