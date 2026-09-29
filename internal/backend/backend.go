// Package backend defines the pluggable data-plane seam between mini-s3's
// protocol layer (S3 handlers today; WebDAV/SFTP frontends later) and storage
// implementations (local filesystem today; ZFS ring, S3 proxy later).
//
// Seam rules, binding for every implementation:
//
//   - A Backend NEVER returns a raw *os.PathError (or any fs-flavored error)
//     upward. Caller-facing failures are reported as values matching
//     *objectmodel.Error (use ToObjectModelError at the fs boundary);
//     *objectmodel.Error identity is what frontends switch on.
//   - Every method honors ctx cancellation: checks at loop and IO boundaries,
//     and operations abandoned via ctx must return an error, never panic.
//   - Implementations MUST be safe for concurrent use by multiple goroutines.
//   - Multipart-upload orchestration lives ABOVE the seam in v1; backends see
//     only whole-object Put/Get/Delete/Stat/List.
//
// Errors are authoritative: Capabilities() is advisory metadata a caller may
// consult to degrade gracefully, but if an operation cannot be served the
// backend reports it via an error (ErrNotSupported or an objectmodel code),
// never by a silent no-op contradicting Capabilities.
package backend

import (
	"context"
	"io"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// Backend is the pluggable data-plane seam. Implementations MUST be safe for
// concurrent use and MUST return errors matching objectmodel.Error codes for
// caller-facing conditions.
type Backend interface {
	Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error)
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error)
	Delete(ctx context.Context, bucket, key string) error
	Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error)
	List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error)
	Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error)
	Capabilities() objectmodel.CapabilitySet
}
