package ipc

// dir_slash_test.go - H1/H2: directory identifiers carry a trailing
// slash (index rowKey := dirKey + "/"). item, move, and download must
// pass that key through. A slash delete must not call Delete (that
// would tombstone the file-shaped key).

import (
	"context"
	"strings"
	"testing"
)

// dirSlashStub records the paths handleFileProvider hands the source.
type dirSlashStub struct {
	stubHandler
	itemPath     string
	downloadPath string
	moveFrom     string
	moveTo       string
	deletePath   string
	deleteCalls  int
	markPath     string
}

func (s *dirSlashStub) Enumerate(string) ([]ItemEntry, error) { return nil, nil }

func (s *dirSlashStub) Item(path string) (ItemEntry, error) {
	s.itemPath = path
	return ItemEntry{Name: "docs", Key: path, IsDir: strings.HasSuffix(path, "/")}, nil
}

func (s *dirSlashStub) Download(_ context.Context, path string) (ItemEntry, error) {
	s.downloadPath = path
	return ItemEntry{Name: "docs", Key: path, IsDir: strings.HasSuffix(path, "/")}, nil
}

func (s *dirSlashStub) Dehydrate(string) error { return nil }

func (s *dirSlashStub) Mark(path string) (MarkData, error) {
	s.markPath = path
	return MarkData{Path: path, Dirty: true}, nil
}

func (s *dirSlashStub) Delete(path string) (DeleteData, error) {
	s.deleteCalls++
	s.deletePath = path
	return DeleteData{Path: path}, nil
}

func (s *dirSlashStub) Move(path, dest string) (MoveData, error) {
	s.moveFrom, s.moveTo = path, dest
	return MoveData{From: path, To: dest}, nil
}

func TestDirKeyKeepsTrailingSlash(t *testing.T) {
	h := &dirSlashStub{}

	resp := handleFileProvider(Request{V: Version, Type: "item", Path: "docs/"}, h)
	if !resp.OK {
		t.Fatalf("item: %s", resp.Error)
	}
	if h.itemPath != "docs/" {
		t.Fatalf("Item path = %q, want %q", h.itemPath, "docs/")
	}

	// One leading slash is trimmed; the trailing slash stays.
	resp = handleFileProvider(Request{V: Version, Type: "item", Path: "/docs/"}, h)
	if !resp.OK {
		t.Fatalf("item /docs/: %s", resp.Error)
	}
	if h.itemPath != "docs/" {
		t.Fatalf("Item(/docs/) = %q, want docs/", h.itemPath)
	}

	resp = handleFileProvider(Request{V: Version, Type: "move", Path: "docs/", Dest: "other/"}, h)
	if !resp.OK {
		t.Fatalf("move: %s", resp.Error)
	}
	if h.moveFrom != "docs/" || h.moveTo != "other/" {
		t.Fatalf("Move = (%q, %q), want (docs/, other/)", h.moveFrom, h.moveTo)
	}

	resp = handleFileProvider(Request{V: Version, Type: "delete", Path: "docs/"}, h)
	if resp.OK || !strings.HasPrefix(resp.Error, "folder delete is not supported") {
		t.Fatalf("delete = (%v, %q), want folder-delete error", resp.OK, resp.Error)
	}
	if h.deleteCalls != 0 {
		t.Fatalf("Delete called %d times for docs/, want 0", h.deleteCalls)
	}

	// File-shaped delete still reaches the source.
	resp = handleFileProvider(Request{V: Version, Type: "delete", Path: "docs/a.txt"}, h)
	if !resp.OK {
		t.Fatalf("file delete: %s", resp.Error)
	}
	if h.deleteCalls != 1 || h.deletePath != "docs/a.txt" {
		t.Fatalf("file Delete = (%d, %q), want (1, docs/a.txt)", h.deleteCalls, h.deletePath)
	}

	resp = handleFileProvider(Request{V: Version, Type: "download", Path: "docs/"}, h)
	if !resp.OK {
		t.Fatalf("download: %s", resp.Error)
	}
	if h.downloadPath != "docs/" {
		t.Fatalf("Download path = %q, want docs/", h.downloadPath)
	}

	// mark stays on normalizePath (file-shaped; the slash is stripped).
	resp = handleFileProvider(Request{V: Version, Type: "mark", Path: "docs/"}, h)
	if !resp.OK {
		t.Fatalf("mark: %s", resp.Error)
	}
	if h.markPath != "docs" {
		t.Fatalf("Mark path = %q, want docs", h.markPath)
	}
}
