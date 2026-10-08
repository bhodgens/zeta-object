package webdav

// zfssurface_cursor_test.go — gateway issue #15: the since-id
// pass-through over the webdav ?events surfaces. Byte-parity method is
// the same as zfssurface_test.go: raw byte comparison of the s3
// pipeline's response against the webdav bridge's response for the SAME
// query — one pipeline (the bridge), so parity is structural; this test
// proves it for the cursor queries specifically (universality rule: the
// cursor lands in all frontends or not at all).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/metadata"
)

// TestZFSSurfaceSinceIDParity pins since-id byte parity across the s3
// pipeline and BOTH webdav path shapes, plus the cursor resume shape.
func TestZFSSurfaceSinceIDParity(t *testing.T) {
	e := newZFSSurfaceEnv(t, "zfs-cursor-bkt")
	// Give every canned event a cursor id (the stub provider surfaces
	// whatever the events carry).
	e.stub.configure(
		[]metadata.ObjectEvent{
			{ID: 4, Op: "setattr", Key: "doc.txt", Txg: 1003},
			{ID: 3, Op: "truncate", Key: "doc.txt", Txg: 1002, SizeOld: 11, SizeNew: 42},
			{ID: 2, Op: "remove", Key: "gone.log", Txg: 1001},
			{ID: 1, Op: "create", Key: "doc.txt", Txg: 1000, SizeNew: 11},
		},
		metadata.HistoryDetail{Dataset: "stub/ds", RecordsLost: 3, RingSwaps: 1},
		nil,
	)

	queries := []string{
		"?events&since-id=2",
		"?events&since-id=0",
		"?events&since-id=99&max-events=2",
		"?events&since-id=1",
	}
	for _, q := range queries {
		s3Code, s3Hdr, s3Body := e.s3Get("/"+e.bucket+q)
		if s3Code != http.StatusOK || s3Hdr.Get("Content-Type") != "application/json" {
			t.Fatalf("s3 %s baseline: status %d (body %s)", q, s3Code, s3Body)
		}
		for _, f := range []struct {
			name   string
			target string
			get    func(string) (int, http.Header, string)
		}{
			{"modeB root", "/?events&" + q[len("?events&"):], e.davGet},
			{"modeA collection", "/" + e.bucket + "/?events&" + q[len("?events&"):], e.davAGet},
		} {
			code, _, body := f.get(f.target)
			if code != s3Code {
				t.Fatalf("%s %s: status = %d, want the s3 status %d", f.name, f.target, code, s3Code)
			}
			if body != s3Body {
				t.Fatalf("%s %s: body drift from the s3 wire\ns3:  %s\ndav: %s", f.name, f.target, s3Body, body)
			}
		}
	}

	// File surface: key-scoped since-id is byte-identical too.
	s3Code, _, s3Body := e.s3Get("/"+e.bucket+"/doc.txt?events&since-id=2")
	if s3Code != http.StatusOK {
		t.Fatalf("s3 file since-id: status %d", s3Code)
	}
	code, _, body := e.davGet("/doc.txt?events&since-id=2")
	if code != s3Code || body != s3Body {
		t.Fatalf("file since-id parity break: dav %d/%q vs s3 %d/%q", code, body, s3Code, s3Body)
	}

	// Cursor resume shape: strictly-after, entries carry id, count is
	// the exact post-cursor remainder (ids 1,2 dropped -> 2 entries).
	var resp struct {
		Events []struct {
			ID int64  `json:"id"`
			Op string `json:"op"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(s3Body), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", s3Body, err)
	}
	if len(resp.Events) != 2 || resp.Events[0].ID != 4 || resp.Events[1].ID != 3 {
		t.Fatalf("since-id=2 events = %+v, want exactly ids 4,3 (no duplicate no gap)", resp.Events)
	}

	// Invalid cursor over webdav: the SAME 400 InvalidArgument the s3
	// surface writes (shared bridge validation).
	code, _, body = e.davGet("/?events&since-id=-1")
	if code != http.StatusBadRequest || !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("webdav invalid since-id = %d %q, want 400 InvalidArgument", code, body)
	}
}
