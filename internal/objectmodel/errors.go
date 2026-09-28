package objectmodel

import "fmt"

// Error is the backend-neutral error taxonomy. Frontends map Code onto their
// protocol's error representation; HTTPStatus carries the canonical S3
// default for the S3 frontend.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Code constants use the exact S3 error-code strings.
const (
	CodeNoSuchKey           = "NoSuchKey"
	CodeNoSuchBucket        = "NoSuchBucket"
	CodePreconditionFailed  = "PreconditionFailed"
	CodeNotModified         = "NotModified"
	CodeBucketAlreadyExists = "BucketAlreadyExists"
	CodeNotImplemented      = "NotImplemented"
	CodeInvalidArgument     = "InvalidArgument"
	CodeAccessDenied        = "AccessDenied"
	CodeInternalError       = "InternalError"
)

// NewError builds a custom error; prefer the Err* constructors for known codes.
func NewError(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, HTTPStatus: status}
}

func ErrNoSuchKey(key string) *Error {
	return NewError(CodeNoSuchKey, fmt.Sprintf("The specified key does not exist: %s", key), 404)
}

func ErrNoSuchBucket(bucket string) *Error {
	return NewError(CodeNoSuchBucket, fmt.Sprintf("The specified bucket does not exist: %s", bucket), 404)
}

func ErrPreconditionFailed() *Error {
	return NewError(CodePreconditionFailed, "At least one of the pre-conditions you specified did not hold", 412)
}

func ErrNotModified() *Error {
	return NewError(CodeNotModified, "Not Modified", 304)
}

func ErrBucketAlreadyExists(bucket string) *Error {
	return NewError(CodeBucketAlreadyExists, fmt.Sprintf("The requested bucket name already exists: %s", bucket), 409)
}

func ErrNotImplemented(op string) *Error {
	return NewError(CodeNotImplemented, fmt.Sprintf("A header or query you provided implies functionality that is not implemented: %s", op), 501)
}

func ErrInvalidArgument(msg string) *Error {
	return NewError(CodeInvalidArgument, msg, 400)
}

func ErrAccessDenied() *Error {
	return NewError(CodeAccessDenied, "Access Denied", 403)
}

func ErrInternalError(msg string) *Error {
	return NewError(CodeInternalError, msg, 500)
}
