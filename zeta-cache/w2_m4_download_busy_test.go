package main

// w2_m4_download_busy_test.go - the W2-M4 IPC surface: a Download that
// hits a coalesced sync (the engine busy with another pass) must answer
// with a "sync in progress" error string - NOT the raw gate message
// ("sync: a pass is already running"), whose leading "sync:" prefix and
// generic wording the Swift error mapper could lump with
// connectivity-shaped failures (.serverUnreachable = domain-wide retry
// forever). "sync in progress" contains none of the mapper's
// connectivity markers, so it stays a per-item local error. Pinned as a
// response-shape test over a REAL socket.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
	zsync "github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// gateFS parks the Nth Propfind until release - enough to hold the
// engine's single-run gate while the IPC download races it.
type gateFS struct {
	*transport.MemFS
	mu     sync.Mutex
	n      int
	after  int           // park AFTER this many Propfinds
	block  chan struct{} // closed to release the parked call
	parked chan struct{} // closed once a call is parked
}

func (g *gateFS) Propfind(ctx context.Context, key string, recursive bool) ([]transport.Entry, error) {
	g.mu.Lock()
	g.n++
	park := g.n == g.after
	g.mu.Unlock()
	if park {
		select {
		case <-g.parked:
		default:
			close(g.parked)
		}
		select {
		case <-g.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.MemFS.Propfind(ctx, key, recursive)
}

// busyFixture is guiFixture's shape with an engine the test built
// itself (so the transport is the gate wrapper).
type busyFixture struct {
	t       *testing.T
	store   *index.Store
	fs      *gateFS
	handler *guiHandler
	path    string // socket path
}

// newFixtureOn serves handler over a short real unix socket.
func newFixtureOn(t *testing.T, cfg *config.Config, store *index.Store, eng *zsync.Engine, handler *guiHandler) *busyFixture {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "zc-busy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	sock := filepath.Join(tmp, "z.ipc")
	srv, err := ipc.ServeWithHandler(sock, cfg.ServerURL, cfg.Bucket,
		guiStatusSource{db: store, cfg: cfg, engine: eng}, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return &busyFixture{t: t, store: store, fs: nil, handler: handler, path: sock}
}

// ask sends one request over a fresh connection.
func (f *busyFixture) ask(req ipc.Request) ipc.Response {
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

func TestW2M4DownloadBusyNamesSyncInProgress(t *testing.T) {
	// The fixture builds its engine around the gate wrapper FROM THE
	// START (internal/sync owns its engine's fields; no setter seam).
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
	gfs := &gateFS{MemFS: transport.NewMemFS(), after: 1,
		block: make(chan struct{}), parked: make(chan struct{})}
	eng, err := zsync.NewEngine(zsync.Options{
		Transport: gfs, Store: store, CacheDir: dir,
		Now: func() time.Time { return time.Unix(1791288000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ServerURL: "https://srv", Bucket: "bkt", CacheDir: dir,
		Quota: config.Quota{MaxCacheBytes: 100000, Policy: "lru", HighWaterPct: 90, LowWaterPct: 70, TombstoneDays: 30},
	}
	handler := newGUIHandler(cfg, store, eng, nil)
	f := newFixtureOn(t, cfg, store, eng, handler)

	// Pass 1: parks inside the first Propfind, holding the gate.
	pass1Done := make(chan error, 1)
	go func() { pass1Done <- eng.SyncOnce(bgCtx()) }()
	select {
	case <-gfs.parked:
	case <-time.After(2 * time.Second):
		t.Fatal("pass 1 never parked inside Propfind")
	}

	// Remote-only file so the IPC Download MUST run a sync (a
	// materialized file answers without touching the gate).
	gfs.PutRaw("busy.txt", "body")
	resp := f.ask(ipc.Request{V: ipc.Version, Type: "download", Path: "busy.txt"})
	if resp.OK {
		close(gfs.block)
		<-pass1Done
		t.Fatal("download while a pass is live = ok, want the busy error")
	}
	if !strings.Contains(resp.Error, "sync in progress") {
		t.Fatalf("download busy error = %q, want the benign 'sync in progress' shape", resp.Error)
	}
	// The internal gate message must not leak to the wire.
	if strings.Contains(resp.Error, "already running") {
		t.Fatalf("download busy error = %q leaks the internal gate message", resp.Error)
	}
	// None of the Swift mapper's connectivity markers may appear - any
	// one of them maps the error to .serverUnreachable (domain-wide
	// retry forever), the exact outcome W2-M4 forbids.
	for _, marker := range []string{
		"cannot reach", "connection refused", "connection reset",
		"broken pipe", "socket", "closed the connection", "failed writing",
	} {
		if strings.Contains(strings.ToLower(resp.Error), marker) {
			t.Fatalf("download busy error = %q contains connectivity marker %q", resp.Error, marker)
		}
	}

	close(gfs.block)
	if err := <-pass1Done; err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	// After the gate frees, the download succeeds (the busy state was
	// transient, not a failure of the path).
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp = f.ask(ipc.Request{V: ipc.Version, Type: "download", Path: "busy.txt"})
		if resp.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("download after gate release still failing: %q", resp.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
