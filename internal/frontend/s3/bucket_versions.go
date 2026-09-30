package s3

// bucket_versions.go — ListObjectVersions (GET /bucket?versions) sub-resource
// (leaf 5.1 [a]-2, found by ceph/s3-tests: the suite's per-test teardown
// nukes buckets via list_object_versions + delete_objects; without this
// sub-resource every cleanup failed with BucketNotEmpty and poisoned the
// whole run with setup ERRORs).
//
// mini-s3 is not versioned (documented divergence), so the response reports
// every current object as exactly one version with the fixed "null" version
// ID — the same wire shape real S3 produces for an unversioned bucket. This
// is protocol plumbing, NOT versioning support: version-specific reads
// (GET ?versionId=...), DeleteMarkers, and suspension/enabled semantics stay
// unimplemented.

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

// nullVersionID is the version ID real S3 reports for objects in
// unversioned buckets.
const nullVersionID = "null"

// listObjectVersionsHandler serves GET /bucket?versions.
func listObjectVersionsHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
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
