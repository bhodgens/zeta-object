package s3

// tagging_handlers_test.go — the ?tagging sub-resource + x-amz-tagging
// handler-surface tests (tagging tree leaf 03). All object fixtures go
// through fsbackend (the real production write path), and the dispatch is
// exercised through f.serveHTTP so the sub-resource routing is covered.

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// serveTagging builds a Frontend, installs the dev (zero-auth)
// authenticator for the test, and dispatches the request through the real
// routing pipeline (serveHTTP → objectLevelDispatch → handler).
func serveTagging(t *testing.T, method, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	InstallDevAuthenticator(auth.NewDevAuthenticator(nil))
	t.Cleanup(func() { InstallDevAuthenticator(nil) })
	f := &Frontend{}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.serveHTTP(w, req)
	return w
}

// tagFixtureURL is a served request target for a tagged-object test.
func tagFixtureURL(bucket, key, query string) string {
	return fmt.Sprintf("/%s/%s%s", bucket, key, query)
}

func TestTaggingSubresource_PutThenGetRoundTrip(t *testing.T) {
	_, _ = tagTestEnv(t, "tag-bucket", "obj.txt")

	body := `<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag><Tag><Key>team</Key><Value>core</Value></Tag></TagSet></Tagging>`
	w := serveTagging(t, "PUT", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), body, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PUT ?tagging: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	w = serveTagging(t, "GET", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET ?tagging: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var doc objectmodel.Tagging
	if err := xml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal Tagging XML: %v\nbody: %s", err, w.Body.String())
	}
	if len(doc.TagSet.Tags) != 2 {
		t.Fatalf("expected 2 tags, got %+v", doc.TagSet.Tags)
	}
	got := map[string]string{}
	for _, tag := range doc.TagSet.Tags {
		got[tag.Key] = tag.Value
	}
	if got["env"] != "prod" || got["team"] != "core" {
		t.Errorf("tag mismatch: %v", got)
	}
}

func TestTaggingSubresource_GetUntaggedObjectIsNoSuchTagSet(t *testing.T) {
	_, _ = tagTestEnv(t, "tag-bucket", "obj.txt")

	w := serveTagging(t, "GET", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 NoSuchTagSet, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NoSuchTagSet") {
		t.Errorf("expected NoSuchTagSet in body, got: %s", w.Body.String())
	}
}

func TestTaggingSubresource_PutInvalidTagXML(t *testing.T) {
	_, _ = tagTestEnv(t, "tag-bucket", "obj.txt")

	// aws: prefix is reserved → InvalidTag.
	body := `<Tagging><TagSet><Tag><Key>aws:reserved</Key><Value>x</Value></Tag></TagSet></Tagging>`
	w := serveTagging(t, "PUT", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidTag") {
		t.Errorf("expected InvalidTag in body, got: %s", w.Body.String())
	}

	// 11 tags exceeds the limit → InvalidTag.
	var sb strings.Builder
	sb.WriteString("<Tagging><TagSet>")
	for i := range 11 {
		fmt.Fprintf(&sb, "<Tag><Key>k%d</Key><Value>v</Value></Tag>", i)
	}
	sb.WriteString("</TagSet></Tagging>")
	w = serveTagging(t, "PUT", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), sb.String(), nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "InvalidTag") {
		t.Fatalf("expected 400 InvalidTag for 11 tags, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTaggingSubresource_DeleteTags(t *testing.T) {
	_, _ = tagTestEnv(t, "tag-bucket", "obj.txt")

	body := `<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag></TagSet></Tagging>`
	if w := serveTagging(t, "PUT", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), body, nil); w.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}

	w := serveTagging(t, "DELETE", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?tagging: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// After delete, GET yields NoSuchTagSet again.
	w = serveTagging(t, "GET", tagFixtureURL("tag-bucket", "obj.txt", "?tagging"), "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE: expected 404, got %d", w.Code)
	}
}

func TestTaggingSubresource_MissingObjectIsNoSuchKey(t *testing.T) {
	_, _ = tagTestEnv(t, "tag-bucket", "obj.txt")

	w := serveTagging(t, "GET", tagFixtureURL("tag-bucket", "ghost.txt", "?tagging"), "", nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NoSuchKey") {
		t.Fatalf("expected 404 NoSuchKey, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTaggingSubresource_MissingBucketIsNoSuchBucket(t *testing.T) {
	setupS3TestEnv(t)

	w := serveTagging(t, "GET", tagFixtureURL("ghost-bucket", "obj.txt", "?tagging"), "", nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Fatalf("expected 404 NoSuchBucket, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPutObject_XAmzTaggingHeader_StoresTags(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	w := serveTagging(t, "PUT", "/tag-bucket/new.txt", "hello", map[string]string{
		"x-amz-tagging": "env=prod&team=core",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	store := tagStoreFor(env.dataDir + "/tag-bucket")
	tags, err := store.Get("new.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tags["env"] != "prod" || tags["team"] != "core" {
		t.Errorf("expected stored tags env=prod team=core, got %v", tags)
	}
}

func TestPutObject_XAmzTaggingHeader_InvalidIsRejected(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	w := serveTagging(t, "PUT", "/tag-bucket/new.txt", "hello", map[string]string{
		"x-amz-tagging": "aws:reserved=x",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 InvalidTag, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidTag") {
		t.Errorf("expected InvalidTag in body, got: %s", w.Body.String())
	}
	// The object must NOT have been created.
	if _, err := env.b.Stat(t.Context(), "tag-bucket", "new.txt"); err == nil {
		t.Error("object must not exist after a rejected tag header")
	}
}

func TestPutObject_NoTaggingHeader_NoTags(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	w := serveTagging(t, "PUT", "/tag-bucket/plain.txt", "hello", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	store := tagStoreFor(env.dataDir + "/tag-bucket")
	tags, err := store.Get("plain.txt")
	if err != nil || len(tags) != 0 {
		t.Errorf("expected no tags, got %v err=%v", tags, err)
	}
}

func TestGetObject_HeadObject_TagCountHeader(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	// PUT with tags, then GET and HEAD must both carry TagCount.
	w := serveTagging(t, "PUT", "/tag-bucket/tagged.bin", "data", map[string]string{
		"x-amz-tagging": "a=1&b=2&c=3",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}

	w = serveTagging(t, "GET", "/tag-bucket/tagged.bin", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	if got := w.Header().Get("x-amz-tagging-count"); got != "3" {
		t.Errorf("GET TagCount header: expected 3, got %q", got)
	}

	w = serveTagging(t, "HEAD", "/tag-bucket/tagged.bin", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if got := w.Header().Get("x-amz-tagging-count"); got != "3" {
		t.Errorf("HEAD TagCount header: expected 3, got %q", got)
	}
}

func TestGetObject_HeadObject_UntaggedNoTagCount(t *testing.T) {
	_, _ = tagTestEnv(t, "tag-bucket", "plain.txt")

	w := serveTagging(t, "GET", "/tag-bucket/plain.txt", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	if got := w.Header().Get("x-amz-tagging-count"); got != "" {
		t.Errorf("untagged GET must not carry TagCount, got %q", got)
	}

	w = serveTagging(t, "HEAD", "/tag-bucket/plain.txt", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if got := w.Header().Get("x-amz-tagging-count"); got != "" {
		t.Errorf("untagged HEAD must not carry TagCount, got %q", got)
	}
}

func TestCopyObject_DefaultDirectiveCopiesTags(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	// Source with tags (via the real PUT path).
	if w := serveTagging(t, "PUT", "/tag-bucket/src.txt", "payload", map[string]string{
		"x-amz-tagging": "env=prod",
	}); w.Code != http.StatusOK {
		t.Fatalf("PUT src: %d %s", w.Code, w.Body.String())
	}

	// COPY without x-amz-tagging-directive → tags carried over.
	w := serveTagging(t, "PUT", "/tag-bucket/dst.txt", "", map[string]string{
		"x-amz-copy-source": "tag-bucket/src.txt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("COPY: %d %s", w.Code, w.Body.String())
	}
	store := tagStoreFor(env.dataDir + "/tag-bucket")
	tags, err := store.Get("dst.txt")
	if err != nil {
		t.Fatalf("Get dst: %v", err)
	}
	if tags["env"] != "prod" {
		t.Errorf("expected copied tags env=prod, got %v", tags)
	}
}

func TestCopyObject_ReplaceDirectiveUsesHeader(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	if w := serveTagging(t, "PUT", "/tag-bucket/src.txt", "payload", map[string]string{
		"x-amz-tagging": "env=prod&old=yes",
	}); w.Code != http.StatusOK {
		t.Fatalf("PUT src: %d %s", w.Code, w.Body.String())
	}

	// REPLACE + a new header tag set.
	w := serveTagging(t, "PUT", "/tag-bucket/dst.txt", "", map[string]string{
		"x-amz-copy-source":       "tag-bucket/src.txt",
		"x-amz-tagging-directive": "REPLACE",
		"x-amz-tagging":           "fresh=1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("COPY: %d %s", w.Code, w.Body.String())
	}
	store := tagStoreFor(env.dataDir + "/tag-bucket")
	tags, err := store.Get("dst.txt")
	if err != nil {
		t.Fatalf("Get dst: %v", err)
	}
	if len(tags) != 1 || tags["fresh"] != "1" {
		t.Errorf("expected exactly fresh=1 after REPLACE, got %v", tags)
	}
}

func TestCopyObject_ReplaceDirectiveInvalidHeaderIsRejected(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	if w := serveTagging(t, "PUT", "/tag-bucket/src.txt", "payload", nil); w.Code != http.StatusOK {
		t.Fatalf("PUT src: %d %s", w.Code, w.Body.String())
	}

	w := serveTagging(t, "PUT", "/tag-bucket/dst.txt", "", map[string]string{
		"x-amz-copy-source":       "tag-bucket/src.txt",
		"x-amz-tagging-directive": "REPLACE",
		"x-amz-tagging":           "aws:bad=x",
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "InvalidTag") {
		t.Fatalf("expected 400 InvalidTag, got %d: %s", w.Code, w.Body.String())
	}
	// The destination must not exist.
	if _, err := env.b.Stat(t.Context(), "tag-bucket", "dst.txt"); err == nil {
		t.Error("destination must not exist after a rejected COPY tag header")
	}
}

func TestCopyObject_CopyDirectiveExplicit(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "tag-bucket")

	if w := serveTagging(t, "PUT", "/tag-bucket/src.txt", "payload", map[string]string{
		"x-amz-tagging": "keep=me",
	}); w.Code != http.StatusOK {
		t.Fatalf("PUT src: %d %s", w.Code, w.Body.String())
	}

	// Explicit COPY directive: the header (if any) is ignored.
	w := serveTagging(t, "PUT", "/tag-bucket/dst.txt", "", map[string]string{
		"x-amz-copy-source":       "tag-bucket/src.txt",
		"x-amz-tagging-directive": "COPY",
		"x-amz-tagging":           "ignored=yes",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("COPY: %d %s", w.Code, w.Body.String())
	}
	store := tagStoreFor(env.dataDir + "/tag-bucket")
	tags, err := store.Get("dst.txt")
	if err != nil {
		t.Fatalf("Get dst: %v", err)
	}
	if len(tags) != 1 || tags["keep"] != "me" {
		t.Errorf("expected exactly keep=me after COPY, got %v", tags)
	}
}
