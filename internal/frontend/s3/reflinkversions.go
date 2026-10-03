// reflinkversions.go — the reflink (block-clone) versioning mode
// (s3-versioning tree leaf 06).
//
// On a versioning-ENABLED bucket in reflink mode, BEFORE the plain
// overwrite, the CURRENT object file is reflink-copied (FICLONE —
// reflinkclone_linux.go / reflinkclone_other.go) into the DISJOINT
// versions directory
//
//	<bucket>/.metadata/.versions-r/<key-sha>/<versionId>
//
// (key-sha = sha256 hex of the key, the sidecar store's hostile-key
// discipline; the `.versions-r` name keeps the two mechanisms' on-disk
// state from ever mixing). Bookkeeping is the SAME versioned-sidecar
// shape sidecar mode uses (Versioning/Versions/CurrentVersionID in the
// key's .meta sidecar), so ?versions, ?versionId and delete markers
// work identically — the only difference is that the version DATA file
// is a block clone made at capture time instead of a rewritten copy.
//
// Fail-soft: a FICLONE failure (cross-dataset, pre-2.2 ZFS, non-ZFS dev
// FS, darwin) is ONE WARN log and NO version record — the PUT never
// breaks. The capture/record split is KEPT (the backend still rewrites
// the sidecar wholesale on Put): the clone lands at capture time, the
// sidecar entry after the successful Put.
//
// Charter: .versions-r is object-derived state in the bucket's own
// metadata area (same class as .versions); no server-owned identity
// state.
package s3

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// reflinkVersionsDirName is the DISJOINT versions-directory name for the
// reflink mechanism (sidecar mode uses .versions; the state never mixes).
const reflinkVersionsDirName = ".versions-r"

// reflinkCloneFn is the clone seam: production wires the GOOS-split
// reflinkClone; tests substitute a fake (the ioctl itself is proven
// against real ZFS by the zfs-validate harness section 11). Var (not a
// hookMu hook): the clone is a pure fs operation with no process state.
var reflinkCloneFn = reflinkClone

// reflinkVersionStore implements versionStore over the reflink layout.
// ALL bookkeeping (State/SetState/sidecar read-modify-write/List/Open
// bookkeeping half) is composed from sidecarVersionStore — the ONLY
// difference is the versions-directory name and where the data file
// comes from (a capture-time clone, written by the handler integration,
// not by PutVersion).
type reflinkVersionStore struct {
	sidecarVersionStore
}

// versionsDir overrides the embedded sidecar store's layout:
// <bucket>/.metadata/.versions-r/<key-sha>.
func (s reflinkVersionStore) versionsDir(key string) string {
	return filepath.Join(s.bucketPath, ".metadata", reflinkVersionsDirName, keySha(key))
}

// versionDataPath is redefined (not just versionsDir): the embedded
// sidecarVersionStore.versionDataPath would resolve through the
// EMBEDDED versionsDir (.versions), so the reflink layout overrides it
// too — <bucket>/.metadata/.versions-r/<key-sha>/<versionId>.
func (s reflinkVersionStore) versionDataPath(key, versionID string) string {
	return filepath.Join(s.versionsDir(key), versionID)
}

// Open mirrors the embedded sidecar store's id-resolution but opens the
// data file through the REFLINK layout (.versions-r) — method promotion
// would otherwise dispatch to the embedded sidecarVersionStore.Open and
// resolve bytes from the wrong directory.
func (s reflinkVersionStore) Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error) {
	vs, err := s.readVersionedSidecar(key)
	if err != nil {
		return nil, VersionEntry{}, err
	}
	if len(vs.Versions) == 0 {
		return nil, VersionEntry{}, objectmodel.ErrNoSuchKey(key)
	}
	idx := -1
	if versionID == "" || versionID == vs.Versions[0].ID {
		idx = 0
	} else {
		for i, e := range vs.Versions {
			if e.ID == versionID {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return nil, VersionEntry{}, objectmodel.ErrNoSuchKey(key)
	}
	e := vs.Versions[idx]
	entry := VersionEntry{
		ID:             e.ID,
		IsLatest:       idx == 0,
		IsDeleteMarker: e.IsDeleteMarker,
		Size:           e.Size,
		ETag:           e.ETag,
		LastModified:   e.LastModified,
	}
	if e.IsDeleteMarker {
		return nil, entry, ErrIsDeleteMarker
	}
	f, err := os.Open(s.versionDataPath(key, e.ID)) //nolint:gosec // G703: key validated; versionID matched against sidecar entries, never used raw in a path.
	if err != nil {
		return nil, entry, err
	}
	return f, entry, nil
}

// compile-time pin: the reflink store implements the frozen Contract 1
// interface via the embedded sidecar bookkeeping.
var _ versionStore = reflinkVersionStore{}

// ---------- capture-time clone (handler integration) ----------

// capturedReflinkVersion is the capture-time product of the reflink
// path: the clone is ALREADY on disk under .versions-r; the record step
// (after the successful backend Put) writes only the sidecar entry.
type capturedReflinkVersion struct {
	// id was minted at capture time (the clone's file name).
	id string
	// size + etag describe the OLD object (the clone's bytes); etag is
	// the sidecar's stored ETag of the old object.
	size int64
	etag string
	// cloneOK reports whether the FICLONE succeeded. false = fail-soft:
	// the PUT proceeds, no version record is written (one WARN was
	// logged at capture).
	cloneOK bool
}

// captureReflinkObjectVersion reflink-copies the object's CURRENT file
// into .versions-r under a freshly minted version id, BEFORE the
// caller's overwrite. errNoPriorVersion means the object does not exist
// (a create records nothing). A clone failure is FAIL-SOFT: the
// returned capture has cloneOK=false and nil error — the PUT must
// proceed (one WARN here, no version record later).
func captureReflinkObjectVersion(bucketPath, objectName string) (*capturedReflinkVersion, error) {
	meta, ok := readObjectMetaForList(filepath.Join(bucketPath, ".metadata"), objectName)
	if !ok {
		return nil, errNoPriorVersion // no sidecar: a create — no prior version
	}
	if meta.StoragePath == "" {
		meta.StoragePath = resolveObjectDataPath(bucketPath, objectName, &meta)
	}
	fi, err := os.Stat(meta.StoragePath) //nolint:gosec // G304: sidecar StoragePath (backend-frozen) with the canonical-path fallback; validated key.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoPriorVersion // sidecar present but data gone: nothing to preserve
		}
		return nil, err
	}

	s := reflinkVersionStore{sidecarVersionStore{bucketPath: bucketPath}}
	id, err := newStateVersionID()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.versionsDir(objectName), 0o755); err != nil {
		return nil, fmt.Errorf("s3: creating reflink versions dir for %s: %w", objectName, err)
	}
	dst := s.versionDataPath(objectName, id)
	// O_CREAT|O_EXCL: the id is freshly minted (unix-micros + 4 random
	// bytes) — an existing file would be a collision, not a clone target.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G304: store-derived path (bucketPath + versions dir + validated key sha + minted id).
	if err != nil {
		return nil, fmt.Errorf("s3: creating reflink dest for %s: %w", objectName, err)
	}
	f.Close() //nolint:errcheck // reflinkClone owns the error path; an empty file is removed on failure
	if err := reflinkCloneFn(dst, meta.StoragePath); err != nil {
		// Fail-soft (leaf-06 contract): remove the empty dest, log ONE
		// WARN, skip the version record — never break the PUT.
		os.Remove(dst) //nolint:errcheck // best-effort cleanup of the failed clone target
		if errors.Is(err, errReflinkUnsupported) {
			log.Printf("reflink versioning unsupported on this platform (%s); version for %s skipped (fail-soft)", bucketPath, objectName)
		} else {
			log.Printf("reflink clone failed for %s (FICLONE unsupported on this filesystem?): version skipped (fail-soft): %v", objectName, err)
		}
		return &capturedReflinkVersion{id: id, cloneOK: false}, nil
	}
	return &capturedReflinkVersion{id: id, size: fi.Size(), etag: meta.ETag, cloneOK: true}, nil
}

// recordCapturedReflinkObjectVersion writes the sidecar bookkeeping
// entry for a capture-time clone (called AFTER the successful backend
// Put — the backend owns and rewrites the sidecar wholesale, so the
// pre-write history must ride the capture exactly as in sidecar mode).
// The DATA file already exists (the capture-time clone); this is
// sidecar-only work: entry prepended newest-first under lockObject.
func recordCapturedReflinkObjectVersion(bucketPath, objectName string, captured *capturedReflinkVersion) error {
	s := reflinkVersionStore{sidecarVersionStore{bucketPath: bucketPath}}
	unlock := lockObject(s.sidecarPath(objectName))
	defer unlock()

	vs, err := s.readVersionedSidecar(objectName)
	if err != nil {
		if !isNoSuchKeyErr(err) {
			return err
		}
		vs = versionedSidecar{}
	}
	entry := sidecarVersionEntry{
		ID:           captured.id,
		Size:         captured.size,
		ETag:         captured.etag,
		LastModified: time.Now().UTC(),
	}
	vs.Versioning = versioningEnabled
	vs.Versions = append([]sidecarVersionEntry{entry}, vs.Versions...)
	vs.CurrentVersionID = captured.id
	return s.writeVersionedSidecar(objectName, vs)
}

// PutVersion on the reflink store records a version from a READER (the
// frozen Contract 1 write surface; the handler's hot path uses the
// capture/record pair above). The data file is a plain atomic write
// into the .versions-r layout (no source file exists to clone — this
// form is for tests and callers that already hold the bytes). Retention
// applies here too (the store-level write surface must uphold the same
// per-key cap).
func (s reflinkVersionStore) PutVersion(bucket, key string, r io.Reader, size int64, etag string) (VersionEntry, error) {
	data, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return VersionEntry{}, fmt.Errorf("s3: reading version body for %s: %w", key, err)
	}
	if int64(len(data)) != size {
		return VersionEntry{}, fmt.Errorf("s3: version body for %s: got %d bytes, want %d", key, len(data), size)
	}
	id, err := newStateVersionID()
	if err != nil {
		return VersionEntry{}, err
	}
	unlock := lockObject(s.sidecarPath(key))
	defer unlock()

	// Data file first: a crash between the two writes leaves a data file
	// with no sidecar entry — orphaned bytes, never a lying sidecar.
	if err := os.MkdirAll(s.versionsDir(key), 0o755); err != nil {
		return VersionEntry{}, fmt.Errorf("s3: creating reflink versions dir for %s: %w", key, err)
	}
	if err := writeFileAtomic(s.versionDataPath(key, id), data, 0o644); err != nil {
		return VersionEntry{}, fmt.Errorf("s3: writing reflink version data for %s: %w", key, err)
	}
	vs, err := s.readVersionedSidecar(key)
	if err != nil {
		if !isNoSuchKeyErr(err) {
			return VersionEntry{}, err
		}
		vs = versionedSidecar{}
	}
	now := time.Now().UTC()
	entry := sidecarVersionEntry{ID: id, Size: size, ETag: etag, LastModified: now}
	vs.Versioning = versioningEnabled
	vs.Versions = append([]sidecarVersionEntry{entry}, vs.Versions...)
	vs.CurrentVersionID = id
	if err := s.writeVersionedSidecar(key, vs); err != nil {
		return VersionEntry{}, err
	}
	pruneReflinkVersions(s.bucketPath, key)
	return VersionEntry{ID: id, IsLatest: true, Size: size, ETag: etag, LastModified: now}, nil
}

// ---------- retention pruning (leaf 06, zfs_versioning_reflink_retention) ----------

// pruneReflinkVersions enforces the per-key retention cap after a
// successful capture+record: keep the NEWEST N version entries (N =
// reflinkRetentionFor(); 0 = unlimited), delete the OLDEST version DATA
// files beyond the cap AND then rewrite the sidecar without their
// entries.
//
// Counting: VERSIONS only — delete-marker entries are never counted and
// never pruned (markers are history truth, hold no data file).
//
// Crash-safe order (the recordCapturedObjectVersion invariant): data
// files FIRST, sidecar rewrite SECOND — a crash between the two leaves
// orphaned bytes on disk, never a sidecar entry whose data file is
// already gone (a lying sidecar). Prune failures are logged and never
// propagate: housekeeping must not fail a PUT that already succeeded.
func pruneReflinkVersions(bucketPath, key string) {
	retention := reflinkRetentionFor()
	if retention <= 0 {
		return // 0/unset = unlimited
	}
	s := reflinkVersionStore{sidecarVersionStore{bucketPath: bucketPath}}
	unlock := lockObject(s.sidecarPath(key))
	defer unlock()

	vs, err := s.readVersionedSidecar(key)
	if err != nil {
		if !isNoSuchKeyErr(err) {
			log.Printf("reflink retention: reading sidecar for %s: %v (skipping prune)", key, err)
		}
		return
	}
	// Collect the version (non-marker) entries beyond the newest N.
	// Versions array is newest-first; markers pass through untouched.
	var prunedIDs []string
	kept := 0
	keptVersions := make([]sidecarVersionEntry, 0, len(vs.Versions))
	for _, e := range vs.Versions {
		if e.IsDeleteMarker {
			keptVersions = append(keptVersions, e)
			continue
		}
		if kept < retention {
			kept++
			keptVersions = append(keptVersions, e)
			continue
		}
		prunedIDs = append(prunedIDs, e.ID)
	}
	if len(prunedIDs) == 0 {
		return
	}
	// 1. Data files FIRST: after this loop the bytes are gone; the
	// sidecar still lists them until step 2 (briefly listing entries
	// whose bytes are gone beats the reverse: a lying sidecar is the
	// invariant violation, transient orphans are not).
	for _, id := range prunedIDs {
		if err := os.Remove(s.versionDataPath(key, id)); err != nil && !os.IsNotExist(err) {
			// The rewrite below keeps the entry when its data file
			// could not be removed (never lie about readable data).
			log.Printf("reflink retention: removing version data %s/%s: %v (entry kept)", key, id, err)
			prunedIDs = pruneDropID(prunedIDs, id)
		}
	}
	// 2. Sidecar rewrite WITHOUT the pruned entries (only ids whose
	// data file is actually gone).
	if len(prunedIDs) == 0 {
		return
	}
	byID := make(map[string]struct{}, len(prunedIDs))
	for _, id := range prunedIDs {
		byID[id] = struct{}{}
	}
	filtered := make([]sidecarVersionEntry, 0, len(keptVersions))
	for _, e := range keptVersions {
		if _, drop := byID[e.ID]; drop {
			continue
		}
		filtered = append(filtered, e)
	}
	vs.Versions = filtered
	if err := s.writeVersionedSidecar(key, vs); err != nil {
		// Entries whose bytes are gone may linger in the sidecar (a
		// subsequent Open answers NoSuchKey honestly — no current-data
		// substitution); the next prune pass retries.
		log.Printf("reflink retention: rewriting sidecar for %s: %v (pruned entries may linger until the next pass)", key, err)
	}
}

// pruneDropID removes id from prunedIDs (order-preserving).
func pruneDropID(ids []string, id string) []string {
	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// ---------- reflinkBothVersionStore ("both" mode, leaf 06) ----------

// reflinkBothVersionStore implements versionStore for the leaf-06
// "both" mode: reflink + sidecar-layout history MERGED, snapshots stay
// OUT. Both stores share the same bucket-level state marker and the
// same per-key sidecar bookkeeping (one Versions array) — writes and
// delete markers go through the REFLINK store (whose sidecar writes the
// shared array), while List/Open consult the reflink layout first and
// fall back to the plain sidecar layout for history recorded before an
// operator's mode switch (sidecar -> reflink, or via "both"). A version
// id unknown to both layouts is NoSuchKey (404-class); a delete-marker
// id is ErrIsDeleteMarker regardless of which layout holds it.
type reflinkBothVersionStore struct {
	reflink reflinkVersionStore
	sidecar sidecarVersionStore
}

// PutVersion writes through the reflink store (the live mechanism).
func (m reflinkBothVersionStore) PutVersion(bucket, key string, r io.Reader, size int64, etag string) (VersionEntry, error) {
	return m.reflink.PutVersion(bucket, key, r, size, etag)
}

// PutDeleteMarker appends a delete marker through the reflink store
// (delete markers exist in both mode).
func (m reflinkBothVersionStore) PutDeleteMarker(bucket, key string) (VersionEntry, error) {
	return m.reflink.PutDeleteMarker(bucket, key)
}

// State reads the SHARED bucket-level state marker (one file for every
// mechanism).
func (m reflinkBothVersionStore) State(bucket string) (string, error) {
	return m.reflink.State(bucket)
}

// SetState writes the SHARED bucket-level state marker.
func (m reflinkBothVersionStore) SetState(bucket, state string) error {
	return m.reflink.SetState(bucket, state)
}

// List renders the SHARED sidecar Versions array (both layouts append
// to the same per-key array; the reflink store is the canonical reader).
func (m reflinkBothVersionStore) List(bucket, key string) ([]VersionEntry, error) {
	return m.reflink.List(bucket, key)
}

// Open resolves a version id across BOTH data layouts: the shared
// sidecar array says which id exists and whether it is a delete marker;
// the bytes come from .versions-r first, then the plain .versions
// layout (legacy sidecar-mode history). The sidecar version data of the
// CURRENT entry (no versionId) resolves against the same order.
func (m reflinkBothVersionStore) Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error) {
	vs, err := m.reflink.readVersionedSidecar(key)
	if err != nil {
		// No sidecar at all: fall back to the plain sidecar store's
		// Open for sidecar-mode history whose sidecar array was
		// wiped by a backend Put (the capture/record reconstruction).
		if !isNoSuchKeyErr(err) {
			return nil, VersionEntry{}, err
		}
		return m.sidecar.Open(bucket, key, versionID)
	}
	if len(vs.Versions) == 0 {
		return nil, VersionEntry{}, objectmodel.ErrNoSuchKey(key)
	}
	idx := -1
	if versionID == "" || versionID == vs.Versions[0].ID {
		idx = 0
	} else {
		for i, e := range vs.Versions {
			if e.ID == versionID {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		// Unknown to the shared array: maybe sidecar-layout history
		// whose array no longer carries the id — let the plain store
		// answer (its own array may still resolve it).
		return m.sidecar.Open(bucket, key, versionID)
	}
	e := vs.Versions[idx]
	entry := VersionEntry{
		ID:             e.ID,
		IsLatest:       idx == 0,
		IsDeleteMarker: e.IsDeleteMarker,
		Size:           e.Size,
		ETag:           e.ETag,
		LastModified:   e.LastModified,
	}
	if e.IsDeleteMarker {
		return nil, entry, ErrIsDeleteMarker
	}
	// Reflink layout first, then the plain sidecar layout.
	f, rErr := os.Open(m.reflink.versionDataPath(key, e.ID)) //nolint:gosec // G703: key validated; versionID matched against sidecar entries, never used raw in a path.
	if rErr == nil {
		return f, entry, nil
	}
	if !os.IsNotExist(rErr) {
		return nil, entry, rErr
	}
	f, sErr := os.Open(m.sidecar.versionDataPath(key, e.ID)) //nolint:gosec // G703: same trusted components (sidecar layout).
	if sErr != nil {
		if os.IsNotExist(sErr) {
			return nil, entry, objectmodel.ErrNoSuchKey(key)
		}
		return nil, entry, sErr
	}
	return f, entry, nil
}
