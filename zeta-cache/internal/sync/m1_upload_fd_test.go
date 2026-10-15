package sync

// m1_upload_fd_test.go - M1: upload's Put body is an *os.File that mustOpen
// used to return and never close. do() must not Close req.Body (W2-H1:
// that closed the caller's handle and every real upload died with "file
// already closed"). The caller closes the file after Put returns, so the
// handle is still readable inside Put and closed before upload returns.
// A second Close is the deterministic pin (fd-count races are not).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// putFileCapture records the *os.File Put was handed and, after the inner
// Put returns, proves the handle is still readable (W2-H1). It does not
// close the file - that is the caller's job (M1).
type putFileCapture struct {
	*transport.MemFS
	file        *os.File
	afterPut    []byte
	afterPutErr error
}

func (p *putFileCapture) Put(ctx context.Context, key string, body io.ReadSeeker, etag string) (string, error) {
	f, ok := body.(*os.File)
	if !ok {
		p.afterPutErr = fmt.Errorf("put body type %T, want *os.File", body)
	} else {
		p.file = f
	}
	out, err := p.MemFS.Put(ctx, key, body, etag)
	if f == nil {
		return out, err
	}
	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		p.afterPutErr = seekErr
		return out, err
	}
	b, readErr := io.ReadAll(f)
	if readErr != nil {
		p.afterPutErr = readErr
		return out, err
	}
	p.afterPut = b
	return out, err
}

// TestUploadClosesFileAfterPut: a dirty never-uploaded file goes through
// upload -> Put. After SyncOnce returns the caller's *os.File must be
// closed (second Close is os.ErrClosed). Pre-fix mustOpen leaked the fd
// and the second Close succeeded.
func TestUploadClosesFileAfterPut(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"files", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	mem := transport.NewMemFS()
	cap := &putFileCapture{MemFS: mem}
	eng, err := NewEngine(Options{
		Transport: cap,
		Store:     st,
		CacheDir:  dir,
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	const key = "fresh.txt"
	const body = "brand new"
	if err := os.WriteFile(eng.cachePath(key), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPath(bg(), index.Resource{
		Path: key, Size: int64(len(body)), Hydrated: true, Dirty: true,
	}, index.OpLocalWrite, "m1 seed dirty"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SyncOnce(bg()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if cap.afterPutErr != nil {
		t.Fatalf("caller's file not readable after Put: %v (W2-H1: Put must not close it)", cap.afterPutErr)
	}
	if string(cap.afterPut) != body {
		t.Fatalf("post-Put read = %q, want %q", cap.afterPut, body)
	}
	if got := mem.ServerBodyOrEmpty(key); got != body {
		t.Fatalf("server body = %q, want %q (upload did not run)", got, body)
	}
	if cap.file == nil {
		t.Fatal("Put did not receive an *os.File")
	}
	if err := cap.file.Close(); err == nil {
		t.Fatal("upload returned with the put file still open (fd leak)")
	} else if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("second Close = %v, want os.ErrClosed", err)
	}
}
