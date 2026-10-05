package h3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// membackend_test.go — an in-memory backend.Backend stub for the loopback
// serve tests (same discipline as the webdav package's stubBackend; the
// frozen backend seam is consumed read-only here).

type memObject struct {
	obj  objectmodel.Object
	body []byte
}

// memBackend is an in-memory Backend.
type memBackend struct {
	mu      sync.Mutex
	buckets map[string]map[string]*memObject // bucket -> key -> object
}

func newH3MemBackend() *memBackend {
	return &memBackend{buckets: map[string]map[string]*memObject{}}
}

var _ interface {
	Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error)
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error)
} = (*memBackend)(nil)

func (m *memBackend) Get(_ context.Context, bucket, key string, _ objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.buckets[bucket][key]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return io.NopCloser(bytes.NewReader(o.body)), o.obj, nil
}

func (m *memBackend) Put(_ context.Context, bucket, key string, data io.Reader, size int64, _ objectmodel.PutOptions) (objectmodel.Object, error) {
	body, err := io.ReadAll(data)
	if err != nil {
		return objectmodel.Object{}, fmt.Errorf("memBackend Put read: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[bucket]
	if !ok {
		b = map[string]*memObject{}
		m.buckets[bucket] = b
	}
	obj := objectmodel.Object{
		Key:          key,
		Size:         int64(len(body)),
		ETag:         fmt.Sprintf("etag-%s", key),
		LastModified: time.Now().UTC(),
		ContentType:  "text/plain",
	}
	b[key] = &memObject{obj: obj, body: body}
	return obj, nil
}

func (m *memBackend) Delete(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[bucket][key]; !ok {
		return objectmodel.ErrNoSuchKey(key)
	}
	delete(m.buckets[bucket], key)
	return nil
}

func (m *memBackend) Stat(_ context.Context, bucket, key string) (objectmodel.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.buckets[bucket][key]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return o.obj, nil
}

func (m *memBackend) List(_ context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.buckets[bucket] {
		keys = append(keys, k)
	}
	sortStrings(keys)
	page := objectmodel.ListPage{Objects: []objectmodel.Object{}}
	for _, k := range keys {
		if p.Prefix != "" && !strings.HasPrefix(k, p.Prefix) {
			continue
		}
		page.Objects = append(page.Objects, m.buckets[bucket][k].obj)
	}
	return page, nil
}

func (m *memBackend) Buckets(_ context.Context) ([]objectmodel.BucketInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []objectmodel.BucketInfo
	for b := range m.buckets {
		out = append(out, objectmodel.BucketInfo{Name: b})
	}
	return out, nil
}

func (m *memBackend) Capabilities() objectmodel.CapabilitySet {
	return objectmodel.CapabilitySet{}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
