package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestPutOsFileBodySurvivesRequestClose pins W2-H1: the production Put
// call sites pass the caller's *os.File. http.NewRequestWithContext adopts
// an io.ReadCloser body as-is, so do()'s GetBody dance closed the caller's
// handle and every real upload failed with "file already closed" (unit
// doubles pass strings.Reader, which net/http wraps in its own NopCloser,
// so only this test shape can catch it). newRequest now wraps the body in
// NopCloser itself; the server must receive the full body.
func TestPutOsFileBodySurvivesRequestClose(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		for {
			m, err := r.Body.Read(buf)
			got = append(got, buf[:m]...)
			if err != nil {
				break
			}
		}
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "body.bin")
	if err := os.WriteFile(p, []byte("hello-os-file-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := NewClient(Options{ServerURL: srv.URL, BasicAuthUser: "ak", BasicAuthPass: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(context.Background(), "k", f, ""); err != nil {
		t.Fatalf("Put with *os.File body: %v", err)
	}
	if string(got) != "hello-os-file-body" {
		t.Fatalf("server got %q - body lost (W2-H1 confirmed)", got)
	}
	// The caller's handle must still be usable after the PUT: do() must
	// never close the *os.File it was handed (the regression closed the
	// CALLER's file, so a post-upload re-read failed "file already
	// closed").
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("post-Put Seek on the caller's file: %v (handle was closed)", err)
	}
	after, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("post-Put ReadAll on the caller's file: %v (handle was closed)", err)
	}
	if string(after) != "hello-os-file-body" {
		t.Fatalf("post-Put file content = %q, want the original body", after)
	}
}
