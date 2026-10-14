package fusefs

// r1_edit_upload_test.go - the R1 regression: edits to ALREADY-SYNCED
// files must upload with If-Match of the pre-edit etag.
//
// Before the fix, MarkDirty cleared the row's etag and UploadFile read
// CleanETag (which blanks dirty rows): every edit of a synced file went
// out as a create-only PUT (If-None-Match:*), the server answered 412 on
// the existing object, the row stayed dirty forever, and the SIGTERM
// bounded-flush unmount refused with "N file(s) still dirty".

import (
	"errors"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// R1a: MarkDirty preserves the row's etag; UploadFile If-Matches it and
// the edit overwrites the existing object cleanly.
func TestR1EditUploadUsesPreEditETag(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "v1")
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	cleanETag, _ := h.idx.CleanETag("doc.txt")
	if cleanETag == "" {
		t.Fatal("precondition: hydrated row must carry the server etag")
	}

	// Edit via the mount: write + Flush (marks dirty) - the exact flow
	// that used to clear the etag. The write fully covers the old body
	// (11 bytes > 2), so the committed generation is exactly "edited body".
	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	fh, _, errno := fn.Open(h.ctx, 0)
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	if _, errno := fn.Write(h.ctx, fh, []byte("edited body"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatal("arrange: row must be dirty after the edit flush")
	}
	// R1 (a): the dirty row KEEPS the pre-edit etag.
	if got, _ := h.idx.KnownETag("doc.txt"); got != cleanETag {
		t.Fatalf("MarkDirty cleared the etag: KnownETag = %q, want %q", got, cleanETag)
	}

	// R1 (b): the edit upload OVERWRITES (If-Match) - the memfs model
	// answers 412 to a create-only PUT on an existing object, so
	// success here proves the If-Match path was taken.
	if err := h.fs.UploadFile(h.ctx, "doc.txt"); err != nil {
		t.Fatalf("UploadFile(edit) = %v, want clean overwrite via If-Match", err)
	}
	if h.idx.isDirty("doc.txt") {
		t.Fatal("edit upload must clean the row")
	}
	if got := h.tr.ServerBodyOrEmpty("doc.txt"); got != "edited body" {
		t.Fatalf("server body = %q, want the edited content", got)
	}
}

// R1c: the edit upload against a CHANGED server generation 412s, the
// error surfaces, and the row stays dirty (the scan's conflict path owns
// it). The local bytes are never touched.
func TestR1EditUploadAgainstChangedGenerationStaysDirty(t *testing.T) {
	h := newHarness(t)
	h.put("doc.txt", "server generation")
	if errno := h.fs.ensureHydrated(h.ctx, "doc.txt"); errno != 0 {
		t.Fatalf("hydrate: %v", errno)
	}
	cleanETag, _ := h.idx.CleanETag("doc.txt")

	fn := &fileNode{fs: h.fs, key: "doc.txt"}
	fh, _, errno := fn.Open(h.ctx, 0)
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	if _, errno := fn.Write(h.ctx, fh, []byte("local edit"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fn.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	// Another client moved the object while our row is dirty.
	h.tr.PutRaw("doc.txt", "RACED")

	err := h.fs.UploadFile(h.ctx, "doc.txt")
	if !errors.Is(err, transport.ErrConflict) {
		t.Fatalf("UploadFile err = %v, want ErrConflict (412 surfaced)", err)
	}
	if !h.idx.isDirty("doc.txt") {
		t.Fatal("412 must keep the row dirty")
	}
	// The preserved etag is what the If-Match carried (proven by the
	// 412: the server's current generation is "RACED"'s md5).
	if got, _ := h.idx.KnownETag("doc.txt"); got != cleanETag {
		t.Fatalf("row etag moved on 412: %q, want %q", got, cleanETag)
	}
}

// R1 matrix pin (fusefs side): a never-uploaded row keeps etag=="" through
// MarkDirty, so its upload stays create-only (matrix-8 semantics - the
// memfs answers 412 when the create races an existing object).
func TestR1NeverUploadedRowStaysCreateOnly(t *testing.T) {
	h := newHarness(t)
	root := h.root()
	child, fh, _, errno := root.Create(h.ctx, "fresh.txt", 0, 0, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}
	fnF := child.Operations().(*fileNode)
	if _, errno := fnF.Write(h.ctx, fh, []byte("brand new"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := fnF.Flush(h.ctx, fh); errno != 0 {
		t.Fatalf("Flush: %v", errno)
	}
	if got, _ := h.idx.KnownETag("fresh.txt"); got != "" {
		t.Fatalf("never-uploaded row must keep etag \"\", got %q", got)
	}
	// Create-only semantics: the PUT succeeds on the absent object...
	if err := h.fs.UploadFile(h.ctx, "fresh.txt"); err != nil {
		t.Fatalf("UploadFile(fresh) = %v", err)
	}
	if got := h.tr.ServerBodyOrEmpty("fresh.txt"); got != "brand new" {
		t.Fatalf("server body = %q", got)
	}
}
