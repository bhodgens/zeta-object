package main

// Leaf 4.7 — native fuzz targets over the four hand-rolled parsers.
//
// Each target's assertion is "does not panic and terminates", plus a cheap
// invariant where one exists. Seeds are drawn from real wire shapes (the
// same inputs the table tests exercise). Run with `make fuzz` (30s each) or
// a full `go test -fuzz=FuzzXxx -fuzztime=10m .`.

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"testing"
)

// FuzzStripJSON5Comments: stripJSON5Comments must never panic. Identity
// invariant: input that is already valid JSON (hence contains no comments)
// must come back unchanged.
func FuzzStripJSON5Comments(f *testing.F) {
	seeds := []string{
		`{"cmd": "ls -la", "trigger": "put"}`,
		`[1, 2, 3]`,
		`{"a": {"b": [true, false, null]}}`,
		`{"url": "https://example.com//path", "n": 1}`,
		`{"s": "keep // this", "t": "and /* this */ too"}`,
		`{"esc": "a\"b\\c\u0041\n\t"}`,
		`{ /* block */ "a": 1 } // trailing`,
		`{"a": 1, // line comment
  "b": 2}`,
		`""`,
		`// only a comment`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out := stripJSON5Comments(data)
		if json.Valid(data) && string(out) != string(data) {
			t.Fatalf("stripJSON5Comments mutated valid JSON: in=%q out=%q", data, out)
		}
	})
}

// FuzzParseRangeHeader: must never panic; outcome is one of the three known
// values; a partial outcome always describes a slice within [0, size).
func FuzzParseRangeHeader(f *testing.F) {
	seeds := []string{
		"bytes=0-99",
		"bytes=0-",
		"bytes=-100",
		"bytes=5-2",
		"bytes=-",
		"bytes=0-1,3-4",
		"items=0-10",
		"",
		"bytes=99999999999999999999-0",
		"bytes= 10 - 20 ",
	}
	for _, s := range seeds {
		f.Add(s, int64(1000))
		f.Add(s, int64(0))
	}
	f.Fuzz(func(t *testing.T, spec string, size int64) {
		if size < 0 {
			t.Skip("negative object size is not a reachable wire state (size comes from file size)")
		}
		rr := parseRangeHeader(spec, size)
		switch rr.Outcome {
		case rangeFull:
		case rangeUnsatisfiable:
		case rangePartial:
			if rr.Start < 0 || rr.Length < 0 || rr.Start+rr.Length > size {
				t.Fatalf("parseRangeHeader(%q, %d) = partial start=%d length=%d: slice out of bounds",
					spec, size, rr.Start, rr.Length)
			}
		default:
			t.Fatalf("parseRangeHeader(%q, %d) returned unknown outcome %d", spec, size, rr.Outcome)
		}
	})
}

// FuzzMatchPathGlob: must never panic (or hang) for any pattern+key pair,
// including patterns full of regexp/glob metacharacters. Empty-pattern and
// metachar behavior is pinned by the seeds.
func FuzzMatchPathGlob(f *testing.F) {
	seedPairs := [][2]string{
		{"ephemeral/tmp/a.log", "ephemeral/**"},
		{"a/b/c.txt", "**/*.txt"},
		{"logs/2026/app.log", "logs/*/*.log"},
		{"logs/app.log", "logs/*/*.log"},
		{"plain.txt", "*.txt"},
		{"plain.txt", ""},
		{"", "*"},
		{"a(b)[c].txt", "a(b)[c].txt"},
		{"x", "a**b"},
		{"deep/a/b/c", "deep/**"},
	}
	for _, p := range seedPairs {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, path, pattern string) {
		_ = matchPathGlob(path, pattern)
	})
}

// FuzzParseCopySource: must never panic; the bucket half never contains "/"
// or "?" and the key half never contains "?" (both are cut before return).
func FuzzParseCopySource(f *testing.F) {
	seeds := []string{
		"srcbucket/photos/beach.jpg",
		"/srcbucket/photos/beach.jpg",
		"srcbucket/key?versionId=abc123",
		"/srcbucket/key?versionId=null",
		"bucket-with.dots/key with spaces.txt",
		"bucketonly",
		"",
		"/",
		"?versionId=x",
		"a/b/c/d",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, copySource string) {
		srcBucket, srcKey, hasVersionID := parseCopySource(copySource)
		if containsAny(srcBucket, "/?") {
			t.Fatalf("parseCopySource(%q): bucket %q contains / or ?", copySource, srcBucket)
		}
		if containsAny(srcKey, "?") {
			t.Fatalf("parseCopySource(%q): key %q contains ?", copySource, srcKey)
		}
		_ = hasVersionID
	})
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, r := range s {
			if r == c {
				return true
			}
		}
	}
	return false
}

// FuzzGetCanonicalURI: the SigV4 canonical-URI path is computed from raw
// client bytes (URL.EscapedPath / RequestURI re-encoding). It must never
// panic and must always return a non-empty path starting with "/" — the
// signature canonical form requires it.
func FuzzGetCanonicalURI(f *testing.F) {
	seeds := []string{
		"/",
		"/bucket",
		"/bucket/key",
		"/bucket/key with spaces",
		"/bucket/uni%C3%A7ode",
		"/bucket/a%2Fb",
		"/bucket/./dot",
		"/bucket/../dotdot",
		"/bucket//double//slash",
		"/%zz-bad-escape",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rawPath string) {
		// Build the request the way the server receives one: via the HTTP
		// request URL. Unparseable inputs skip — the server's mux rejects
		// those before auth (and getCanonicalURI) ever run.
		u, err := url.ParseRequestURI("https://h" + rawPath)
		if err != nil {
			t.Skip("not a parseable request URI — rejected by the HTTP stack before getCanonicalURI")
		}
		// httptest.NewRequest panics on targets containing spaces or other
		// malformed request-line bytes; url.Parse accepts them but no real
		// HTTP request line can carry them, so skip (the server's request
		// parser rejects those before getCanonicalURI).
		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			t.Skip("not a buildable http.Request — rejected by the HTTP stack before getCanonicalURI")
		}
		got := getCanonicalURI(req)
		if got == "" {
			t.Fatalf("getCanonicalURI(%q) = empty string", rawPath)
		}
		if got[0] != '/' {
			t.Fatalf("getCanonicalURI(%q) = %q: does not start with /", rawPath, got)
		}
	})
}

// FuzzErrorToXML: error code and message come from request-derived strings
// (S3 error codes, upstream error text). Marshaling must never panic and
// must produce well-formed XML that round-trips through the decoder with
// the same Code and Message — an encoding bug here is a cross-protocol
// response bug.
func FuzzErrorToXML(f *testing.F) {
	seeds := []struct{ code, message string }{
		{"NoSuchKey", "The specified key does not exist."},
		{"NoSuchBucket", "bucket <gone>"},
		{"AccessDenied", `quote " and ' apostrophe`},
		{"&Code", "amp &<brackets>"},
		{"", ""},
		{"Code\nWith\nNewlines", "tab	here"},
	}
	for _, s := range seeds {
		f.Add(s.code, s.message)
	}
	f.Fuzz(func(t *testing.T, code, message string) {
		x := errorToXML(code, message)
		if x == "" {
			t.Fatal("errorToXML returned empty body")
		}
		var decoded struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if err := xml.Unmarshal([]byte(x), &decoded); err != nil {
			t.Fatalf("errorToXML(%q, %q) = %q: not well-formed XML: %v", code, message, x, err)
		}
		// Round-trip comparison: errorToXML must behave exactly like a
		// direct xml.Marshal of the same fields. encoding/xml replaces
		// invalid UTF-8 and XML-forbidden control characters with U+FFFD
		// (documented) — whatever the marshaller does, errorToXML must do
		// identically; the point of this target is that the wrapper adds
		// no divergence of its own.
		var direct struct {
			XMLName xml.Name `xml:"Error"`
			Code    string   `xml:"Code"`
			Message string   `xml:"Message"`
		}
		direct.Code, direct.Message = code, message
		want, err := xml.Marshal(direct)
		if err != nil {
			t.Skipf("unmarshalable input: %v", err)
		}
		var wantDecoded struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if err := xml.Unmarshal(want, &wantDecoded); err != nil {
			t.Skipf("direct marshal not decodable: %v", err)
		}
		if decoded.Code != wantDecoded.Code || decoded.Message != wantDecoded.Message {
			t.Fatalf("errorToXML(%q, %q) = Code %q Message %q; direct marshal gives Code %q Message %q",
				code, message, decoded.Code, decoded.Message, wantDecoded.Code, wantDecoded.Message)
		}
	})
}
