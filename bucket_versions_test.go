package main

// RED→GREEN tests for leaf 5.1 [a]-2: ListObjectVersions (GET /bucket?versions).
// The ceph/s3-tests teardown (nuke_prefixed_buckets) drives every bucket
// cleanup through list_object_versions + delete_objects; without this
// sub-resource the suite's per-test setup errored 276 times.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListObjectVersionsHandler_BasicShape(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	// Two objects.
	for _, key := range []string{"alpha", "beta/gamma"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/test-bucket/"+key, strings.NewReader("data-"+key))
		putObjectHandler(w, req, "test-bucket", key)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s: expected 200, got %d", key, w.Code)
		}
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?versions", nil)
	listObjectVersionsHandler(w, req, "test-bucket")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"ListVersionsResult", "<Key>alpha</Key>", "<Key>beta/gamma</Key>", "<VersionId>null</VersionId>", "<IsLatest>true</IsLatest>"} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q; body:\n%s", want, body)
		}
	}
}

func TestListObjectVersionsHandler_NoSuchBucket(t *testing.T) {
	env := setupTestEnv(t)
	_ = env

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/missing?versions", nil)
	listObjectVersionsHandler(w, req, "missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Errorf("expected NoSuchBucket error, got: %s", w.Body.String())
	}
}

func TestListObjectVersionsHandler_PrefixAndMaxKeys(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "test-bucket")

	for _, key := range []string{"a/1", "a/2", "b/1"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/test-bucket/"+key, strings.NewReader("d"))
		putObjectHandler(w, req, "test-bucket", key)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s: expected 200, got %d", key, w.Code)
		}
	}

	// Prefix filter.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket?versions&prefix=a/", nil)
	listObjectVersionsHandler(w, req, "test-bucket")
	body := w.Body.String()
	if strings.Count(body, "<Key>") != 2 {
		t.Errorf("prefix filter: expected 2 keys, body:\n%s", body)
	}
	if strings.Contains(body, "<Key>b/1</Key>") {
		t.Errorf("prefix filter leaked b/1:\n%s", body)
	}

	// max-keys truncation.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/test-bucket?versions&max-keys=2", nil)
	listObjectVersionsHandler(w2, req2, "test-bucket")
	if !strings.Contains(w2.Body.String(), "<IsTruncated>true</IsTruncated>") {
		t.Errorf("max-keys=2: expected IsTruncated true, body:\n%s", w2.Body.String())
	}
	if got := strings.Count(w2.Body.String(), "<Key>"); got != 2 {
		t.Errorf("max-keys=2: expected exactly 2 keys, got %d:\n%s", got, w2.Body.String())
	}
}
