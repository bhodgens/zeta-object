package objectmodel

import (
	"errors"
	"io"
	"testing"
)

func TestErrorImplementsError(t *testing.T) {
	var e error = ErrNoSuchKey("photos/cat.jpg")
	if !errors.As(e, new(*Error)) {
		t.Fatal("ErrNoSuchKey must return *Error usable as error")
	}
	if e.Error() == "" {
		t.Fatal("Error() must be non-empty")
	}
	if e, _ := errors.AsType[*Error](io.EOF); e != nil {
		// sanity: non-Error errors don't convert; keeps errors.As usage honest
		t.Fatal("io.EOF must not convert to *Error")
	}
}

func TestErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name   string
		got    *Error
		code   string
		status int
	}{
		{"NoSuchKey", ErrNoSuchKey("k"), CodeNoSuchKey, 404},
		{"NoSuchBucket", ErrNoSuchBucket("b"), CodeNoSuchBucket, 404},
		{"PreconditionFailed", ErrPreconditionFailed(), CodePreconditionFailed, 412},
		{"NotModified", ErrNotModified(), CodeNotModified, 304},
		{"BucketAlreadyExists", ErrBucketAlreadyExists("b"), CodeBucketAlreadyExists, 409},
		{"NotImplemented", ErrNotImplemented("versioning"), CodeNotImplemented, 501},
		{"InvalidArgument", ErrInvalidArgument("bad"), CodeInvalidArgument, 400},
		{"AccessDenied", ErrAccessDenied(), CodeAccessDenied, 403},
		{"InternalError", ErrInternalError("boom"), CodeInternalError, 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.Code != tc.code {
				t.Errorf("Code = %q, want %q", tc.got.Code, tc.code)
			}
			if tc.got.HTTPStatus != tc.status {
				t.Errorf("HTTPStatus = %d, want %d", tc.got.HTTPStatus, tc.status)
			}
			if tc.got.Message == "" {
				t.Error("Message should be populated by constructors")
			}
		})
	}
}

func TestNewErrorCustomStatus(t *testing.T) {
	e := NewError(CodeNoSuchKey, "custom", 499)
	if e.HTTPStatus != 499 || e.Code != CodeNoSuchKey {
		t.Errorf("NewError must honor explicit status: %+v", e)
	}
}
