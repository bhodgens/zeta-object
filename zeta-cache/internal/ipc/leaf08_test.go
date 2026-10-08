package ipc

// leaf08_test.go - wire-level tests for the leaf-08 request types over a
// REAL unix socket: every new type handled, unknown-type rejection
// preserved, capability errors, validation, and concurrent clients.

import (
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// leaf08Stub implements Handler + ConflictResolver with canned answers.
type leaf08Stub struct {
	stubHandler
	conflicts   []ConflictItem
	deleted     []Tombstone
	lastResolve struct {
		path, mode string
		rm         bool
	}
	lastRestore string
	lastEvict   string
	evictErr    error
}

func (h *leaf08Stub) Conflicts() ([]ConflictItem, error) { return h.conflicts, nil }
func (h *leaf08Stub) DeletedPaths() ([]Tombstone, error) { return h.deleted, nil }
func (h *leaf08Stub) ResolveConflict(path, mode string, removeCopy bool) error {
	h.lastResolve.path, h.lastResolve.mode, h.lastResolve.rm = path, mode, removeCopy
	return nil
}
func (h *leaf08Stub) RestoreDeleted(path string) error { h.lastRestore = path; return nil }
func (h *leaf08Stub) EvictPath(path string) error {
	h.lastEvict = path
	return h.evictErr
}

func startLeaf08Server(t *testing.T, h *leaf08Stub) string {
	t.Helper()
	path := shortSocket(t)
	srv, err := ServeWithHandler(path, "https://srv", "bkt", nil, h)
	if err != nil {
		t.Fatalf("ServeWithHandler: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	return path
}

func TestConflictsListWire(t *testing.T) {
	h := &leaf08Stub{conflicts: []ConflictItem{
		{Path: "doc.txt", Kind: KindConflictCopy, CopyPath: "doc (conflicted copy 2026-10-07).txt"},
		{Path: "n.md", Kind: KindKeptLocal},
	}}
	path := startLeaf08Server(t, h)
	resp := ask(t, path, Request{V: Version, Type: "conflicts.list"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("conflicts.list = (%v, %+v), want ok + extra", resp.OK, resp.Extra)
	}
	var data ConflictData
	if err := remarshal(resp.Extra, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Conflicts) != 2 ||
		data.Conflicts[0].Kind != "conflict-copy" || data.Conflicts[1].Kind != "kept-local" {
		t.Fatalf("conflicts = %+v", data)
	}
}

func TestConflictsResolveWire(t *testing.T) {
	h := &leaf08Stub{}
	path := startLeaf08Server(t, h)
	resp := ask(t, path, Request{V: Version, Type: "conflicts.resolve", Path: "doc.txt", Mode: ModeKeepRemote})
	if !resp.OK {
		t.Fatalf("resolve = %q", resp.Error)
	}
	if h.lastResolve.path != "doc.txt" || h.lastResolve.mode != "keep-remote" || h.lastResolve.rm {
		t.Fatalf("handler got %+v, want doc.txt/keep-remote/no-remove", h.lastResolve)
	}
	// The confirm flag rides through.
	resp = ask(t, path, Request{V: Version, Type: "conflicts.resolve", Path: "d", Mode: ModeKeepLocal, Confirm: true})
	if !resp.OK || !h.lastResolve.rm {
		t.Fatalf("resolve confirm = (%v, rm=%v)", resp.OK, h.lastResolve.rm)
	}
	// Invalid mode rejected BEFORE the handler.
	resp = ask(t, path, Request{V: Version, Type: "conflicts.resolve", Path: "d", Mode: "delete-both"})
	if resp.OK {
		t.Error("invalid mode = ok, want error")
	}
	if h.lastResolve.mode != "keep-local" {
		t.Errorf("handler received the invalid mode %q", h.lastResolve.mode)
	}
}

func TestDeletedListAndRestoreWire(t *testing.T) {
	h := &leaf08Stub{deleted: []Tombstone{{Path: "gone.txt", DeletedAt: 100, ExpiresAt: 200}}}
	path := startLeaf08Server(t, h)
	resp := ask(t, path, Request{V: Version, Type: "deleted.list"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("deleted.list = (%v, %+v)", resp.OK, resp.Extra)
	}
	var data TombstoneData
	if err := remarshal(resp.Extra, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Tombstones) != 1 || data.Tombstones[0].Path != "gone.txt" {
		t.Fatalf("deleted = %+v", data)
	}
	resp = ask(t, path, Request{V: Version, Type: "deleted.restore", Path: "gone.txt"})
	if !resp.OK || h.lastRestore != "gone.txt" {
		t.Fatalf("restore = (%v, %q)", resp.OK, h.lastRestore)
	}
	resp = ask(t, path, Request{V: Version, Type: "deleted.restore"})
	if resp.OK || resp.Error == "" {
		t.Error("restore without path = ok, want validation error")
	}
}

func TestEvictWire(t *testing.T) {
	h := &leaf08Stub{evictErr: errors.New("refused, file is dirty")}
	path := startLeaf08Server(t, h)
	resp := ask(t, path, Request{V: Version, Type: "evict", Path: "cache/big.bin"})
	if resp.OK || h.lastEvict != "cache/big.bin" {
		t.Fatalf("evict = (%v, %q), want handler error surfaced", resp.OK, h.lastEvict)
	}
	if resp.Error != "refused, file is dirty" {
		t.Errorf("error = %q, want the handler's refusal verbatim", resp.Error)
	}
	h.evictErr = nil
	resp = ask(t, path, Request{V: Version, Type: "evict", Path: "cache/big.bin"})
	if !resp.OK {
		t.Fatalf("evict ok path = %q", resp.Error)
	}
	resp = ask(t, path, Request{V: Version, Type: "evict"})
	if resp.OK {
		t.Error("evict without path = ok, want validation error")
	}
}

func TestLeaf08WithoutCapabilityRejected(t *testing.T) {
	// The leaf-07-only stub (no ConflictResolver methods).
	h := &stubHandler{}
	path := shortSocket(t)
	srv, err := ServeWithHandler(path, "https://srv", "bkt", nil, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	for _, typ := range []string{"conflicts.list", "conflicts.resolve", "deleted.list", "deleted.restore", "evict"} {
		resp := ask(t, path, Request{V: Version, Type: typ, Path: "x", Mode: "keep-local"})
		if resp.OK || resp.Error == "" {
			t.Errorf("%s without capability = ok, want capability error", typ)
		}
	}
}

func TestUnknownTypeStillRejectedWithLeaf08(t *testing.T) {
	h := &leaf08Stub{}
	path := startLeaf08Server(t, h)
	resp := ask(t, path, Request{V: Version, Type: "frobnicate"})
	if resp.OK || resp.Error != "unknown request type" {
		t.Errorf("unknown type = (%v, %q), want the leaf-01 rejection", resp.OK, resp.Error)
	}
}

func TestConcurrentClients(t *testing.T) {
	h := &leaf08Stub{conflicts: []ConflictItem{{Path: "a", Kind: KindKeptLocal}}}
	path := startLeaf08Server(t, h)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			conn, err := net.Dial("unix", path)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			if err := json.NewEncoder(conn).Encode(Request{V: Version, Type: "status"}); err != nil {
				errs <- err
				return
			}
			var resp Response
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err := json.NewDecoder(conn).Decode(&resp); err != nil {
				errs <- err
				return
			}
			if !resp.OK {
				errs <- errors.New("status not ok under concurrency")
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// remarshal round-trips the generic Extra value through JSON into a
// typed payload (what a non-Go client experiences on the wire).
func remarshal(v, into any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}
