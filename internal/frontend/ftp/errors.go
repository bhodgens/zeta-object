// errors.go — objectmodel.Error → FTP reply mapping. FTP has no structured
// error channel: 5xx is permanent, 4xx transient (ctx cancel/timeouts → 426
// mid-transfer). The mapping is the ONLY place codes render as replies.
package ftp

import (
	"context"
	"errors"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// ftpError carries the reply code + message the driver wants surfaced.
type ftpError struct {
	code    int
	message string
}

// Error renders "code message" (the ftp reply text).
func (e *ftpError) Error() string { return e.message }

// mapBackendError converts a Backend error into an ftpError. Unknown errors
// map to 550 (action not taken) — never a silent success.
func mapBackendError(err error) error {
	if err == nil {
		return nil
	}
	if fe, ok := errors.AsType[*ftpError](err); ok {
		return fe
	}
	// ctx cancellation / deadline mid-operation: transient (4xx family).
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &ftpError{code: 426, message: "Operation aborted: connection closed (transfer interrupted)"}
	}
	oe, ok := errors.AsType[*objectmodel.Error](err)
	if !ok {
		return &ftpError{code: 550, message: "Action not taken: " + err.Error()}
	}
	switch oe.Code {
	case objectmodel.CodeNoSuchKey, objectmodel.CodeNoSuchBucket:
		return &ftpError{code: 550, message: "No such file or directory"}
	case objectmodel.CodeAccessDenied:
		return &ftpError{code: 550, message: "Access denied"}
	case objectmodel.CodeInvalidArgument:
		return &ftpError{code: 501, message: "Syntax error: invalid argument"}
	case objectmodel.CodeNotImplemented, objectmodel.CodePreconditionFailed, objectmodel.CodeNotModified:
		// Precondition/NotModified cannot occur (no conditional caps);
		// NotImplemented backs the degradation contract (502-class).
		return &ftpError{code: 502, message: "Command not implemented for this capability set"}
	case objectmodel.CodeBucketAlreadyExists:
		return &ftpError{code: 550, message: "Directory already exists"}
	default:
		return &ftpError{code: 550, message: "Action not taken"}
	}
}
