package main

// gui_handler_test.go - leaf-08 daemon-side tests: the full guiHandler
// over a REAL unix socket with a REAL engine + scheduler (memfs-backed),
// plus the honest refusals of EvictPath and the status source.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// guiFixture is a scheduler-less full stack: store + engine + handler,
// served over a real socket. The scheduler is nil ON PURPOSE for most
// tests (Pause/Resume answer the capability error); the pin/tombstone
// tests use the scheduler variant below.
type guiFixture struct {
	t      *testing.T
	dir    string
	store  *index.Store
	fs     *transport.MemFS
	engine *zsync.Engine
	path   string // socket path
}

func newGuiFixture(t *testing.T) *guiFixture {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"files", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := index.Open(filepath.Join(dir, "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fs := transport.NewMemFS()
	eng, err := zsync.NewEngine(zsync.Options{
		Transport: fs, Store: store, CacheDir: dir,
		Now: func() time.Time { return time.Unix(1791288000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &guiFixture{t: t, dir: dir, store: store, fs: fs, engine: eng}

	cfg := &config.Config{
		ServerURL: "https://srv", Bucket: "bkt", CacheDir: dir,
		Quota: config.Quota{MaxCacheBytes: 100000, Policy: "lru", HighWaterPct: 90, LowWaterPct: 70, TombstoneDays: 30},
	}
	handler := newGUIHandler(cfg, store, eng, nil)
	// macOS's sockaddr_un sun_path is ~104 bytes: keep the socket path
	// short (the same trick internal/ipc's tests use).
	tmp, err := os.MkdirTemp("/tmp", "zc-gui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	sock := filepath.Join(tmp, "z.ipc")
	srv, err := ipc.ServeWithHandler(sock, cfg.ServerURL, cfg.Bucket, guiStatusSource{db: store, cfg: cfg, engine: eng}, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	f.path = sock
	return f
}

// ask sends one request over a fresh connection.
func (f *guiFixture) ask(req ipc.Request) ipc.Response {
	f.t.Helper()
	conn, err := net.Dial("unix", f.path)
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		f.t.Fatalf("encode: %v", err)
	}
	var resp ipc.Response
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		f.t.Fatalf("decode: %v", err)
	}
	return resp
}

// bgCtx is the test context.
func bgCtx() context.Context { return context.Background() }

// cachePath mirrors the engine's on-disk layout (<cacheDir>/files/<key>).
func (f *guiFixture) cachePath(key string) string {
	return filepath.Join(f.dir, "files", filepath.FromSlash(key))
}

// arrangeConflict runs the matrix-3 conflict through a real SyncOnce.
func (f *guiFixture) arrangeConflict() {
	f.t.Helper()
	f.fs.PutRaw("doc.txt", "v2")
	if err := os.WriteFile(f.cachePath("doc.txt"), []byte("local edit"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "doc.txt", ETag: transport.MD5Hex([]byte("v1")), Size: 2, Hydrated: true, Dirty: true,
	}, index.OpLocalWrite, "test arrange"); err != nil {
		f.t.Fatal(err)
	}
	if err := f.engine.SyncOnce(bgCtx()); err != nil {
		f.t.Fatal(err)
	}
}

func TestGuiHandlerConflictsResolveOverSocket(t *testing.T) {
	f := newGuiFixture(t)
	f.arrangeConflict()

	resp := f.ask(ipc.Request{V: ipc.Version, Type: "conflicts.list"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("conflicts.list = (%v, %q)", resp.OK, resp.Error)
	}
	b, _ := json.Marshal(resp.Extra)
	var list ipc.ConflictData
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Conflicts) != 1 || list.Conflicts[0].Path != "doc.txt" ||
		list.Conflicts[0].Kind != ipc.KindConflictCopy {
		t.Fatalf("conflicts = %s", b)
	}

	resp = f.ask(ipc.Request{V: ipc.Version, Type: "conflicts.resolve", Path: "doc.txt", Mode: ipc.ModeKeepLocal})
	if !resp.OK {
		t.Fatalf("resolve keep-local = %q", resp.Error)
	}
	if got := f.fs.ServerBodyOrEmpty("doc.txt"); got != "local edit" {
		t.Errorf("server after keep-local = %q, want the local version", got)
	}
	// Record cleared: the list is now empty.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "conflicts.list"})
	b, _ = json.Marshal(resp.Extra)
	_ = json.Unmarshal(b, &list)
	if len(list.Conflicts) != 0 {
		t.Errorf("conflicts after resolve = %s", b)
	}
}

func TestGuiHandlerDeletedRestoreOverSocket(t *testing.T) {
	f := newGuiFixture(t)
	// Locally deleted only: the server still serves it -> restore hit.
	f.fs.PutRaw("gone.txt", "server copy")
	if err := os.WriteFile(f.cachePath("gone.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "gone.txt", ETag: transport.MD5Hex([]byte("x")), Size: 1, Hydrated: true,
	}, "", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeletePath(bgCtx(), "gone.txt", "test"); err != nil {
		t.Fatal(err)
	}

	resp := f.ask(ipc.Request{V: ipc.Version, Type: "deleted.list"})
	if !resp.OK {
		t.Fatalf("deleted.list = %q", resp.Error)
	}
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "deleted.restore", Path: "gone.txt"})
	if !resp.OK {
		t.Fatalf("restore = %q", resp.Error)
	}
	b, err := os.ReadFile(f.cachePath("gone.txt")) // #nosec G304 -- test path
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "server copy" {
		t.Errorf("restored body = %q", string(b))
	}

	// Honest miss: the server no longer serves it.
	_ = f.fs.Delete(bgCtx(), "gone.txt", "")
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "deleted.restore", Path: "gone.txt"})
	if resp.OK || resp.Error == "" {
		t.Fatalf("restore of a server-gone path = ok, want honest error")
	}
}

func TestGuiHandlerEvictRefusals(t *testing.T) {
	f := newGuiFixture(t)
	// Unknown path.
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "evict", Path: "nope.bin"}); resp.OK {
		t.Error("evict unknown = ok, want refusal")
	}
	// Dirty path.
	f.fs.PutRaw("d.txt", "v")
	if err := os.WriteFile(f.cachePath("d.txt"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "d.txt", ETag: transport.MD5Hex([]byte("v")), Size: 1, Hydrated: true, Dirty: true,
	}, index.OpLocalWrite, "test dirty"); err != nil {
		t.Fatal(err)
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "evict", Path: "d.txt"}); resp.OK {
		t.Error("evict dirty = ok, want refusal")
	}
	// Pinned path.
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "p.txt", ETag: "e", Size: 1, Hydrated: true, Pinned: true,
	}, "", "seed pinned"); err != nil {
		t.Fatal(err)
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "evict", Path: "p.txt"}); resp.OK {
		t.Error("evict pinned = ok, want refusal")
	}
	// Clean + hydrated path evicts (row demoted, file gone).
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "c.txt", ETag: "e", Size: 1, Hydrated: true,
	}, "", "seed clean"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cachePath("c.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "evict", Path: "c.txt"}); !resp.OK {
		t.Fatalf("evict clean = %q, want ok", resp.Error)
	}
	if _, err := os.Stat(f.cachePath("c.txt")); !os.IsNotExist(err) {
		t.Error("cache file survived evict")
	}
	row, err := f.store.Get(bgCtx(), "c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if row.Hydrated {
		t.Errorf("row still hydrated after evict: %+v", row)
	}
}

func TestGuiHandlerStatusLive(t *testing.T) {
	f := newGuiFixture(t)
	f.arrangeConflict()
	resp := f.ask(ipc.Request{V: ipc.Version, Type: "status"})
	if !resp.OK || resp.Data == nil {
		t.Fatalf("status = (%v, %q)", resp.OK, resp.Error)
	}
	if resp.Data.Conflicts != 1 {
		t.Errorf("live conflicts = %d, want 1", resp.Data.Conflicts)
	}
	if resp.Data.State != zsync.StateIdle {
		t.Errorf("live state = %q", resp.Data.State)
	}
	if resp.Data.Server != "https://srv" || resp.Data.Bucket != "bkt" {
		t.Errorf("identity fields = %q/%q", resp.Data.Server, resp.Data.Bucket)
	}
}

func TestGuiHandlerPauseWithoutSchedulerIsHonest(t *testing.T) {
	f := newGuiFixture(t) // scheduler is nil
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "pause"}); resp.OK {
		t.Error("pause without scheduler = ok, want capability error")
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "resume"}); resp.OK {
		t.Error("resume without scheduler = ok, want capability error")
	}
}
