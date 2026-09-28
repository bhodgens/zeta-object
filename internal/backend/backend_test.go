package backend

import (
	"context"
	"io"
	"testing"

	"mini-s3/internal/objectmodel"
)

// compile-time shape probe: a stub satisfying the interface must exist.
type stubBackend struct{}

func (stubBackend) Get(context.Context, string, string, objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	return nil, objectmodel.Object{}, nil
}
func (stubBackend) Put(context.Context, string, string, io.Reader, int64, objectmodel.PutOptions) (objectmodel.Object, error) {
	return objectmodel.Object{}, nil
}
func (stubBackend) Delete(context.Context, string, string) error { return nil }
func (stubBackend) Stat(context.Context, string, string) (objectmodel.Object, error) {
	return objectmodel.Object{}, nil
}
func (stubBackend) List(context.Context, string, objectmodel.ListParams) (objectmodel.ListPage, error) {
	return objectmodel.ListPage{}, nil
}
func (stubBackend) Buckets(context.Context) ([]objectmodel.BucketInfo, error) { return nil, nil }
func (stubBackend) Capabilities() objectmodel.CapabilitySet                   { return objectmodel.CapabilitySet{} }

func TestBackendInterfaceShape(t *testing.T) {
	var _ Backend = stubBackend{} // compile-time assertion
}
