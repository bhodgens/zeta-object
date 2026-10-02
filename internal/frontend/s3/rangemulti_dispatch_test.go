package s3

// rangemulti_dispatch_test.go — handler-level dispatch tests for the
// multi-range GET path (multirange-get-2026-10 leaf 03): multi-span Range
// headers serve 206 multipart/byteranges; single-span and malformed headers
// keep the exact existing behavior; over-cap span counts fall back to a
// full-body 200.

import (
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"
)

// multiRangeContentTypeBoundary extracts the boundary parameter from a
// multipart/byteranges Content-Type header value.
func multiRangeContentTypeBoundary(t *testing.T, ct string) string {
	t.Helper()
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("unparsable Content-Type %q: %v", ct, err)
	}
	if mt != "multipart/byteranges" {
		t.Fatalf("expected multipart/byteranges, got %q", ct)
	}
	return params["boundary"]
}

func TestGetObject_MultiRange206(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=0-1,5-6"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 multipart, got %d: %s", w.Code, w.Body.String())
	}
	boundary := multiRangeContentTypeBoundary(t, w.Header().Get("Content-Type"))
	body := w.Body.String()
	for _, want := range []string{
		"Content-Range: bytes 0-1/10",
		"Content-Range: bytes 5-6/10",
		"--" + boundary,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("multipart body missing %q; body: %.200s", want, body)
		}
	}
	// The exact span bytes must appear: content[0:2] and content[5:7].
	if !strings.Contains(body, content[0:2]) || !strings.Contains(body, content[5:7]) {
		t.Errorf("multipart body missing span bytes; body: %.200s", body)
	}
}

func TestGetObject_MultiRangeThreeSpans(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=0-0,4-5,9-9"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 multipart, got %d: %s", w.Code, w.Body.String())
	}
	multiRangeContentTypeBoundary(t, w.Header().Get("Content-Type"))
	body := w.Body.String()
	for _, want := range []string{
		"Content-Range: bytes 0-0/10",
		"Content-Range: bytes 4-5/10",
		"Content-Range: bytes 9-9/10",
		content[0:1], content[4:6], content[9:10],
	} {
		if !strings.Contains(body, want) {
			t.Errorf("multipart body missing %q; body: %.200s", want, body)
		}
	}
}

func TestGetObject_MultiRangeOverlappingCoalesced(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	// 0-3 and 2-5 overlap → coalesced into 0-5, served as one part.
	w := rangeGet(t, map[string]string{"Range": "bytes=0-3,2-5"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 multipart, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Content-Range: bytes 0-5/10") {
		t.Errorf("expected coalesced span bytes 0-5; body: %.200s", body)
	}
	if strings.Contains(body, "Content-Range: bytes 2-5/10") {
		t.Errorf("uncoalesced span still present; body: %.200s", body)
	}
	if !strings.Contains(body, content[0:6]) {
		t.Errorf("expected coalesced bytes; body: %.200s", body)
	}
}

// The single-span path is UNCHANGED: still a plain 206 with a top-level
// Content-Range, NOT multipart.
func TestGetObject_MultiDispatchSingleSpanUnchanged(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	w := rangeGet(t, map[string]string{"Range": "bytes=0-1"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		t.Errorf("single span must not be multipart, got %q", ct)
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 0-1/10" {
		t.Errorf("expected Content-Range 'bytes 0-1/10', got %q", cr)
	}
	if got := w.Body.String(); got != content[0:2] {
		t.Errorf("expected body %q, got %q", content[0:2], got)
	}
}

// Over the cap: 101 disjoint spans against a 10-byte object is impossible,
// so build a bigger object via writeRangeObject and request 101 spans with a
// stride that keeps them disjoint after coalescing.
func TestGetObject_MultiRangeOverCapFallsBack200(t *testing.T) {
	content := strings.Repeat("x", 4096)
	writeRangeObject(t, setupS3TestEnv(t), "range-bucket", "data.bin", content, testObjectModTime())
	// 101 disjoint 2-byte spans (stride 40) → 202 bytes requested, 101 parts.
	var specs []string
	for i := 0; i < 101; i++ {
		start := i * 40
		specs = append(specs, itoaRangeSpan(start, start+1))
	}
	spec := "bytes=" + strings.Join(specs, ",")
	w := rangeGet(t, map[string]string{"Range": spec})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 full body over cap, got %d", w.Code)
	}
	if got := w.Body.String(); got != content {
		t.Errorf("expected full body, got %d bytes", len(got))
	}
}

func testObjectModTime() time.Time {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
}

// itoaRangeSpan formats "start-end".
func itoaRangeSpan(start, end int) string {
	return u64ToString(int64(start)) + "-" + u64ToString(int64(end))
}

func u64ToString(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func TestGetObject_MultiRangeMalformedUnchanged(t *testing.T) {
	_, content, _ := setupRangeObject(t)
	for _, spec := range []string{"bytes=abc", "chunks=0-5,7-8", "bytes="} {
		w := rangeGet(t, map[string]string{"Range": spec})
		if w.Code != http.StatusOK {
			t.Errorf("Range %q: expected 200 (malformed ignored), got %d", spec, w.Code)
			continue
		}
		if got := w.Body.String(); got != content {
			t.Errorf("Range %q: expected full body, got %q", spec, got)
		}
	}
}
