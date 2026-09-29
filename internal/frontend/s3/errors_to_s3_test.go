package s3_test

// errors_to_s3_test.go — pin tests for the bughunt-gateway-2026-09-29
// error-mapping fixes:
//   - D2: InternalError-class wire bodies never contain the underlying
//     error string (which embeds absolute filesystem paths); the detailed
//     error goes to the server log only.
//   - D3: ListBuckets on an unreadable dataDir with no custom buckets
//     returns 500 InternalError (not a silent 200-empty listing).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mini-s3/internal/backend"
	"mini-s3/internal/frontend/s3"
	"mini-s3/internal/objectmodel"
)

// failingBucketsBackend is a Backend double whose Buckets() always fails
// (the unreadable-dataDir simulation). All other operations delegate to
// ErrInternalError — ListBuckets never touches them.
type failingBucketsBackend struct{}

func (failingBucketsBackend) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	return nil, errors.New("open /secret/dataDir: permission denied")
}

func (failingBucketsBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	return nil, objectmodel.Object{}, objectmodel.ErrInternalError("not implemented in test double")
}

func (failingBucketsBackend) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	return objectmodel.Object{}, objectmodel.ErrInternalError("not implemented in test double")
}

func (failingBucketsBackend) Delete(ctx context.Context, bucket, key string) error {
	return objectmodel.ErrInternalError("not implemented in test double")
}

func (failingBucketsBackend) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	return objectmodel.Object{}, objectmodel.ErrInternalError("not implemented in test double")
}

func (failingBucketsBackend) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	return objectmodel.ListPage{}, objectmodel.ErrInternalError("not implemented in test double")
}

func (failingBucketsBackend) Capabilities() objectmodel.CapabilitySet {
	return objectmodel.CapabilitySet{}
}

// TestS3ErrorFromInternalErrorHidesDetail pins D2 at the mapping level:
// the InternalError code mapping is preserved but the message is the
// generic fixed text, never the underlying error string.
func TestS3ErrorFromInternalErrorHidesDetail(t *testing.T) {
	leaky := objectmodel.NewError(objectmodel.CodeInternalError,
		"open /Users/someone/secret/dataDir/bucket/meta.json: permission denied", 500)
	code, message, status := s3.S3ErrorFromFn(leaky)
	if code != "InternalError" {
		t.Fatalf("code = %q, want InternalError", code)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if strings.Contains(message, "/") || strings.Contains(message, "secret") || strings.Contains(message, "permission") {
		t.Fatalf("message leaks underlying error detail: %q", message)
	}
	if message != "Internal Server Error" {
		t.Fatalf("message = %q, want the generic fixed text", message)
	}
}

// TestWriteS3ErrorFromNoPathLeak pins D2 on the wire: a 500 body must not
// carry any of the underlying error string (which embeds an absolute
// filesystem path).
func TestWriteS3ErrorFromNoPathLeak(t *testing.T) {
	leaky := errors.New("stat /var/folders/zz/tmp/dataDir/bucket: no such file or directory")

	rec := httptest.NewRecorder()
	s3.WriteS3ErrorFromFn(rec, leaky)
	body := rec.Body.String()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	for _, leak := range []string{"/var/folders", "dataDir", "no such file"} {
		if strings.Contains(body, leak) {
			t.Fatalf("500 body leaks underlying error content %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "InternalError") {
		t.Fatalf("500 body missing InternalError code: %s", body)
	}
}

// TestListBucketsUnreadableDataDir500 pins D3 end-to-end: ListBuckets
// over a dataDir whose discovery fails (no custom buckets configured)
// answers 500 InternalError, never a silent 200-empty listing.
func TestListBucketsUnreadableDataDir500(t *testing.T) {
	srv := newTestServer(t)
	// The installed lookup now resolves every bucket (including the
	// dataDir's "") to a backend whose Buckets() fails — the unreadable
	// dataDir simulation.
	s3.InstallBackendLookup(func(bucket string) (backend.Backend, error) {
		return failingBucketsBackend{}, nil
	})

	resp := doSigned(t, srv, "GET", "/", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("ListBuckets with failing discovery = %d (body %s), want 500", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "InternalError") {
		t.Fatalf("body missing InternalError code: %s", body)
	}
}
