package sync

// helpers_test.go - fixtures shared by the leaf-04 acceptance tests:
// a temp cache dir laid out like the leaf-02 data plane (files/ +
// staging/), an index Store over a temp DB, and the memfs transport.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// fixedNow pins the engine clock so conflict-copy names are deterministic
// ("2026-10-06" - the exact shape the leaf names).
var fixedNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// harness is one engine under test with its temp world.
type harness struct {
	t     *testing.T
	dir   string
	fs    *transport.MemFS
	store *index.Store
	eng   *Engine
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"files", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatalf("cache dir: %v", err)
		}
	}
	dbPath := filepath.Join(dir, "index.db")
	st, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("index open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs := transport.NewMemFS()
	eng, err := NewEngine(Options{
		Transport: fs,
		Store:     st,
		CacheDir:  dir,
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return &harness{t: t, dir: dir, fs: fs, store: st, eng: eng}
}

// putLocal writes a cache file (files/<key>) like the FUSE layer would.
func (h *harness) putLocal(key, body string) {
	h.t.Helper()
	dst := h.eng.cachePath(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		h.t.Fatalf("mkdir for %s: %v", key, err)
	}
	if err := os.WriteFile(dst, []byte(body), 0o600); err != nil {
		h.t.Fatalf("write %s: %v", key, err)
	}
}

// localBody reads a cache file back ("" when absent).
func (h *harness) localBody(key string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.eng.cachePath(key)) // #nosec G304 -- test path
	if err != nil {
		return ""
	}
	return string(b)
}

// seedClean marks a key as clean+hydrated with the given (server-truth)
// etag, as after a confirmed upload or download.
func (h *harness) seedClean(key, etag string) {
	h.t.Helper()
	if err := h.store.PutPath(context.Background(), index.Resource{
		Path: key, ETag: etag, Size: int64(len(h.localBody(key))), Hydrated: true,
	}, "", "test seed"); err != nil {
		h.t.Fatalf("seed %s: %v", key, err)
	}
}

// seedDirty marks a key dirty (FUSE wrote bytes the server has not seen).
func (h *harness) seedDirty(key, knownETag string) {
	h.t.Helper()
	if err := h.store.PutPath(context.Background(), index.Resource{
		Path: key, ETag: knownETag, Size: int64(len(h.localBody(key))), Hydrated: true, Dirty: true,
	}, index.OpLocalWrite, "test seed dirty"); err != nil {
		h.t.Fatalf("seed dirty %s: %v", key, err)
	}
}

// row fetches an index row ("" etag row when absent).
func (h *harness) row(key string) index.Resource {
	h.t.Helper()
	r, err := h.store.Get(context.Background(), key)
	if err != nil {
		return index.Resource{Path: key}
	}
	return r
}
