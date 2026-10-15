package fusefs

// m1_upload_fd_test.go - M1: UploadFile's Put body is an *os.File that
// mustSeek used to return and never close. do() must not Close req.Body
// (W2-H1). The caller closes the file after Put returns: still readable
// inside Put, closed before UploadFile returns. A second Close is the pin.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// putFileCapture records the *os.File Put was handed and, after the inner
// Put returns, proves the handle is still readable (W2-H1). It does not
// close the file.
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

// TestUploadClosesFileAfterPut: UploadFile must close the file it opened
// for Put before returning. Pre-fix mustSeek leaked the fd and a second
// Close succeeded.
func TestUploadClosesFileAfterPut(t *testing.T) {
	mem := transport.NewMemFS()
	cap := &putFileCapture{MemFS: mem}
	fs, err := Mount(Options{
		CacheDir:  t.TempDir(),
		Bucket:    "bkt",
		Index:     newMemIndex(),
		Transport: cap,
	})
	if err != nil {
		t.Fatal(err)
	}
	const key = "up.txt"
	const body = "payload"
	if err := os.WriteFile(fs.cachePath(key), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fs.UploadFile(context.Background(), key); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if cap.afterPutErr != nil {
		t.Fatalf("caller's file not readable after Put: %v (W2-H1: Put must not close it)", cap.afterPutErr)
	}
	if string(cap.afterPut) != body {
		t.Fatalf("post-Put read = %q, want %q", cap.afterPut, body)
	}
	if got := mem.ServerBodyOrEmpty(key); got != body {
		t.Fatalf("server body = %q, want %q", got, body)
	}
	if cap.file == nil {
		t.Fatal("Put did not receive an *os.File")
	}
	if err := cap.file.Close(); err == nil {
		t.Fatal("UploadFile returned with the put file still open (fd leak)")
	} else if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("second Close = %v, want os.ErrClosed", err)
	}
}
