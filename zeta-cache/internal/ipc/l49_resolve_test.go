package ipc

// l49_resolve_test.go - the L49 regression: conflicts.resolve reports
// copyRemoved from the FACT (did a conflicted-copy file actually get
// removed), not by echoing the request's confirm flag.

import (
	"testing"
)

// resolveStub resolves with canned facts: copyRemoved=true only when the
// handler actually removed a copy (mirrors the engine's contract).
type resolveStub struct {
	stubHandler
	factCopyRemoved bool
	lastRemoveCopy  bool
}

func (h *resolveStub) ResolveConflict(path, mode string, removeCopy bool) (ResolveResult, error) {
	h.lastRemoveCopy = removeCopy
	return ResolveResult{CopyRemoved: h.factCopyRemoved}, nil
}

func (h *resolveStub) Conflicts() ([]ConflictItem, error) { return nil, nil }
func (h *resolveStub) DeletedPaths() ([]Tombstone, error) { return nil, nil }
func (h *resolveStub) RestoreDeleted(path string) error   { return nil }
func (h *resolveStub) EvictPath(path string) error        { return nil }

// startResolveServer serves h directly (generic over Handler+ConflictResolver).
func startResolveServer(t *testing.T, h Handler) string {
	t.Helper()
	path := shortSocket(t)
	srv, err := ServeWithHandler(path, "https://srv", "bkt", nil, h)
	if err != nil {
		t.Fatalf("ServeWithHandler: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	return path
}

func TestResolveAckCopyRemovedReportsFact(t *testing.T) {
	h := &resolveStub{}
	path := startResolveServer(t, h)

	// confirm=true but the record had NO copy (kept-local): the ack must
	// NOT claim a copy was removed.
	resp := ask(t, path, Request{V: Version, Type: "conflicts.resolve", Path: "d.txt", Mode: ModeKeepLocal, Confirm: true})
	if !resp.OK {
		t.Fatalf("resolve = %q", resp.Error)
	}
	if !h.lastRemoveCopy {
		t.Errorf("handler did not receive the confirm flag")
	}
	var data ResolveData
	if err := remarshal(resp.Extra, &data); err != nil {
		t.Fatal(err)
	}
	if data.CopyRemoved {
		t.Fatalf("no conflicted copy existed; ack must report copyRemoved=false, got %+v", data)
	}

	// Same flag, but this time a copy WAS removed: ack reports true.
	h.factCopyRemoved = true
	resp = ask(t, path, Request{V: Version, Type: "conflicts.resolve", Path: "d.txt", Mode: ModeKeepRemote, Confirm: true})
	if !resp.OK {
		t.Fatalf("resolve = %q", resp.Error)
	}
	if err := remarshal(resp.Extra, &data); err != nil {
		t.Fatal(err)
	}
	if !data.CopyRemoved {
		t.Fatalf("copy was removed; ack must report copyRemoved=true, got %+v", data)
	}

	// confirm=false never removes: false either way.
	h.factCopyRemoved = false
	resp = ask(t, path, Request{V: Version, Type: "conflicts.resolve", Path: "d.txt", Mode: ModeKeepLocal})
	if !resp.OK {
		t.Fatalf("resolve = %q", resp.Error)
	}
	if err := remarshal(resp.Extra, &data); err != nil {
		t.Fatal(err)
	}
	if data.CopyRemoved {
		t.Fatalf("confirm=false must never report copyRemoved=true, got %+v", data)
	}
}
