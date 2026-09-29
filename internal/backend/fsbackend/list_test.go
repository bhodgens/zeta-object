// Package fsbackend — list_test.go: table-driven List tests porting the
// pre-seam ListObjectsV2 storage-half semantics. encoding-type=url stays
// HANDLER-side by design (the backend returns raw keys; the S3 frontend
// encodes) — noted here per the leaf spec.
package fsbackend

import (
	"context"
	"strings"
	"testing"

	"mini-s3/internal/objectmodel"
)

func mustPutAll(t *testing.T, f *FS, bucket string, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		if _, err := f.Put(context.Background(), bucket, k, strings.NewReader(v), int64(len(v)), objectmodel.PutOptions{}); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
}

func keysOf(page objectmodel.ListPage) []string {
	keys := make([]string, len(page.Objects))
	for i, o := range page.Objects {
		keys[i] = o.Key
	}
	return keys
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListFlatAndNested(t *testing.T) {
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{
		"a.txt": "1",
		"a/b/c": "2", // "/" in keys is a real directory on disk
		"d":     "3",
	})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "a/b/c", "d"}
	if got := keysOf(page); !equalSlices(got, want) {
		t.Errorf("keys = %v, want lexicographic %v", got, want)
	}
	if page.IsTruncated {
		t.Error("unexpected truncation")
	}
}

func TestListPrefixFilter(t *testing.T) {
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{
		"dir/a":   "1",
		"dir/b":   "2",
		"other/c": "3",
	})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Prefix: "dir/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"dir/a", "dir/b"}) {
		t.Errorf("keys = %v, want [dir/a dir/b]", got)
	}
	if len(page.CommonPrefixes) != 0 {
		t.Errorf("prefix-only list must not group, got %v", page.CommonPrefixes)
	}
}

func TestListDelimiterGrouping(t *testing.T) {
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{
		"a.txt":  "1",
		"b/1":    "2",
		"b/2":    "3",
		"b2.txt": "4",
	})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"a.txt", "b2.txt"}) {
		t.Errorf("keys = %v, want [a.txt b2.txt]", got)
	}
	// Prefixes merge-sort with objects: "b/" < "b2.txt".
	if got := page.CommonPrefixes; !equalSlices(got, []string{"b/"}) {
		t.Errorf("common prefixes = %v, want [b/]", got)
	}
}

func TestListMarkerExclusion(t *testing.T) {
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{"k01": "1", "k02": "2", "k03": "3", "k04": "4"})

	// StartAfter excludes at-or-below (including the marker key itself).
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{StartAfter: "k02"})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"k03", "k04"}) {
		t.Errorf("start-after keys = %v, want [k03 k04]", got)
	}

	// ContinuationToken lists its boundary key (exclude strictly below).
	page, err = f.List(context.Background(), "bkt", objectmodel.ListParams{ContinuationToken: "k02"})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"k02", "k03", "k04"}) {
		t.Errorf("token keys = %v, want [k02 k03 k04]", got)
	}
}

func TestListMaxKeysTruncation(t *testing.T) {
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{"k01": "1", "k02": "2", "k03": "3", "k04": "4", "k05": "5"})

	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{MaxKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"k01", "k02"}) {
		t.Errorf("page 1 = %v, want [k01 k02]", got)
	}
	if !page.IsTruncated {
		t.Fatal("page 1 must be truncated")
	}
	if page.NextToken == "" {
		t.Fatal("truncated page must carry NextToken")
	}

	page2, err := f.List(context.Background(), "bkt", objectmodel.ListParams{MaxKeys: 2, ContinuationToken: page.NextToken})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page2); !equalSlices(got, []string{"k03", "k04"}) {
		t.Errorf("page 2 = %v, want [k03 k04]", got)
	}
	page3, err := f.List(context.Background(), "bkt", objectmodel.ListParams{MaxKeys: 2, ContinuationToken: page2.NextToken})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page3); !equalSlices(got, []string{"k05"}) {
		t.Errorf("page 3 = %v, want [k05]", got)
	}
	if page3.IsTruncated || page3.NextToken != "" {
		t.Errorf("final page truncated=%t token=%q, want false/empty", page3.IsTruncated, page3.NextToken)
	}
}

func TestListDelimiterPaginationMergedOrder(t *testing.T) {
	// Leaf 5.1 [a]-4: the token is the last emitted item in MERGED order,
	// and a token inside a prefix group consumes that group.
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{
		"a.txt": "1", "boo/1": "2", "boo/2": "3", "boo/3": "4", "z.txt": "5",
	})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/", MaxKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"a.txt"}) {
		t.Errorf("page 1 keys = %v, want [a.txt]", got)
	}
	if got := page.CommonPrefixes; !equalSlices(got, []string{"boo/"}) {
		t.Errorf("page 1 prefixes = %v, want [boo/]", got)
	}
	if !page.IsTruncated || page.NextToken == "" {
		t.Fatalf("page 1 truncated=%t token=%q", page.IsTruncated, page.NextToken)
	}
	// The token (last emitted item, "boo/") must consume the whole group.
	page2, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/", MaxKeys: 5, ContinuationToken: page.NextToken})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page2); !equalSlices(got, []string{"z.txt"}) {
		t.Errorf("page 2 keys = %v, want [z.txt]", got)
	}
	if got := page2.CommonPrefixes; len(got) != 0 {
		t.Errorf("page 2 must not re-emit boo/, got %v", got)
	}
}

func TestListMissingBucket(t *testing.T) {
	f, _ := newTestFS(t)
	if _, err := f.List(context.Background(), "nope", objectmodel.ListParams{}); err == nil {
		t.Fatal("List on missing bucket must error")
	}
}

func TestListMaxKeysZero(t *testing.T) {
	// Conformance pin at the seam: MaxKeys 0 = implementation default
	// (unbounded for small fixtures). The pre-seam max-keys=0 → empty
	// behavior is the S3 handler's short-circuit, applied before it calls
	// through the seam (pinned handler-side in handlers_backend_flip_test.go).
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{"a": "1"})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{MaxKeys: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 1 || page.IsTruncated {
		t.Errorf("MaxKeys=0 (unset) page = %+v, want the unbounded default", page)
	}
}
