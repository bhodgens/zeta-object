// markerhide.go — the READ-side delete-marker visibility rule in ONE place,
// shared by every webdav plain view of an object (GET/HEAD today, PROPFIND
// as of the listing fix that introduced this file).
//
// The write side is the versioning DELETE: on a versioning-Enabled bucket
// it records a delete marker where the mode has them and SUPPRESSES the
// plain delete, so the data file survives on disk while the object is gone
// from every plain view. The s3 GET consults that marker
// (s3.PlainObjectDeleteMarker404, the export of plainObjectDeleteMarker404
// through versioning_bridge.go) and answers 404; without the same consult on
// a webdav view, that view reports an object the server elsewhere reports
// as deleted — and a client syncing from the listing re-downloads it.
//
// The consult is deliberately FAIL-OPEN, and that is s3 parity, not a
// webdav choice:
//
//   - a marker-read error (an unreadable or torn .metadata sidecar, a
//     transient EIO) proceeds with the plain view. s3's gate reads
//     `mErr == nil && markerHidden`, so it proceeds on error too. The
//     failure direction matters: hiding a readable object turns a
//     metadata fault into silent data disappearance for the client, which
//     is strictly worse than the bug being fixed.
//   - bucketPath == "" (no resolver wired) takes the plain path with no
//     consult at all — the unit-test seam, mirroring the write paths'
//     lockRoot-nil contract.
//
// get.go's deleteMarkerHides still carries its own copy of this rule (it
// also renders the 404 itself). It should delegate here once its file's
// owner is available; keeping the predicate pure here means that follow-up
// is a one-line change and no behavior move.
package webdav

import (
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// objectHiddenByDeleteMarker reports whether a plain webdav view of
// (bucket, key) must NOT see the object because the key's versioned
// history's latest entry is a delete marker.
//
// Every false arm is load-bearing, not defensive:
//
//   - key == "": the root and bucket-level collections are prefixes, not
//     objects — there is nothing a marker could hide.
//   - bucketPath == "": versioning is unwired (unit-test seam), so no
//     consult runs and the plain view is byte-identical to pre-versioning
//     behavior.
//   - a consult error: fail OPEN (see the file comment). A versioned
//     bucket whose sidecar cannot be read must still LIST its readable
//     objects; treating the error as "hidden" would silently drop real
//     objects from a listing during a transient fault.
//
// The caller is responsible for what to do with a true result: drop the
// row (PROPFIND) or answer 404 (GET/HEAD).
func (f *Frontend) objectHiddenByDeleteMarker(bucket, key string) bool {
	if key == "" {
		return false
	}
	bucketPath := f.bucketPath(bucket)
	if bucketPath == "" {
		return false
	}
	hidden, err := s3.PlainObjectDeleteMarker404(bucketPath, bucket, key)
	if err != nil {
		// s3 parity: the store could not answer, so the plain view
		// proceeds. Never a silent hide.
		return false
	}
	return hidden
}
