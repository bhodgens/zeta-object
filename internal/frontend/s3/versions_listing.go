// versions_listing.go — the GET /{bucket}?versions listing surface
// (s3-versioning tree leaf 04, Contract 2).
//
// Two mutually exclusive renderers behind one handler:
//
//   - buckets whose versioning was EVER enabled (the bucket-level state
//     marker <bucket>/.metadata/.versioning exists — it is created by
//     SetState and never removed): a real ListObjectVersions rendered
//     from the version store — for every key, one <Version> entry per
//     non-marker version and one <DeleteMarker> entry per marker,
//     NEWEST-FIRST within each key, keys in lexicographic order.
//   - NEVER-versioned buckets (no marker): the legacy current-only
//     listing is served BYTE-IDENTICAL to pre-versioning behavior —
//     every object is one <Version> entry with the fixed "null"
//     version id. This wire shape is pinned by tests (bucket_versions_
//     test.go, moved_multipart_routing_test.go) and ceph/s3-tests
//     teardown depends on it.
//
// The versioned renderer's envelope reuses the legacy ListVersionsResult
// (Name/Prefix/KeyMarker/VersionIdMarker/MaxKeys/IsTruncated + Version
// entries) and adds DeleteMarker entries interleaved with Versions in
// one newest-first stream, exactly as real S3 renders the document.
package s3

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// readObjectMetaForList reads and unmarshals one object's metadata file for
// listing purposes; returns ok=false when missing/corrupt (skipped entry).
func readObjectMetaForList(metadataDir, key string) (ObjectMetadata, bool) {
	metaPath := filepath.Join(metadataDir, key+".meta")
	metaJSON, err := os.ReadFile(metaPath) //nolint:gosec // G703: key validated by validateObjectKey; no traversal possible.
	if err != nil {
		return ObjectMetadata{}, false
	}
	var meta ObjectMetadata
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return ObjectMetadata{}, false
	}
	return meta, true
}

// nullVersionID is the version ID real S3 reports for objects in
// unversioned buckets.
const nullVersionID = "null"

// listObjectVersionsHandler serves GET /bucket?versions, choosing the
// versioned or legacy renderer by the bucket's ever-versioned marker.
func listObjectVersionsHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucketPath := getBucketPath(bucketName)
	if bucketEverVersioned(bucketPath) {
		listBucketVersionsVersioned(w, r, bucketName, bucketPath)
		return
	}
	listBucketVersionsLegacy(w, r, bucketName)
}

// bucketEverVersioned reports whether the bucket's versioning was ever
// enabled: the bucket-level state marker exists. The marker is written
// by every SetState (Enabled/Suspended/Off-clears) and never removed, so
// its presence is exactly "ever touched by ?versioning" (Contract 2's
// "when versioning was ever enabled"). A missing marker — every
// pre-versioning bucket — is false, routing to the byte-identical
// legacy listing. Bucket existence was already checked by the caller's
// gate (neverVersionedListingMeta); anything else is not-versioned.
func bucketEverVersioned(bucketPath string) bool {
	marker := filepath.Join(bucketPath, ".metadata", ".versioning")
	if _, err := os.Stat(marker); err != nil { //nolint:gosec // G703: bucketPath is the resolved bucket root, never client input.
		return false
	}
	return true
}

// listBucketVersionsVersioned renders the versioned listing from the
// request-resolved version store.
func listBucketVersionsVersioned(w http.ResponseWriter, r *http.Request, bucketName, bucketPath string) {
	listBucketVersionsWithStore(w, r, bucketName, bucketPath, versionStoreForBucket(bucketPath))
}

// listBucketVersionsWithStore renders the versioned listing from the
// given version store (Contract 2 wire shape). The store is a
// parameter (the handler passes versionStoreForBucket; tests script
// snapshot/merge stores — a nil zmetad DB handle cannot satisfy the
// snapshot store's resolver, so handler-level snapshots tests inject
// the store exactly as leaf 03's merge tests do). Keys come from the
// metadata sidecar walk (the same collectObjectKeys the legacy listing
// uses — a key exists if it has a sidecar, marker-hidden included),
// and each key's history renders via store.List: <Version> for
// non-marker entries, <DeleteMarker> for markers, NEWEST FIRST within
// the key; the store's own List already orders newest-first and flags
// IsLatest on the key's newest entry (the merge store preserves that
// flag). Keys render in lexicographic order so the overall document is
// newest-first per key, stable across pages. A version-id-marker
// resumes INSIDE the marker key: entries are skipped up to and
// including the marker id (the id of the last entry the previous page
// emitted), then the key's remaining newer... older entries continue.
// A marker id no longer present in the key's history skips the key
// (the window moved; repeating entries would be worse than advancing).
func listBucketVersionsWithStore(w http.ResponseWriter, r *http.Request, bucketName, bucketPath string, store versionStore) {
	metadataDir := filepath.Join(bucketPath, ".metadata")

	query := r.URL.Query()
	prefix := query.Get("prefix")
	keyMarker := query.Get("key-marker")
	if keyMarker != "" && query.Get("encoding-type") == "url" {
		if decoded, err := url.QueryUnescape(keyMarker); err == nil {
			keyMarker = decoded
		}
	}
	maxKeys := 1000
	if v := query.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxKeys = n
		}
	}

	// Encoding + max-keys=0 first: the envelope goes out before any
	// per-key store work (leaf-2.4 fix 13 parity — max-keys=0 is an
	// empty listing, never truncated, and never needs the store).
	encodeKeys := query.Get("encoding-type") == "url"
	result := ListVersionsResult{
		Name:            bucketName,
		Prefix:          prefix,
		KeyMarker:       keyMarker,
		VersionIDMarker: query.Get("version-id-marker"),
		MaxKeys:         maxKeys,
		IsTruncated:     false,
		Versions:        []ObjectVersion{},
	}
	if encodeKeys {
		result.EncodingType = "url"
	}
	if maxKeys == 0 {
		writeXML(w, http.StatusOK, result)
		return
	}

	keys, err := collectObjectKeys(metadataDir)
	if err != nil {
		log.Printf("Error walking metadata directory %s for ListObjectVersions: %v", metadataDir, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		writeS3Error(w, "InternalError", "Error listing object versions.", http.StatusInternalServerError)
		return
	}
	sort.Strings(keys)

	owner := Owner{ID: "zetaobject-user", DisplayName: "zetaobject-user"}
	versionIDMarker := query.Get("version-id-marker")

	out := verListingCursor{
		result:    &result,
		owner:     owner,
		maxKeys:   maxKeys,
		encode:    encodeKeys,
		truncated: false,
	}
	for _, key := range keys {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		// key-marker ordering: keys at or before the marker are done —
		// EXCEPT the marker key itself when a version-id-marker is
		// present (real S3 resumes INSIDE the marker key, see the
		// mid-key skip in appendKeyVersions).
		if keyMarker != "" && (key < keyMarker || (key == keyMarker && versionIDMarker == "")) {
			continue
		}
		if out.emitted >= maxKeys {
			out.truncated = true
			break
		}

		entries, err := store.List(bucketName, key)
		if err != nil {
			if isNoSuchKeyErr(err) {
				// A sidecar exists but carries no version
				// history (written before versioning was
				// enabled, never overwritten since): the key
				// has no versions to render — skip honestly.
				continue
			}
			log.Printf("Error listing versions of %s in bucket %s: %v", strconv.Quote(key), strconv.Quote(bucketName), err) //nolint:gosec // G706: strconv.Quote-escaped values.
			writeS3Error(w, "InternalError", "Error listing object versions.", http.StatusInternalServerError)
			return
		}

		appendKeyVersions(&out, bucketName, key, keyMarker, versionIDMarker, entries)
		if out.truncated {
			break
		}
	}
	result.IsTruncated = out.truncated
	if out.truncated && out.lastKey != "" {
		result.NextKeyMarker = out.lastKey
		if encodeKeys {
			result.NextKeyMarker = s3URLEncode(out.lastKey)
		}
		result.NextVersionIDMarker = out.lastVersionID
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Successfully served ListObjectVersions for bucket %s (%d versions)", strconv.Quote(bucketName), out.emitted)
}

// verListingCursor carries the versioned listing's accumulation state
// across the per-key render helper (the envelope, the emission budget,
// and the truncation markers).
type verListingCursor struct {
	result        *ListVersionsResult
	owner         Owner
	maxKeys       int
	encode        bool
	emitted       int
	truncated     bool
	lastKey       string
	lastVersionID string
}

// appendKeyVersions renders ONE key's store history into the listing:
// mid-key resume past the version-id-marker when this is the marker
// key, then <Version>/<DeleteMarker> entries newest-first until the
// key's history or the page budget runs out (truncated=true records a
// budget stop). A marker id absent from the key's history skips the
// whole key (the window moved; repeating entries would be worse than
// advancing).
func appendKeyVersions(out *verListingCursor, bucketName, key, keyMarker, versionIDMarker string, entries []VersionEntry) {
	// Mid-key resume: skip up to and including the version-id-marker
	// within the key itself (real S3's version-id-marker ordering
	// inside one key).
	if key == keyMarker && keyMarker != "" && versionIDMarker != "" {
		found := false
		skipped := 0
		for _, entry := range entries {
			skipped++
			if entry.ID == versionIDMarker {
				found = true
				break
			}
		}
		if !found {
			return
		}
		entries = entries[skipped:]
		if len(entries) == 0 {
			return
		}
	}

	keyOut := key
	if out.encode {
		keyOut = s3URLEncode(key)
	}
	for _, entry := range entries {
		if entry.IsDeleteMarker {
			out.result.DeleteMarkers = append(out.result.DeleteMarkers, DeleteMarkerEntry{
				Key:          keyOut,
				VersionID:    entry.ID,
				IsLatest:     entry.IsLatest,
				LastModified: s3Timestamp(entry.LastModified),
				Owner:        out.owner,
			})
		} else {
			out.result.Versions = append(out.result.Versions, ObjectVersion{
				Key:          keyOut,
				VersionID:    entry.ID,
				IsLatest:     entry.IsLatest,
				LastModified: s3Timestamp(entry.LastModified),
				ETag:         fmt.Sprintf("%q", entry.ETag),
				Size:         entry.Size,
				StorageClass: "STANDARD",
				Owner:        out.owner,
			})
		}
		out.lastKey = key
		out.lastVersionID = entry.ID
		out.emitted++
		if out.emitted >= out.maxKeys {
			out.truncated = true
			return
		}
	}
}

// listBucketVersionsLegacy is the pre-versioning ?versions listing
// (leaf 5.1 [a]-2), moved here VERBATIM from bucket_versions.go: every
// current object is one <Version> entry with the fixed "null" version
// id — the wire shape real S3 produces for an unversioned bucket. This
// is protocol plumbing, NOT versioning support. Never-versioned buckets
// keep this handler byte-identical (pinned by tests).
func listBucketVersionsLegacy(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucketPath := getBucketPath(bucketName)
	metadataDir := filepath.Join(bucketPath, ".metadata")

	if _, err := os.Stat(bucketPath); os.IsNotExist(err) { //nolint:gosec // G703: bucketPath derived from validateBucketName-checked name
		log.Printf("Bucket %s does not exist for ListObjectVersions", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	query := r.URL.Query()
	prefix := query.Get("prefix")
	keyMarker := query.Get("key-marker")
	// M4 (bughunt-postF2-2026-09-29): with encoding-type=url the client
	// echoes the ENCODED NextKeyMarker back; decode it before the
	// lexicographic comparison or keys between the raw and decoded forms
	// are silently skipped (raw '%' = 0x25 vs decoded ' ' = 0x20).
	if keyMarker != "" && query.Get("encoding-type") == "url" {
		if decoded, err := url.QueryUnescape(keyMarker); err == nil {
			keyMarker = decoded
		}
	}
	maxKeys := 1000
	if v := query.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxKeys = n
		}
	}

	keys, err := collectObjectKeys(metadataDir)
	if err != nil {
		log.Printf("Error walking metadata directory %s for ListObjectVersions: %v", metadataDir, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		writeS3Error(w, "InternalError", "Error listing object versions.", http.StatusInternalServerError)
		return
	}
	sort.Strings(keys)

	result := ListVersionsResult{
		Name:            bucketName,
		Prefix:          prefix,
		KeyMarker:       keyMarker,
		VersionIDMarker: query.Get("version-id-marker"),
		MaxKeys:         maxKeys,
		IsTruncated:     false,
		Versions:        []ObjectVersion{},
	}
	encodeKeys := query.Get("encoding-type") == "url"
	if encodeKeys {
		result.EncodingType = "url"
	}

	// Leaf-2.4 fix 13 parity (object_handlers.go): max-keys=0 → empty
	// listing, never truncated. Without this the loop below emits ALL
	// versions (its budget check is skipped when maxKeys == 0).
	if maxKeys == 0 {
		result.MaxKeys = 0
		result.IsTruncated = false
		writeXML(w, http.StatusOK, result)
		return
	}

	emitted := 0
	truncated := false
	lastKey := ""
	lastVersionID := ""
	for _, key := range keys {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		if keyMarker != "" && key <= keyMarker {
			continue
		}
		if maxKeys > 0 && emitted >= maxKeys {
			truncated = true
			break
		}

		meta, ok := readObjectMetaForList(metadataDir, key)
		if !ok {
			continue
		}

		keyOut := key
		if encodeKeys {
			keyOut = s3URLEncode(key)
		}
		result.Versions = append(result.Versions, ObjectVersion{
			Key:          keyOut,
			VersionID:    nullVersionID,
			IsLatest:     true,
			LastModified: meta.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         fmt.Sprintf("%q", meta.ETag),
			Size:         meta.ContentLength,
			StorageClass: "STANDARD",
			Owner:        Owner{ID: "zetaobject-user", DisplayName: "zetaobject-user"},
		})
		lastKey = key
		lastVersionID = nullVersionID
		emitted++
	}
	result.IsTruncated = truncated
	// S3 convention: a truncated ?versions page carries the marker to resume
	// from — the last emitted key/versionId. Markers are keys, so they get
	// the same encoding treatment as entry keys (regression review: the
	// per-entry Key was encoded but markers were not).
	if truncated && lastKey != "" {
		result.NextKeyMarker = lastKey
		if encodeKeys {
			result.NextKeyMarker = s3URLEncode(lastKey)
		}
		result.NextVersionIDMarker = lastVersionID
	}

	writeXML(w, http.StatusOK, result)
	log.Printf("Successfully served ListObjectVersions for bucket %s (%d versions)", strconv.Quote(bucketName), len(result.Versions))
}
