// errors_to_s3.go — THE single fs/backend-error → S3 error mapping table
// (leaf-02 spec: the mapping table lives in ONE place). Handlers pass a
// backend/objectmodel error here and get (code, message, httpStatus) for
// writeS3Error. *os.PathError must never reach this point unwrapped: the
// seam contract (internal/backend) guarantees objectmodel.Error identity.
package s3

import (
	"errors"
	"net/http"

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// s3ErrorFrom returns the (code, message, status) triple for a backend
// error. Errors carrying objectmodel identity map via their code; the
// package sentinels and anything else fall back to InternalError. The
// "not exist." phrasing and statuses match the pre-seam writeS3Error calls
// verbatim so responses stay byte-identical.
func s3ErrorFrom(err error) (code, message string, status int) {
	var omErr *objectmodel.Error
	if errors.As(err, &omErr) {
		switch omErr.Code {
		case objectmodel.CodeNoSuchBucket:
			// Pre-seam message (object_handlers.go NoSuchBucket sites).
			return "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound
		case objectmodel.CodeNoSuchKey:
			// Pre-seam message (getObjectHandler NoSuchKey sites).
			return "NoSuchKey", "The specified key does not exist.", http.StatusNotFound
		case objectmodel.CodeInvalidArgument:
			return "InvalidArgument", omErr.Message, http.StatusBadRequest
		default:
			return omErr.Code, omErr.Message, omErr.HTTPStatus
		}
	}
	switch {
	case errors.Is(err, backend.ErrNotSupported):
		return "NotImplemented", "The requested operation is not implemented.", http.StatusNotImplemented
	default:
		return "InternalError", "Internal Server Error", http.StatusInternalServerError
	}
}

// writeS3ErrorFrom writes the mapped error response.
func writeS3ErrorFrom(w http.ResponseWriter, err error) {
	code, message, status := s3ErrorFrom(err)
	writeS3Error(w, code, message, status)
}
