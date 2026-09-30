// stubbackend_test.go — the recording Backend stub shared by the webdav
// package tests (leaf 01/02/03's table tests). All methods record or serve
// deterministic state; no filesystem access.
package webdav

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// timeNow is the test clock (UTC, trimmed — deterministic headers).
func timeNow() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }

// stubBackend is an in-memory Backend with paging support for List.
type stubBackend struct {
	mu       sync.Mutex
	objects  map[string]objectmodel.Object     // key → object (content in bodies)
	bodies   map[string][]byte                 // key → content
	pages    map[string][]objectmodel.ListPage // per-bucket scripted pages (pagination tests)
	putCalls []putCall
	calls    []string
	failStat bool // force Stat to return a non-not-found error
}

type putCall struct {
	bucket, key string
	size        int64
	contentType string
	body        []byte
	ifMatch     string
	ifNoneMatch string
}

func newStubBackend() *stubBackend {
	return &stubBackend{
		objects: map[string]objectmodel.Object{},
		bodies:  map[string][]byte{},
		pages:   map[string][]objectmodel.ListPage{},
	}
}

// seed puts an object directly into the store.
func (s *stubBackend) seed(bucket, key string, content []byte, opts ...func(*objectmodel.Object)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj := objectmodel.Object{
		Key:          key,
		Size:         int64(len(content)),
		ETag:         fmt.Sprintf("etag-%s-%s", bucket, key),
		LastModified: timeNow(),
		ContentType:  "text/plain",
	}
	for _, o := range opts {
		o(&obj)
	}
	s.objects[bucket+"\x00"+key] = obj
	s.bodies[bucket+"\x00"+key] = content
}

func (s *stubBackend) Get(_ context.Context, bucket, key string, _ objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "get")
	obj, ok := s.objects[bucket+"\x00"+key]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return io.NopCloser(bytes.NewReader(s.bodies[bucket+"\x00"+key])), obj, nil
}

func (s *stubBackend) Put(_ context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, err := io.ReadAll(data)
	if err != nil {
		return objectmodel.Object{}, err
	}
	s.putCalls = append(s.putCalls, putCall{
		bucket: bucket, key: key, size: size,
		contentType: opts.ContentType, body: body,
		ifMatch: opts.IfMatch, ifNoneMatch: opts.IfNoneMatch,
	})
	obj := objectmodel.Object{
		Key:          key,
		Size:         int64(len(body)),
		ETag:         fmt.Sprintf("etag-%s-%s-new", bucket, key),
		LastModified: timeNow(),
		ContentType:  opts.ContentType,
	}
	s.objects[bucket+"\x00"+key] = obj
	s.bodies[bucket+"\x00"+key] = body
	return obj, nil
}

func (s *stubBackend) Delete(_ context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "delete:"+bucket+"/"+key)
	delete(s.objects, bucket+"\x00"+key)
	delete(s.bodies, bucket+"\x00"+key)
	return nil
}

func (s *stubBackend) Stat(_ context.Context, bucket, key string) (objectmodel.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "stat")
	if s.failStat {
		return objectmodel.Object{}, objectmodel.ErrInternalError("forced stat failure")
	}
	obj, ok := s.objects[bucket+"\x00"+key]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return obj, nil
}

func (s *stubBackend) List(_ context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "list")
	// Scripted pages win (pagination tests): each List call pops the next
	// scripted page until the queue is empty.
	if pages, ok := s.pages[bucket]; ok {
		if len(pages) == 0 {
			return objectmodel.ListPage{}, nil
		}
		page := pages[0]
		s.pages[bucket] = pages[1:]
		return page, nil
	}
	var keys []string
	for fullKey := range s.objects {
		b, k, _ := strings.Cut(fullKey, "\x00")
		if b != bucket || !strings.HasPrefix(k, p.Prefix) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	page := objectmodel.ListPage{}
	seenPrefix := map[string]bool{}
	for _, k := range keys {
		rest := strings.TrimPrefix(k, p.Prefix)
		if p.Delimiter != "" {
			if idx := strings.Index(rest, p.Delimiter); idx >= 0 {
				cp := p.Prefix + rest[:idx+len(p.Delimiter)]
				if !seenPrefix[cp] {
					seenPrefix[cp] = true
					page.CommonPrefixes = append(page.CommonPrefixes, cp)
				}
				continue
			}
		}
		page.Objects = append(page.Objects, s.objects[bucket+"\x00"+k])
	}
	return page, nil
}

func (s *stubBackend) Buckets(_ context.Context) ([]objectmodel.BucketInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "buckets")
	seen := map[string]bool{}
	var out []objectmodel.BucketInfo
	for fullKey := range s.objects {
		b, _, _ := strings.Cut(fullKey, "\x00")
		if !seen[b] {
			seen[b] = true
			out = append(out, objectmodel.BucketInfo{Name: b})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *stubBackend) Capabilities() objectmodel.CapabilitySet { return objectmodel.CapabilitySet{} }

// hasKey reports seeded presence (test assertions).
func (s *stubBackend) hasKey(bucket, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[bucket+"\x00"+key]
	return ok
}
