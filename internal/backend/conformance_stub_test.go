// The conformance suite's own regression harness: a trivial in-memory
// Backend proves the suite is runnable by third-party backends without a
// real storage layer. PERMANENT per the leaf spec — it keeps the suite
// honest as the fs backend evolves and offers leaf 03 a trivial backend for
// registry tests if useful.
package backend_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

func TestConformanceAgainstStub(t *testing.T) {
	RunBackendConformance(t, "stub", func(t *testing.T) backend.Backend {
		return newMemStub()
	})
}

// --- in-memory stub backend -------------------------------------------------

// memStub is a map-based Backend, concurrency-safe via a single mutex
// (minimally satisfying the interface's concurrency requirement).
type memStub struct {
	mu      sync.Mutex
	objects map[string]map[string]memObject // bucket -> key -> object
}

type memObject struct {
	data []byte
	meta objectmodel.Object
}

func newMemStub() backend.Backend {
	return &memStub{objects: map[string]map[string]memObject{}}
}

func (m *memStub) Get(ctx context.Context, bucket, key string, _ objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	if err := ctx.Err(); err != nil {
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

func (m *memStub) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	if err := ctx.Err(); err != nil {
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
		b = map[string]memObject{}
		m.objects[bucket] = b
	}
	meta := objectmodel.Object{
		Key:          key,
		Size:         int64(len(buf)),
		ETag:         objectmodel.QuotedETag(fmt.Sprintf("etag-%s-%d", key, time.Now().UnixNano())),
		LastModified: time.Now(),
		ContentType:  opts.ContentType,
		Metadata:     opts.Metadata,
	}
	b[key] = memObject{data: buf, meta: meta}
	return meta, nil
}

func (m *memStub) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[bucket]
	if !ok {
		return objectmodel.ErrNoSuchBucket(bucket)
	}
	// PIN honored: delete of a missing key in an existing bucket is a no-op.
	delete(b, key)
	return nil
}

func (m *memStub) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	if err := ctx.Err(); err != nil {
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

func (m *memStub) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	if err := ctx.Err(); err != nil {
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
						// BUGHUNT B12(a)/B1 semantics: a continuation
						// token lying INSIDE or AT a roll-up group
						// (token has the group as a prefix) means the
						// page that issued the token already emitted
						// the group — consume it instead of re-emitting.
						if marker != "" && strings.HasPrefix(marker, cp) {
							seenCP[cp] = true
							continue
						}
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

func (m *memStub) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	if err := ctx.Err(); err != nil {
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

func (m *memStub) Capabilities() objectmodel.CapabilitySet {
	return objectmodel.CapabilitySet{}
}
