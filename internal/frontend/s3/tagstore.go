// tagstore.go — the S3 object-tagging storage seam (tagging tree leaf 03).
//
// Charter compliance: tags are stored in the object's OWN sidecar file
// (<bucket>/.metadata/<key>.meta, optional "tags" JSON field, leaf 02) —
// no server-owned tagging state exists. The seam exists so the ZFS-native
// store (upstream zfs-metadata#13) slots in later without re-touching
// handlers; in v1 ZFS buckets use the same sidecar path (documented in
// README — parity is trivial because there is exactly one path).
package s3

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// tagStore is the object-tagging storage seam. key is the S3 object key;
// tags is the validated tag map. Get on an object without a sidecar
// returns an objectmodel.ErrNoSuchKey error; an object WITH a sidecar but
// no tags returns an empty (non-nil) map.
type tagStore interface {
	Get(key string) (map[string]string, error)
	Put(key string, tags map[string]string) error
	Delete(key string) error
}

// tagStoreFor resolves the tag store for a bucket's filesystem root.
// v1: sidecarTagStore only; the zmetad-backed implementation lands when
// upstream zfs-metadata#13 ships.
func tagStoreFor(bucketPath string) tagStore {
	return sidecarTagStore{bucketPath: bucketPath}
}

// sidecarTagStore implements tagStore over the per-object .meta sidecar
// (read-modify-write of the frozen sidecar JSON; the tags field is
// omitempty so tag-free sidecars stay byte-identical to the pre-tagging
// form — HARD requirement, leaf 02).
type sidecarTagStore struct {
	bucketPath string
}

// sidecarPath is the sidecar file for an object key (frozen layout:
// <bucket>/.metadata/<key>.meta — the same path math every handler uses).
func (s sidecarTagStore) sidecarPath(key string) string {
	return filepath.Join(s.bucketPath, ".metadata", key+".meta")
}

// readSidecar loads the object's sidecar. A missing sidecar is
// ErrNoSuchKey (the sidecar exists iff the object exists — both are
// written/deleted together by the backend).
func (s sidecarTagStore) readSidecar(key string) (objectmodel.LegacyObjectMetadata, error) {
	//nolint:gosec // G703: key is validateObjectKey-checked at every handler entry (traversal/`.metadata` segments rejected); the path is the frozen sidecar layout shared with every handler.
	raw, err := os.ReadFile(s.sidecarPath(key))
	if err != nil {
		if os.IsNotExist(err) {
			return objectmodel.LegacyObjectMetadata{}, objectmodel.ErrNoSuchKey(key)
		}
		return objectmodel.LegacyObjectMetadata{}, err
	}
	var meta objectmodel.LegacyObjectMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return objectmodel.LegacyObjectMetadata{}, fmt.Errorf("parsing sidecar for %s: %w", key, err)
	}
	return meta, nil
}

// writeSidecar atomically replaces the sidecar (temp file + rename in the
// same directory, the same durability shape as the backend's
// writeFileAtomicJSON) so a crash never leaves a truncated sidecar.
func (s sidecarTagStore) writeSidecar(key string, meta objectmodel.LegacyObjectMetadata) error {
	path := s.sidecarPath(key)
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tagstore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op after a successful rename
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close() //nolint:errcheck // best-effort close on the error path
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil { //nolint:gosec // G302: 0644 matches the frozen sidecar file mode.
		return err
	}
	//nolint:gosec // G703: same validated-key path as readSidecar.
	return os.Rename(tmpName, path)
}

// Get returns the object's tags. An object without a sidecar is
// ErrNoSuchKey; a sidecar without tags yields an empty map.
func (s sidecarTagStore) Get(key string) (map[string]string, error) {
	meta, err := s.readSidecar(key)
	if err != nil {
		return nil, err
	}
	if meta.Tags == nil {
		return map[string]string{}, nil
	}
	return meta.Tags, nil
}

// Put sets the object's tags (read-modify-write; every other sidecar
// field is preserved verbatim). A nil/empty tags map removes the tags
// field entirely, restoring the byte-compat sidecar form.
func (s sidecarTagStore) Put(key string, tags map[string]string) error {
	meta, err := s.readSidecar(key)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		meta.Tags = nil
	} else {
		meta.Tags = tags
	}
	return s.writeSidecar(key, meta)
}

// Delete removes the object's tags. A missing object is ErrNoSuchKey;
// deleting tags from an untagged object succeeds (S3 idempotency).
func (s sidecarTagStore) Delete(key string) error {
	meta, err := s.readSidecar(key)
	if err != nil {
		return err
	}
	meta.Tags = nil
	return s.writeSidecar(key, meta)
}

// writeInvalidTag renders a tag-validation failure as the S3 InvalidTag
// error (400). objectmodel's validation errors carry InvalidArgument
// identity; the S3 wire taxonomy for tag violations is InvalidTag.
func writeInvalidTag(w http.ResponseWriter, err error) {
	writeS3Error(w, "InvalidTag", err.Error(), http.StatusBadRequest)
}

// ---- ?tagging sub-resource handlers (tagging tree leaf 03) ----

// bucketAndStore resolves the bucket path + tag store for the tagging
// handlers, with the shared NoSuchBucket check.
func bucketAndStore(w http.ResponseWriter, bucketName, op string) (string, tagStore, bool) {
	if !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for %s", strconv.Quote(bucketName), op)
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return "", nil, false
	}
	bucketPath := getBucketPath(bucketName)
	return bucketPath, tagStoreFor(bucketPath), true
}

// getObjectTaggingHandler implements GetObjectTagging (GET
// /bucket/key?tagging): the Tagging XML envelope. Per S3, an object with
// no tag set yields the 404 NoSuchTagSet error.
func getObjectTaggingHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	if err := validateObjectKey(objectName); err != nil {
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}
	_, store, ok := bucketAndStore(w, bucketName, "GetObjectTagging")
	if !ok {
		return
	}

	tags, err := store.Get(objectName)
	if err != nil {
		log.Printf("GetObjectTagging %s/%s failed: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeS3ErrorFrom(w, err)
		return
	}
	if len(tags) == 0 {
		// S3 taxonomy: no tag set on the object → NoSuchTagSet (404).
		log.Printf("GetObjectTagging %s/%s: no tag set", strconv.Quote(bucketName), strconv.Quote(objectName))
		writeS3Error(w, "NoSuchTagSet", "The TagSet does not exist.", http.StatusNotFound)
		return
	}
	// Body comes from objectmodel.TagsToXML (the shared Contract 1
	// renderer) — written via writeXMLBytes, which emits the same prolog +
	// Content-Type as writeXML (the encoder route would escape
	// pre-rendered XML).
	//nolint:gosec // G705: TagsToXML xml-escapes every key/value (encoding/xml Marshal); the bytes are a rendered document, not raw client input.
	writeXMLBytes(w, http.StatusOK, objectmodel.TagsToXML(tags))
	log.Printf("Successfully served GetObjectTagging for %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))
}

// putObjectTaggingHandler implements PutObjectTagging (PUT
// /bucket/key?tagging): parse the Tagging XML body, validate against the
// S3 limits (InvalidTag on violation), and replace the object's tags.
func putObjectTaggingHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	if err := validateObjectKey(objectName); err != nil {
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}
	_, store, ok := bucketAndStore(w, bucketName, "PutObjectTagging")
	if !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading PutObjectTagging body for %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeS3Error(w, "InternalError", "Error reading request body.", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	tags, err := objectmodel.TagsFromXML(body)
	if err != nil {
		log.Printf("Invalid tag set in PutObjectTagging for %s/%s: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeInvalidTag(w, err)
		return
	}
	if err := store.Put(objectName, tags); err != nil {
		log.Printf("PutObjectTagging %s/%s failed: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeS3ErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	log.Printf("Successfully served PutObjectTagging for %s/%s (%d tags)", strconv.Quote(bucketName), strconv.Quote(objectName), len(tags))
}

// deleteObjectTaggingHandler implements DeleteObjectTagging (DELETE
// /bucket/key?tagging): removes the object's tags entirely (restoring
// the byte-compat sidecar form). Idempotent per S3.
func deleteObjectTaggingHandler(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	if err := validateObjectKey(objectName); err != nil {
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return
	}
	_, store, ok := bucketAndStore(w, bucketName, "DeleteObjectTagging")
	if !ok {
		return
	}

	if err := store.Delete(objectName); err != nil {
		log.Printf("DeleteObjectTagging %s/%s failed: %v", strconv.Quote(bucketName), strconv.Quote(objectName), err)
		writeS3ErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	log.Printf("Successfully served DeleteObjectTagging for %s/%s", strconv.Quote(bucketName), strconv.Quote(objectName))
}

// requestTagsFromHeader parses and validates an x-amz-tagging request
// header into a tag map. An absent header yields nil tags with no error
// (the caller writes nothing — byte-compat). The S3 wire taxonomy for a
// bad header is InvalidTag (S3 returns InvalidTag for header-encoded tag
// violations).
func requestTagsFromHeader(r *http.Request) (map[string]string, error) {
	header := r.Header.Get("x-amz-tagging")
	if header == "" {
		return nil, errNoTagHeader
	}
	tags, err := objectmodel.ParseTagHeader(header)
	if err != nil {
		return nil, err
	}
	if err := objectmodel.ValidateTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// errNoTagHeader signals "no x-amz-tagging header present" from
// requestTagsFromHeader (an empty tag set on the wire is a valid
// REPLACE-with-empty, distinct from an absent header).
var errNoTagHeader = errors.New("no x-amz-tagging header")
