// versionstore.go — the S3 versioning storage seam (s3-versioning
// tree leaf 01).
//
// Charter compliance: versioned data lives in the bucket's own metadata
// area (<bucket>/.metadata/.versions/<key-sha>/<versionId>) and version
// bookkeeping lives in the object's OWN sidecar file (<key>.meta gains
// optional Versioning/Versions/CurrentVersionID fields) — object-derived
// state in the bucket's own metadata area, like uploads staging. No
// server-owned identity state exists. Sidecar JSON is additive with
// omitempty on every new field (HARD requirement: pre-versioning sidecars
// decode fine and new sidecars stay byte-identical to the old form when
// versioning was never enabled).
package s3

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// VersionEntry is one entry in a bucket/key's version history (Contract 1,
// FROZEN).
type VersionEntry struct {
	ID             string
	IsLatest       bool
	IsDeleteMarker bool
	Size           int64
	ETag           string
	LastModified   time.Time
}

// versionStore is the versioning storage seam (Contract 1, FROZEN).
// bucket is the bucket NAME; the store resolves paths from its own
// configuration. key is the S3 object key.
type versionStore interface {
	State(bucket string) (string, error) // "Off"|"Enabled"|"Suspended"
	SetState(bucket, state string) error
	PutVersion(bucket, key string, r io.Reader, size int64, etag string) (VersionEntry, error)
	PutDeleteMarker(bucket, key string) (VersionEntry, error) // snapshot store: ErrDeleteMarkersUnsupported
	List(bucket, key string) ([]VersionEntry, error)          // newest first
	Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error)
}

// versioning state values (the sidecar marker and the wire form agree).
const (
	versioningOff      = "Off"
	versioningEnabled  = "Enabled"
	versioningSuspended = "Suspended"
)

// ErrIsDeleteMarker is returned by Open when the requested versionId
// identifies a delete marker (no data exists to read). Handlers map it to
// the 405 MethodNotAllowed + x-amz-delete-marker wire form (Contract 2).
var ErrIsDeleteMarker = errors.New("s3: version is a delete marker")

// ErrDeleteMarkersUnsupported is returned by PutDeleteMarker on stores
// without delete-marker semantics (the ZFS snapshot store).
var ErrDeleteMarkersUnsupported = errors.New("s3: delete markers are not supported by this versioning mechanism")

// errVersioningUnimplemented is the leaf-01 placeholder for the snapshots
// and both modes (leaf 02 implements zfsSnapshotVersionStore; the merge
// wrapper lands with leaf 03).
var errVersioningUnimplemented = errors.New("s3: versioning mode not implemented")

// ErrVersioningModeInvalid reports an unknown zfs_versioning mode from
// config (leaf 03 validates at config load; the factory defends too).
var ErrVersioningModeInvalid = errors.New("s3: invalid versioning mode")

// versionStoreFor resolves the version store for a bucket given the
// server's zfs_versioning mode: "sidecar" | "snapshots" | "both"
// (Contract 1). Non-ZFS buckets always get the sidecar store; mode applies
// to ZFS-backed buckets only (leaf 03 wiring decides bucket kind — the
// factory takes the mode it is told to use). zdb may be nil for the
// sidecar mode (it is only needed by the ZFS snapshot store).
func versionStoreFor(bucketPath string, zdb *metadata.ZmetadDB, mode string) versionStore {
	switch mode {
	case "sidecar", "":
		return sidecarVersionStore{bucketPath: bucketPath}
	case "snapshots":
		return zfsSnapshotVersionStore{bucketPath: bucketPath, zdb: zdb}
	case "both":
		// Leaf 03: mergeStore{sidecarVersionStore{...},
		// zfsSnapshotVersionStore{...}}.
		return unimplementedVersionStore{mode: mode}
	default:
		return unimplementedVersionStore{mode: mode}
	}
}

// unimplementedVersionStore is the leaf-01 placeholder for the snapshots
// and both modes; every method returns errVersioningUnimplemented so a
// mis-wired mode fails loudly instead of silently corrupting state.
type unimplementedVersionStore struct {
	mode string
}

func (u unimplementedVersionStore) State(bucket string) (string, error) {
	return "", fmt.Errorf("%w: %s", errVersioningUnimplemented, u.mode)
}

func (u unimplementedVersionStore) SetState(bucket, state string) error {
	return fmt.Errorf("%w: %s", errVersioningUnimplemented, u.mode)
}

func (u unimplementedVersionStore) PutVersion(bucket, key string, r io.Reader, size int64, etag string) (VersionEntry, error) {
	return VersionEntry{}, fmt.Errorf("%w: %s", errVersioningUnimplemented, u.mode)
}

func (u unimplementedVersionStore) PutDeleteMarker(bucket, key string) (VersionEntry, error) {
	return VersionEntry{}, fmt.Errorf("%w: %s", errVersioningUnimplemented, u.mode)
}

func (u unimplementedVersionStore) List(bucket, key string) ([]VersionEntry, error) {
	return nil, fmt.Errorf("%w: %s", errVersioningUnimplemented, u.mode)
}

func (u unimplementedVersionStore) Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error) {
	return nil, VersionEntry{}, fmt.Errorf("%w: %s", errVersioningUnimplemented, u.mode)
}

// ---------- sidecarVersionStore (dotfile buckets + opt-in ZFS) ----------

// sidecarVersionStore implements versionStore over the per-object .meta
// sidecar plus versioned data files under
// <bucket>/.metadata/.versions/<key-sha>/<versionId> (key-sha = sha256 hex
// of the key, avoiding path-length/hostile-key issues). Sidecar updates go
// through writeFileAtomicJSON; per-key writes serialize via lockObject.
type sidecarVersionStore struct {
	bucketPath string
}

// versionedSidecar is the additive extension of the object sidecar
// (embedded LegacyObjectMetadata keeps every existing field and the old
// byte form; all new fields are omitempty). Version entries are stored
// newest-first.
type versionedSidecar struct {
	objectmodel.LegacyObjectMetadata
	// Versioning is the bucket-level state marker mirrored onto the
	// object sidecar: "Off"|"Enabled"|"Suspended". Empty = Off
	// (pre-versioning sidecars).
	Versioning string `json:",omitempty"`
	// Versions is the per-key version history (newest first).
	Versions []sidecarVersionEntry `json:",omitempty"`
	// CurrentVersionID is the ID of the latest non-delete-marker version
	// (empty when the latest entry is a delete marker or no versions
	// exist).
	CurrentVersionID string `json:",omitempty"`
}

// sidecarVersionEntry is one Versions[] element.
type sidecarVersionEntry struct {
	ID             string    `json:"id"`
	IsDeleteMarker bool      `json:"isDeleteMarker,omitempty"`
	Size           int64     `json:"size"`
	ETag           string    `json:"eTag"`
	LastModified   time.Time `json:"lastModified"`
}

// stateSidecar is the bucket-level versioning state marker file
// (<bucket>/.metadata/.versioning). Bucket-level state is
// path-independent, so it lives in ONE place per bucket rather than being
// mirrored per object; the object sidecar's Versioning field mirrors it
// for objects written while enabled (advisory).
type stateSidecar struct {
	State string `json:"state"`
}

func (s sidecarVersionStore) statePath() string {
	return filepath.Join(s.bucketPath, ".metadata", ".versioning")
}

func (s sidecarVersionStore) sidecarPath(key string) string {
	return filepath.Join(s.bucketPath, ".metadata", key+".meta")
}

// keySha hex-encodes sha256(key) — the versions-dir directory name for a
// key (pure function of the key, no path material from the key itself).
func keySha(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// versionsDir is <bucket>/.metadata/.versions/<key-sha>.
func (s sidecarVersionStore) versionsDir(key string) string {
	return filepath.Join(s.bucketPath, ".metadata", ".versions", keySha(key))
}

// versionDataPath is the data file for one versionId:
// <bucket>/.metadata/.versions/<key-sha>/<versionId>.
func (s sidecarVersionStore) versionDataPath(key, versionID string) string {
	return filepath.Join(s.versionsDir(key), versionID)
}

// newStateVersionID mints a version ID: <unix-micros>-<8hex> (opaque,
// lexicographically sortable within a key's history, matches the
// derived-listing txg style).
func newStateVersionID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("s3: generating version id suffix: %w", err)
	}
	return strconv.FormatInt(time.Now().UnixMicro(), 10) + "-" + hex.EncodeToString(b[:]), nil
}

// State reads the bucket-level versioning state. A missing marker file is
// "Off" (never-versioned buckets, zero behavior change).
func (s sidecarVersionStore) State(bucket string) (string, error) {
	raw, err := os.ReadFile(s.statePath()) //nolint:gosec // G703: bucketPath is the resolved bucket root, not client input.
	if err != nil {
		if os.IsNotExist(err) {
			return versioningOff, nil
		}
		return "", err
	}
	var sc stateSidecar
	if err := parseJSONStrictEnough(raw, &sc); err != nil {
		return "", fmt.Errorf("s3: parsing versioning state for %s: %w", bucket, err)
	}
	if sc.State == "" {
		return versioningOff, nil
	}
	return sc.State, nil
}

// SetState writes the bucket-level versioning state marker atomically.
// state must be one of Off/Enabled/Suspended.
func (s sidecarVersionStore) SetState(bucket, state string) error {
	switch state {
	case versioningOff, versioningEnabled, versioningSuspended:
	default:
		return fmt.Errorf("s3: invalid versioning state %q", state)
	}
	sc := stateSidecar{State: state}
	if err := writeFileAtomicJSON(s.statePath(), sc, 0o644); err != nil {
		return fmt.Errorf("s3: writing versioning state for %s: %w", bucket, err)
	}
	return nil
}

// readVersionedSidecar loads the object's sidecar into the versioned
// envelope. A missing sidecar is objectmodel.ErrNoSuchKey (same contract
// as the tag store); old sidecars without the new fields decode with zero
// values (backward-compat HARD requirement).
func (s sidecarVersionStore) readVersionedSidecar(key string) (versionedSidecar, error) {
	var vs versionedSidecar
	raw, err := os.ReadFile(s.sidecarPath(key)) //nolint:gosec // G703: key is validateObjectKey-checked at every handler entry.
	if err != nil {
		if os.IsNotExist(err) {
			return vs, objectmodel.ErrNoSuchKey(key)
		}
		return vs, err
	}
	if err := parseJSONStrictEnough(raw, &vs); err != nil {
		return vs, fmt.Errorf("s3: parsing sidecar for %s: %w", key, err)
	}
	return vs, nil
}

// writeVersionedSidecar atomically replaces the sidecar. Empty Versioning
// state and nil Versions omit the new fields entirely, restoring the
// byte-compat pre-versioning sidecar form.
func (s sidecarVersionStore) writeVersionedSidecar(key string, vs versionedSidecar) error {
	path := s.sidecarPath(key)
	// Nested keys map to nested sidecar paths; the store stays
	// self-sufficient even when called before the backend created the
	// sidecar tree.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("s3: creating sidecar dir for %s: %w", key, err)
	}
	return writeFileAtomicJSON(path, vs, 0o644)
}

// PutVersion writes one version: the data file under the versions dir
// (writeFileAtomic), then the sidecar Versions update (writeFileAtomicJSON
// under lockObject on the sidecar path — read-modify-write preserving
// every other sidecar field). The new entry is prepended newest-first and
// becomes the sole IsLatest holder.
func (s sidecarVersionStore) PutVersion(bucket, key string, r io.Reader, size int64, etag string) (VersionEntry, error) {
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
		return VersionEntry{}, fmt.Errorf("s3: creating versions dir for %s: %w", key, err)
	}
	if err := writeFileAtomic(s.versionDataPath(key, id), data, 0o644); err != nil {
		return VersionEntry{}, fmt.Errorf("s3: writing version data for %s: %w", key, err)
	}

	vs, err := s.readVersionedSidecar(key)
	if err != nil {
		// A version for a key with no sidecar yet is legal (the store
		// is self-contained); only parse errors are fatal.
		if !isNoSuchKeyErr(err) {
			return VersionEntry{}, err
		}
		vs = versionedSidecar{}
	}
	now := time.Now().UTC()
	entry := sidecarVersionEntry{
		ID:           id,
		Size:         size,
		ETag:         etag,
		LastModified: now,
	}
	vs.Versioning = versioningEnabled
	vs.Versions = append([]sidecarVersionEntry{entry}, vs.Versions...)
	vs.CurrentVersionID = id
	if err := s.writeVersionedSidecar(key, vs); err != nil {
		return VersionEntry{}, err
	}
	return VersionEntry{
		ID:             id,
		IsLatest:       true,
		IsDeleteMarker: false,
		Size:           size,
		ETag:           etag,
		LastModified:   now,
	}, nil
}

// PutDeleteMarker appends a delete-marker entry (newest-first, no data
// touched). The marker becomes the sole IsLatest holder.
func (s sidecarVersionStore) PutDeleteMarker(bucket, key string) (VersionEntry, error) {
	unlock := lockObject(s.sidecarPath(key))
	defer unlock()

	vs, err := s.readVersionedSidecar(key)
	if err != nil {
		// A delete marker for a key with no sidecar is still legal
		// (DELETE of a never-versioned key while versioning enabled);
		// only parse errors are fatal.
		if !isNoSuchKeyErr(err) {
			return VersionEntry{}, err
		}
		vs = versionedSidecar{}
	}
	now := time.Now().UTC()
	id, err := newStateVersionID()
	if err != nil {
		return VersionEntry{}, err
	}
	entry := sidecarVersionEntry{
		ID:             id,
		IsDeleteMarker: true,
		LastModified:   now,
	}
	vs.Versioning = versioningEnabled
	vs.Versions = append([]sidecarVersionEntry{entry}, vs.Versions...)
	vs.CurrentVersionID = "" // latest is a delete marker: nothing current
	if err := s.writeVersionedSidecar(key, vs); err != nil {
		return VersionEntry{}, err
	}
	return VersionEntry{
		ID:             id,
		IsLatest:       true,
		IsDeleteMarker: true,
		Size:           0,
		ETag:           "",
		LastModified:   now,
	}, nil
}

// List returns the key's version history newest-first, with IsLatest set
// on exactly the first entry. A key with no versions is objectmodel.ErrNoSuchKey.
func (s sidecarVersionStore) List(bucket, key string) ([]VersionEntry, error) {
	vs, err := s.readVersionedSidecar(key)
	if err != nil {
		return nil, err
	}
	if len(vs.Versions) == 0 {
		return nil, objectmodel.ErrNoSuchKey(key)
	}
	out := make([]VersionEntry, len(vs.Versions))
	for i, e := range vs.Versions {
		out[i] = VersionEntry{
			ID:             e.ID,
			IsLatest:       i == 0,
			IsDeleteMarker: e.IsDeleteMarker,
			Size:           e.Size,
			ETag:           e.ETag,
			LastModified:   e.LastModified,
		}
	}
	return out, nil
}

// Open opens one version's data file: versionID "" or the current
// version's ID reads the latest; a delete-marker ID is ErrIsDeleteMarker;
// an unknown ID is objectmodel.ErrNoSuchKey.
func (s sidecarVersionStore) Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error) {
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

// parseJSONStrictEnough decodes JSON tolerantly (unknown fields ignored —
// forward compatibility across sidecar writers).
func parseJSONStrictEnough(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}

// isNoSuchKeyErr reports whether err carries objectmodel NoSuchKey
// identity.
func isNoSuchKeyErr(err error) bool {
	if omErr, ok := errors.AsType[*objectmodel.Error](err); ok {
		return omErr.Code == objectmodel.CodeNoSuchKey
	}
	return false
}

// compile-time pins: the sidecar store implements the frozen Contract 1
// interface, and its legacy metadata embedding keeps the frozen sidecar
// JSON keys.
var (
	_ versionStore             = sidecarVersionStore{}
	_ objectmodel.LegacyObjectMetadata = versionedSidecar{}.LegacyObjectMetadata
)
