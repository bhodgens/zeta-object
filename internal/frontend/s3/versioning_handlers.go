// versioning_handlers.go — S3 versioning wire surface (s3-versioning
// tree leaf 03, Contract 2):
//
//	PUT /{bucket}?versioning     — SetBucketVersioning (Enabled|Suspended|Off)
//	GET /{bucket}?versioning     — GetBucketVersioning (echoes state; the
//	                               Status element is absent for Off)
//	GET /{bucket}/{key}?versionId=<id> — versioned read (200 +
//	                               x-amz-version-id; delete-marker id →
//	                               405 + x-amz-delete-marker; unknown
//	                               sidecar-form id → 400 InvalidArgument;
//	                               snapshot-form id with an expired
//	                               window / absent key → honest 404)
//
// plus the versioning branches the object PUT/DELETE handlers call:
// versioned PUT copies the old current bytes into the version store
// BEFORE the plain overwrite (versioning records the OLD version), and
// versioned DELETE writes a delete marker without touching the data
// file. OFF buckets never enter any of this code (the plain paths are
// byte-identical to pre-versioning behavior — pinned by tests).
//
// The version store is resolved per request through versionStoreFor:
// the mode comes from the installed zfs_versioning config, and zdb is
// the shared zmetad DB handle (both startup-installed seams).
package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VersioningConfiguration is the S3 VersioningConfiguration XML document
// (Contract 2). For Off (and never-versioned buckets) GET renders the
// envelope with a nil Status — real S3 omits the <Status> element.
type VersioningConfiguration struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ VersioningConfiguration"`
	Status  *string  `xml:"Status,omitempty"`
}

// versionStoreForBucket resolves the version store for a request against
// an existing bucket: the installed zfs_versioning mode plus the shared
// zmetad DB handle. bucketPath must be the resolved bucket root.
func versionStoreForBucket(bucketPath string) versionStore {
	return versionStoreFor(bucketPath, zmetadDBFor(), zfsVersioningModeFor())
}

// objectVersionedDispatch routes the ?versionId sub-resource (leaf 03):
// a versioned read on GET/HEAD. Returns false when the request is not a
// versioned read (the caller falls through to the plain method switch);
// true when the response was fully written. Validation order mirrors the
// plain GET path: key first, then bucket existence.
func objectVersionedDispatch(w http.ResponseWriter, r *http.Request, bucketName, objectName string) bool {
	versionID, ok := r.URL.Query()["versionId"]
	if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	if err := validateObjectKey(objectName); err != nil {
		writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
		return true
	}
	if !bucketExists(bucketName) {
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return true
	}
	objectVersionedRead(w, r, bucketName, objectName, versionID[0])
	return true
}

// putBucketVersioningHandler implements SetBucketVersioning
// (PUT /{bucket}?versioning). Body:
// <VersioningConfiguration><Status>Enabled|Suspended</Status>...
// An absent/empty Status element means Off (the S3 form with no
// MFADelete block; a body with no Status clears versioning).
func putBucketVersioningHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	if !validBucket(bucketName) || !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for PutBucketVersioning", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeS3Error(w, "InternalError", "Error reading request body.", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var cfg struct {
		// Namespace-agnostic decode: Go's xml package requires the
		// document's root namespace to match XMLName exactly, and
		// real clients emit both namespaced and bare documents. The
		// status vocabulary is validated below instead.
		Status *string `xml:"Status"`
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := xml.Unmarshal(body, &cfg); err != nil {
			log.Printf("MalformedXML in PutBucketVersioning for bucket %s: %v", strconv.Quote(bucketName), err)
			writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest)
			return
		}
	}

	state := versioningOff
	if cfg.Status != nil {
		switch *cfg.Status {
		case versioningEnabled, versioningSuspended:
			state = *cfg.Status
		default:
			log.Printf("Invalid versioning status %q for bucket %s", strconv.Quote(*cfg.Status), strconv.Quote(bucketName))
			writeS3Error(w, "MalformedXML", "The Status element must be one of Enabled, Suspended.", http.StatusBadRequest)
			return
		}
	}

	bucketPath := getBucketPath(bucketName)
	if err := versionStoreForBucket(bucketPath).SetState(bucketName, state); err != nil {
		log.Printf("Error setting versioning state for bucket %s: %v", strconv.Quote(bucketName), err)
		writeS3ErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getBucketVersioningHandler implements GetBucketVersioning
// (GET /{bucket}?versioning): the VersioningConfiguration envelope
// echoing the state; Off renders WITHOUT the Status element (real S3's
// wire form for never-versioned buckets). Unknown bucket → 404.
func getBucketVersioningHandler(w http.ResponseWriter, r *http.Request, bucketName string) {
	if !validBucket(bucketName) || !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for GetBucketVersioning", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return
	}

	bucketPath := getBucketPath(bucketName)
	state, err := versionStoreForBucket(bucketPath).State(bucketName)
	if err != nil {
		log.Printf("Error reading versioning state for bucket %s: %v", strconv.Quote(bucketName), err)
		writeS3ErrorFrom(w, err)
		return
	}
	out := VersioningConfiguration{}
	if state != "" && state != versioningOff {
		s := state
		out.Status = &s
	}
	writeXML(w, http.StatusOK, out)
}

// objectVersionedRead serves GET (or HEAD) /{bucket}/{key}?versionId=<id>
// (Contract 2): 200 + x-amz-version-id; delete-marker id → 405 Method
// Not Allowed + x-amz-delete-marker: true; a sidecar-form id unknown to
// the store → 400 InvalidArgument (real S3 semantics); a snapshot-form
// id whose window is gone, or a key absent in that snapshot, → honest
// 404 NoSuchKey (the store cannot distinguish "never existed" from
// "expired", and 404 is the truthful answer for an expired window).
func objectVersionedRead(w http.ResponseWriter, r *http.Request, bucketName, objectName, versionID string) {
	bucketPath := getBucketPath(bucketName)
	rc, entry, err := versionStoreForBucket(bucketPath).Open(bucketName, objectName, versionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrIsDeleteMarker):
			w.Header().Set("x-amz-delete-marker", "true")
			writeS3Error(w, "MethodNotAllowed",
				"The specified method is not allowed against this resource.",
				http.StatusMethodNotAllowed)
		case isNoSuchKeyErr(err) && !isSidecarShapedVersionID(versionID):
			log.Printf("GetObject version %s/%s?versionId=%s: not found (expired or absent snapshot window)",
				strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(versionID)) //nolint:gosec // G706: strconv.Quote-escaped values.
			writeS3Error(w, "NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
		case isNoSuchKeyErr(err):
			log.Printf("GetObject version %s/%s?versionId=%s: unknown version id",
				strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(versionID)) //nolint:gosec // G706: strconv.Quote-escaped values.
			writeS3Error(w, "InvalidArgument", "Invalid version id specified", http.StatusBadRequest)
		default:
			log.Printf("GetObject version %s/%s?versionId=%s failed: %v",
				strconv.Quote(bucketName), strconv.Quote(objectName), strconv.Quote(versionID), err) //nolint:gosec // G706: strconv.Quote-escaped values.
			writeS3ErrorFrom(w, err)
		}
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "binary/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("x-amz-version-id", entry.ID)
	if entry.ETag != "" {
		w.Header().Set("ETag", strconv.Quote(entry.ETag))
	}
	if !entry.LastModified.IsZero() {
		w.Header().Set("Last-Modified", entry.LastModified.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, rc); err != nil {
			log.Printf("Error streaming version %s of %s/%s: %v",
				strconv.Quote(entry.ID), strconv.Quote(bucketName), strconv.Quote(objectName), err) //nolint:gosec // G706: strconv.Quote-escaped values.
		}
	}
}

// isSidecarShapedVersionID reports whether id matches the sidecar
// version-id form ^[0-9]{16}-[0-9a-f]{8}$ (the same shape
// versionstore_test's versionIDRe pins). Used ONLY to split the
// 404-vs-400 wire rule for unknown ids: sidecar ids are server-minted
// and opaque, so an unknown one is a client error (400); snapshot names
// are host policy whose windows expire (404 is honest).
func isSidecarShapedVersionID(id string) bool {
	if len(id) != 16+1+8 || id[16] != '-' {
		return false
	}
	for _, c := range id[:16] {
		if c < '0' || c > '9' {
			return false
		}
	}
	for _, c := range id[17:] {
		digit := c >= '0' && c <= '9'
		letter := c >= 'a' && c <= 'f'
		if !digit && !letter {
			return false
		}
	}
	return true
}

// capturedObjectVersion is the object's pre-overwrite state: the bytes,
// the old ETag, and the sidecar's prior version history (the backend Put
// rewrites the sidecar wholesale, so the record step must restore the
// history it captured). nil (pointer) means "no prior object" — a create
// writes no version.
type capturedObjectVersion struct {
	data         []byte
	etag         string
	priorEntries []sidecarVersionEntry
}

// errNoPriorVersion is the sentinel returned by captureCurrentObjectVersion
// when the object does not exist (a create records no version). A typed
// sentinel keeps the capture contract explicit instead of a nil,nil return.
var errNoPriorVersion = errors.New("s3: no prior object version to record")

// captureCurrentObjectVersion reads the object's CURRENT bytes + version
// history for the record step, BEFORE the caller's plain overwrite.
// errNoPriorVersion means the object does not exist (a create records
// nothing). Only real I/O failures return any other error.
func captureCurrentObjectVersion(bucketPath, objectName string) (*capturedObjectVersion, error) {
	s := sidecarVersionStore{bucketPath: bucketPath}
	meta, ok := readObjectMetaForList(filepath.Join(bucketPath, ".metadata"), objectName)
	if !ok {
		return nil, errNoPriorVersion // no sidecar: a create — no prior version
	}
	if meta.StoragePath == "" {
		meta.StoragePath = resolveObjectDataPath(bucketPath, objectName, &meta)
	}
	raw, err := os.ReadFile(meta.StoragePath) //nolint:gosec // G304/G703: sidecar StoragePath (backend-frozen) with the canonical-path fallback; validated key.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoPriorVersion // sidecar present but data gone: nothing to preserve
		}
		return nil, err
	}
	// Prior history lives in the (about-to-be-rewritten) sidecar; a
	// sidecar without version fields carries no history (zero values).
	vs, vsErr := s.readVersionedSidecar(objectName)
	if vsErr != nil && !isNoSuchKeyErr(vsErr) {
		return nil, vsErr
	}
	return &capturedObjectVersion{data: raw, etag: meta.ETag, priorEntries: vs.Versions}, nil
}

// recordCapturedObjectVersion writes the captured pre-overwrite bytes as
// one version and RESTORES the captured history beneath the new entry.
// Called AFTER the successful backend Put: the backend owns the sidecar
// format and rewrites it wholesale on Put, so the pre-write history must
// ride the capture (a plain PutVersion read-modify-write would start
// from an already-wiped sidecar and collapse the history to one entry).
// Version recording is ALWAYS the sidecar mechanism (snapshots mode
// never reaches this path — the handler gate skips it; snapshots are
// host policy and the plain overwrite stands).
func recordCapturedObjectVersion(bucketPath, bucketName, objectName string, captured *capturedObjectVersion) error {
	s := sidecarVersionStore{bucketPath: bucketPath}
	unlock := lockObject(s.sidecarPath(objectName))
	defer unlock()

	id, err := newStateVersionID()
	if err != nil {
		return err
	}
	// Data file first: a crash between the two writes leaves a data file
	// with no sidecar entry — orphaned bytes, never a lying sidecar.
	if err := os.MkdirAll(s.versionsDir(objectName), 0o755); err != nil {
		return fmt.Errorf("s3: creating versions dir for %s: %w", objectName, err)
	}
	if err := writeFileAtomic(s.versionDataPath(objectName, id), captured.data, 0o644); err != nil {
		return fmt.Errorf("s3: writing version data for %s: %w", objectName, err)
	}

	// Sidecar: keep the POST-write legacy fields (they describe the new
	// current object), restore the captured history beneath the new entry.
	vs, err := s.readVersionedSidecar(objectName)
	if err != nil {
		if !isNoSuchKeyErr(err) {
			return err
		}
		vs = versionedSidecar{}
	}
	entry := sidecarVersionEntry{
		ID:           id,
		Size:         int64(len(captured.data)),
		ETag:         captured.etag,
		LastModified: time.Now().UTC(),
	}
	vs.Versioning = versioningEnabled
	vs.Versions = append([]sidecarVersionEntry{entry}, captured.priorEntries...)
	vs.CurrentVersionID = id
	return s.writeVersionedSidecar(objectName, vs)
}

// deleteObjectVersionedMarker records a delete marker for the key and
// reports whether the plain backend delete must be SUPPRESSED
// (versioning Enabled): the data file and sidecar stay, plain GET
// answers 404 via the marker. Suspended/Off proceed to the plain delete
// (plain-overwrite semantics per the S3 suspension model).
func deleteObjectVersionedMarker(bucketPath, bucketName, objectName string) (suppressPlainDelete bool, err error) {
	store := versionStoreForBucket(bucketPath)
	state, err := store.State(bucketName)
	if err != nil {
		return false, err
	}
	if state != versioningEnabled {
		return false, nil
	}
	if _, err := store.PutDeleteMarker(bucketName, objectName); err != nil {
		return false, err
	}
	return true, nil
}

// plainObjectDeleteMarker404 reports whether a plain GET/HEAD on the key
// must answer the delete-marker 404 (with x-amz-delete-marker: true):
// the key has a versioned history whose latest entry is a delete marker.
// OFF/never-versioned buckets are unaffected (the List consult errors
// NoSuchKey → false; the caller's normal path answers the plain 404).
func plainObjectDeleteMarker404(bucketPath, bucketName, objectName string) (bool, error) {
	store := versionStoreForBucket(bucketPath)
	state, err := store.State(bucketName)
	if err != nil {
		return false, err
	}
	if state == versioningOff {
		return false, nil
	}
	versions, err := store.List(bucketName, objectName)
	if err != nil {
		if isNoSuchKeyErr(err) {
			return false, nil // no versioned history: not marker-hidden
		}
		return false, err
	}
	if len(versions) == 0 {
		return false, nil
	}
	return versions[0].IsDeleteMarker, nil
}
