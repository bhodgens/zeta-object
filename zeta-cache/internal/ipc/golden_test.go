package ipc

// golden_test.go - the protocol-conformance fixtures (leaf-08
// acceptance): the Go encoder's byte-exact response for every request
// type is pinned in testdata/golden/*.json. The Swift GUI decodes the
// SAME files in gui/macos/Tests (protocol conformance across
// languages); this test regenerates nothing - it asserts.
//
// Regenerate with: go test ./internal/ipc -run TestGoldenFixtures -update

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden fixtures")

// goldenCase is one request -> pinned response pair.
type goldenCase struct {
	name string
	req  Request
}

func goldenCases() []goldenCase {
	flag := true
	return []goldenCase{
		{name: "status", req: Request{V: 1, Type: "status"}},
		{name: "pause", req: Request{V: 1, Type: "pause"}},
		{name: "resume", req: Request{V: 1, Type: "resume"}},
		{name: "pin", req: Request{V: 1, Type: "pin", Path: "docs/keep.txt", Pin: &flag}},
		{name: "pins", req: Request{V: 1, Type: "pins"}},
		{name: "tombstones", req: Request{V: 1, Type: "tombstones"}},
		{name: "conflicts-list", req: Request{V: 1, Type: "conflicts.list"}},
		{name: "conflicts-resolve", req: Request{V: 1, Type: "conflicts.resolve", Path: "doc.txt", Mode: "keep-local", Confirm: true}},
		{name: "deleted-list", req: Request{V: 1, Type: "deleted.list"}},
		{name: "deleted-restore", req: Request{V: 1, Type: "deleted.restore", Path: "notes/old.txt"}},
		{name: "evict", req: Request{V: 1, Type: "evict", Path: "cache/big.bin"}},
		// fileprovider-2026-10 leaf-01 additions (the FileProviderItem
		// surface; the Swift fileprovider package decodes the same
		// fixtures).
		{name: "enumerate", req: Request{V: 1, Type: "enumerate", Path: "docs"}},
		{name: "item", req: Request{V: 1, Type: "item", Path: "docs/a.txt"}},
		{name: "download", req: Request{V: 1, Type: "download", Path: "docs/a.txt"}},
		{name: "dehydrate", req: Request{V: 1, Type: "dehydrate", Path: "cache/big.bin"}},
		{name: "mark", req: Request{V: 1, Type: "mark", Path: "docs/new.txt"}},
		{name: "delete", req: Request{V: 1, Type: "delete", Path: "docs/old.txt"}},
		{name: "move", req: Request{V: 1, Type: "move", Path: "docs/old.txt", Dest: "docs/new.txt"}},
		{name: "download-missing-path", req: Request{V: 1, Type: "download"}},
		{name: "move-missing-to", req: Request{V: 1, Type: "move", Path: "docs/old.txt"}},
		{name: "unknown-type", req: Request{V: 1, Type: "frobnicate"}},
		{name: "bad-version", req: Request{V: 99, Type: "status"}},
		{name: "resolve-missing-mode", req: Request{V: 1, Type: "conflicts.resolve", Path: "doc.txt"}},
		{name: "restore-missing-path", req: Request{V: 1, Type: "deleted.restore"}},
		{name: "evict-missing-path", req: Request{V: 1, Type: "evict"}},
	}
}

// fullStubHandler is the stub extended with the leaf-08 capability.
type fullStubHandler struct {
	stubHandler
	conflicts []ConflictItem
	lastMode  string
	lastRm    bool
	lastRest  string
	lastEvict string
}

func (h *fullStubHandler) Conflicts() ([]ConflictItem, error) { return h.conflicts, nil }
func (h *fullStubHandler) ResolveConflict(path, mode string, removeCopy bool) error {
	h.lastMode, h.lastRm = mode, removeCopy
	return nil
}
func (h *fullStubHandler) RestoreDeleted(path string) error { h.lastRest = path; return nil }
func (h *fullStubHandler) EvictPath(path string) error      { h.lastEvict = path; return nil }
func (h *fullStubHandler) DeletedPaths() ([]Tombstone, error) {
	return h.tombs, nil
}

// FileProviderSource capability (fileprovider-2026-10 leaf-01): canned
// entries for the golden fixtures.
func (h *fullStubHandler) Enumerate(dirKey string) ([]ItemEntry, error) {
	return []ItemEntry{
		{Name: "a.txt", Key: dirKey + "a.txt", IsDir: false, Size: 5, Mtime: 1728211200,
			Materialized: true, CachePath: "files/" + dirKey + "a.txt"},
		{Name: "sub", Key: dirKey + "sub/", IsDir: true},
	}, nil
}

func (h *fullStubHandler) Item(path string) (ItemEntry, error) {
	if path == "nope.txt" {
		return ItemEntry{}, fmt.Errorf("no such item: %s", path)
	}
	return ItemEntry{Name: "a.txt", Key: path, Size: 5, Mtime: 1728211200,
		Materialized: true, CachePath: "files/" + path}, nil
}

func (h *fullStubHandler) Download(ctx context.Context, path string) (ItemEntry, error) {
	return ItemEntry{Name: "a.txt", Key: path, Size: 5, Mtime: 1728211200,
		Materialized: true, CachePath: "files/" + path}, nil
}

func (h *fullStubHandler) Dehydrate(path string) error { h.lastEvict = path; return nil }
func (h *fullStubHandler) Mark(path string) (MarkData, error) {
	return MarkData{Path: path, Dirty: true}, nil
}
func (h *fullStubHandler) Delete(path string) (DeleteData, error) {
	return DeleteData{Path: path}, nil
}
func (h *fullStubHandler) Move(path, dest string) (MoveData, error) {
	return MoveData{From: path, To: dest}, nil
}

// goldenServer serves ONE request on a fresh connection and returns the
// raw response bytes (the exact encoder output the fixture pins).
func goldenServer(t *testing.T, h Handler, req Request) []byte {
	t.Helper()
	path := shortSocket(t)
	srv, err := ServeWithHandler(path, "https://cache.example", "bkt", nil, h)
	if err != nil {
		t.Fatalf("ServeWithHandler: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

func TestGoldenFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	if *update {
		_ = os.MkdirAll(dir, 0o755)
	}
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			h := &fullStubHandler{}
			h.conflicts = []ConflictItem{
				{Path: "doc.txt", Kind: KindConflictCopy, CopyPath: "doc (conflicted copy 2026-10-07).txt"},
				{Path: "notes/todo.md", Kind: KindKeptLocal},
			}
			h.pins = []string{"docs/keep.txt", "photos/raws"}
			h.tombs = []Tombstone{{Path: "notes/old.txt", DeletedAt: 1728211200, ExpiresAt: 1730803200}}
			resp := goldenServer(t, h, tc.req)
			// Normalize: exact bytes the encoder wrote (trailing \n kept).
			file := filepath.Join(dir, tc.name+".json")
			if *update {
				if err := os.WriteFile(file, resp, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file) // #nosec G304 -- testdata path
			if err != nil {
				t.Fatalf("fixture missing (run with -update): %v", err)
			}
			if string(want) != string(resp) {
				t.Errorf("golden mismatch for %s:\n want: %s\n got:  %s", tc.name, want, resp)
			}
		})
	}
}
