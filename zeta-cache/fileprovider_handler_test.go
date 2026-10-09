package main

// fileprovider_handler_test.go - fileprovider-2026-10 leaf-01 daemon-side
// tests: the guiHandler's FileProviderSource surface over a REAL unix
// socket with a REAL engine (memfs-backed transport): enumerate, item,
// download, dehydrate, mark/delete/move, plus the honest refusals.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// seedClean puts a clean hydrated row + disk file (the steady state).
func seedClean(t *testing.T, f *guiFixture, key, body string) {
	t.Helper()
	f.fs.PutRaw(key, body)
	if err := os.MkdirAll(filepath.Dir(f.cachePath(key)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cachePath(key), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: key, ETag: transport.MD5Hex([]byte(body)), Mtime: 1728211200,
		Size: int64(len(body)), Hydrated: true,
	}, "", "test seed"); err != nil {
		t.Fatal(err)
	}
}

// remarshalExtra round-trips the generic Extra value through JSON into a
// typed payload (what a non-Go client experiences on the wire).
func remarshalExtra(t *testing.T, v any, into any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

func TestFileProviderEnumerateWire(t *testing.T) {
	f := newGuiFixture(t)
	seedClean(t, f, "docs/a.txt", "alpha")
	seedClean(t, f, "docs/sub/b.txt", "beta")
	seedClean(t, f, "top.txt", "top")
	// A tombstoned row must never surface.
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "docs/gone.txt", ETag: "e", Hydrated: true,
	}, "", "seed tombstone soon"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeletePath(bgCtx(), "docs/gone.txt", "test"); err != nil {
		t.Fatal(err)
	}

	resp := f.ask(ipc.Request{V: ipc.Version, Type: "enumerate", Path: "docs"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("enumerate docs = (%v, %q)", resp.OK, resp.Error)
	}
	var data ipc.EnumerateData
	remarshalExtra(t, resp.Extra, &data)
	var names []string
	for _, e := range data.Entries {
		names = append(names, e.Name)
		if e.CachePath == "" && !e.IsDir {
			t.Errorf("entry %s missing cachePath: %+v", e.Name, e)
		}
	}
	if len(names) != 2 || !contains(names, "a.txt") || !contains(names, "sub") || contains(names, "gone.txt") {
		t.Fatalf("entries = %v, want exactly a.txt + sub (tombstoned row suppressed)", names)
	}
	for _, e := range data.Entries {
		if e.Name == "gone.txt" {
			t.Errorf("tombstoned row surfaced: %+v", e)
		}
		if e.Name == "a.txt" {
			if e.IsDir || !e.Materialized || e.Dirty || e.Size != 5 || e.CachePath != "files/docs/a.txt" {
				t.Errorf("a.txt entry = %+v", e)
			}
		}
		if e.Name == "sub" && !e.IsDir {
			t.Errorf("sub entry = %+v, want isDir", e)
		}
	}
	// Root enumerate ("").
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "enumerate"})
	if !resp.OK {
		t.Fatalf("enumerate root = %q", resp.Error)
	}
	remarshalExtra(t, resp.Extra, &data)
	names = nil
	for _, e := range data.Entries {
		names = append(names, e.Name)
	}
	if !contains(names, "docs") || !contains(names, "top.txt") {
		t.Fatalf("root entries = %v, want docs + top.txt", names)
	}
	// Leading-slash and trailing-slash forms normalize to the same set.
	resp2 := f.ask(ipc.Request{V: ipc.Version, Type: "enumerate", Path: "/docs/"})
	if !resp2.OK {
		t.Fatalf("enumerate /docs/ = %q", resp2.Error)
	}
	var data2 ipc.EnumerateData
	remarshalExtra(t, resp2.Extra, &data2)
	if len(data2.Entries) != len(data.Entries) {
		t.Fatalf("normalized enumerate = %d entries, want %d", len(data2.Entries), len(data.Entries))
	}
	// Unknown dir: empty entries, never null.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "enumerate", Path: "nope"})
	if !resp.OK {
		t.Fatalf("enumerate unknown dir = %q, want ok with empty entries", resp.Error)
	}
	remarshalExtra(t, resp.Extra, &data)
	if len(data.Entries) != 0 {
		t.Fatalf("unknown dir entries = %d, want 0", len(data.Entries))
	}
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

func TestFileProviderItemWire(t *testing.T) {
	f := newGuiFixture(t)
	seedClean(t, f, "docs/a.txt", "alpha")

	resp := f.ask(ipc.Request{V: ipc.Version, Type: "item", Path: "docs/a.txt"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("item = (%v, %q)", resp.OK, resp.Error)
	}
	var data ipc.ItemData
	remarshalExtra(t, resp.Extra, &data)
	it := data.Item
	if it.Key != "docs/a.txt" || it.Name != "a.txt" || it.IsDir ||
		!it.Materialized || it.Size != 5 || it.CachePath != "files/docs/a.txt" || it.Mtime != 1728211200 {
		t.Fatalf("item = %+v", it)
	}
	// Unknown path: ok:false naming the path (extension maps to .noSuchItem).
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "item", Path: "nope.bin"})
	if resp.OK || resp.Error == "" {
		t.Error("item unknown = ok, want error")
	}
	// Tombstoned path: not found.
	_ = f.store.DeletePath(bgCtx(), "docs/a.txt", "test")
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "item", Path: "docs/a.txt"})
	if resp.OK {
		t.Error("item tombstoned = ok, want error")
	}
	// Missing path field: validation error.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "item"})
	if resp.OK {
		t.Error("item without path = ok, want validation error")
	}
}

func TestFileProviderDownloadWire(t *testing.T) {
	f := newGuiFixture(t)
	// Remote-only file: hydration materializes it.
	f.fs.PutRaw("remote.txt", "remote body")

	resp := f.ask(ipc.Request{V: ipc.Version, Type: "download", Path: "remote.txt"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("download = (%v, %q)", resp.OK, resp.Error)
	}
	var data ipc.DownloadData
	remarshalExtra(t, resp.Extra, &data)
	if !data.Materialized || data.Path != "remote.txt" || data.CachePath != "files/remote.txt" {
		t.Fatalf("download ack = %+v", data)
	}
	b, err := os.ReadFile(f.cachePath("remote.txt")) // #nosec G304 -- test path
	if err != nil || string(b) != "remote body" {
		t.Fatalf("materialized body = %q (err %v)", string(b), err)
	}
	// Downloading an already-materialized file answers without a sync.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "download", Path: "remote.txt"})
	if !resp.OK {
		t.Fatalf("re-download = %q", resp.Error)
	}
	// Unknown path: honest error.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "download", Path: "nope.bin"})
	if resp.OK || resp.Error == "" {
		t.Error("download unknown = ok, want honest error")
	}
	// Missing path field.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "download"})
	if resp.OK {
		t.Error("download without path = ok, want validation error")
	}
}

func TestFileProviderDehydrateWire(t *testing.T) {
	f := newGuiFixture(t)
	// Clean + hydrated -> dehydrates (file gone, row demoted).
	seedClean(t, f, "c.txt", "x")
	// Dirty -> refused.
	f.fs.PutRaw("d.txt", "v")
	if err := os.WriteFile(f.cachePath("d.txt"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "d.txt", ETag: transport.MD5Hex([]byte("v")), Size: 1, Hydrated: true, Dirty: true,
	}, index.OpLocalWrite, "test dirty"); err != nil {
		t.Fatal(err)
	}
	// Pinned -> refused.
	if err := f.store.PutPath(bgCtx(), index.Resource{
		Path: "p.txt", ETag: "e", Size: 1, Hydrated: true, Pinned: true,
	}, "", "seed pinned"); err != nil {
		t.Fatal(err)
	}
	// Tombstoned -> refused.
	f.fs.PutRaw("t.txt", "t")
	if err := f.store.PutPath(bgCtx(), index.Resource{Path: "t.txt", ETag: "e", Hydrated: true}, "", "seed t"); err != nil {
		t.Fatal(err)
	}
	_ = f.store.DeletePath(bgCtx(), "t.txt", "test")

	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "dehydrate", Path: "d.txt"}); resp.OK {
		t.Error("dehydrate dirty = ok, want refusal")
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "dehydrate", Path: "p.txt"}); resp.OK {
		t.Error("dehydrate pinned = ok, want refusal")
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "dehydrate", Path: "t.txt"}); resp.OK {
		t.Error("dehydrate tombstoned = ok, want refusal")
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "dehydrate", Path: "nope.bin"}); resp.OK {
		t.Error("dehydrate unknown = ok, want refusal")
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "dehydrate"}); resp.OK {
		t.Error("dehydrate without path = ok, want validation error")
	}
	resp := f.ask(ipc.Request{V: ipc.Version, Type: "dehydrate", Path: "c.txt"})
	if !resp.OK {
		t.Fatalf("dehydrate clean = %q, want ok", resp.Error)
	}
	if _, err := os.Stat(f.cachePath("c.txt")); !os.IsNotExist(err) {
		t.Error("cache file survived dehydrate")
	}
	row, err := f.store.Get(bgCtx(), "c.txt")
	if err != nil || row.Hydrated {
		t.Errorf("row after dehydrate = (%+v, %v), want demoted", row, err)
	}
}

func TestFileProviderMarkDeleteMoveWire(t *testing.T) {
	f := newGuiFixture(t)

	// mark: bytes exist on disk (extension wrote them), row is created
	// dirty from the disk truth.
	if err := os.MkdirAll(filepath.Dir(f.cachePath("new.txt")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cachePath("new.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := f.ask(ipc.Request{V: ipc.Version, Type: "mark", Path: "new.txt"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("mark = (%v, %q)", resp.OK, resp.Error)
	}
	var mark ipc.MarkData
	remarshalExtra(t, resp.Extra, &mark)
	if mark.Path != "new.txt" || !mark.Dirty {
		t.Fatalf("mark ack = %+v", mark)
	}
	row, err := f.store.Get(bgCtx(), "new.txt")
	if err != nil || !row.Dirty || row.Size != 5 || !row.Hydrated {
		t.Fatalf("row after mark = (%+v, %v)", row, err)
	}
	// mark without the file: error naming the contract.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "mark", Path: "ghost.txt"})
	if resp.OK {
		t.Error("mark without cache file = ok, want error")
	}

	// delete: tombstone + file gone.
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "delete", Path: "new.txt"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("delete = (%v, %q)", resp.OK, resp.Error)
	}
	var del ipc.DeleteData
	remarshalExtra(t, resp.Extra, &del)
	if del.Path != "new.txt" {
		t.Fatalf("delete ack = %+v", del)
	}
	if _, err := os.Stat(f.cachePath("new.txt")); !os.IsNotExist(err) {
		t.Error("cache file survived delete")
	}
	row, err = f.store.Get(bgCtx(), "new.txt")
	if err != nil || !row.Deleted {
		t.Fatalf("row after delete = (%+v, %v), want tombstone", row, err)
	}

	// move: cache rename + row re-point (old key tombstoned).
	seedClean(t, f, "old.txt", "migrate me")
	resp = f.ask(ipc.Request{V: ipc.Version, Type: "move", Path: "old.txt", Dest: "renamed.txt"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("move = (%v, %q)", resp.OK, resp.Error)
	}
	var mv ipc.MoveData
	remarshalExtra(t, resp.Extra, &mv)
	if mv.From != "old.txt" || mv.To != "renamed.txt" {
		t.Fatalf("move ack = %+v", mv)
	}
	if _, err := os.Stat(f.cachePath("old.txt")); !os.IsNotExist(err) {
		t.Error("old cache file survived move")
	}
	b, err := os.ReadFile(f.cachePath("renamed.txt")) // #nosec G304 -- test path
	if err != nil || string(b) != "migrate me" {
		t.Fatalf("moved body = %q (err %v)", string(b), err)
	}
	newRow, err := f.store.Get(bgCtx(), "renamed.txt")
	if err != nil || newRow.Deleted || !newRow.Hydrated {
		t.Fatalf("moved row = (%+v, %v)", newRow, err)
	}
	oldRow, err := f.store.Get(bgCtx(), "old.txt")
	if err != nil || !oldRow.Deleted {
		t.Fatalf("old row after move = (%+v, %v), want tombstone", oldRow, err)
	}
	// move unknown -> honest error; move without dest -> validation.
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "move", Path: "ghost", Dest: "x"}); resp.OK {
		t.Error("move unknown = ok, want error")
	}
	if resp := f.ask(ipc.Request{V: ipc.Version, Type: "move", Path: "renamed.txt"}); resp.OK {
		t.Error("move without to = ok, want validation error")
	}
}

func TestFileProviderWithoutCapabilityRejected(t *testing.T) {
	// A Handler without FileProviderSource: the new types must answer
	// the capability error (the leaf-08 rule). A stub lives in package
	// ipc's tests; here a bare-status listener serves as the minimal
	// no-capability daemon.
	tmp, err := os.MkdirTemp("/tmp", "zc-fp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	sock := filepath.Join(tmp, "z.ipc")
	srv, err := ipc.Serve(sock, "https://srv", "bkt", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	for _, typ := range []string{"enumerate", "item", "download", "dehydrate", "mark", "delete", "move"} {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(conn).Encode(ipc.Request{V: ipc.Version, Type: typ, Path: "x", Dest: "y"}); err != nil {
			t.Fatal(err)
		}
		var resp ipc.Response
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if resp.OK || resp.Error == "" {
			t.Errorf("%s without capability = ok, want capability error", typ)
		}
	}
}

func TestFileProviderUnknownTypeStillRejected(t *testing.T) {
	f := newGuiFixture(t)
	resp := f.ask(ipc.Request{V: ipc.Version, Type: "frobnicate"})
	if resp.OK || resp.Error != "unknown request type" {
		t.Errorf("unknown type = (%v, %q), want the leaf-01 rejection", resp.OK, resp.Error)
	}
}

func TestFileProviderConcurrentClients(t *testing.T) {
	f := newGuiFixture(t)
	seedClean(t, f, "docs/a.txt", "alpha")
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			conn, err := net.Dial("unix", f.path)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			if err := json.NewEncoder(conn).Encode(ipc.Request{V: ipc.Version, Type: "enumerate", Path: "docs"}); err != nil {
				errs <- err
				return
			}
			var resp ipc.Response
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err := json.NewDecoder(conn).Decode(&resp); err != nil {
				errs <- err
				return
			}
			if !resp.OK {
				errs <- fmt.Errorf("enumerate not ok under concurrency: %q", resp.Error)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
