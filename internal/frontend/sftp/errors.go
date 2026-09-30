// errors.go — objectmodel.Error → sftp.StatusCode mapping. This is the
// ONLY place backend errors render as SSH_FX_* statuses:
//   - NoSuchKey/NoSuchBucket            → SSH_FX_NO_SUCH_FILE
//   - AccessDenied (grant denials)      → SSH_FX_PERMISSION_DENIED
//   - unrepresentable ops (degradation) → SSH_FX_OP_UNSUPPORTED
//   - ctx cancel                        → SSH_FX_FAILURE
//   - anything else                     → SSH_FX_FAILURE (never a silent
//     success that faked the capability).
package sftp

import (
	"context"
	"errors"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
	"github.com/pkg/sftp"
)

// errUnsupported is the capability-degradation error (SSH_FX_OP_UNSUPPORTED).
var errUnsupported = sftp.ErrSSHFxOpUnsupported

// errPermission is the denial error (SSH_FX_PERMISSION_DENIED).
var errPermission = sftp.ErrSSHFxPermissionDenied

// errNoSuchFile is the miss error (SSH_FX_NO_SUCH_FILE).
var errNoSuchFile = sftp.ErrSSHFxNoSuchFile

// mapBackendError converts a Backend error into a protocol-appropriate
// sftp status error. Unknown errors map to SSH_FX_FAILURE.
func mapBackendError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sftp.ErrSSHFxFailure
	}
	var oe *objectmodel.Error
	if !errors.As(err, &oe) {
		return sftp.ErrSSHFxFailure
	}
	switch oe.Code {
	case objectmodel.CodeNoSuchKey, objectmodel.CodeNoSuchBucket:
		return sftp.ErrSSHFxNoSuchFile
	case objectmodel.CodeAccessDenied:
		return sftp.ErrSSHFxPermissionDenied
	case objectmodel.CodeNotImplemented, objectmodel.CodePreconditionFailed, objectmodel.CodeNotModified:
		// Cannot occur under the declared caps; keep degradation honest.
		return sftp.ErrSSHFxOpUnsupported
	case objectmodel.CodeInvalidArgument:
		return sftp.ErrSSHFxOpUnsupported
	case objectmodel.CodeBucketAlreadyExists:
		return sftp.ErrSSHFxFailure
	default:
		return sftp.ErrSSHFxFailure
	}
}
