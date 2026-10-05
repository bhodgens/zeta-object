package webdav

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// rangeBody reads the full response body as a string (test helper).
func rangeBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}

func TestGET_Range(t *testing.T) {
	content := "0123456789"
	tests := []struct {
		name string

		method        string
		rng           string
		inm           string
		collection    bool
		wantStatus    int
		wantCL        string // "" = header must be absent
		wantCR        string // "" = header must be absent
		wantBody      string
		wantMultipart bool     // multi-span: parse boundary, count parts
		wantParts     []string // per-part Content-Range lines (multipart)
	}{
		{
			name:       "single span 0-4",
			rng:        "bytes=0-4",
			wantStatus: 206,
			wantCL:     "5",
			wantCR:     "bytes 0-4/10",
			wantBody:   "01234",
		},
		{
			name:       "suffix -3",
			rng:        "bytes=-3",
			wantStatus: 206,
			wantCL:     "3",
			wantCR:     "bytes 7-9/10",
			wantBody:   "789",
		},
		{
			name:       "open-ended 8-",
			rng:        "bytes=8-",
			wantStatus: 206,
			wantCL:     "2",
			wantCR:     "bytes 8-9/10",
			wantBody:   "89",
		},
		{
			name:       "malformed spec ignored",
			rng:        "bytes=abc",
			wantStatus: 200,
			wantCL:     "10",
			wantBody:   content,
		},
		{
			name:       "unsatisfiable 416",
			rng:        "bytes=50-",
			wantStatus: 416,
			wantCR:     "bytes */10",
			wantBody:   "",
		},
		{
			name:          "multi-span multipart",
			rng:           "bytes=0-1,5-6",
			wantStatus:    206,
			wantMultipart: true,
			wantParts:     []string{"Content-Range: bytes 0-1/10", "Content-Range: bytes 5-6/10"},
		},
		{
			name:       "HEAD with range",
			method:     "HEAD",
			rng:        "bytes=0-4",
			wantStatus: 206,
			wantCL:     "5",
			wantCR:     "bytes 0-4/10",
			wantBody:   "",
		},
		{
			name:       "If-None-Match wins over Range",
			rng:        "bytes=0-4",
			inm:        `"abc"`,
			wantStatus: 304,
			wantBody:   "",
		},
		{
			name:       "Range on collection ignored",
			rng:        "bytes=0-4",
			collection: true,
			wantStatus: 200,
			wantBody:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, be := newTestFrontend(Config{})
			be.seed("photos", "a.txt", []byte(content), func(o *objectmodel.Object) {
				o.ETag = `"abc"`
			})
			be.seed("photos", "dir/", []byte(""), func(o *objectmodel.Object) {})
			path := "/photos/a.txt"
			if tt.collection {
				// Collections resolve with the trailing slash (pinned
				// GET decision: a prefix without the slash is a 404).
				path = "/photos/dir/"
			}
			method := tt.method
			if method == "" {
				method = "GET"
			}
			req := httptest.NewRequest(method, path, nil)
			if tt.rng != "" {
				req.Header.Set("Range", tt.rng)
			}
			if tt.inm != "" {
				req.Header.Set("If-None-Match", tt.inm)
			}
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Range"); got != tt.wantCR {
				t.Fatalf("Content-Range = %q, want %q", got, tt.wantCR)
			}
			if tt.collection {
				tt.wantCL = "0" // collection always answers Content-Length: 0
			}
			if got := rec.Header().Get("Content-Length"); got != tt.wantCL {
				t.Fatalf("Content-Length = %q, want %q", got, tt.wantCL)
			}
			body := rangeBody(t, rec)
			if !tt.wantMultipart {
				if body != tt.wantBody {
					t.Fatalf("body = %q, want %q", body, tt.wantBody)
				}
				return
			}
			// Multi-span: multipart/byteranges with the requested parts,
			// each carrying its own Content-Range and Content-Type.
			ct := rec.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "multipart/byteranges; boundary=") {
				t.Fatalf("Content-Type = %q, want multipart/byteranges with boundary", ct)
			}
			boundary := strings.TrimPrefix(ct, "multipart/byteranges; boundary=")
			for _, part := range tt.wantParts {
				if !strings.Contains(body, part+"\r\n") {
					t.Fatalf("multipart body missing part header %q:\n%s", part, body)
				}
			}
			if n := strings.Count(body, "--"+boundary+"\r\n"); n != 2 {
				t.Fatalf("multipart part count = %d, want 2:\n%s", n, body)
			}
			if !strings.Contains(body, "Content-Type: text/plain\r\n") {
				t.Fatalf("multipart parts missing object Content-Type:\n%s", body)
			}
			if !strings.Contains(body, "\r\n01\r\n") || !strings.Contains(body, "\r\n56\r\n") {
				t.Fatalf("multipart part payloads wrong:\n%s", body)
			}
		})
	}
}
