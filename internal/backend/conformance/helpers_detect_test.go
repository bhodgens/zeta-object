package conformance

// helpers_detect_test.go — proves the suite's HELPER failure surface works
// (the same expected-failure-in-a-child-run pattern as detection_test.go,
// whose probe table covers only the contract subtests). Each probe twists a
// runnerStub so one helper's success-check branch fires; the child run fails
// by design and the parent asserts every violation was DETECTED.

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

func helperProbes() []violationProbe {
	return []violationProbe{
		{"assertObjectBasics-Key", "Put reported the wrong key", func() backend.Backend {
			p := &wrongFieldStub{field: "key"}
			p.ensureMaps()
			return p
		}},
		{"assertObjectBasics-Size", "Put reported the wrong size", func() backend.Backend {
			p := &wrongFieldStub{field: "size"}
			p.ensureMaps()
			return p
		}},
		{"assertObjectBasics-ETag", "Put left the ETag empty", func() backend.Backend {
			p := &wrongFieldStub{field: "etag"}
			p.ensureMaps()
			return p
		}},
		{"assertObjectBasics-LastModified", "Put left LastModified zero", func() backend.Backend {
			p := &wrongFieldStub{field: "lastModified"}
			p.ensureMaps()
			return p
		}},
		{"assertObjectBasics-ContentType", "Put dropped the request content type", func() backend.Backend {
			p := &wrongFieldStub{field: "contentType"}
			p.ensureMaps()
			return p
		}},
		{"assertObjectBasics-Metadata", "Put dropped the request metadata", func() backend.Backend {
			p := &wrongFieldStub{field: "metadata"}
			p.ensureMaps()
			return p
		}},
		{"assertSameObject", "Get metadata diverged from the Put result", func() backend.Backend {
			p := &driftMetaStub{}
			p.ensureMaps()
			return p
		}},
		{"closeAndReport", "a Get reader's Close error went unreported", func() backend.Backend {
			return &badCloseBackend{}
		}},
		{"concurrentOp", "a concurrent Put failed and it went unreported", func() backend.Backend {
			b := newFailing()
			b.failPut = true
			return b
		}},
	}
}

// TestHelpersDetectViolations spawns a child `go test` running the helper
// probes; the child fails by design and the parent requires every probe to
// have been caught by its helper.
func TestHelpersDetectViolations(t *testing.T) {
	if testing.Short() {
		t.Skip("helper probes spawn a child test binary; skipped in -short")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(thisFile)

	results := filepath.Join(t.TempDir(), "helper-detection-results.txt")
	cmd := exec.Command("go", "test", "-run", "TestHelperProbesChild", "-count=1", "./")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "ZETAOBJECT_HELPER_PROBES=1", "ZETAOBJECT_DETECTION_RESULTS="+results)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child helper-probe run unexpectedly PASSED:\n%s", output)
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
	for _, p := range helperProbes() {
		if !detected[p.name] {
			t.Errorf("helpers FAILED TO DETECT %q (%s)", p.name, p.why)
		}
	}
}

// TestHelperProbesChild is the child entry point (mirrors
// TestViolationProbesChild): run every probe, record the catches, fail.
func TestHelperProbesChild(t *testing.T) {
	resultsPath := os.Getenv("ZETAOBJECT_DETECTION_RESULTS")
	if os.Getenv("ZETAOBJECT_HELPER_PROBES") != "1" || resultsPath == "" {
		t.Skip("helper probes run only under TestHelpersDetectViolations")
	}
	var sb strings.Builder
	for _, p := range helperProbes() {
		caught := !t.Run(p.name, func(t *testing.T) {
			runHelperProbe(t, p.name, func(*testing.T) backend.Backend { return p.factory() })
		})
		if caught {
			sb.WriteString("DETECTED " + p.name + "\n")
		}
	}
	if writeErr := os.WriteFile(resultsPath, []byte(sb.String()), 0o644); writeErr != nil {
		t.Fatalf("write detection results: %v", writeErr)
	}
	t.FailNow()
}

// runHelperProbe drives exactly one helper against one twisted backend.
func runHelperProbe(t *testing.T, name string, factory func(*testing.T) backend.Backend) {
	t.Helper()
	switch name {
	case "assertObjectBasics-Key", "assertObjectBasics-Size", "assertObjectBasics-ETag",
		"assertObjectBasics-LastModified", "assertObjectBasics-ContentType", "assertObjectBasics-Metadata":
		testPutGetRoundTrip(t, factory)
	case "assertSameObject":
		testPutGetRoundTrip(t, factory)
	case "closeAndReport":
		b := factory(t)
		errs := make(chan error, 1)
		rc, _, err := b.Get(context.Background(), bucket, "k", objectmodel.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		closeAndReport(rc, errs)
		close(errs)
		for err := range errs {
			t.Errorf("closeAndReport did not report the Close failure: %v", err)
		}
	case "concurrentOp":
		b := factory(t)
		materializeBucket(t, b, bucket)
		errs := make(chan error, 1)
		concurrentOp(context.Background(), b, 0, 0, errs) // (0+0)%3==0 → Put
		close(errs)
		for err := range errs {
			t.Errorf("concurrentOp did not report the Put failure: %v", err)
		}
	default:
		t.Fatalf("probe: unknown helper subtest %q", name)
	}
}

// --- probe backends -----------------------------------------------------------

// wrongFieldStub corrupts exactly one field of the stored metadata so the
// matching assertObjectBasics check fires.
type wrongFieldStub struct {
	field string
	runnerStub
}

func (m *wrongFieldStub) Put(ctx context.Context, bkt, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	m.ensureMaps()
	meta, err := m.runnerStub.Put(ctx, bkt, key, data, size, opts)
	if err != nil {
		return meta, err
	}
	switch m.field {
	case "key":
		meta.Key = "corrupted-" + meta.Key
	case "size":
		meta.Size = meta.Size + 1
	case "etag":
		meta.ETag = ""
	case "lastModified":
		meta.LastModified = zeroTime()
	case "contentType":
		if meta.ContentType != "" {
			meta.ContentType = "text/x-corrupted"
		}
	case "metadata":
		if len(meta.Metadata) > 0 {
			meta.Metadata = map[string]string{"corrupted": "yes"}
		}
	}
	m.mu.Lock()
	if b, ok := m.objects[bkt]; ok {
		if o, ok := b[key]; ok {
			o.meta = meta
			b[key] = o
		}
	}
	m.mu.Unlock()
	return meta, nil
}

// driftMetaStub: Get returns metadata that drifted from the Put result.
type driftMetaStub struct{ runnerStub }

func (m *driftMetaStub) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	m.ensureMaps()
	rc, obj, err := m.runnerStub.Get(ctx, bkt, key, opts)
	if err != nil {
		return rc, obj, err
	}
	obj.ContentType = "text/x-drifted"
	return rc, obj, nil
}

// badCloseBackend: Get returns a reader whose Close fails.
type badCloseBackend struct{ failingBackend }

func (b *badCloseBackend) Get(ctx context.Context, bkt, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	rc, obj, err := b.failingBackend.Get(ctx, bkt, key, opts)
	if err != nil {
		return rc, obj, err
	}
	return errCloser{ReadCloser: rc}, obj, nil
}

type errCloser struct{ io.ReadCloser }

func (e errCloser) Close() error { return errors.New("probe: close failed") }

func zeroTime() time.Time { return time.Time{} }
