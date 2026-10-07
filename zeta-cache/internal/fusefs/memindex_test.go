package fusefs

// memindex_test.go - in-memory Index seam stub for unit tests (the real
// DB is leaf 03's; the placeholder internal/index has no schema yet).

import (
	"sort"
	"strings"
	"sync"
)

type memRow struct {
	etag  string // last clean server ETag ("" unknown)
	dirty bool
	isDir bool
	size  int64
	mtime int64
}

type memIndex struct {
	mu   sync.Mutex
	rows map[string]*memRow
}

func newMemIndex() *memIndex { return &memIndex{rows: map[string]*memRow{}} }

func (m *memIndex) CleanETag(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[key]; ok && !r.dirty {
		return r.etag, nil
	}
	return "", nil
}

func (m *memIndex) MarkDirty(key string, size, mtime int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(key)
	r.dirty = true
	r.size = size
	r.mtime = mtime
	return nil
}

func (m *memIndex) MarkClean(key string, etag string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(key)
	r.dirty = false
	r.etag = etag
	if size > 0 || r.size == 0 {
		r.size = size
	}
	return nil
}

func (m *memIndex) RecordHydrated(key, etag string, size, mtime int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(key)
	r.dirty = false
	r.etag = etag
	r.size = size
	r.mtime = mtime
	return nil
}

func (m *memIndex) DirtyKeys() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k, r := range m.rows {
		if r.dirty && !r.isDir {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memIndex) DirtyCount() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.dirty && !r.isDir {
			n++
		}
	}
	return n, nil
}

func (m *memIndex) HydratedCount() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, r := range m.rows {
		if !r.isDir && (r.etag != "" || !r.dirty) && k != "" {
			n++
		}
	}
	return n, nil
}

func (m *memIndex) ListPrefixed(dirKey string) ([]IndexRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := ""
	if dirKey != "" {
		prefix = dirKey + "/"
	}
	var out []IndexRow
	seen := map[string]bool{}
	keys := make([]string, 0, len(m.rows))
	for k := range m.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || k == dirKey {
			continue
		}
		rel := strings.TrimSuffix(k[len(prefix):], "/")
		if rel == "" || strings.Contains(rel, "/") || seen[rel] {
			continue
		}
		seen[rel] = true
		r := m.rows[k]
		out = append(out, IndexRow{Key: k, IsDir: r.isDir, Size: r.size, Mtime: r.mtime})
	}
	return out, nil
}

func (m *memIndex) Forget(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, key)
	return nil
}

func (m *memIndex) MoveKey(oldKey, newKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[oldKey]; ok {
		delete(m.rows, oldKey)
		m.rows[newKey] = r
	}
	return nil
}

func (m *memIndex) row(key string) *memRow {
	r, ok := m.rows[key]
	if !ok {
		r = &memRow{}
		m.rows[key] = r
	}
	return r
}

// isDirty is a test assertion helper.
func (m *memIndex) isDirty(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[key] != nil && m.rows[key].dirty
}

var _ Index = (*memIndex)(nil)
