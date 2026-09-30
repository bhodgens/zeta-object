// errors_test.go — leaf 03 Task 1: the davStatus mapping table and the
// <D:error> rendering.
package webdav

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

func TestDavStatus_Table(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"NoSuchKey", objectmodel.ErrNoSuchKey("k"), 404},
		{"NoSuchBucket", objectmodel.ErrNoSuchBucket("b"), 404},
		{"PreconditionFailed", objectmodel.ErrPreconditionFailed(), 412},
		{"BucketAlreadyExists", objectmodel.ErrBucketAlreadyExists("b"), 405},
		{"InvalidArgument", objectmodel.ErrInvalidArgument("x"), 400},
		{"AccessDenied", objectmodel.ErrAccessDenied(), 403},
		{"NotModified", objectmodel.ErrNotModified(), 304},
		{"NotImplemented", objectmodel.ErrNotImplemented("x"), 501},
		{"InternalError", objectmodel.ErrInternalError("x"), 500},
		{"unknown code", objectmodel.NewError("SomethingElse", "mystery", 418), 500},
		{"non-objectmodel error", errors.New("plain"), 500},
		{"nil-ish wrapped", fmt.Errorf("wrap: %w", objectmodel.ErrNoSuchKey("k")), 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := davStatus(tt.err); got != tt.want {
				t.Fatalf("davStatus = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWriteDavError_BodyAndHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeDavError(rec, 403, "propfind-finite-depth")
	if rec.Code != 403 {
		t.Fatalf("status = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/xml") || !strings.Contains(ct, "utf-8") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<D:error xmlns:D="DAV:">`) {
		t.Fatalf("body missing D:error root:\n%s", body)
	}
	if !strings.Contains(body, "<D:propfind-finite-depth/>") {
		t.Fatalf("body missing precondition:\n%s", body)
	}
	if !strings.HasPrefix(body, "<?xml") {
		t.Fatalf("body missing prolog:\n%s", body)
	}
}

func TestWriteDavErrorFrom_MessageEscaped(t *testing.T) {
	rec := httptest.NewRecorder()
	writeDavErrorFrom(rec, objectmodel.ErrNoSuchKey(`a<b>&c`))
	if rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `<b>`) {
		t.Fatalf("message not escaped:\n%s", body)
	}
	if !strings.Contains(body, "a&lt;b&gt;&amp;c") {
		t.Fatalf("escaped message missing:\n%s", body)
	}
}
