package conformance

// The conformance suite's t.Fatalf/t.Error branches are its DETECTION
// surface: they execute only when a Backend violates the pinned contract, so
// a green run of a conforming backend can never reach them (documented as
// unreachable-in-green-runs per the coverage plan's rules).
//
// This file proves the detection surface works anyway. Violating probe
// backends are run in a CHILD `go test` process whose failure is EXPECTED;
// the child records which violations the suite caught and deliberately exits
// non-zero. The parent asserts every violation was detected. This is the
// only way to exercise expected-failure paths while keeping the parent test
// binary (and the repo's green gate) green.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// violationProbe couples a subtest name from Run's dispatch table with a
// factory building a backend that violates exactly that clause.
type violationProbe struct {
	name    string
	why     string
	factory func() backend.Backend
}

// probes: one entry per contract clause. Each factory embeds runnerStub and
// twists exactly one behavior; the suite must catch it (inner subtest fails).
func violationProbes() []violationProbe {
	return []violationProbe{
		{"PutGetRoundTrip", "Put error must fail the round trip", func() backend.Backend {
			b := newFailing()
			b.failPut = true
			return b
		}},
		{"PutOverwrite", "corrupted readback after overwrite", newCorruptRead},
		{"MissingKey", "Get/Stat succeed on a missing key", newPhantomKey},
		{"DeleteMissingKeyNoop", "Delete of missing key returns NoSuchKey (PIN violation)", newStrictDelete},
		{"DeleteMissingBucket", "Delete on missing bucket returns nil (PIN violation)", newLenientDelete},
		{"MissingBucket", "Stat succeeds on a never-materialized bucket", newPhantomBucket},
		{"DeleteRemovesObject", "Delete leaves the object readable", newGhostDelete},
		{"ListEmptyBucket", "empty bucket listing claims truncation", newAlwaysTruncated},
		{"ListPrefixFilter", "Prefix filter ignored", newNoPrefixFilter},
		{"ListDelimiterGrouping", "Delimiter ignored", newNoDelimiter},
		{"ListStartAfter", "StartAfter ignored", newNoStartAfter},
		{"ListOrdering", "keys returned unsorted", newUnsortedList},
		{"BucketsVisible", "Put does not materialize the bucket", newNoMaterialize},
		{"ContextCancellation", "cancelled ctx silently succeeds", newIgnoreContext},
	}
}

// TestSuiteDetectsContractViolations spawns a child `go test` running the
// violating probes; the child fails by design. The parent requires that the
// child FAILED and that every violation was caught by the suite.
func TestSuiteDetectsContractViolations(t *testing.T) {
	if testing.Short() {
		t.Skip("violation probes spawn a child test binary; skipped in -short")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(thisFile)

	results := filepath.Join(t.TempDir(), "detection-results.txt")
	cmd := exec.Command("go", "test", "-run", "TestViolationProbesChild", "-count=1", "./")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "MINIS3_VIOLATION_PROBES=1", "MINIS3_DETECTION_RESULTS="+results)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child violation run unexpectedly PASSED:\n%s", output)
	}
	got, readErr := os.ReadFile(results)
	if readErr != nil {
		t.Fatalf("child did not write detection results (%v); output:\n%s", readErr, output)
	}
	detected := map[string]bool{}
	for line := range strings.SplitSeq(string(got), "\n") {
		if name, ok := strings.CutPrefix(line, "DETECTED "); ok {
			detected[name] = true
		}
	}
	for _, p := range violationProbes() {
		if !detected[p.name] {
			t.Errorf("suite FAILED TO DETECT %q (%s)", p.name, p.why)
		}
	}
}

// TestViolationProbesChild runs every violating probe against the suite and
// records which ones the suite caught. It ALWAYS fails at the end (the
// probes violate the contract by design); it exists only to be spawned by
// TestSuiteDetectsContractViolations and skips in every other run.
func TestViolationProbesChild(t *testing.T) {
	resultsPath := os.Getenv("MINIS3_DETECTION_RESULTS")
	if os.Getenv("MINIS3_VIOLATION_PROBES") != "1" || resultsPath == "" {
		t.Skip("violation probes run only under TestSuiteDetectsContractViolations")
	}
	var sb strings.Builder
	for _, p := range violationProbes() {
		caught := !t.Run(p.name, func(t *testing.T) {
			factoryForName(t, p.name, func(*testing.T) backend.Backend { return p.factory() })
		})
		if caught {
			sb.WriteString("DETECTED " + p.name + "\n")
		}
	}
	if writeErr := os.WriteFile(resultsPath, []byte(sb.String()), 0o644); writeErr != nil {
		t.Fatalf("write detection results: %v", writeErr)
	}
	// Expected failure: the probes violate the contract on purpose. Mark the
	// child failed so the parent's "child must fail" assertion holds.
	t.FailNow()
}

// factoryForName dispatches to the matching suite subtest body so each probe
// targets exactly one behavior. This mirrors Run's dispatch table; add an arm
// when Run grows a new subtest.
func factoryForName(t *testing.T, name string, factory func(*testing.T) backend.Backend) {
	t.Helper()
	switch name {
	case "PutGetRoundTrip":
		testPutGetRoundTrip(t, factory)
	case "PutOverwrite":
		testPutOverwrite(t, factory)
	case "MissingKey":
		testMissingKey(t, factory)
	case "DeleteMissingKeyNoop":
		testDeleteMissingKeyNoop(t, factory)
	case "DeleteMissingBucket":
		testDeleteMissingBucket(t, factory)
	case "MissingBucket":
		testMissingBucket(t, factory)
	case "DeleteRemovesObject":
		testDeleteRemovesObject(t, factory)
	case "ListEmptyBucket":
		testListEmptyBucket(t, factory)
	case "ListPrefixFilter":
		testListPrefixFilter(t, factory)
	case "ListDelimiterGrouping":
		testListDelimiterGrouping(t, factory)
	case "ListStartAfter":
		testListStartAfter(t, factory)
	case "ListOrdering":
		testListOrdering(t, factory)
	case "BucketsVisible":
		testBucketsVisible(t, factory)
	case "ContextCancellation":
		testContextCancellation(t, factory)
	default:
		t.Fatalf("probe: unknown subtest %q", name)
	}
}

// --- violating probe backends -------------------------------------------------

// ensureMaps lazily initializes the embedded stub's object map (probe
// backends embed runnerStub by value; a nil map would panic on promoted Put).
func (m *runnerStub) ensureMaps() {
	if m.objects == nil {
		m.objects = map[string]map[string]memObj{}
	}
}

// Constructors keep every probe's embedded map initialized.
func newCorruptRead() backend.Backend     { c := &corruptReadStub{}; c.ensureMaps(); return c }
func newPhantomKey() backend.Backend      { p := &phantomKeyStub{}; p.ensureMaps(); return p }
func newStrictDelete() backend.Backend    { s := &strictDeleteStub{}; s.ensureMaps(); return s }
func newLenientDelete() backend.Backend   { l := &lenientDeleteStub{}; l.ensureMaps(); return l }
func newPhantomBucket() backend.Backend   { p := &phantomBucketStub{}; p.ensureMaps(); return p }
func newGhostDelete() backend.Backend     { g := &ghostDeleteStub{}; g.ensureMaps(); return g }
func newAlwaysTruncated() backend.Backend { a := &alwaysTruncatedStub{}; a.ensureMaps(); return a }
func newNoPrefixFilter() backend.Backend  { n := &noPrefixFilterStub{}; n.ensureMaps(); return n }
func newNoDelimiter() backend.Backend     { n := &noDelimiterStub{}; n.ensureMaps(); return n }
func newNoStartAfter() backend.Backend    { n := &noStartAfterStub{}; n.ensureMaps(); return n }
func newUnsortedList() backend.Backend    { u := &unsortedListStub{}; u.ensureMaps(); return u }
func newNoMaterialize() backend.Backend   { n := &noMaterializeStub{}; n.ensureMaps(); return n }

func newIgnoreContext() backend.Backend {
	i := &ignoreContextStub{}
	i.ensureMaps()
	// Pre-materialize the bucket so a ctx-ignored Get SUCCEEDS: a silent
	// success under a cancelled context is the violation; a NoSuchBucket
	// error would still satisfy the suite's error check and mask it.
	i.objects[bucket] = map[string]memObj{
		"k": {data: []byte("x"), meta: objectmodel.Object{Key: "k", Size: 1}},
	}
	return i
}

// corruptReadStub: Get returns content with a corrupted tail after Put.
type corruptReadStub struct{ runnerStub }

func (m *corruptReadStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	rc, obj, err := m.runnerStub.Get(ctx, bkt, key, opts)
	if err != nil {
		return rc, obj, err
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	return io.NopCloser(bytes.NewReader(append(data, 'X'))), obj, nil
}

// phantomKeyStub: Get/Stat succeed for keys that were never put.
type phantomKeyStub struct{ runnerStub }

func (m *phantomKeyStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	rc, obj, err := m.runnerStub.Get(ctx, bkt, key, opts)
	if err == nil || !isCode(err, objectmodel.CodeNoSuchKey) {
		return rc, obj, err
	}
	return io.NopCloser(strings.NewReader("")), objectmodel.Object{Key: key}, nil
}

func (m *phantomKeyStub) Stat(ctx context.Context, bkt, key string) (objectmodel.Object, error) {
	m.ensureMaps()
	meta, err := m.runnerStub.Stat(ctx, bkt, key)
	if err == nil || !isCode(err, objectmodel.CodeNoSuchKey) {
		return meta, err
	}
	return objectmodel.Object{Key: key}, nil
}

// strictDeleteStub: Delete of a missing key in an existing bucket errors
// (violates the idempotent-no-op PIN).
type strictDeleteStub struct{ runnerStub }

func (m *strictDeleteStub) Delete(ctx context.Context, bkt, key string) error {
	m.ensureMaps()
	m.mu.Lock()
	_, exists := m.objects[bkt]
	m.mu.Unlock()
	if err := m.runnerStub.Delete(ctx, bkt, key); err != nil {
		return err
	}
	if exists {
		m.mu.Lock()
		_, stillThere := m.objects[bkt][key]
		m.mu.Unlock()
		if !stillThere {
			// the key was missing and was no-op deleted — violate the PIN
			return objectmodel.ErrNoSuchKey(key)
		}
	}
	return nil
}

// lenientDeleteStub: Delete on a missing bucket returns nil (PIN violation).
type lenientDeleteStub struct{ runnerStub }

func (m *lenientDeleteStub) Delete(ctx context.Context, bkt, key string) error {
	m.ensureMaps()
	m.mu.Lock()
	_, exists := m.objects[bkt]
	m.mu.Unlock()
	if !exists {
		return nil
	}
	return m.runnerStub.Delete(ctx, bkt, key)
}

// phantomBucketStub: Stat succeeds against a never-materialized bucket.
type phantomBucketStub struct{ runnerStub }

func (m *phantomBucketStub) Stat(ctx context.Context, bkt, key string) (objectmodel.Object, error) {
	m.ensureMaps()
	meta, err := m.runnerStub.Stat(ctx, bkt, key)
	if err == nil || !isCode(err, objectmodel.CodeNoSuchBucket) {
		return meta, err
	}
	return objectmodel.Object{Key: key}, nil
}

// ghostDeleteStub: Delete claims success but the object survives.
type ghostDeleteStub struct{ runnerStub }

func (m *ghostDeleteStub) Delete(ctx context.Context, bkt, key string) error {
	m.ensureMaps()
	m.mu.Lock()
	_, exists := m.objects[bkt]
	m.mu.Unlock()
	if !exists {
		return objectmodel.ErrNoSuchBucket(bkt)
	}
	return nil // object deliberately NOT removed
}

// alwaysTruncatedStub: every listing claims IsTruncated.
type alwaysTruncatedStub struct{ runnerStub }

func (m *alwaysTruncatedStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	page.IsTruncated = true
	page.NextToken = "probe"
	return page, nil
}

// noPrefixFilterStub: List ignores the Prefix parameter.
type noPrefixFilterStub struct{ runnerStub }

func (m *noPrefixFilterStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	p.Prefix = ""
	return m.runnerStub.List(ctx, bkt, p)
}

// noDelimiterStub: List ignores the Delimiter parameter.
type noDelimiterStub struct{ runnerStub }

func (m *noDelimiterStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	p.Delimiter = ""
	return m.runnerStub.List(ctx, bkt, p)
}

// noStartAfterStub: List ignores StartAfter.
type noStartAfterStub struct{ runnerStub }

func (m *noStartAfterStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	p.StartAfter = ""
	return m.runnerStub.List(ctx, bkt, p)
}

// unsortedListStub: List returns keys reversed.
type unsortedListStub struct{ runnerStub }

func (m *unsortedListStub) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	m.ensureMaps()
	page, err := m.runnerStub.List(ctx, bkt, p)
	if err != nil {
		return page, err
	}
	for i, j := 0, len(page.Objects)-1; i < j; i, j = i+1, j-1 {
		page.Objects[i], page.Objects[j] = page.Objects[j], page.Objects[i]
	}
	return page, nil
}

// noMaterializeStub: Put succeeds but never registers the bucket.
type noMaterializeStub struct{ runnerStub }

func (m *noMaterializeStub) Put(ctx context.Context, bkt, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	m.ensureMaps()
	return m.runnerStub.Put(ctx, "elsewhere", key, data, size, opts)
}

// ignoreContextStub: operations ignore a cancelled context (silent success).
type ignoreContextStub struct{ runnerStub }

func (m *ignoreContextStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	return m.runnerStub.Get(context.Background(), bkt, key, opts)
}

// --- failing backend (targeted operation failures) ----------------------------

// failingBackend fails exactly the operations its flags select; used by the
// PutGetRoundTrip probe (Put must fail) and negative controls.
type failingBackend struct {
	mu      sync.Mutex
	failPut bool
	objects map[string]map[string]memObj
}

func newFailing() *failingBackend {
	return &failingBackend{objects: map[string]map[string]memObj{}}
}

func (f *failingBackend) Get(ctx context.Context, bkt, key string, _ objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[bkt]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchBucket(bkt)
	}
	o, ok := b[key]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return io.NopCloser(strings.NewReader(string(o.data))), o.meta, nil
}

func (f *failingBackend) Put(ctx context.Context, bkt, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut {
		return objectmodel.Object{}, errors.New("probe: put failed")
	}
	buf, _ := io.ReadAll(data)
	meta := objectmodel.Object{
		Key: key, Size: int64(len(buf)),
		ETag: objectmodel.QuotedETag("e"), LastModified: time.Now(),
		ContentType: opts.ContentType, Metadata: opts.Metadata,
	}
	b, ok := f.objects[bkt]
	if !ok {
		b = map[string]memObj{}
		f.objects[bkt] = b
	}
	b[key] = memObj{data: buf, meta: meta}
	return meta, nil
}

func (f *failingBackend) Delete(ctx context.Context, bkt, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.objects[bkt]; ok {
		delete(b, key)
	}
	return nil
}

func (f *failingBackend) Stat(ctx context.Context, bkt, key string) (objectmodel.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[bkt]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchBucket(bkt)
	}
	o, ok := b[key]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return o.meta, nil
}

func (f *failingBackend) List(ctx context.Context, bkt string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[bkt]
	if !ok {
		return objectmodel.ListPage{}, objectmodel.ErrNoSuchBucket(bkt)
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	page := objectmodel.ListPage{}
	for _, k := range keys {
		page.Objects = append(page.Objects, b[k].meta)
	}
	return page, nil
}

func (f *failingBackend) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []objectmodel.BucketInfo
	for name := range f.objects {
		out = append(out, objectmodel.BucketInfo{Name: name})
	}
	return out, nil
}

func (f *failingBackend) Capabilities() objectmodel.CapabilitySet { return objectmodel.CapabilitySet{} }
