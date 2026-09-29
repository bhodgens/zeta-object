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

func TestListTokenPlainKeySharingGroupPrefix(t *testing.T) {
	// B1 regression probe (bughunt finding B1): a continuation token that is
	// a PLAIN key ("a") sharing the roll-up prefix must NOT consume the
	// delimiter group "a/" — S3 rule: NextContinuationToken after key "a"
	// means resume strictly after "a", so a/1 and a/2 must still be listed
	// (and rolled up) on the next page.
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{
		"a":   "1",
		"a/1": "2",
		"a/2": "3",
		"z":   "4",
	})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/", MaxKeys: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"a"}) {
		t.Fatalf("page 1 keys = %v, want [a]", got)
	}
	if !page.IsTruncated || page.NextToken == "" {
		t.Fatalf("page 1 truncated=%t token=%q", page.IsTruncated, page.NextToken)
	}
	t.Logf("page 1 token = %q", page.NextToken)

	page2, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/", MaxKeys: 10, ContinuationToken: page.NextToken})
	if err != nil {
		t.Fatal(err)
	}
	if got := page2.CommonPrefixes; !equalSlices(got, []string{"a/"}) {
		t.Errorf("page 2 prefixes = %v, want [a/] — group lost by token %q", got, page.NextToken)
	}
	if got := keysOf(page2); !equalSlices(got, []string{"z"}) {
		t.Errorf("page 2 keys = %v, want [z]", got)
	}
	if page2.IsTruncated || page2.NextToken != "" {
		t.Errorf("page 2 truncated=%t token=%q, want false/empty", page2.IsTruncated, page2.NextToken)
	}
}

func TestListTokenIsRolledUpPrefixItself(t *testing.T) {
	// Token that IS the roll-up prefix ("a/") consumed its group on the page
	// that issued it → the next page must NOT re-emit "a/".
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{
		"a":   "1",
		"a/1": "2",
		"a/2": "3",
		"z":   "4",
	})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/", MaxKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(page); !equalSlices(got, []string{"a"}) {
		t.Fatalf("page 1 keys = %v, want [a]", got)
	}
	if got := page.CommonPrefixes; !equalSlices(got, []string{"a/"}) {
		t.Fatalf("page 1 prefixes = %v, want [a/]", got)
	}
	if page.NextToken != "a/" {
		t.Fatalf("page 1 token = %q, want \"a/\" (last emitted item)", page.NextToken)
	}

	page2, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/", MaxKeys: 10, ContinuationToken: page.NextToken})
	if err != nil {
		t.Fatal(err)
	}
	if got := page2.CommonPrefixes; len(got) != 0 {
		t.Errorf("page 2 must not re-emit a/, got %v", got)
	}
	if got := keysOf(page2); !equalSlices(got, []string{"z"}) {
		t.Errorf("page 2 keys = %v, want [z]", got)
	}
}

func TestListTokenInsideGroupConsumesGroup(t *testing.T) {
	// Token strictly INSIDE a roll-up group ("a/1") means the page that
	// issued the token already emitted (or passed) the group → the group
	// stays consumed (leaf 5.1 [a]-4 V2 rule, preserved).
	f, _ := newTestFS(t)
	mustPutAll(t, f, "bkt", map[string]string{
		"a/1": "1",
		"a/2": "2",
		"z":   "3",
	})
	page, err := f.List(context.Background(), "bkt", objectmodel.ListParams{Delimiter: "/", ContinuationToken: "a/1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := page.CommonPrefixes; len(got) != 0 {
		t.Errorf("group a/ must be consumed by in-group token, got %v", got)
	}
	if got := keysOf(page); !equalSlices(got, []string{"z"}) {
		t.Errorf("keys = %v, want [z]", got)
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
