package fusefs

// r4_otrunc_prompt_test.go - the R4 regression: an O_TRUNC-only open +
// close (no writes) marks the row dirty via the dirtyNoCommit branch -
// which used to return WITHOUT firing the prompt-upload hook, leaving
// the file dirty until the next poll/sync cycle instead of uploading
// promptly like every other close does.

import (
	"context"
	"os"
	"sync"
	"testing"
)

// promptRecorder is an Uploader stub that records UploadNow calls.
type promptRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (p *promptRecorder) UploadNow(_ context.Context, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, key)
}

func (p *promptRecorder) keys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func TestR4OTruncCloseFiresPromptUpload(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "precious bytes")
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	rec := &promptRecorder{}
	h.fs.up = rec

	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	truncFlags := uint32(os.O_RDWR) | uint32(os.O_TRUNC)
	fh, _, errno := fn.Open(h.ctx, truncFlags)
	if errno != 0 {
		t.Fatalf("Open(O_TRUNC): %v", errno)
	}
	// close(2) with NO writes: the dirtyNoCommit Flush branch.
	hdl := fh.(*fileHandle)
	if errno := fn.Flush(h.ctx, hdl); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatal("arrange: the O_TRUNC close must dirty the row")
	}
	// THE REGRESSION: the prompt-upload hook must fire (fire-and-forget,
	// same as the commit path).
	calls := rec.keys()
	if len(calls) != 1 || calls[0] != "doc.txt" {
		t.Fatalf("UploadNow calls = %v, want exactly [doc.txt]", calls)
	}
}
