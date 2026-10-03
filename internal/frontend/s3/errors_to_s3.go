// errors_to_s3.go — THE single fs/backend-error → S3 error mapping table
// (leaf-02 spec: the mapping table lives in ONE place). Handlers pass a
// backend/objectmodel error here and get (code, message, httpStatus) for
// writeS3Error. *os.PathError must never reach this point unwrapped: the
// seam contract (internal/backend) guarantees objectmodel.Error identity.
package s3

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// s3ErrorFrom returns the (code, message, status) triple for a backend
// error. Errors carrying objectmodel identity map via their code; the
// package sentinels and anything else fall back to InternalError. The
// "not exist." phrasing and statuses match the pre-seam writeS3Error calls
// verbatim so responses stay byte-identical.
//
// D2 (bughunt-gateway-2026-09-29): InternalError-class messages must stay
// generic on the wire — backend error strings carry absolute filesystem
// paths (old handlers used fixed strings). The code mapping is preserved;
// the message is replaced with the legacy fixed text and callers are
// responsible for logging the detailed error server-side.
func s3ErrorFrom(err error) (code, message string, status int) {
	if omErr, ok := errors.AsType[*objectmodel.Error](err); ok {
		switch omErr.Code {
		case objectmodel.CodeNoSuchBucket:
			// Pre-seam message (object_handlers.go NoSuchBucket sites).
			return "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound
		case objectmodel.CodeNoSuchKey:
			// Pre-seam message (getObjectHandler NoSuchKey sites).
			return "NoSuchKey", "The specified key does not exist.", http.StatusNotFound
		case objectmodel.CodeInvalidArgument:
			return "InvalidArgument", omErr.Message, http.StatusBadRequest
		case objectmodel.CodeInternalError:
			// Legacy fixed message: never render the backend's error
			// string (it embeds absolute paths) to the client.
			return "InternalError", "Internal Server Error", http.StatusInternalServerError
		default:
			return omErr.Code, omErr.Message, omErr.HTTPStatus
		}
	}
	switch {
	case errors.Is(err, backend.ErrNotSupported):
		return "NotImplemented", "The requested operation is not implemented.", http.StatusNotImplemented
	case errors.Is(err, ErrDeleteMarkersUnsupported):
		// Snapshots-mode DELETE on a versioning-enabled bucket: the
		// store refuses to fabricate a delete marker (the snapshot
		// window is the history). A 4xx conflict, never a 500 — found
		// by the zfs-validate section 10 probe (leaf 05).
		return "Conflict", "Delete markers are not supported by this bucket's versioning mechanism.", http.StatusConflict
	case errors.Is(err, ErrSnapshotsReadOnly):
		// Snapshots are host policy: per-write version records cannot
		// be created in snapshots mode. Same 4xx reasoning as above.
		return "Conflict", "This bucket's versioning mechanism is read-only.", http.StatusConflict
	default:
		return "InternalError", "Internal Server Error", http.StatusInternalServerError
	}
}

// writeS3ErrorFrom writes the mapped error response, logging the full
// error server-side (wire messages for 500-class errors are generic —
// see s3ErrorFrom).
func writeS3ErrorFrom(w http.ResponseWriter, err error) {
	code, message, status := s3ErrorFrom(err)
	if status >= http.StatusInternalServerError {
		log.Printf("Internal error (code %s): %v", code, logSafe(err))
	}
	writeS3Error(w, code, message, status)
}

// logSafe renders err for the server log: it replaces control characters
// (including CR/LF) with spaces so client-supplied text inside the error
// cannot forge or splice log lines (gosec G706).
func logSafe(err error) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, err.Error())
}
