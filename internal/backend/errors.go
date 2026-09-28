package backend

import (
	"errors"
	"io/fs"

	"mini-s3/internal/objectmodel"
)

// ErrNotSupported is returned for an operation the backend cannot serve at
// all. Capabilities() is the advisory path; errors are the authoritative one.
var ErrNotSupported = errors.New("backend: operation not supported")

// ErrUnknownBackend is returned by registry lookups (leaf 03) for a backend
// type name with no registered constructor. Unknown types never fall back
// silently to fs.
var ErrUnknownBackend = errors.New("backend: unknown backend type")

// mappedError carries a caller-facing objectmodel.Error value while keeping
// the original backend-internal cause reachable (Unwrap exposes both, so
// errors.Is finds the cause and errors.As finds the model error — the %w
// contract of this package's mapping).
type mappedError struct {
	model objectmodel.Error
	cause error
}

// Error renders the caller-facing model message.
func (e *mappedError) Error() string { return e.model.Error() }

// Unwrap exposes both the model error and the original cause.
func (e *mappedError) Unwrap() []error { return []error{&e.model, e.cause} }

// ToObjectModelError maps a backend-internal failure onto a caller-facing
// error carrying an *objectmodel.Error while wrapping the cause so
// errors.Is still finds it (the seam rule: no raw *os.PathError escapes
// upward).
//
// Mapping: chains reaching fs.ErrNotExist → NoSuchKey; fs.ErrInvalid →
// InvalidArgument; anything else → InternalError. Values already carrying
// objectmodel.Error identity, the package sentinels, and nil pass through
// unchanged (no double-wrapping).
func ToObjectModelError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotSupported) || errors.Is(err, ErrUnknownBackend) {
		return err
	}
	var omErr *objectmodel.Error
	if errors.As(err, &omErr) {
		return err
	}
	code, status := objectmodel.CodeInternalError, 500
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code, status = objectmodel.CodeNoSuchKey, 404
	case errors.Is(err, fs.ErrInvalid):
		code, status = objectmodel.CodeInvalidArgument, 400
	}
	return &mappedError{model: *objectmodel.NewError(code, err.Error(), status), cause: err}
}
