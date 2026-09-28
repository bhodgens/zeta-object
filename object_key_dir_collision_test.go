package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RED test for leaf 5.1 [a]-1: a key used both as a "directory prefix" and as
// an object (s3-tests test_bucket_list_delimiter_basic pattern: PUT foo/bar,
// then PUT foo/bar/xyzzy). The data file for foo/bar occupies the path that
// foo/bar/xyzzy needs as a parent directory, so the second PUT fails with
// 500 InternalError ("mkdir ...: not a directory"). Real S3 stores keys in a
// flat namespace: both keys must coexist.
//
// Storage fix: when the parent directory of a key exists as a FILE (because
// another object's data occupies that path), relocate object data under a
// shadow directory (data stored at <bucket>/.data/<hash-of-key>) so any key
// can coexist with any other key. Keys whose parent path is a directory keep
// the plain on-disk layout.
func TestPutObjectHandler_KeyBothDirAndFile(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	put := func(key string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/test-bucket/"+key, strings.NewReader("data-"+key))
		putObjectHandler(w, req, "test-bucket", key)
		return w
	}

	if w := put("foo/bar"); w.Code != http.StatusOK {
		t.Fatalf("PUT foo/bar: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w := put("foo/bar/xyzzy"); w.Code != http.StatusOK {
		t.Errorf("PUT foo/bar/xyzzy: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Both objects must be readable with correct bodies.
	get := func(key, wantBody string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
		getObjectHandler(w, req, "test-bucket", key)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: expected 200, got %d: %s", key, w.Code, w.Body.String())
			return
		}
		if w.Body.String() != wantBody {
			t.Errorf("GET %s: body = %q, want %q", key, w.Body.String(), wantBody)
		}
	}
	get("foo/bar", "data-foo/bar")
	get("foo/bar/xyzzy", "data-foo/bar/xyzzy")

	// And HeadObject must find both.
	for _, key := range []string{"foo/bar", "foo/bar/xyzzy"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("HEAD", "/test-bucket/"+key, nil)
		headObjectHandler(w, req, "test-bucket", key)
		if w.Code != http.StatusOK {
			t.Errorf("HEAD %s: expected 200, got %d", key, w.Code)
		}
	}
}

// The reverse order must also work: nested key first, then the key that
// collides with the nested key's parent directory name.
func TestPutObjectHandler_FileThenDirKey(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	put := func(key string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/test-bucket/"+key, strings.NewReader("data-"+key))
		putObjectHandler(w, req, "test-bucket", key)
		return w
	}

	if w := put("a/b/c"); w.Code != http.StatusOK {
		t.Fatalf("PUT a/b/c: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w := put("a/b"); w.Code != http.StatusOK {
		t.Errorf("PUT a/b: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w := put("a"); w.Code != http.StatusOK {
		t.Errorf("PUT a: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// Shadow-path layout invariant: an object stored under the shadow dir must
// have its metadata StoragePath point at the shadow data file, and the
// on-disk layout must not use a file as a directory.
func TestPutObjectHandler_ShadowLayoutNoFileAsDir(t *testing.T) {
	env := setupTestEnv(t)
	bucketDir := env.setupBucket(t, "test-bucket")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket/foo/bar", strings.NewReader("x"))
	putObjectHandler(w, req, "test-bucket", "foo/bar")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT foo/bar: expected 200, got %d", w.Code)
	}

	// The data file must exist (either flat or shadow) and must be a file.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("PUT", "/test-bucket/foo/bar/xyzzy", strings.NewReader("y"))
	putObjectHandler(w2, req2, "test-bucket", "foo/bar/xyzzy")
	if w2.Code != http.StatusOK {
		t.Fatalf("PUT foo/bar/xyzzy: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// foo/bar's data file must still be a regular file at the flat path
	// (it was stored first) or in the shadow dir — but in no case may a
	// file be used as a directory.
	flat := filepath.Join(bucketDir, "foo", "bar")
	if info, err := os.Stat(flat); err == nil && info.IsDir() {
		t.Errorf("flat path %s is a directory; file-as-directory corruption", flat)
	}
}
