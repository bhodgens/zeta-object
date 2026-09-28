package conformance

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// TestRunAgainstStub proves the exported cross-package runner (the one
// leaf-02's fsbackend and future backends import) executes the full suite.
// The stub here mirrors internal/backend's conformance_stub_test.go harness.
func TestRunAgainstStub(t *testing.T) {
	Run(t, "stub", func(t *testing.T) backend.Backend {
		return newRunnerStub()
	})
}

// --- in-memory stub backend -------------------------------------------------

type runnerStub struct {
	mu      sync.Mutex
	objects map[string]map[string]memObj // bucket -> key -> object
}

type memObj struct {
	data []byte
	meta objectmodel.Object
}

func newRunnerStub() backend.Backend {
	return &runnerStub{objects: map[string]map[string]memObj{}}
}

func ctxErr(ctx context.Context) error { return ctx.Err() }

func (m *runnerStub) Get(ctx context.Context, bucket, key string, _ objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, objectmodel.Object{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[bucket]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchBucket(bucket)
	}
	o, ok := b[key]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return io.NopCloser(bytes.NewReader(o.data)), o.meta, nil
}

func (m *runnerStub) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	if err := ctxErr(ctx); err != nil {
		return objectmodel.Object{}, err
	}
	buf, err := io.ReadAll(data)
	if err != nil {
		return objectmodel.Object{}, err
	}
	if size >= 0 && int64(len(buf)) != size {
		return objectmodel.Object{}, objectmodel.ErrInvalidArgument("size mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[bucket]
	if !ok {
		b = map[string]memObj{}
		m.objects[bucket] = b
	}
	meta := objectmodel.Object{
		Key:          key,
		Size:         int64(len(buf)),
		ETag:         objectmodel.QuotedETag("etag-" + key + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)),
		LastModified: time.Now(),
		ContentType:  opts.ContentType,
		Metadata:     opts.Metadata,
	}
	b[key] = memObj{data: buf, meta: meta}
	return meta, nil
}

func (m *runnerStub) Delete(ctx context.Context, bucket, key string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[bucket]
	if !ok {
		return objectmodel.ErrNoSuchBucket(bucket)
	}
	delete(b, key) // PIN: missing key in existing bucket is an idempotent no-op
	return nil
}

func (m *runnerStub) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	if err := ctxErr(ctx); err != nil {
		return objectmodel.Object{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[bucket]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchBucket(bucket)
	}
	o, ok := b[key]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return o.meta, nil
}

func (m *runnerStub) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	if err := ctxErr(ctx); err != nil {
		return objectmodel.ListPage{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[bucket]
	if !ok {
		return objectmodel.ListPage{}, objectmodel.ErrNoSuchBucket(bucket)
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Effective marker: continuation token (opaque, here the last key) or
	// StartAfter. MaxKeys <= 0 = implementation default (no truncation for
	// small fixtures).
	marker := max(p.ContinuationToken, p.StartAfter)
	maxKeys := p.MaxKeys
	if maxKeys <= 0 {
		maxKeys = len(keys) + 1
	}

	page := objectmodel.ListPage{}
	seenCP := map[string]bool{}
	for _, k := range keys {
		if k <= marker || !strings.HasPrefix(k, p.Prefix) {
			continue
		}
		if p.Delimiter != "" {
			if rest, found := strings.CutPrefix(k, p.Prefix); found {
				if idx := strings.Index(rest, p.Delimiter); idx >= 0 {
					cp := p.Prefix + rest[:idx+len(p.Delimiter)]
					if !seenCP[cp] {
						seenCP[cp] = true
						page.CommonPrefixes = append(page.CommonPrefixes, cp)
					}
					if len(page.Objects)+len(page.CommonPrefixes) >= maxKeys {
						page.IsTruncated = true
						page.NextToken = cp
						return page, nil
					}
					continue
				}
			}
		}
		page.Objects = append(page.Objects, b[k].meta)
		if len(page.Objects)+len(page.CommonPrefixes) >= maxKeys {
			page.IsTruncated = true
			page.NextToken = k
			return page, nil
		}
	}
	return page, nil
}

func (m *runnerStub) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.objects))
	for name := range m.objects {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]objectmodel.BucketInfo, 0, len(names))
	for _, name := range names {
		out = append(out, objectmodel.BucketInfo{Name: name})
	}
	return out, nil
}

func (m *runnerStub) Capabilities() objectmodel.CapabilitySet {
	return objectmodel.CapabilitySet{}
}
