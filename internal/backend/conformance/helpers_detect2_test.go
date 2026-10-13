package conformance

// helpers_detect2_test.go — IN-PROCESS green-run coverage of the suite's
// violation-reporting arms.
//
// detection_test.go and helpers_detect_test.go prove the detection surface
// via child `go test` runs, but the child is a separate binary: its coverage
// never reaches the parent's -coverprofile, so every t.Fatalf/t.Errorf arm
// stayed at count=0. This file drives the SAME detection code in-process.
//
// Mechanic (verified against the testing package): a plain
// `new(testing.T)` is a valid, unregistered T. Passing it to a suite subtest
// or helper executes the real detection code inside THIS test binary (so
// coverage counters increment); a t.Fatalf on the detached T calls
// runtime.Goexit, which unwinds only the probe's worker goroutine, so the
// real run stays green. Each probe then asserts REAL behavior: the detached
// T MUST report Failed() — i.e. the suite caught the planted violation. A
// probe whose violation went undetected fails the run, exactly like the
// child-run harnesses, just without a second process.

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// runViolating runs probe against a detached *testing.T inside a worker
// goroutine (so a Fatalf's runtime.Goexit kills only the probe) and requires
// the suite to have flagged the violation.
func runViolating(t *testing.T, why string, probe func(fake *testing.T)) {
	t.Helper()
	fake := new(testing.T)
	done := make(chan struct{})
	go func() {
		defer close(done)
		probe(fake)
	}()
	<-done
	if !fake.Failed() {
		t.Errorf("suite FAILED TO DETECT violation (%s)", why)
	}
}

// expectOneError drains errs and requires exactly one reported failure —
// the real behavior assertion for the channel-reporting helpers.
func expectOneError(t *testing.T, errs chan error, why string) {
	t.Helper()
	close(errs)
	n := 0
	for err := range errs {
		if err == nil {
			t.Errorf("%s: helper reported a nil error", why)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%s: helper reported %d errors, want exactly 1", why, n)
	}
}

// --- helper-function arms (direct calls) --------------------------------------

func TestWantCodeDetectsBadErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}
	runViolating(t, "wantCode(nil err) must fail", func(fake *testing.T) {
		wantCode(fake, nil, objectmodel.CodeNoSuchKey)
	})
	runViolating(t, "wantCode(wrong code) must fail", func(fake *testing.T) {
		wantCode(fake, errors.New("probe: plain error carries no code"), objectmodel.CodeNoSuchKey)
	})
}

func TestAssertObjectBasicsDetectsCorruptedFields(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}
	// One object violating every check at once: wrong key, wrong size,
	// empty ETag, zero LastModified, wrong ContentType, wrong Metadata —
	// all six Errorf arms must fire and mark the detached T failed.
	runViolating(t, "assertObjectBasics flags every corrupted field", func(fake *testing.T) {
		bad := objectmodel.Object{
			Key:         "corrupted-key",
			Size:        99,
			ContentType: "text/x-other",
			Metadata:    map[string]string{"corrupted": "yes"},
		}
		assertObjectBasics(fake, bad, "hello.txt", 17,
			objectmodel.PutOptions{
				ContentType: "text/plain",
				Metadata:    map[string]string{"origin": "conformance"},
			})
	})
}

func TestAssertSameObjectDetectsDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}
	runViolating(t, "assertSameObject flags field drift", func(fake *testing.T) {
		want := objectmodel.Object{Key: "k", Size: 1, ETag: `"e"`, LastModified: time.Now()}
		got := want
		got.Key = "other"
		assertSameObject(fake, got, want)
	})
	runViolating(t, "assertSameObject flags metadata drift", func(fake *testing.T) {
		want := objectmodel.Object{
			Key: "k", Size: 1, ETag: `"e"`, LastModified: time.Now(),
			Metadata: map[string]string{"origin": "conformance"},
		}
		got := want
		got.Metadata = map[string]string{"origin": "drifted"}
		assertSameObject(fake, got, want)
	})
}

func TestCloseAndReportReportsCloseFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}
	errs := make(chan error, 1)
	closeAndReport(errCloser{ReadCloser: io.NopCloser(strings.NewReader("x"))}, errs)
	expectOneError(t, errs, "closeAndReport must report a Close failure")
}

func TestConcurrentOpReportsFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}
	ctx := context.Background()

	// (0+0)%3==0 → Put; failPut makes the Put fail → must be reported.
	errs := make(chan error, 1)
	concurrentOp(ctx, newFailingWithFailPut(), 0, 0, errs)
	expectOneError(t, errs, "concurrentOp must report a failed Put")

	// (0+1)%3==1 → Get; a plain (non-NoSuchKey) error must be reported.
	errs = make(chan error, 1)
	concurrentOp(ctx, newGetFailStub(), 0, 1, errs)
	expectOneError(t, errs, "concurrentOp must report an unexpected Get error")

	// (0+1)%3==1 → Get; a reader whose Read fails must be reported. The
	// key is pre-materialized so Get itself succeeds and the failure
	// surfaces at read time.
	rf := newReadFailStub()
	if _, perr := rf.Put(ctx, bucket, "hot", strings.NewReader("x"), 1, objectmodel.PutOptions{}); perr != nil {
		t.Fatalf("pre-materialize Put: %v", perr)
	}
	errs = make(chan error, 1)
	concurrentOp(ctx, rf, 0, 1, errs)
	expectOneError(t, errs, "concurrentOp must report a failing body read")

	// (0+1)%3==1 → Get; a reader whose Close fails must be reported.
	bc := newBadCloseStub()
	if _, perr := bc.Put(ctx, bucket, "hot", strings.NewReader("x"), 1, objectmodel.PutOptions{}); perr != nil {
		t.Fatalf("pre-materialize Put: %v", perr)
	}
	errs = make(chan error, 1)
	concurrentOp(ctx, bc, 0, 1, errs)
	expectOneError(t, errs, "concurrentOp must report a failing body close")

	// (0+2)%3==2 → Delete; a plain (non-NoSuchKey) error must be reported.
	errs = make(chan error, 1)
	concurrentOp(ctx, newDeleteFailStub(), 0, 2, errs)
	expectOneError(t, errs, "concurrentOp must report an unexpected Delete error")
}

func newFailingWithFailPut() backend.Backend {
	b := newFailing()
	b.failPut = true
	return b
}

// --- helper-function arms (detached T) ----------------------------------------

func TestMaterializeBucketDetectsFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}
	runViolating(t, "materializeBucket reports a failed Put", func(fake *testing.T) {
		materializeBucket(fake, newFailingWithFailPut(), bucket)
	})
	runViolating(t, "materializeBucket reports a failed Delete", func(fake *testing.T) {
		materializeBucket(fake, newDeleteFailStub(), bucket)
	})
}

func TestPutAllDetectsPutFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}
	runViolating(t, "putAll reports a failed Put", func(fake *testing.T) {
		putAll(fake, newFailingWithFailPut(), bucket, map[string]string{"k": "v"})
	})
}

// --- subtest arms: happy path corrupted at exactly one point -------------------

func TestSubtestsDetectViolationsInProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes; skipped in -short")
	}

	// Each entry drives one suite subtest with a backend violating one
	// clause; runViolating requires the subtest to catch it.
	probes := []struct {
		name    string
		why     string
		factory func() backend.Backend
		run     func(t *testing.T, factory func(*testing.T) backend.Backend)
	}{
		// PutGetRoundTrip: Put failure, Get failure, read failure, close
		// failure, corrupted content.
		{"PutGetRoundTrip-PutFail", "Put error must fail the round trip",
			newFailingWithFailPut, testPutGetRoundTrip},
		{"PutGetRoundTrip-GetFail", "Get error must fail the round trip",
			newGetFailStub, testPutGetRoundTrip},
		{"PutGetRoundTrip-ReadFail", "a failing body read must fail the round trip",
			newReadFailStub, testPutGetRoundTrip},
		{"PutGetRoundTrip-CloseFail", "a failing body close must fail the round trip",
			newBadCloseStub, testPutGetRoundTrip},
		{"PutGetRoundTrip-CorruptContent", "corrupted readback must fail the round trip",
			newCorruptRead, testPutGetRoundTrip},

		// PutOverwrite.
		{"PutOverwrite-PutV1Fail", "first Put failure must fail the overwrite test",
			newFirstPutFailStub, testPutOverwrite},
		{"PutOverwrite-PutV2Fail", "second Put failure must fail the overwrite test",
			newSecondPutFailStub, testPutOverwrite},
		{"PutOverwrite-GetFail", "Get failure after overwrite must fail the test",
			newGetFailStub, testPutOverwrite},
		{"PutOverwrite-CloseFail", "failing close after overwrite must fail the test",
			newBadCloseStub, testPutOverwrite},
		{"PutOverwrite-CorruptContent", "corrupted overwrite readback must fail the test",
			newCorruptRead, testPutOverwrite},
		{"PutOverwrite-WrongSize", "wrong size after overwrite must fail the test",
			newWrongSizeGetStub, testPutOverwrite},
		{"PutOverwrite-WrongContentType", "dropped content type after overwrite must fail the test",
			newWrongContentTypeGetStub, testPutOverwrite},

		// Missing key/bucket error contracts.
		{"MissingKey-PhantomKey", "Get/Stat succeeding on a missing key must be caught",
			newPhantomKey, testMissingKey},
		{"DeleteMissingKeyNoop-StrictDelete", "NoSuchKey on idempotent delete must be caught",
			newStrictDeleteExMaterialize, testDeleteMissingKeyNoop},
		{"DeleteMissingBucket-LenientDelete", "nil delete on a missing bucket must be caught",
			newLenientDelete, testDeleteMissingBucket},
		{"MissingBucket-PhantomBucket", "success on a never-materialized bucket must be caught",
			newPhantomAllStub, testMissingBucket},

		// DeleteRemovesObject.
		{"DeleteRemovesObject-PutFail", "Put failure must fail the delete test",
			newFailingWithFailPut, testDeleteRemovesObject},
		{"DeleteRemovesObject-DeleteFail", "Delete failure must fail the delete test",
			newDeleteFailStub, testDeleteRemovesObject},
		{"DeleteRemovesObject-GhostDelete", "an object surviving Delete must be caught",
			newGhostDelete, testDeleteRemovesObject},

		// Delimiter pagination completeness.
		{"DelimPagination-ListFail", "List failure must fail the pagination test",
			newListFailStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-ExtraKey", "an unexpected key must be caught",
			newExtraKeyStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-DupPage", "a repeated page (ignored token) must be caught",
			newIgnoreContinuationStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-NeverConverges", "a non-converging pagination must be caught",
			newEndlessStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-WrongPageSize", "a short mid-run page must be caught",
			newDelimShortPageStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-DupInPage", "a key repeated within/across pages must be caught",
			newDelimDupKeyStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-EmptyToken", "truncation with an empty token must be caught",
			newTruncatedNoTokenStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-SwallowedKey", "a lost key must be caught",
			newSwallowKeyStub, testDelimiterPaginationTokenCompleteness},
		{"DelimPagination-SinglePage", "MaxKeys ignored (one page) must be caught",
			newIgnoreMaxKeysStub, testDelimiterPaginationTokenCompleteness},

		// ContextCancellation: every op silently succeeding under a
		// cancelled ctx must be caught.
		{"ContextCancellation-IgnoreContext", "cancelled ctx silently succeeding must be caught",
			newIgnoreContextAllStub, testContextCancellation},

		// List family.
		{"ListEmptyBucket-ListFail", "List failure on an empty bucket must fail the test",
			newListFailStub, testListEmptyBucket},
		{"ListEmptyBucket-NonEmpty", "objects in an empty bucket listing must be caught",
			newNonEmptyListStub, testListEmptyBucket},
		{"ListEmptyBucket-AlwaysTruncated", "false truncation must be caught",
			newAlwaysTruncated, testListEmptyBucket},
		{"ListPrefixFilter-ListFail", "List failure must fail the prefix test",
			newListFailStub, testListPrefixFilter},
		{"ListPrefixFilter-Ignored", "an ignored Prefix must be caught",
			newNoPrefixFilter, testListPrefixFilter},
		{"ListPrefixFilter-Grouped", "grouping in a prefix listing must be caught",
			newCPInPrefixListStub, testListPrefixFilter},
		{"ListPrefixFilter-AlwaysTruncated", "false truncation must be caught",
			newAlwaysTruncated, testListPrefixFilter},
		{"ListDelimiterGrouping-ListFail", "List failure must fail the delimiter test",
			newListFailStub, testListDelimiterGrouping},
		{"ListDelimiterGrouping-Ignored", "an ignored Delimiter must be caught",
			newNoDelimiter, testListDelimiterGrouping},
		{"ListDelimiterGrouping-WrongPrefixes", "wrong common prefixes must be caught",
			newWrongPrefixesStub, testListDelimiterGrouping},
		{"ListDelimiterGrouping-AlwaysTruncated", "false truncation must be caught",
			newAlwaysTruncated, testListDelimiterGrouping},
		{"ListPagination-ListFail", "List failure must fail the pagination test",
			newListFailStub, testListPagination},
		{"ListPagination-DupPage", "a repeated page must be caught",
			newIgnoreContinuationStub, testListPagination},
		{"ListPagination-NeverConverges", "a non-converging pagination must be caught",
			newEndlessStub, testListPagination},
		{"ListPagination-TooManyPages", "a pagination that never finishes must hit the page-count guard",
			newFlatConvergeSlowStub, testListPagination},
		{"ListPagination-PageCount", "wrong page count must be caught",
			newPagesMiscountStub, testListPagination},
		{"ListPagination-WrongPageSize", "a short mid-run page must be caught",
			newFlatShortPageStub, testListPagination},
		{"ListPagination-DupInPage", "a key repeated across pages must be caught",
			newFlatDupKeyStub, testListPagination},
		{"ListPagination-EmptyToken", "truncation with an empty token must be caught",
			newTruncatedNoTokenStub, testListPagination},
		{"ListPagination-SwallowedKey", "a lost key must be caught",
			newUnionMissStub, testListPagination},
		{"ListStartAfter-ListFail", "List failure must fail the StartAfter test",
			newListFailStub, testListStartAfter},
		{"ListStartAfter-Ignored", "an ignored StartAfter must be caught",
			newNoStartAfter, testListStartAfter},
		{"ListOrdering-ListFail", "List failure must fail the ordering test",
			newListFailStub, testListOrdering},
		{"ListOrdering-Unsorted", "unsorted listing must be caught",
			newUnsortedList, testListOrdering},

		// BucketsVisible.
		{"BucketsVisible-PutFail", "Put failure must fail the bucket-visibility test",
			newFailingWithFailPut, testBucketsVisible},
		{"BucketsVisible-BucketsFail", "Buckets failure must fail the test",
			newBucketsFailStub, testBucketsVisible},
		{"BucketsVisible-NilSlice", "a nil Buckets slice must be caught",
			newBucketsNilStub, testBucketsVisible},
		{"BucketsVisible-NoMaterialize", "an unmaterialized bucket must be caught",
			newNoMaterialize, testBucketsVisible},

		// ConcurrentSameKey: a reported concurrent failure must fail the
		// test via the errs loop. The stub must let the setup Put succeed
		// (materializeBucket) and fail only the concurrent Puts.
		{"ConcurrentSameKey-PutFail", "a reported concurrent Put failure must fail the test",
			newSecondPutFailStub, testConcurrentSameKey},
	}

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			runViolating(t, p.why, func(fake *testing.T) {
				p.run(fake, func(*testing.T) backend.Backend { return p.factory() })
			})
		})
	}
}

// --- violating probe backends (new; reuse existing stubs where possible) -------

// firstPutFailStub: only the FIRST Put fails — the overwrite test's v1 leg
// (the failPut flag hits both legs, so it cannot isolate this arm).
type firstPutFailStub struct {
	runnerStub
	calls atomic.Int32
}

func newFirstPutFailStub() backend.Backend { f := &firstPutFailStub{}; f.ensureMaps(); return f }

func (m *firstPutFailStub) Put(ctx context.Context, bkt, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	m.ensureMaps()
	if m.calls.Add(1) == 1 {
		return objectmodel.Object{}, errors.New("probe: first put failed")
	}
	return m.runnerStub.Put(ctx, bkt, key, data, size, opts)
}

// getFailStub: Get always fails with a plain (non-contract) error after a
// successful Put.
type getFailStub struct{ runnerStub }

func newGetFailStub() backend.Backend { g := &getFailStub{}; g.ensureMaps(); return g }

func (m *getFailStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	return nil, objectmodel.Object{}, errors.New("probe: get failed")
}

// secondPutFailStub: the second Put fails (first succeeds) — the overwrite
// test's v2 leg. The counter is atomic: the ConcurrentSameKey probe shares
// this stub from concurrent goroutines.
type secondPutFailStub struct {
	runnerStub
	calls atomic.Int32
}

func newSecondPutFailStub() backend.Backend { s := &secondPutFailStub{}; s.ensureMaps(); return s }

func (m *secondPutFailStub) Put(ctx context.Context, bkt, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	m.ensureMaps()
	if m.calls.Add(1) >= 2 {
		return objectmodel.Object{}, errors.New("probe: second put failed")
	}
	return m.runnerStub.Put(ctx, bkt, key, data, size, opts)
}

// errReader fails every Read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("probe: read failed") }

// readFailStub: Get returns a reader whose Read fails (Close is clean, so
// the read error is what must be reported).
type readFailStub struct{ runnerStub }

func newReadFailStub() backend.Backend { r := &readFailStub{}; r.ensureMaps(); return r }

func (m *readFailStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	if _, _, err := m.runnerStub.Get(ctx, bkt, key, opts); err != nil {
		return nil, objectmodel.Object{}, err
	}
	return io.NopCloser(errReader{}), objectmodel.Object{Key: key}, nil
}

// badCloseStub: runnerStub-backed Get returning a reader whose Close fails
// (badCloseBackend embeds failingBackend; this one pairs with subtests whose
// Puts must succeed via the richer runnerStub behaviors).
type badCloseStub struct{ runnerStub }

func newBadCloseStub() backend.Backend { b := &badCloseStub{}; b.ensureMaps(); return b }

func (m *badCloseStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	rc, obj, err := m.runnerStub.Get(ctx, bkt, key, opts)
	if err != nil {
		return rc, obj, err
	}
	return errCloser{ReadCloser: rc}, obj, nil
}

// wrongSizeGetStub / wrongContentTypeGetStub: corrupt one metadata field of
// every GET so the overwrite test's post-Get field checks fire (a Put-side
// corruption is masked: the same corrupted meta is both the Put result and
// the stored metadata).
type wrongSizeGetStub struct{ runnerStub }

func newWrongSizeGetStub() backend.Backend { w := &wrongSizeGetStub{}; w.ensureMaps(); return w }

func (m *wrongSizeGetStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	rc, obj, err := m.runnerStub.Get(ctx, bkt, key, opts)
	if err != nil {
		return rc, obj, err
	}
	obj.Size = obj.Size + 1
	return rc, obj, nil
}

type wrongContentTypeGetStub struct{ runnerStub }

func newWrongContentTypeGetStub() backend.Backend {
	w := &wrongContentTypeGetStub{}
	w.ensureMaps()
	return w
}

func (m *wrongContentTypeGetStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	rc, obj, err := m.runnerStub.Get(ctx, bkt, key, opts)
	if err != nil {
		return rc, obj, err
	}
	if obj.ContentType != "" {
		obj.ContentType = "text/x-corrupted"
	}
	return rc, obj, nil
}

// strictDeleteExMaterializeStub: strictDeleteStub but the materialize
// probe-put (key ".materialize") is exempted — the idempotent-noop Delete
// it issues during setup must succeed or testDeleteMissingKeyNoop never
// reaches the violation.
type strictDeleteExMaterializeStub struct{ strictDeleteStub }

func newStrictDeleteExMaterialize() backend.Backend {
	s := &strictDeleteExMaterializeStub{}
	s.ensureMaps()
	return s
}

func (m *strictDeleteExMaterializeStub) Delete(ctx context.Context, bkt, key string) error {
	m.ensureMaps()
	if key == ".materialize" {
		return m.runnerStub.Delete(ctx, bkt, key)
	}
	return m.strictDeleteStub.Delete(ctx, bkt, key)
}

// phantomAllStub: Get, Stat, AND List all succeed against a
// never-materialized bucket.
type phantomAllStub struct{ runnerStub }

func newPhantomAllStub() backend.Backend { p := &phantomAllStub{}; p.ensureMaps(); return p }

func (m *phantomAllStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	rc, obj, err := m.runnerStub.Get(ctx, bkt, key, opts)
	if err == nil || !isCode(err, objectmodel.CodeNoSuchBucket) {
		return rc, obj, err
	}
	return io.NopCloser(strings.NewReader("")), objectmodel.Object{Key: key}, nil
}

func (m *phantomAllStub) Stat(ctx context.Context, bkt, key string) (objectmodel.Object, error) {
	m.ensureMaps()
	meta, err := m.runnerStub.Stat(ctx, bkt, key)
	if err == nil || !isCode(err, objectmodel.CodeNoSuchBucket) {
		return meta, err
	}
	return objectmodel.Object{Key: key}, nil
}

func (m *phantomAllStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err == nil || !isCode(err, objectmodel.CodeNoSuchBucket) {
		return page, err
	}
	return objectmodel.ListPage{Objects: []objectmodel.Object{{Key: "phantom"}}}, nil
}

// deleteFailStub: Delete always fails with a plain error (Put/Get/Stat keep
// runnerStub behavior, so the object is still there afterwards).
type deleteFailStub struct{ runnerStub }

func newDeleteFailStub() backend.Backend { d := &deleteFailStub{}; d.ensureMaps(); return d }

func (m *deleteFailStub) Delete(ctx context.Context, bkt, key string) error {
	m.ensureMaps()
	return errors.New("probe: delete failed")
}

// listFailStub: List always fails with a plain error.
type listFailStub struct{ runnerStub }

func newListFailStub() backend.Backend { l := &listFailStub{}; l.ensureMaps(); return l }

func (m *listFailStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	return objectmodel.ListPage{}, errors.New("probe: list failed")
}

// extraKeyStub: every page carries one unexpected object.
type extraKeyStub struct{ runnerStub }

func newExtraKeyStub() backend.Backend { e := &extraKeyStub{}; e.ensureMaps(); return e }

func (m *extraKeyStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	page.Objects = append(page.Objects, objectmodel.Object{Key: "zz-unexpected"})
	return page, nil
}

// ignoreContinuationStub: every page is page one (the token is dropped), so
// keys and common prefixes repeat across pages.
type ignoreContinuationStub struct{ runnerStub }

func newIgnoreContinuationStub() backend.Backend {
	i := &ignoreContinuationStub{}
	i.ensureMaps()
	return i
}

func (m *ignoreContinuationStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	p.ContinuationToken = ""
	return m.runnerStub.List(ctx, bkt, p)
}

// truncatedNoTokenStub: every non-empty page claims truncation but hands
// back an empty continuation token.
type truncatedNoTokenStub struct{ runnerStub }

func newTruncatedNoTokenStub() backend.Backend {
	t := &truncatedNoTokenStub{}
	t.ensureMaps()
	return t
}

func (m *truncatedNoTokenStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	if len(page.Objects)+len(page.CommonPrefixes) > 0 {
		page.IsTruncated = true
		page.NextToken = ""
	}
	return page, nil
}

// swallowKeyStub: the first object ever listed never surfaces in any
// listing (later pages unaffected).
type swallowKeyStub struct {
	runnerStub
	dropped bool
}

func newSwallowKeyStub() backend.Backend {
	s := &swallowKeyStub{}
	s.ensureMaps()
	return s
}

func (m *swallowKeyStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	if !m.dropped && len(page.Objects) > 0 {
		m.dropped = true
		page.Objects = page.Objects[1:]
	}
	return page, nil
}

// endlessStub: every page is the SAME first page with a fresh token, so
// pagination never converges — both pagination tests' non-convergence
// fatal guards must fire.
type endlessStub struct{ runnerStub }

func newEndlessStub() backend.Backend { e := &endlessStub{}; e.ensureMaps(); return e }

func (m *endlessStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	p.ContinuationToken = ""
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	if len(page.Objects)+len(page.CommonPrefixes) > 0 {
		page.IsTruncated = true
		page.NextToken = "probe-endless"
	}
	return page, nil
}

// flatShortPageStub: the SECOND page holds one object instead of two, so
// the flat pagination test's "want 2 objects except final short page"
// check fires (the first and final pages stay correctly shaped, so the
// earlier guards are not what catches it).
//
// pagesMiscountStub: pagination converges (no in-loop guard fires) but in
// TWO pages instead of three — pages != 3 is what must catch it.
type pagesMiscountStub struct{ runnerStub }

func newPagesMiscountStub() backend.Backend { p := &pagesMiscountStub{}; p.ensureMaps(); return p }

func (m *pagesMiscountStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	// Snapshot so the stub never mutates the stored metas backing later
	// pages.
	objs := make([]objectmodel.Object, len(page.Objects))
	copy(objs, page.Objects)
	page.Objects = objs
	// Page TWO: report its first key only (page two is a full page here,
	// so the in-loop shape guard does NOT fire; the run ends 2+1+2 — three
	// pages — and pages != 3 is not what catches it... but re-counting:
	// page two short means the run reshapes to 2+1+2). To hit the page
	// COUNT check instead, the run must converge in two pages: on page
	// one, report both keys; on page two (marker k02), report the three
	// remaining keys with IsTruncated=false. The in-loop guards only
	// reject full-shaped pages, so only pages != 3 can catch this.
	if p.ContinuationToken == "k02" {
		page.IsTruncated = false
		page.NextToken = ""
		// nothing to trim: the stub returns the remaining three keys,
		// violating the pinned "MaxKeys=2 pages" shape without tripping
		// any in-loop guard.
	}
	return page, nil
}

// unionMissStub: pagination converges with correctly shaped pages, but the
// LAST key of the run is dropped from the final page only — every in-loop
// guard passes and only the post-loop union check can catch it.
type unionMissStub struct{ runnerStub }

func newUnionMissStub() backend.Backend { u := &unionMissStub{}; u.ensureMaps(); return u }

func (m *unionMissStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	if !page.IsTruncated && len(page.Objects) == 1 {
		// Final page (k05 alone): drop it — seen ends missing k05.
		// Snapshot: never mutate the slice the stub map still holds.
		page.Objects = nil
	}
	return page, nil
}

// delimShortPageStub: a mid-run delimiter page carries ONE item instead of
// maxKeys=2 — the delimiter test's in-loop page-shape guard must fire
// (pages still < 3 at that point, so the pages guard is not what catches
// it).
type delimShortPageStub struct{ runnerStub }

func newDelimShortPageStub() backend.Backend { d := &delimShortPageStub{}; d.ensureMaps(); return d }

func (m *delimShortPageStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	// Snapshot: never mutate the slices the stub map still holds.
	objs := make([]objectmodel.Object, len(page.Objects))
	copy(objs, page.Objects)
	cps := make([]string, len(page.CommonPrefixes))
	copy(cps, page.CommonPrefixes)
	page.Objects, page.CommonPrefixes = objs, cps
	items := len(page.Objects) + len(page.CommonPrefixes)
	if p.ContinuationToken != "" && page.IsTruncated && items == 2 {
		if len(page.Objects) > 0 {
			page.Objects = page.Objects[1:]
		} else {
			page.CommonPrefixes = page.CommonPrefixes[1:]
		}
	}
	return page, nil
}

// delimDupKeyStub: the FIRST page's items are duplicated at the START of
// page two (token "a" re-emitted) — the in-loop cross-page duplicate guards
// must fire before any shape or convergence check.
type delimDupKeyStub struct{ runnerStub }

func newDelimDupKeyStub() backend.Backend { d := &delimDupKeyStub{}; d.ensureMaps(); return d }

func (m *delimDupKeyStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	// Snapshot: never mutate the slices the stub map still holds.
	objs := make([]objectmodel.Object, len(page.Objects))
	copy(objs, page.Objects)
	cps := make([]string, len(page.CommonPrefixes))
	copy(cps, page.CommonPrefixes)
	page.Objects, page.CommonPrefixes = objs, cps
	if p.ContinuationToken == "a/" {
		page.Objects = append([]objectmodel.Object{{Key: "a", Size: 1}}, objs...)
	}
	return page, nil
}

// wrongPrefixesStub: the delimiter grouping test's key/object assertions
// pass but the CommonPrefixes are wrong (duplicated prefix) — only the
// prefixes equality check can catch it.
type wrongPrefixesStub struct{ runnerStub }

func newWrongPrefixesStub() backend.Backend { w := &wrongPrefixesStub{}; w.ensureMaps(); return w }

func (m *wrongPrefixesStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	// Snapshot: never mutate the slice the stub map still holds.
	cps := make([]string, len(page.CommonPrefixes))
	copy(cps, page.CommonPrefixes)
	if len(cps) == 1 && cps[0] == "b/" {
		// Wrong roll-up: the a/ group is missing and b/ appears twice —
		// unequal to the pinned ["b/"] in both content and length.
		cps = []string{"b/", "b/"}
		page.CommonPrefixes = cps
	}
	return page, nil
}

// flatShortPageStub: the SECOND page of the flat pagination carries one
// object instead of two while still truncating — the in-loop page-shape
// guard (want 2 except final short page) must fire. It fires while pages
// == 2, so the pages guard is not what catches it.
type flatShortPageStub struct{ runnerStub }

func newFlatShortPageStub() backend.Backend { f := &flatShortPageStub{}; f.ensureMaps(); return f }

func (m *flatShortPageStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	// Snapshot: never mutate the slice the stub map still holds.
	objs := make([]objectmodel.Object, len(page.Objects))
	copy(objs, page.Objects)
	page.Objects = objs
	if p.ContinuationToken == "k02" && len(page.Objects) == 2 {
		page.Objects = objs[:1]
	}
	return page, nil
}

// flatDupKeyStub: page two re-emits "k02" (the token key) before its own
// items — the in-loop cross-page duplicate guard must fire before the
// shape check.
type flatDupKeyStub struct{ runnerStub }

func newFlatDupKeyStub() backend.Backend { f := &flatDupKeyStub{}; f.ensureMaps(); return f }

func (m *flatDupKeyStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	// Snapshot: never mutate the slice the stub map still holds.
	objs := make([]objectmodel.Object, len(page.Objects))
	copy(objs, page.Objects)
	page.Objects = objs
	if p.ContinuationToken == "k02" {
		page.Objects = append([]objectmodel.Object{{Key: "k02", Size: 2}}, objs...)
	}
	return page, nil
}

// flatConvergeSlowStub: every page mints TWO FRESH keys (zz-<n>) and keeps
// truncating, so the flat pagination never ends — the >10-pages
// non-convergence guard must fire. The flat test has no unexpected-key
// check, so minted keys reach no earlier guard.
type flatConvergeSlowStub struct {
	runnerStub
	n atomic.Int32
}

func newFlatConvergeSlowStub() backend.Backend {
	f := &flatConvergeSlowStub{}
	f.ensureMaps()
	return f
}

func (m *flatConvergeSlowStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	if _, err := m.runnerStub.List(ctx, bkt, p); err != nil {
		return objectmodel.ListPage{}, err
	}
	a := m.n.Add(1)
	b := m.n.Add(1)
	return objectmodel.ListPage{
		Objects: []objectmodel.Object{
			{Key: "zz-" + strconv.FormatInt(int64(a), 10), Size: 1},
			{Key: "zz-" + strconv.FormatInt(int64(b), 10), Size: 1},
		},
		IsTruncated: true,
		NextToken:   "zz-" + strconv.FormatInt(int64(b), 10),
	}, nil
}

// ignoreMaxKeysStub: MaxKeys is reset to the implementation default, so
// every listing fits on one page.
type ignoreMaxKeysStub struct{ runnerStub }

func newIgnoreMaxKeysStub() backend.Backend {
	i := &ignoreMaxKeysStub{}
	i.ensureMaps()
	return i
}

func (m *ignoreMaxKeysStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	p.MaxKeys = 0
	return m.runnerStub.List(ctx, bkt, p)
}

// ignoreContextAllStub: every operation ignores a cancelled context. The
// bucket is pre-materialized so the ops SUCCEED (a silent success under a
// cancelled ctx is the violation; a NoSuchBucket error would mask it).
type ignoreContextAllStub struct{ runnerStub }

func newIgnoreContextAllStub() backend.Backend {
	i := &ignoreContextAllStub{}
	i.ensureMaps()
	i.objects[bucket] = map[string]memObj{
		"k": {data: []byte("x"), meta: objectmodel.Object{Key: "k", Size: 1}},
	}
	return i
}

func (m *ignoreContextAllStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	return m.runnerStub.Get(context.Background(), bkt, key, opts)
}

func (m *ignoreContextAllStub) Put(ctx context.Context, bkt, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	m.ensureMaps()
	return m.runnerStub.Put(context.Background(), bkt, key, data, size, opts)
}

func (m *ignoreContextAllStub) Stat(ctx context.Context, bkt, key string) (objectmodel.Object, error) {
	m.ensureMaps()
	return m.runnerStub.Stat(context.Background(), bkt, key)
}

func (m *ignoreContextAllStub) Delete(ctx context.Context, bkt, key string) error {
	m.ensureMaps()
	return m.runnerStub.Delete(context.Background(), bkt, key)
}

func (m *ignoreContextAllStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	return m.runnerStub.List(context.Background(), bkt, p)
}

func (m *ignoreContextAllStub) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	m.ensureMaps()
	return m.runnerStub.Buckets(context.Background())
}

// nonEmptyListStub: an "empty" bucket listing returns one object.
type nonEmptyListStub struct{ runnerStub }

func newNonEmptyListStub() backend.Backend { n := &nonEmptyListStub{}; n.ensureMaps(); return n }

func (m *nonEmptyListStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	if _, err := m.runnerStub.List(ctx, bkt, p); err != nil {
		return objectmodel.ListPage{}, err
	}
	return objectmodel.ListPage{Objects: []objectmodel.Object{{Key: "ghost"}}}, nil
}

// cpInPrefixListStub: a prefix-filtered listing illegally carries a common
// prefix.
type cpInPrefixListStub struct{ runnerStub }

func newCPInPrefixListStub() backend.Backend {
	c := &cpInPrefixListStub{}
	c.ensureMaps()
	return c
}

func (m *cpInPrefixListStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	if len(page.CommonPrefixes) == 0 {
		page.CommonPrefixes = []string{"dir/"}
	}
	return page, nil
}

// bucketsFailStub / bucketsNilStub: Buckets fails, or returns nil with nil.
type bucketsFailStub struct{ runnerStub }

func newBucketsFailStub() backend.Backend { b := &bucketsFailStub{}; b.ensureMaps(); return b }

func (m *bucketsFailStub) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	m.ensureMaps()
	return nil, errors.New("probe: buckets failed")
}

type bucketsNilStub struct{ runnerStub }

func newBucketsNilStub() backend.Backend { b := &bucketsNilStub{}; b.ensureMaps(); return b }

func (m *bucketsNilStub) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	m.ensureMaps()
	return nil, nil
}
