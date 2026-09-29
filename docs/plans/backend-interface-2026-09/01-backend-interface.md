# Backend Interface + Conformance Suite - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** master.md (this directory)
- **Scope:** Author the frozen `Backend` interface, the error taxonomy bridging
  `objectmodel.Error`, and the reusable interface-level conformance test suite.
- **Dependencies:** `object-model-2026-09` tree complete (internal/objectmodel exists with
  its pinned types). No sibling leaves.
- **Estimated Context:** 45K (objectmodel API reading + generation + iteration)
- **Concurrency Group:** A

## Goal

Create package `internal/backend` containing: (1) the `Backend` interface exactly as pinned
in the parent's Contract 1, (2) `errors.go` with the error taxonomy that maps backend
failures onto `objectmodel.Error` codes so handlers never see raw `*os.PathError`, and (3)
`conformance_test.go` exporting a test function that exercises every Backend method against
a contract — any future backend (fs, ZFS ring, S3 proxy) proves correctness by running this
suite against its own instance in a temp root.

This leaf deliberately ships NO storage implementation; it is the seam only. Behavioral code
lands in leaf 02.

## Context

zeta-object is a Go 1.27 single-binary S3 server, module `zeta-object`, currently package `main` at
repo root. Handlers touch the filesystem directly (~97 `os.*` calls). This tree inserts a
Backend seam between the S3 protocol layer and storage. The neutral object model lives in
`internal/objectmodel` (created by the sibling `object-model-2026-09` tree — it MUST already
be merged when you start; if `ls internal/objectmodel` fails, STOP and report).

Key files to understand before implementing:
- `internal/objectmodel/*.go` — the pinned neutral types this package imports: `Object`,
  `GetOptions`, `PutOptions`, `ListParams`, `ListPage`, `BucketInfo`, `CapabilitySet`,
  `Error` (codes such as NoSuchBucket / NoSuchKey). Read the actual files; this leaf codes
  against what is there, NOT against this document's guesses.
- `docs/plans/backend-interface-2026-09/master.md` — Contracts 1–6 (frozen).

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/backend/backend.go
package backend

import (
	"context"
	"io"

	"zeta-object/internal/objectmodel"
)

type Backend interface {
	Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error)
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error)
	Delete(ctx context.Context, bucket, key string) error
	Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error)
	List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error)
	Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error)
	Capabilities() objectmodel.CapabilitySet
}
```

```go
// File: internal/backend/errors.go — semantic contract (exact sentinels finalized
// against objectmodel.Error's actual code set):
//   - backends return errors that errors.Is/As match against objectmodel.Error
//     codes for caller-facing conditions (NoSuchBucket, NoSuchKey, ...);
//   - ErrNotSupported: operation the backend cannot serve at all;
//   - ErrUnknownBackend: registry lookup miss (used by leaf 03).
package backend

var ErrNotSupported = errors.New("backend: operation not supported")
var ErrUnknownBackend = errors.New("backend: unknown backend type")
```

```go
// File: internal/backend/conformance_test.go — exported for reuse by other packages:
func RunBackendConformance(t *testing.T, name string, factory func(t *testing.T) Backend)
// Factory receives a testing.T-scoped fixture (fresh temp root per invocation) and
// returns a Backend ready for use. The suite covers:
//   Put→Get round-trip (content + objectmodel.Object fields),
//   Put overwrite of same key,
//   Get/Stat/Delete on missing key → objectmodel NoSuchKey identity,
//   Get/Delete/Stat/Buckets on missing bucket → NoSuchBucket identity,
//   Delete removes object (subsequent Stat → NoSuchKey),
//   Delete missing key → NoSuchKey (idempotent-no vs error: pin S3-consistent behavior),
//   List: empty bucket, prefix filter, delimiter grouping, marker/pagination,
//   List ordering is lexicographic by key,
//   Buckets reflects created/visible buckets,
//   Capabilities: returns zero-value-safe set (suite does not assert specific flags —
//     capability semantics are backend-owned),
//   ctx cancellation honored (factory may return a ctx-aware backend; suite asserts
//     cancelled-ctx Get/Put do not panic and return an error),
//   concurrent Put+Get+Delete on the same key under -race (no torn reads).
```

### What This Leaf Consumes

```go
// From internal/objectmodel (sibling tree object-model-2026-09 — ALREADY MERGED):
package objectmodel

type Object struct { /* pinned by sibling tree: key, size, etag, contentType,
                        lastModified, custom metadata, etc. — read the package */ }
type GetOptions struct{}
type PutOptions struct{ /* content-type, custom metadata, ... per sibling tree */ }
type ListParams struct{ /* prefix, delimiter, marker, maxKeys */ }
type ListPage struct{ /* objects, common prefixes, next marker, truncated */ }
type BucketInfo struct{ /* name, created, path/... */ }
type CapabilitySet struct{ /* capability flags */ }
type Error struct{ /* code + message; code constants NoSuchBucket, NoSuchKey, ... */ }
```

Do NOT re-declare or shadow any objectmodel type. If a type you need is absent or shaped
differently from the examples above, STOP and report the exact gap (orchestrator
reconciles contracts; leaves never fork the model).

## Tasks

### Task 1: Package skeleton + frozen Backend interface

**Objective:** Create `internal/backend` with the interface exactly as pinned.

**Files:**
- Create: `internal/backend/backend.go`
- Test: `internal/backend/backend_test.go`

**Step 1: Write failing test**

```go
package backend

import (
	"context"
	"io"
	"testing"

	"zeta-object/internal/objectmodel"
)

// compile-time shape probe: a stub satisfying the interface must exist.
type stubBackend struct{}

func (stubBackend) Get(context.Context, string, string, objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	return nil, objectmodel.Object{}, nil
}
func (stubBackend) Put(context.Context, string, string, io.Reader, int64, objectmodel.PutOptions) (objectmodel.Object, error) {
	return objectmodel.Object{}, nil
}
func (stubBackend) Delete(context.Context, string, string) error { return nil }
func (stubBackend) Stat(context.Context, string, string) (objectmodel.Object, error) {
	return objectmodel.Object{}, nil
}
func (stubBackend) List(context.Context, string, objectmodel.ListParams) (objectmodel.ListPage, error) {
	return objectmodel.ListPage{}, nil
}
func (stubBackend) Buckets(context.Context) ([]objectmodel.BucketInfo, error) { return nil, nil }
func (stubBackend) Capabilities() objectmodel.CapabilitySet                   { return objectmodel.CapabilitySet{} }

func TestBackendInterfaceShape(t *testing.T) {
	var _ Backend = stubBackend{} // compile-time assertion
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/backend/ -run TestBackendInterfaceShape -v`
Expected: FAIL — `undefined: Backend` (package has no interface yet).

**Step 3: Write minimal implementation**

`internal/backend/backend.go` with package clause, imports, and the interface verbatim from
"What This Leaf Exposes" (doc comment on the interface: "Backend is the pluggable
data-plane seam. Implementations MUST be safe for concurrent use and MUST return errors
matching objectmodel.Error codes for caller-facing conditions.").

**Step 4: Run test to verify pass**

Run: `go test ./internal/backend/ -run TestBackendInterfaceShape -v`
Expected: PASS

### Task 2: Error taxonomy

**Objective:** Sentinels + objectmodel.Error mapping helpers so no backend leaks raw fs errors.

**Files:**
- Create: `internal/backend/errors.go`
- Test: `internal/backend/errors_test.go`

**Step 1: Write failing test**

```go
package backend

import (
	"errors"
	"os"
	"testing"

	"zeta-object/internal/objectmodel" // import path per actual tree layout
)

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want objectmodel.ErrorCode // field/constants per actual objectmodel API
	}{
		{"notExist", os.ErrNotExist, objectmodel.ErrCodeNoSuchKey},
		{"notSupported", ErrNotSupported, ""}, // sentinel: no objectmodel code
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToObjectModelError(tc.err)
			if tc.want != "" && !objectmodel.IsCode(got, tc.want) { // adapt to actual API
				t.Fatalf("want code %v, got %v", tc.want, got)
			}
		})
	}
}

func TestSentinels(t *testing.T) {
	if !errors.Is(ErrNotSupported, ErrNotSupported) {
		t.Fatal("sentinel identity broken")
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/backend/ -run 'TestErrorMapping|TestSentinels' -v`
Expected: FAIL — `undefined: ToObjectModelError` (and sentinel names).

**Step 3: Write minimal implementation**

`errors.go` provides: `ErrNotSupported`, `ErrUnknownBackend`, and `ToObjectModelError(err)
error` — maps `fs.ErrNotExist`-style causes onto `objectmodel.Error` with the caller-facing
code, wrapping with `%w` so `errors.Is` still finds the cause. The exact objectmodel error
construction/conventions come from the actual package — adapt the test's commented
"adapt to actual API" lines to the real identifiers BEFORE the green step, not after.

**Step 4: Run test to verify pass**

Run: `go test ./internal/backend/ -v`
Expected: PASS (whole package so far)

### Task 3: Conformance suite

**Objective:** The reusable contract test every backend runs against itself.

**Files:**
- Create: `internal/backend/conformance_test.go`
- Test (consumer proof): `internal/backend/conformance_stub_test.go`

**Step 1: Write failing test**

Write `RunBackendConformance(t *testing.T, name string, factory func(t *testing.T) Backend)`
covering the behaviors enumerated in "What This Leaf Exposes", each as a `t.Run` subtest,
table-driven where inputs vary (keys, prefixes, delimiters, markers). Then write the stub
consumer proving reuse:

```go
// conformance_stub_test.go — a trivial in-memory Backend proves the suite is runnable
// by third-party backends without a real storage layer. DELETE THIS STUB in leaf 02's
// wave? No: keep it permanently — it is the suite's own regression harness.
func TestConformanceAgainstStub(t *testing.T) {
	RunBackendConformance(t, "stub", func(t *testing.T) Backend {
		return newMemStub(t) // in-memory map-based stub defined in this file
	})
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/backend/ -run TestConformanceAgainstStub -v`
Expected: FAIL initially — `RunBackendConformance` undefined (or stub fails behaviors,
which is exactly what the suite is for; fix the STUB, never loosen the suite).

**Step 3: Write minimal implementation**

All conformance logic lives in `conformance_test.go`. Suite rules:
- Every expectation is phrased in objectmodel terms (codes, types) — never fs terms.
- Where S3-adjacent semantics are ambiguous (e.g. Delete of a missing key: S3 is 204-noop),
  the suite pins ONE behavior and documents it in a comment; leaf 02 must make fsbackend
  match that pin, and the pin must be stated in this file, not negotiated later.
- The same-key concurrency subtest uses goroutines + `sync.WaitGroup`; it is meaningful
  under `-race` only but runs always.
- The cancelled-ctx subtest asserts error-return (not panic), not latency.

**Step 4: Run test to verify pass**

Run: `go test -race ./internal/backend/ -v`
Expected: PASS, all subtests.

### Task 4: Gates + doc comments

**Objective:** Package is gofmt/vet-clean and its doc comments state the seam rules.

**Files:**
- Modify: `internal/backend/backend.go` (doc comments only)

**Step 1: Write failing test** — none (docs-only task); the "test" is the gate:

Run: `gofmt -l internal/backend/ && go vet ./internal/backend/`
Expected: empty output, exit 0. Fix until true.

**Step 3: Implementation**

Package doc on `backend.go`: seam rules — no `*os.PathError` escapes, ctx must be honored,
implementations must be concurrency-safe, multipart orchestration lives above the seam.
Doc on `Capabilities()`: advisory for callers; errors are authoritative.

**Step 4: Verify**

Run: `go build ./... && go vet ./... && go test ./internal/backend/ -count=1 -v`
Expected: PASS.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and tests passing (`go test -race ./internal/backend/`)
- [ ] Interface contract matches the parent's frozen signature character-for-character
- [ ] All files at exact specified paths (`internal/backend/{backend,errors}.go` + tests)
- [ ] No re-declared objectmodel types; only imports from the real package
- [ ] Conformance suite is exported and runnable by an external package (stub proves it)
- [ ] Every ambiguous semantic has a written pin comment in conformance_test.go
- [ ] gofmt clean, `go vet ./...` clean
- [ ] No deviations from spec (or documented below)
- [ ] No scope creep — no fsbackend, no registry, no handler changes in this leaf

**DO NOT COMMIT.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Every test in the task is present and passing (ask for RED/GREEN evidence)
- [ ] `Backend` interface matches parent Contract 1 exactly (7 methods, signatures verbatim)
- [ ] Error taxonomy: sentinels + mapping exist; no objectmodel type re-declared
- [ ] Conformance suite exported, table-driven, pins ambiguous semantics in comments
- [ ] Code follows project conventions (stdlib only, gofmt, errors-as-values)
- [ ] No bugs, no security issues
- [ ] No scope creep beyond specified tasks

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The objectmodel package is the source of truth for every type name in this leaf. The
  code stubs above show the EXPECTED shape from the pinned contract; where the real
  package differs, the real package wins and you adapt mechanically (identifiers only,
  never semantics). If a semantic piece is missing (e.g. no ErrorCode constants), STOP
  and report — do not invent objectmodel API.
- The mem stub in conformance_stub_test.go is deliberately permanent: it keeps the suite
  honest when the fs backend evolves and gives leaf 03 a trivial backend for registry
  tests if useful.
- Keep the suite runtime small (<2s): no sleeps; concurrency subtest is goroutine-based.
- Future backends run this via `backendtest`-style import of the test file — Go does not
  export test funcs across packages; leaf 02 consumes the suite by placing its test file
  in `internal/backend`'s package path OR the suite file is symlinked/copied per backend
  package (standard Go pattern: `internal/backend/conformance_test.go` exposes
  `RunBackendConformance`; `fsbackend` package's test file lives in package
  `backend_test`? — resolve this: the pragmatic pattern is exporting the runner from a
  non-test file `internal/backend/conformance/conformance.go` with signature
  `func Run(t *testing.T, name string, factory func(*testing.T) Backend)` IF cross-package
  reuse is blocked. Decide during Task 3 and document the choice in the file header.
  Either shape is acceptable; the contract is that fsbackend (leaf 02) runs the full
  suite against its own instance with zero duplication of assertions.)
