// versioning.go — quic-h3-2026-10 leaf 05: protocol-independent version
// capture on the webdav write paths.
//
// A bucket with zfs_versioning enabled is versioned — the protocol that
// writes the bytes must not change that. Every webdav PUT/DELETE
// resolves the SAME per-request version store the s3 handlers resolve
// (s3.versionStoreForBucket → the installed zfs_versioning mode + the
// shared zmetad handle) and drives the SAME capture/record machinery:
//
//   - PUT overwrite (versioning Enabled): the OLD bytes are captured
//     BEFORE the plain overwrite; the version RECORD lands AFTER the
//     successful backend Put (the backend rewrites the sidecar
//     wholesale, so a pre-write record would be destroyed by the write
//     itself — the pinned capture/record invariant, identical order to
//     the s3 object handler branch).
//   - DELETE (versioning Enabled): a delete marker where the mode has
//     them (sidecar/reflink/both), plain delete suppressed — exactly
//     what s3's deleteObjectVersionedMarker records. In snapshots mode
//     the store refuses markers; webdav surfaces the same 409 Conflict
//     the s3 wire maps ErrDeleteMarkersUnsupported to. Off/Suspended:
//     plain delete, byte-identical pre-versioning path.
//   - MOVE (rename): a metadata op on the fs — it manufactures NO
//     versions (pinned no-op; the destination Put goes through the
//     capture path like any overwrite, the source Delete never records).
//
// Failure-mode parity with s3: a capture failure (other than
// errNoPriorVersion) FAILS CLOSED — the write is rejected before the
// backend Put, exactly as the s3 PUT handler rejects with 500
// (object_handlers.go: the versioning branch returns before any write
// when capture fails). The reflink clone failure is the one fail-OPEN
// case: s3's captureReflinkObjectVersion returns cloneOK=false and the
// PUT proceeds with no version record; webdav inherits that through the
// shared capture, so the webdav PUT proceeds identically.
//
// The webdav wire renders capture failures as 500 (davStatus's
// internal-error default — the generic <D:error> body), mirroring s3's
// generic InternalError; no versioning-specific wire form is added.
package webdav

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// versioningCapture is the pre-overwrite state produced by
// captureCurrentVersion (nil when nothing was captured: unversioned
// bucket, Suspended/Off state, or a create).
type versioningCapture = s3.CapturedObjectVersion

// versioningEnabled mirrors the s3 package's versioning state value.
const versioningEnabled = s3.VersioningEnabled

// captureBeforePut is the webdav PUT's versioning gate: resolve the
// store per request the way s3 does, read the bucket state, and capture
// the OLD object bytes when versioning is Enabled. Code order mirrors
// the s3 object handler branch exactly (state check → capture → the
// caller's backend Put → record).
//
// Returns (nil, false, nil) when the bucket is not versioned-Enabled
// (the plain path, byte-identical to pre-versioning behavior) or when
// the object is a create (ErrNoPriorVersion — nothing old to record).
// ok=true carries a live capture; a real capture failure returns an
// error and the caller must reject the write BEFORE the backend Put
// (fail-closed, s3 parity).
func captureBeforePut(bucketPath, bucket, key string) (captured *versioningCapture, ok bool, err error) {
	// Fail CLOSED on the state read (bughunt M6, s3 parity). The bucket
	// cannot say whether it is versioned, and the only safe answer to
	// "unknown" on a versioning gate is Enabled: the DELETE half of this
	// same file ALREADY fails closed (deleteObjectVersionedMarker returns
	// the state error, and handleDELETE answers 500 before any delete), so
	// swallowing it here made PUT and DELETE disagree about the SAME bucket
	// state. Treating the error as "not versioned" turned a transient EIO
	// or a torn .versioning marker into a silent unversioned overwrite -
	// the one fail-open hole in an otherwise fail-closed design (capture
	// failures already fail closed below). s3's identical gate
	// (object_handlers.go) rejects the same way, so both frontends produce
	// the SAME decision for the SAME state.
	//
	// A bucket that is genuinely never versioned is NOT affected: the
	// store answers Off with a nil error (one missing-file stat), which
	// takes the plain path exactly as before.
	state, err := s3.VersionStoreStateForBucket(bucketPath, bucket)
	if err != nil {
		log.Printf("webdav versioning: reading state for %s: %v (rejecting the write)", bucket, err)
		return nil, false, err
	}
	if state != versioningEnabled {
		return nil, false, nil
	}
	captured, capErr := s3.CaptureCurrentObjectVersionForBucket(bucketPath, key)
	if capErr != nil {
		if errors.Is(capErr, s3.ErrNoPriorVersion) {
			return nil, false, nil // a create — no prior version to record
		}
		return nil, false, capErr
	}
	return captured, true, nil
}

// recordAfterPut lands the sidecar record for a capture AFTER the
// successful backend Put (the backend rewrote the sidecar wholesale;
// the shared record step restores the captured history beneath the new
// entry). Record failures FAIL the response (s3 parity: the s3 handler
// answers 500 when the record step fails, after the data landed).
func recordAfterPut(bucketPath, bucket, key string, captured *versioningCapture) error {
	return s3.RecordCapturedObjectVersionForBucket(bucketPath, bucket, key, captured)
}

// deleteMarkerOrPlain implements the webdav DELETE versioning branch:
// on a versioning-Enabled bucket a delete marker is recorded where the
// mode has them and the plain backend delete is SUPPRESSED (data file
// stays; plain GET answers 404 via the marker). The bool reports
// whether the plain delete was suppressed; the error carries the store
// failure (including ErrDeleteMarkersUnsupported in snapshots mode —
// the caller maps it to 409, the s3 wire parity).
func deleteMarkerOrPlain(bucketPath, bucket, key string) (suppressPlainDelete bool, err error) {
	return s3.DeleteObjectVersionedMarkerForBucket(bucketPath, bucket, key)
}

// webdavVersioningConflict reports whether err is the
// snapshots-mode-refuses-markers class (mapped to 409 on the wire, the
// s3 error mapping's Conflict status for ErrDeleteMarkersUnsupported /
// ErrSnapshotsReadOnly).
func webdavVersioningConflict(err error) bool {
	return errors.Is(err, s3.ErrDeleteMarkersUnsupported) || errors.Is(err, s3.ErrSnapshotsReadOnly)
}

// moveSourceDelete is MOVE's source-delete half with the DELETE
// semantics the s3 CopyObject+DeleteObject counterpart produces. On a
// versioning-Enabled sidecar/reflink/both bucket that pair's Delete
// records a delete marker for the SOURCE and suppresses the plain
// delete (the data file survives) — the rename therefore records
// exactly what its s3 counterpart records, nothing more: the MOVE as a
// whole manufactures no version entry beyond what its own steps
// produce. Snapshots mode refuses the marker (ErrDeleteMarkers-
// Unsupported → 409); OFF/Suspended/unwired: plain delete, unchanged.
// A PURE rename of an unversioned key (never written while versioning
// was enabled) produces NO sidecar history at all — pinned by
// TestWebdavMOVE_NoVersionsPinned.
func (f *Frontend) moveSourceDelete(r *http.Request, src resource) error {
	// Gate on the RESOLVED path, not the resolver field (bughunt H1):
	// production wires only WithLockStoreRoot, so the old
	// `f.bucketPathFn != nil` test made this branch dead and a MOVE's
	// source delete destroyed the bytes instead of recording the marker
	// its s3 DeleteObject counterpart records.
	bucketPath := f.bucketPath(src.bucket)
	if bucketPath != "" {
		suppress, markerErr := deleteMarkerOrPlain(bucketPath, src.bucket, src.key)
		if markerErr != nil {
			log.Printf("webdav MOVE %s/%s: versioned source marker: %v", strconv.Quote(src.bucket), strconv.Quote(src.key), markerErr)
			return markerErr
		}
		if suppress {
			return nil
		}
	}
	return f.be.Delete(r.Context(), src.bucket, src.key)
}
