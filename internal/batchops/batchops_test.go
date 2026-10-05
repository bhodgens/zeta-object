// batchops_test.go — quic-h3-2026-10 leaf 07 Task 1: the protocol-free
// batch execution core, TDD. The op interface is a test double (fakeExec)
// that records every call — batchops imports NO frontend, and the
// 400-before-any-execution rule is pinned by asserting the double saw
// ZERO calls for every malformed-manifest class.
package batchops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// fakeExec is the Executor test double: it records calls in execution
// order and can fail scripted items.
type fakeExec struct {
	mu    sync.Mutex
	calls []string // "copy:from->to", "move:from->to", "delete:key" in call order
	ifMs  []string // the ifMatch each call carried (parallel to calls)
	fail  map[string]error
}

func (f *fakeExec) record(kind, a, b, ifMatch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b == "" {
		f.calls = append(f.calls, kind+":"+a)
	} else {
		f.calls = append(f.calls, kind+":"+a+"->"+b)
	}
	f.ifMs = append(f.ifMs, ifMatch)
	for prefix, err := range f.fail {
		if strings.HasPrefix(f.calls[len(f.calls)-1], prefix) {
			return err
		}
	}
	return nil
}

func (f *fakeExec) Copy(_ context.Context, from, to, ifMatch string) error {
	return f.record("copy", from, to, ifMatch)
}

func (f *fakeExec) Move(_ context.Context, from, to, ifMatch string) error {
	return f.record("move", from, to, ifMatch)
}

func (f *fakeExec) Delete(_ context.Context, key, ifMatch string) error {
	return f.record("delete", key, "", ifMatch)
}

func (f *fakeExec) saw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// keyOK is the test validator: the same segment rules the frontends'
// validators enforce (.metadata/.zfs/.. rejection) — identical behavior,
// pinned here because the shared validator cannot be imported (that
// would make batchops depend on a frontend).
func keyOK(key string) error {
	if key == "" {
		return errors.New("empty key")
	}
	for seg := range strings.SplitSeq(key, "/") {
		switch seg {
		case "..", ".metadata", ".zfs":
			return errors.New("rejected segment: " + seg)
		}
	}
	return nil
}

func newRunner(f *fakeExec) *Runner {
	return &Runner{Exec: f, ValidateKey: keyOK}
}

// manifest3 is the canonical copy/move/delete manifest.
func manifest3() Manifest {
	return Manifest{Operations: []Operation{
		{Op: "copy", From: "a/old.txt", To: "b/new.txt"},
		{Op: "move", From: "a/x.bin", To: "archive/x.bin"},
		{Op: "delete", From: "tmp/junk"},
	}}
}

// TestRun_ExecutesInManifestOrderWithPerItemResults: ordered execution,
// one ok result per item carrying its manifest index.
func TestRun_ExecutesInManifestOrderWithPerItemResults(t *testing.T) {
	f := &fakeExec{}
	results := newRunner(f).Run(context.Background(), manifest3())

	wantCalls := []string{
		"copy:a/old.txt->b/new.txt",
		"move:a/x.bin->archive/x.bin",
		"delete:tmp/junk",
	}
	got := f.saw()
	if len(got) != len(wantCalls) {
		t.Fatalf("double saw %v, want %v", got, wantCalls)
	}
	for i := range wantCalls {
		if got[i] != wantCalls[i] {
			t.Fatalf("call[%d] = %q, want %q (execution must be sequential in manifest order)", i, got[i], wantCalls[i])
		}
	}
	if len(results) != 3 {
		t.Fatalf("results = %d entries, want 3: %+v", len(results), results)
	}
	for i, r := range results {
		if r.Index != i {
			t.Errorf("results[%d].Index = %d, want %d", i, r.Index, i)
		}
		if r.Status != StatusOK {
			t.Errorf("results[%d].Status = %q, want %q", i, r.Status, StatusOK)
		}
		if r.Code != "" || r.Message != "" {
			t.Errorf("results[%d] carries error fields on success: %+v", i, r)
		}
	}
}

// TestRun_PartialSuccess_ItemFailureDoesNotStopLaterItems: a mid-manifest
// failure is a per-item result; the remaining items still execute (NO
// cross-item atomicity — the honest contract).
func TestRun_PartialSuccess_ItemFailureDoesNotStopLaterItems(t *testing.T) {
	f := &fakeExec{fail: map[string]error{
		"move:a/x.bin": objectmodel.ErrNoSuchKey("a/x.bin"),
	}}
	results := newRunner(f).Run(context.Background(), manifest3())

	if len(f.saw()) != 3 {
		t.Fatalf("double saw %d calls, want 3 (item 2 failing must not stop item 3): %v", len(f.saw()), f.saw())
	}
	if results[1].Status != StatusError {
		t.Fatalf("results[1].Status = %q, want %q", results[1].Status, StatusError)
	}
	if results[1].Code != objectmodel.CodeNoSuchKey {
		t.Errorf("results[1].Code = %q, want %q", results[1].Code, objectmodel.CodeNoSuchKey)
	}
	if results[1].Message == "" {
		t.Errorf("results[1].Message is empty — per-item errors must name the problem")
	}
	if results[2].Status != StatusOK {
		t.Errorf("results[2].Status = %q, want %q (item 3 executed after item 2 failed)", results[2].Status, StatusOK)
	}
	if results[0].Status != StatusOK {
		t.Errorf("results[0].Status = %q, want %q (earlier success is not rolled back)", results[0].Status, StatusOK)
	}
}

// TestRun_PreconditionFailedMapsToConflict: an ifMatch precondition
// failure is the "conflict" status, not "error" (Contract 9's response
// shape).
func TestRun_PreconditionFailedMapsToConflict(t *testing.T) {
	f := &fakeExec{fail: map[string]error{
		"copy:": objectmodel.ErrPreconditionFailed(),
	}}
	results := newRunner(f).Run(context.Background(), manifest3())
	if results[0].Status != StatusConflict {
		t.Fatalf("results[0].Status = %q, want %q", results[0].Status, StatusConflict)
	}
	if results[0].Code != objectmodel.CodePreconditionFailed {
		t.Errorf("results[0].Code = %q, want %q", results[0].Code, objectmodel.CodePreconditionFailed)
	}
}

// TestRun_IfMatchReachesTheExecutor: an item's ifMatch is passed through
// to the op interface verbatim (the executor enforces the precondition
// against its single-op path).
func TestRun_IfMatchReachesTheExecutor(t *testing.T) {
	f := &fakeExec{}
	m := Manifest{Operations: []Operation{
		{Op: "copy", From: "a", To: "b", IfMatch: "etag-1"},
		{Op: "move", From: "c", To: "d", IfMatch: "etag-2"},
		{Op: "delete", From: "k", IfMatch: "etag-3"},
	}}
	newRunner(f).Run(context.Background(), m)
	want := []string{"etag-1", "etag-2", "etag-3"}
	for i, w := range want {
		if f.ifMs[i] != w {
			t.Errorf("call[%d] ifMatch = %q, want %q", i, f.ifMs[i], w)
		}
	}
}

// TestProcess_400BeforeAnyExecution: every malformed-manifest class —
// bad JSON, unknown op, invalid key (.metadata AND .zfs), copy without
// destination, from==to, over-limit — is a request error and the double
// saw ZERO calls. This is the 400-before-execution proof.
func TestProcess_400BeforeAnyExecution(t *testing.T) {
	over := make([]Operation, MaxOperations+1)
	for i := range over {
		over[i] = Operation{Op: "delete", From: "k"}
	}
	cases := []struct {
		name string
		body string
	}{
		{"bad JSON", `{"operations": [`},
		{"not JSON at all", "this is not json"},
		{"unknown op", `{"operations":[{"op":"purge","from":"a"}]}`},
		{"missing op", `{"operations":[{"from":"a"}]}`},
		{"metadata segment key", `{"operations":[{"op":"delete","from":".metadata/x"}]}`},
		{"zfs segment key", `{"operations":[{"op":"copy","from":"a","to":".zfs/snap"}]}`},
		{"traversal key", `{"operations":[{"op":"delete","from":"../escape"}]}`},
		{"copy without to", `{"operations":[{"op":"copy","from":"a"}]}`},
		{"copy from==to", `{"operations":[{"op":"copy","from":"a","to":"a"}]}`},
		{"empty delete key", `{"operations":[{"op":"delete","from":""}]}`},
		{"over limit", func() string {
			var sb strings.Builder
			sb.WriteString(`{"operations":[`)
			for i := range over {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(`{"op":"delete","from":"k"}`)
			}
			sb.WriteString(`]}`)
			return sb.String()
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExec{}
			_, err := newRunner(f).Process(context.Background(), []byte(tc.body))
			if err == nil {
				t.Fatalf("Process succeeded, want a request (400-class) error")
			}
			if calls := f.saw(); len(calls) != 0 {
				t.Fatalf("400-before-execution violated: the double saw %v", calls)
			}
		})
	}
}

// TestMaxOperationsIsNamedAndPinned: the limit is a named constant with
// this test (Contract 9); exactly MaxOperations items is legal.
func TestMaxOperationsIsNamedAndPinned(t *testing.T) {
	if MaxOperations != 1000 {
		t.Fatalf("MaxOperations = %d, want 1000", MaxOperations)
	}
	var sb strings.Builder
	sb.WriteString(`{"operations":[`)
	for i := range MaxOperations {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"op":"delete","from":"k"}`)
	}
	sb.WriteString(`]}`)
	f := &fakeExec{}
	resp, err := newRunner(f).Process(context.Background(), []byte(sb.String()))
	if err != nil {
		t.Fatalf("exactly MaxOperations items must be legal: %v", err)
	}
	if len(resp.Results) != MaxOperations {
		t.Fatalf("results = %d, want %d", len(resp.Results), MaxOperations)
	}
}

// TestProcess_RunsValidManifest: the happy path — parse, validate, run.
func TestProcess_RunsValidManifest(t *testing.T) {
	f := &fakeExec{}
	body := `{"operations":[{"op":"copy","from":"a/x","to":"b/y"},{"op":"delete","from":"k"}]}`
	resp, err := newRunner(f).Process(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(resp.Results) != 2 || resp.Results[0].Status != StatusOK || resp.Results[1].Status != StatusOK {
		t.Fatalf("results = %+v, want two ok results", resp.Results)
	}
	if len(f.saw()) != 2 {
		t.Fatalf("double saw %v, want 2 calls", f.saw())
	}
}

// TestDefaultKeyValidatorPinsTheSharedRules: the packaged validator
// pins the SAME segment rules the frontends' key validators enforce —
// .metadata and .zfs segments are rejected, lookalike segments are fine.
func TestDefaultKeyValidatorPinsTheSharedRules(t *testing.T) {
	rejected := []string{"", ".metadata", ".metadata/x", "a/.metadata/b", ".zfs", "a/.zfs/snap", "../escape", "a/../b", strings.Repeat("k", 1025)}
	for _, key := range rejected {
		if err := DefaultKeyValidator(key); err == nil {
			t.Errorf("DefaultKeyValidator(%q) accepted, want rejection", key)
		}
	}
	accepted := []string{"a/x", "x.metadata", "a..b", "..hidden", ".hidden/keep", strings.Repeat("k", 1024)}
	for _, key := range accepted {
		if err := DefaultKeyValidator(key); err != nil {
			t.Errorf("DefaultKeyValidator(%q) rejected: %v", key, err)
		}
	}
}

// TestRun_NonObjectModelErrorMapsToInternalError: an executor failure
// without objectmodel identity is a real I/O failure class — reported
// per-item as InternalError, never silently ok.
func TestRun_NonObjectModelErrorMapsToInternalError(t *testing.T) {
	f := &fakeExec{fail: map[string]error{"delete:": errors.New("disk on fire")}}
	results := newRunner(f).Run(context.Background(), Manifest{Operations: []Operation{{Op: "delete", From: "k"}}})
	if results[0].Status != StatusError || results[0].Code != objectmodel.CodeInternalError {
		t.Fatalf("results[0] = %+v, want error/InternalError", results[0])
	}
}
