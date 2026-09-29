// Package conformance holds the reusable Backend contract suite.
//
// Placement decision (documented per the leaf spec): the runner is a plain
// exported function in a NON-test file, because Go cannot export test
// functions across package boundaries. Backend implementations living in
// other packages (internal/backend/fsbackend from leaf 02, future zfsring /
// s3proxy) import this package and call Run with a factory that builds their
// backend on a fresh root — zero duplication of assertions. The in-package
// entry point pinned by the leaf (backend.RunBackendConformance) delegates
// here.
//
// Pinned semantics (ambiguous S3-adjacent behaviors, frozen here — leaf 02's
// fs backend MUST match; never negotiated later):
//
//   - Bucket creation is implicit: the Backend seam has no CreateBucket in
//     v1, so a Put into a well-formed bucket name that does not exist yet
//     materializes it (visible to Buckets thereafter). Get/Stat/Delete/List
//     on a never-materialized bucket return NoSuchBucket.
//   - Delete of a missing key in an existing bucket returns nil (S3 is a
//     204 no-op; the suite pins idempotent-no).
//   - Delete on a missing bucket returns NoSuchBucket (S3: 404).
//   - List ordering is lexicographic by key; CommonPrefixes are deduplicated
//     and merge-sorted with objects into one lexicographic sequence.
//   - ContinuationToken, when set, acts as an opaque marker equivalent to
//     StartAfter; NextToken is empty when IsTruncated is false.
//   - MaxKeys <= 0 means "implementation default" (no truncation for small
//     fixtures); the suite always passes MaxKeys > 0 when testing pagination.
//   - ETags are opaque: the suite asserts non-empty, never a specific form.
//   - Capabilities() is advisory: the suite only requires a zero-value-safe
//     return; capability flag semantics are backend-owned.
package conformance

import (
	"context"
	"errors"
	"io"
	"maps"
	"sort"
	"strings"
	"sync"
	"testing"

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// Run exercises every Backend contract against the instance produced by
// factory. The factory receives a testing.T-scoped fixture and MUST return a
// Backend backed by a fresh, empty root per invocation; the suite calls it
// once per subtest so subtests are fully isolated. A fallible constructor
// (config parse, root open) reports failure itself via t.Fatal/t.Skipf inside
// the factory — the signature matches the leaf's pinned shape exactly.
func Run(t *testing.T, name string, factory func(t *testing.T) backend.Backend) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Run("PutGetRoundTrip", func(t *testing.T) { testPutGetRoundTrip(t, factory) })
		t.Run("PutOverwrite", func(t *testing.T) { testPutOverwrite(t, factory) })
		t.Run("MissingKey", func(t *testing.T) { testMissingKey(t, factory) })
		t.Run("DeleteMissingKeyNoop", func(t *testing.T) { testDeleteMissingKeyNoop(t, factory) })
		t.Run("DeleteMissingBucket", func(t *testing.T) { testDeleteMissingBucket(t, factory) })
		t.Run("MissingBucket", func(t *testing.T) { testMissingBucket(t, factory) })
		t.Run("DeleteRemovesObject", func(t *testing.T) { testDeleteRemovesObject(t, factory) })
		t.Run("DelimiterPaginationTokenCompleteness", func(t *testing.T) { testDelimiterPaginationTokenCompleteness(t, factory) })
		t.Run("ListEmptyBucket", func(t *testing.T) { testListEmptyBucket(t, factory) })
		t.Run("ListPrefixFilter", func(t *testing.T) { testListPrefixFilter(t, factory) })
		t.Run("ListDelimiterGrouping", func(t *testing.T) { testListDelimiterGrouping(t, factory) })
		t.Run("ListPagination", func(t *testing.T) { testListPagination(t, factory) })
		t.Run("ListStartAfter", func(t *testing.T) { testListStartAfter(t, factory) })
		t.Run("ListOrdering", func(t *testing.T) { testListOrdering(t, factory) })
		t.Run("BucketsVisible", func(t *testing.T) { testBucketsVisible(t, factory) })
		t.Run("CapabilitiesZeroSafe", func(t *testing.T) { testCapabilitiesZeroSafe(t, factory) })
		t.Run("ContextCancellation", func(t *testing.T) { testContextCancellation(t, factory) })
		t.Run("ConcurrentSameKey", func(t *testing.T) { testConcurrentSameKey(t, factory) })
	})
}

const (
	bucket        = "conf-bucket"
	missingBucket = "conf-missing-bucket"
)

// --- individual behaviors -------------------------------------------------

func testPutGetRoundTrip(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	content := "hello, conformance"
	opts := objectmodel.PutOptions{
		ContentType: "text/plain",
		Metadata:    map[string]string{"origin": "conformance"},
	}
	put, err := b.Put(ctx, bucket, "hello.txt", strings.NewReader(content), int64(len(content)), opts)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	assertObjectBasics(t, put, "hello.txt", int64(len(content)), opts)

	rc, obj, err := b.Get(ctx, bucket, "hello.txt", objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, readErr := io.ReadAll(rc)
	closeErr := rc.Close()
	if readErr != nil {
		t.Fatalf("read body: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close body: %v", closeErr)
	}
	if string(data) != content {
		t.Fatalf("round-trip content = %q, want %q", data, content)
	}
	assertSameObject(t, obj, put)
}

func testPutOverwrite(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	v1, v2 := "version-one", "version-two-with-longer-content"
	if _, err := b.Put(ctx, bucket, "over.txt", strings.NewReader(v1), int64(len(v1)), objectmodel.PutOptions{}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	put, err := b.Put(ctx, bucket, "over.txt", strings.NewReader(v2), int64(len(v2)), objectmodel.PutOptions{ContentType: "text/x-v2"})
	if err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	rc, obj, err := b.Get(ctx, bucket, "over.txt", objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != v2 {
		t.Fatalf("content after overwrite = %q, want %q", data, v2)
	}
	if obj.Size != int64(len(v2)) {
		t.Fatalf("size after overwrite = %d, want %d", obj.Size, len(v2))
	}
	if obj.ContentType != "text/x-v2" {
		t.Fatalf("content type after overwrite = %q, want text/x-v2", obj.ContentType)
	}
	assertObjectBasics(t, put, "over.txt", int64(len(v2)), objectmodel.PutOptions{ContentType: "text/x-v2"})
}

func testMissingKey(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	materializeBucket(t, b, bucket)

	if _, _, err := b.Get(ctx, bucket, "nope.txt", objectmodel.GetOptions{}); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchKey)
	} else {
		t.Error("Get on missing key: want error, got nil")
	}
	if _, err := b.Stat(ctx, bucket, "nope.txt"); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchKey)
	} else {
		t.Error("Stat on missing key: want error, got nil")
	}
}

// PIN: Delete of a missing key in an existing bucket is an idempotent no-op
// returning nil (S3 answers 204). Backends must NOT return NoSuchKey here.
func testDeleteMissingKeyNoop(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	materializeBucket(t, b, bucket)
	if err := b.Delete(ctx, bucket, "nope.txt"); err != nil {
		t.Fatalf("Delete of missing key = %v, want nil (pinned idempotent no-op)", err)
	}
}

// PIN: Delete on a never-materialized bucket returns NoSuchBucket (S3: 404),
// not nil and not NoSuchKey.
func testDeleteMissingBucket(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	if err := b.Delete(context.Background(), missingBucket, "k.txt"); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchBucket)
		return
	}
	t.Error("Delete on missing bucket: want error, got nil")
}

func testMissingBucket(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	if _, _, err := b.Get(ctx, missingBucket, "k.txt", objectmodel.GetOptions{}); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchBucket)
	} else {
		t.Error("Get on missing bucket: want error, got nil")
	}
	if _, err := b.Stat(ctx, missingBucket, "k.txt"); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchBucket)
	} else {
		t.Error("Stat on missing bucket: want error, got nil")
	}
	if _, err := b.List(ctx, missingBucket, objectmodel.ListParams{}); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchBucket)
	} else {
		t.Error("List on missing bucket: want error, got nil")
	}
}

func testDeleteRemovesObject(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	if _, err := b.Put(ctx, bucket, "gone.txt", strings.NewReader("x"), 1, objectmodel.PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, bucket, "gone.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// BUGHUNT B12(c): the portable form of "the data file is gone" — the
	// seam-visible effect of removal is Stat flipping to NoSuchKey (a raw
	// file-existence check would be backend-specific and non-portable).
	if _, err := b.Stat(ctx, bucket, "gone.txt"); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchKey)
	} else {
		t.Error("Stat after Delete: want NoSuchKey, got nil error")
	}
	// And a Get of the deleted object must also report NoSuchKey (the data
	// is truly gone, not merely unindexed).
	if _, _, err := b.Get(ctx, bucket, "gone.txt", objectmodel.GetOptions{}); err != nil {
		wantCode(t, err, objectmodel.CodeNoSuchKey)
	} else {
		t.Error("Get after Delete: want NoSuchKey, got nil error")
	}
}

// BUGHUNT B12(a): pin the delimiter+pagination token completeness. The
// continuation token on a delimiter listing is the page's LAST EMITTED item
// in merged order (a plain key OR a roll-up prefix); a resume from that
// token must re-derive every un-emitted group and key, so the union of all
// pages equals the full listing. This also pins the plain-key token case
// (a key that sorts strictly before an un-emitted roll-up group must not
// consume that group — bughunt B1 semantics).
func testDelimiterPaginationTokenCompleteness(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	// "a" is a plain key that sorts BEFORE the un-emitted group "a/";
	// the rest exercise multi-page roll-ups.
	putAll(t, b, bucket, map[string]string{
		"a":      "1",
		"a/1":    "2",
		"a/2":    "3",
		"b/1":    "4",
		"b/2":    "5",
		"c.txt":  "6",
		"d/deep": "7",
	})
	const maxKeys = 2
	wantAll := map[string]bool{
		"a": true, "a/1": true, "a/2": true,
		"b/1": true, "b/2": true, "c.txt": true, "d/deep": true,
	}

	// Union of a full delimiter pagination must equal the flat listing.
	// S3 semantics: keys inside a delimiter group surface ONLY as a
	// CommonPrefix — so a wanted key is accounted for when it is listed
	// as an object OR covered by some returned common prefix.
	params := objectmodel.ListParams{Delimiter: "/", MaxKeys: maxKeys}
	union := map[string]bool{}
	prefixes := map[string]bool{}
	pages := 0
	for {
		page, err := b.List(ctx, bucket, params)
		if err != nil {
			t.Fatalf("List page %d: %v", pages+1, err)
		}
		pages++
		for _, o := range page.Objects {
			if !wantAll[o.Key] {
				t.Errorf("page %d: unexpected key %q", pages, o.Key)
			}
			if union[o.Key] {
				t.Errorf("page %d: duplicate key %q across pages", pages, o.Key)
			}
			union[o.Key] = true
		}
		for _, cp := range page.CommonPrefixes {
			if prefixes[cp] {
				t.Errorf("page %d: duplicate common prefix %q across pages", pages, cp)
			}
			prefixes[cp] = true
		}
		if pages > 20 {
			t.Fatal("pagination did not converge")
		}
		if !page.IsTruncated {
			break
		}
		if page.NextToken == "" {
			t.Fatal("IsTruncated with empty NextToken")
		}
		params.ContinuationToken = page.NextToken
	}
	accounted := func(k string) bool {
		if union[k] {
			return true
		}
		for cp := range prefixes {
			if strings.HasPrefix(k, cp) {
				return true
			}
		}
		return false
	}
	for k := range wantAll {
		if !accounted(k) {
			t.Errorf("delimiter pagination missed key %q (a lost roll-up group swallows keys)", k)
		}
	}
	if pages < 3 {
		t.Errorf("pages = %d, want >= 3 (five items at maxKeys=2 must span multiple pages)", pages)
	}
}

// BUGHUNT B12(b): ContextCancellation must cover Stat, Delete, List, and
// Buckets in addition to Get/Put (pinned elsewhere): a cancelled ctx yields
// an error return — never a panic, never a silent success.
func testContextCancellation(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Pinned: a cancelled ctx yields an error return (never a panic); the
	// suite asserts error-ness, not latency or specific error identity.
	if _, _, err := b.Get(ctx, bucket, "k", objectmodel.GetOptions{}); err == nil {
		t.Error("Get with cancelled ctx: want error, got nil")
	}
	if _, err := b.Put(ctx, bucket, "k", strings.NewReader("x"), 1, objectmodel.PutOptions{}); err == nil {
		t.Error("Put with cancelled ctx: want error, got nil")
	}
	if _, err := b.Stat(ctx, bucket, "k"); err == nil {
		t.Error("Stat with cancelled ctx: want error, got nil")
	}
	if err := b.Delete(ctx, bucket, "k"); err == nil {
		t.Error("Delete with cancelled ctx: want error, got nil")
	}
	if _, err := b.List(ctx, bucket, objectmodel.ListParams{}); err == nil {
		t.Error("List with cancelled ctx: want error, got nil")
	}
	if _, err := b.Buckets(ctx); err == nil {
		t.Error("Buckets with cancelled ctx: want error, got nil")
	}
}

func testListEmptyBucket(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	materializeBucket(t, b, bucket)
	page, err := b.List(ctx, bucket, objectmodel.ListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 0 || len(page.CommonPrefixes) != 0 {
		t.Fatalf("empty bucket page = %+v, want no objects/prefixes", page)
	}
	if page.IsTruncated {
		t.Error("empty bucket must not be truncated")
	}
}

func testListPrefixFilter(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	putAll(t, b, bucket, map[string]string{
		"dir/a":   "1",
		"dir/b":   "2",
		"other/c": "3",
	})
	page, err := b.List(ctx, bucket, objectmodel.ListParams{Prefix: "dir/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantKeys := []string{"dir/a", "dir/b"}
	gotKeys := objectKeys(page)
	if !equalStrings(gotKeys, wantKeys) {
		t.Fatalf("keys = %v, want %v", gotKeys, wantKeys)
	}
	if len(page.CommonPrefixes) != 0 {
		t.Fatalf("prefix list must not group, got %v", page.CommonPrefixes)
	}
	if page.IsTruncated {
		t.Error("unexpected truncation")
	}
}

func testListDelimiterGrouping(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	putAll(t, b, bucket, map[string]string{
		"a.txt":  "1",
		"b/1":    "2",
		"b/2":    "3",
		"b2.txt": "4",
	})
	page, err := b.List(ctx, bucket, objectmodel.ListParams{Delimiter: "/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := objectKeys(page); !equalStrings(got, []string{"a.txt", "b2.txt"}) {
		t.Fatalf("keys = %v, want [a.txt b2.txt]", got)
	}
	// Pinned: prefixes merge-sort with objects; "b/" < "b2.txt".
	if got := page.CommonPrefixes; !equalStrings(got, []string{"b/"}) {
		t.Fatalf("common prefixes = %v, want [b/]", got)
	}
	if page.IsTruncated {
		t.Error("unexpected truncation")
	}
}

func testListPagination(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	want := map[string]string{}
	for _, k := range []string{"k01", "k02", "k03", "k04", "k05"} {
		want[k] = "v-" + k
	}
	putAll(t, b, bucket, want)

	params := objectmodel.ListParams{MaxKeys: 2}
	seen := map[string]bool{}
	pages := 0
	for {
		page, err := b.List(ctx, bucket, params)
		if err != nil {
			t.Fatalf("List page %d: %v", pages+1, err)
		}
		pages++
		for _, o := range page.Objects {
			if seen[o.Key] {
				t.Fatalf("duplicate key %s across pages", o.Key)
			}
			seen[o.Key] = true
		}
		if len(page.Objects) != 2 && (page.IsTruncated || pages <= 1) {
			t.Fatalf("page %d = %d objects, want 2 except final short page", pages, len(page.Objects))
		}
		if !page.IsTruncated {
			break
		}
		if page.NextToken == "" {
			t.Fatal("IsTruncated with empty NextToken")
		}
		if pages > 10 {
			t.Fatal("pagination did not converge")
		}
		params.ContinuationToken = page.NextToken // PIN: opaque token == marker
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3 (2+2+1)", pages)
	}
	if !maps.Equal(seen, map[string]bool{"k01": true, "k02": true, "k03": true, "k04": true, "k05": true}) {
		t.Errorf("paginated union = %v, want all five keys", seen)
	}
}

func testListStartAfter(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	putAll(t, b, bucket, map[string]string{"k01": "1", "k02": "2", "k03": "3", "k04": "4"})
	page, err := b.List(ctx, bucket, objectmodel.ListParams{StartAfter: "k02"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := objectKeys(page); !equalStrings(got, []string{"k03", "k04"}) {
		t.Fatalf("keys = %v, want [k03 k04]", got)
	}
}

func testListOrdering(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	putAll(t, b, bucket, map[string]string{"m": "1", "a": "2", "z": "3"})
	page, err := b.List(ctx, bucket, objectmodel.ListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := objectKeys(page); !equalStrings(got, []string{"a", "m", "z"}) {
		t.Fatalf("keys = %v, want lexicographic [a m z]", got)
	}
}

// PIN (restated): buckets materialize implicitly on first Put and are then
// visible to Buckets. Buckets never returns nil alongside a nil error.
func testBucketsVisible(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	ctx := context.Background()
	if _, err := b.Put(ctx, "visible-bucket", "k", strings.NewReader("x"), 1, objectmodel.PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	buckets, err := b.Buckets(ctx)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if buckets == nil {
		t.Fatal("Buckets = nil, want non-nil slice")
	}
	var names []string
	for _, bi := range buckets {
		names = append(names, bi.Name)
	}
	sort.Strings(names)
	if !equalStrings(names, []string{"visible-bucket"}) {
		t.Fatalf("bucket names = %v, want [visible-bucket]", names)
	}
}

func testCapabilitiesZeroSafe(t *testing.T, factory func(*testing.T) backend.Backend) {
	// Advisory only: zero-value-safe return required, no flag assertions
	// (capability semantics are backend-owned).
	_ = factory(t).Capabilities()
}

// Concurrent Put+Get+Delete on the SAME key. Meaningful under -race; runs
// always. The bucket is materialized first: this subtest pins same-key
// contention, not bucket-creation races. Get may legitimately observe
// NoSuchKey while a concurrent Delete wins the race — only unexpected error
// kinds fail.
func testConcurrentSameKey(t *testing.T, factory func(*testing.T) backend.Backend) {
	b := factory(t)
	materializeBucket(t, b, bucket)
	ctx := context.Background()
	const workers, iters = 8, 15
	var wg sync.WaitGroup
	errs := make(chan error, workers*iters)
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range iters {
				switch (w + i) % 3 {
				case 0:
					if _, err := b.Put(ctx, bucket, "hot", strings.NewReader("payload"), 7, objectmodel.PutOptions{}); err != nil {
						errs <- err
					}
				case 1:
					rc, _, err := b.Get(ctx, bucket, "hot", objectmodel.GetOptions{})
					if err != nil {
						if !isCode(err, objectmodel.CodeNoSuchKey) {
							errs <- err
						}
						continue
					}
					if _, err := io.Copy(io.Discard, rc); err != nil {
						errs <- err
					}
					_ = rc.Close()
				case 2:
					if err := b.Delete(ctx, bucket, "hot"); err != nil && !isCode(err, objectmodel.CodeNoSuchKey) {
						errs <- err
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent op failed: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

// materializeBucket creates the bucket via the only seam-available path
// (implicit creation on Put) and empties it again.
func materializeBucket(t *testing.T, b backend.Backend, name string) {
	t.Helper()
	if _, err := b.Put(context.Background(), name, ".materialize", strings.NewReader(""), 0, objectmodel.PutOptions{}); err != nil {
		t.Fatalf("materialize bucket %s: %v", name, err)
	}
	if err := b.Delete(context.Background(), name, ".materialize"); err != nil {
		t.Fatalf("empty materialized bucket %s: %v", name, err)
	}
}

func putAll(t *testing.T, b backend.Backend, bucket string, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		if _, err := b.Put(context.Background(), bucket, k, strings.NewReader(v), int64(len(v)), objectmodel.PutOptions{}); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
}

func objectKeys(p objectmodel.ListPage) []string {
	keys := make([]string, len(p.Objects))
	for i, o := range p.Objects {
		keys[i] = o.Key
	}
	return keys
}

func equalStrings(got, want []string) bool {
	return len(got) == len(want) && slicesEqual(got, want)
}

// sortedKeys renders a key-set for deterministic failure messages.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func slicesEqual(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !isCode(err, code) {
		t.Fatalf("error %v (%T) does not carry code %s", err, err, code)
	}
}

func isCode(err error, code string) bool {
	var omErr *objectmodel.Error
	if !errors.As(err, &omErr) {
		return false
	}
	return omErr.Code == code
}

// assertObjectBasics checks the fields a Put MUST populate from the request.
func assertObjectBasics(t *testing.T, o objectmodel.Object, key string, size int64, opts objectmodel.PutOptions) {
	t.Helper()
	if o.Key != key {
		t.Errorf("Key = %q, want %q", o.Key, key)
	}
	if o.Size != size {
		t.Errorf("Size = %d, want %d", o.Size, size)
	}
	if o.ETag == "" {
		t.Error("ETag empty; backends must always populate the strong validator")
	}
	if o.LastModified.IsZero() {
		t.Error("LastModified zero; backends must stamp server time")
	}
	if opts.ContentType != "" && o.ContentType != opts.ContentType {
		t.Errorf("ContentType = %q, want %q", o.ContentType, opts.ContentType)
	}
	if len(opts.Metadata) > 0 && !maps.Equal(o.Metadata, opts.Metadata) {
		t.Errorf("Metadata = %v, want %v", o.Metadata, opts.Metadata)
	}
}

func assertSameObject(t *testing.T, got, want objectmodel.Object) {
	t.Helper()
	if got.Key != want.Key || got.Size != want.Size || got.ETag != want.ETag ||
		got.ContentType != want.ContentType || !got.LastModified.Equal(want.LastModified) {
		t.Fatalf("Get metadata = %+v, want %+v", got, want)
	}
	if !maps.Equal(got.Metadata, want.Metadata) {
		t.Fatalf("Metadata = %v, want %v", got.Metadata, want.Metadata)
	}
}
