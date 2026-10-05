// versioning_bridge.go — the PRODUCTION bridge the webdav frontend
// drives for protocol-independent version capture (quic-h3-2026-10
// leaf 05). The versioning machinery (capture/record/stores/seams)
// stays unexported; these are the same call sites the s3 handlers use,
// re-exported for the webdav package so capture logic is NEVER
// duplicated across frontends. The mode/zmetad resolution rides the
// SAME process seams (versionStoreForBucket → the installed
// zfs_versioning mode + the shared zmetad handle) — the webdav
// frontend builds no second config view.
package s3

// VersioningEnabled is the bucket-versioning state value ("Enabled")
// the write-path gates compare against.
const VersioningEnabled = versioningEnabled

// CapturedObjectVersion is the exported alias of the handler-level
// capture product (the pre-overwrite state that rides from the capture
// step to the record step).
type CapturedObjectVersion = capturedObjectVersion

// ErrNoPriorVersion is the capture sentinel for "the object does not
// exist" (a create records no version). Callers treat it as a nil
// capture, never a failure.
var ErrNoPriorVersion = errNoPriorVersion

// ErrDeleteMarkersUnsupported is returned by PutDeleteMarker on stores
// without delete-marker semantics (the ZFS snapshot store): the
// snapshots-mode DELETE surface (webdav maps it to 409, mirroring the
// s3 wire's Conflict mapping).
var ErrDeleteMarkersUnsupportedExported = errDeleteMarkersUnsupportedAlias

// ErrSnapshotsReadOnly is returned by PutVersion on the ZFS snapshot
// store (snapshots are host policy; per-write records cannot exist).
var ErrSnapshotsReadOnlyExported = errSnapshotsReadOnlyAlias

// Aliases keep the exported vars pointed at the package sentinels in
// one place (var-initialization order safety).
var (
	errDeleteMarkersUnsupportedAlias = ErrDeleteMarkersUnsupported
	errSnapshotsReadOnlyAlias        = ErrSnapshotsReadOnly
)

// VersionStoreStateForBucket reads the bucket-level versioning state
// through the request-resolved store (the same resolution the s3
// handlers use: installed mode + shared zmetad handle).
func VersionStoreStateForBucket(bucketPath, bucket string) (string, error) {
	return versionStoreForBucket(bucketPath).State(bucket)
}

// CaptureCurrentObjectVersionForBucket captures the object's CURRENT
// state for the record step, BEFORE the caller's plain overwrite.
// Dispatch by the installed zfs_versioning mode, byte-identical to the
// s3 PUT handler's capture (reflink/both clone; every other mode reads
// the old bytes). ErrNoPriorVersion means the object does not exist.
func CaptureCurrentObjectVersionForBucket(bucketPath, objectName string) (*CapturedObjectVersion, error) {
	captured, err := captureCurrentObjectVersion(bucketPath, objectName)
	if err != nil {
		return nil, err
	}
	return captured, nil
}

// RecordCapturedObjectVersionForBucket writes the captured pre-overwrite
// state as one version AFTER the successful backend Put (the backend
// owns the sidecar and rewrites it wholesale; this step restores the
// captured history beneath the new entry). Identical to the s3
// handler's record step, retention included.
func RecordCapturedObjectVersionForBucket(bucketPath, bucketName, objectName string, captured *CapturedObjectVersion) error {
	return recordCapturedObjectVersion(bucketPath, bucketName, objectName, captured)
}

// DeleteObjectVersionedMarkerForBucket is the webdav DELETE versioning
// branch: on a versioning-Enabled bucket a delete marker is recorded
// where the mode has them and the plain backend delete must be
// suppressed (data survives; plain reads answer 404 via the marker).
// Identical to the s3 DELETE handler's marker step.
func DeleteObjectVersionedMarkerForBucket(bucketPath, bucketName, objectName string) (suppressPlainDelete bool, err error) {
	return deleteObjectVersionedMarker(bucketPath, bucketName, objectName)
}
