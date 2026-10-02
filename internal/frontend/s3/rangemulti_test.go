package s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// memFetch returns a fetch callback serving [off, end) of data from an
// in-memory buffer, recording each call for order assertions.
type memFetch struct {
	data  []byte
	calls []Span
}

func (m *memFetch) fetch(off, end int64) (io.ReadCloser, error) {
	m.calls = append(m.calls, Span{Start: off, End: end})
	if off < 0 || end > int64(len(m.data)) || off > end {
		return nil, fmt.Errorf("s3: bad span [%d,%d)", off, end)
	}
	return io.NopCloser(bytes.NewReader(m.data[off:end])), nil
}

// newMultipartTestServer builds a 1KB pattern object and a recorder.
func newMultipartTestServer(t *testing.T, contentType string) (*memFetch, *httptest.ResponseRecorder) {
	t.Helper()
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i)
	}
	f := &memFetch{data: data}
	w := httptest.NewRecorder()
	return f, w
}

// boundaryOf extracts the boundary parameter from the response
// Content-Type header and fails the test when it is absent.
func boundaryOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	ct := w.Header().Get("Content-Type")
	prefix := "multipart/byteranges; boundary="
	if !strings.HasPrefix(ct, prefix) {
		t.Fatalf("Content-Type %q does not carry a boundary", ct)
	}
	return strings.TrimPrefix(ct, prefix)
}

// splitMultipart splits a multipart/byteranges body on the boundary into
// parts, each as (headers block, payload). The preamble and epilogue are
// dropped.
func splitMultipart(t *testing.T, body, boundary string) []part {
	t.Helper()
	first := "--" + boundary
	idx := strings.Index(body, first)
	if idx != 0 {
		t.Fatalf("body does not start with --%s: %q", boundary, body[:min(40, len(body))])
	}
	rest := body[len(first)+len("\r\n"):] // skip the delimiter line's CRLF; headers start here
	parts := []part{}
	for {
		headerEnd := strings.Index(rest, "\r\n\r\n")
		if headerEnd < 0 {
			t.Fatalf("part without header terminator: %q", rest)
		}
		headers := rest[:headerEnd]
		payloadStart := headerEnd + 4
		sep := "\r\n--" + boundary
		next := strings.Index(rest[payloadStart:], sep)
		if next < 0 {
			t.Fatalf("part payload not terminated by boundary: %q", rest[payloadStart:])
		}
		parts = append(parts, part{headers: headers, payload: rest[payloadStart : payloadStart+next]})
		rest = rest[payloadStart+next+len(sep):]
		if strings.HasPrefix(rest, "--") {
			break // closing delimiter
		}
		// Mid-stream delimiter: its trailing CRLF ends the delimiter line;
		// the next part's headers start immediately after it.
		rest = strings.TrimPrefix(rest, "\r\n")
	}
	return parts
}

type part struct {
	headers string
	payload string
}

func TestWriteMultipartByteranges(t *testing.T) {
	t.Run("two spans frame 206 with correct parts and terminator", func(t *testing.T) {
		f, w := newMultipartTestServer(t, "application/octet-stream")
		spans := []Span{{Start: 0, End: 10}, {Start: 100, End: 120}}

		err := WriteMultipartByteranges(w, "k", 1024, "application/octet-stream", spans, f.fetch)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 206 {
			t.Fatalf("status = %d, want 206", w.Code)
		}
		boundary := boundaryOf(t, w)

		body := w.Body.String()
		if !strings.Contains(body, "--"+boundary+"--") {
			t.Fatal("closing boundary missing")
		}

		parts := splitMultipart(t, body, boundary)
		if len(parts) != 2 {
			t.Fatalf("got %d parts, want 2", len(parts))
		}
		wantCR := []string{
			"Content-Range: bytes 0-9/1024",
			"Content-Range: bytes 100-119/1024",
		}
		wantPayload := []string{
			string(f.data[0:10]),
			string(f.data[100:120]),
		}
		for i, p := range parts {
			if !strings.Contains(p.headers, wantCR[i]) {
				t.Errorf("part %d headers %q missing %q", i, p.headers, wantCR[i])
			}
			if p.payload != wantPayload[i] {
				t.Errorf("part %d payload mismatch: got %q want %q", i, p.payload, wantPayload[i])
			}
		}
	})

	t.Run("boundary is 32-char hex safe for CRLF framing", func(t *testing.T) {
		f, w := newMultipartTestServer(t, "")
		spans := []Span{{Start: 0, End: 1024}}
		if err := WriteMultipartByteranges(w, "k", 1024, "", spans, f.fetch); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b := boundaryOf(t, w)
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(b) {
			t.Fatalf("boundary %q is not 32-char lowercase hex", b)
		}
	})

	t.Run("empty spans is a caller bug and writes nothing", func(t *testing.T) {
		f, w := newMultipartTestServer(t, "")
		err := WriteMultipartByteranges(w, "k", 1024, "", nil, f.fetch)
		if err == nil {
			t.Fatal("expected error for empty spans")
		}
		if w.Body.Len() != 0 {
			t.Fatalf("body written despite error: %q", w.Body.String())
		}
		if len(f.calls) != 0 {
			t.Fatalf("fetch called %d times for empty spans", len(f.calls))
		}
	})

	t.Run("empty contentType omits part Content-Type; set propagates", func(t *testing.T) {
		// Empty: no Content-Type header in parts.
		f, w := newMultipartTestServer(t, "")
		spans := []Span{{Start: 0, End: 4}, {Start: 8, End: 12}}
		if err := WriteMultipartByteranges(w, "k", 1024, "", spans, f.fetch); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i, p := range splitMultipart(t, w.Body.String(), boundaryOf(t, w)) {
			if strings.Contains(p.headers, "Content-Type:") {
				t.Errorf("part %d has unexpected Content-Type: %q", i, p.headers)
			}
		}

		// Non-empty: every part carries it.
		f2, w2 := newMultipartTestServer(t, "")
		if err := WriteMultipartByteranges(w2, "k", 1024, "text/plain", spans, f2.fetch); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i, p := range splitMultipart(t, w2.Body.String(), boundaryOf(t, w2)) {
			if !strings.Contains(p.headers, "Content-Type: text/plain") {
				t.Errorf("part %d missing Content-Type: text/plain (headers %q)", i, p.headers)
			}
		}
	})

	t.Run("single span still renders valid multipart", func(t *testing.T) {
		f, w := newMultipartTestServer(t, "text/plain")
		spans := []Span{{Start: 5, End: 15}}
		if err := WriteMultipartByteranges(w, "k", 1024, "text/plain", spans, f.fetch); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parts := splitMultipart(t, w.Body.String(), boundaryOf(t, w))
		if len(parts) != 1 {
			t.Fatalf("got %d parts, want 1", len(parts))
		}
		if !strings.Contains(parts[0].headers, "Content-Range: bytes 5-14/1024") {
			t.Errorf("part headers missing Content-Range: %q", parts[0].headers)
		}
		if parts[0].payload != string(f.data[5:15]) {
			t.Errorf("payload mismatch: got %q", parts[0].payload)
		}
	})

	t.Run("fetch is called lazily in ascending span order", func(t *testing.T) {
		f, w := newMultipartTestServer(t, "")
		spans := []Span{{Start: 200, End: 210}, {Start: 0, End: 10}, {Start: 100, End: 110}}
		if err := WriteMultipartByteranges(w, "k", 1024, "", spans, f.fetch); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []Span{{Start: 0, End: 10}, {Start: 100, End: 110}, {Start: 200, End: 210}}
		if len(f.calls) != len(want) {
			t.Fatalf("fetch called %d times, want %d", len(f.calls), len(want))
		}
		for i, c := range f.calls {
			if c != want[i] {
				t.Errorf("fetch call %d = %v, want %v (ascending order)", i, c, want[i])
			}
		}
	})

	t.Run("mid-stream fetch error returns error after part 1 flushed", func(t *testing.T) {
		data := make([]byte, 1024)
		for i := range data {
			data[i] = byte(i)
		}
		calls := 0
		fetch := func(off, end int64) (io.ReadCloser, error) {
			calls++
			if calls == 2 {
				return nil, errors.New("s3: storage exploded")
			}
			return io.NopCloser(bytes.NewReader(data[off:end])), nil
		}
		w := httptest.NewRecorder()
		spans := []Span{{Start: 0, End: 10}, {Start: 100, End: 110}}

		err := WriteMultipartByteranges(w, "k", 1024, "", spans, fetch)
		if err == nil {
			t.Fatal("expected mid-stream fetch error to propagate")
		}
		if !strings.Contains(err.Error(), "storage exploded") {
			t.Errorf("error lost the underlying cause: %v", err)
		}
		if w.Body.Len() == 0 {
			t.Fatal("part 1 bytes were not flushed before the error")
		}
		if !strings.Contains(w.Body.String(), string(data[0:10])) {
			t.Error("part 1 payload not present in flushed output")
		}
		if calls != 2 {
			t.Errorf("fetch called %d times, want 2", calls)
		}
	})
}
