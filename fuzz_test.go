package main

// Leaf 4.7 — native fuzz targets over the four hand-rolled parsers.
//
// Each target's assertion is "does not panic and terminates", plus a cheap
// invariant where one exists. Seeds are drawn from real wire shapes (the
// same inputs the table tests exercise). Run with `make fuzz` (30s each) or
// a full `go test -fuzz=FuzzXxx -fuzztime=10m .`.

import (
	"encoding/json"
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
		switch rr.outcome {
		case rangeFull:
		case rangeUnsatisfiable:
		case rangePartial:
			if rr.start < 0 || rr.length < 0 || rr.start+rr.length > size {
				t.Fatalf("parseRangeHeader(%q, %d) = partial start=%d length=%d: slice out of bounds",
					spec, size, rr.start, rr.length)
			}
		default:
			t.Fatalf("parseRangeHeader(%q, %d) returned unknown outcome %d", spec, size, rr.outcome)
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
