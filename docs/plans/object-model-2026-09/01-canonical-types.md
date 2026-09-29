# Canonical Types & Error Taxonomy - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Create the `internal/objectmodel/` package with the pinned canonical types, normalization helpers, and the backend-neutral error taxonomy, plus table-driven tests.
- **Dependencies:** none (this is the first leaf of the contract-owner tree)
- **Estimated Context:** 45K (exploration + generation + iteration + overhead)
- **Concurrency Group:** A (runs alone; leaf 02 compiles against this leaf's output)
- **Do NOT commit.**

## Goal

Deliver `internal/objectmodel/` — the neutral object model every future
Backend, Frontend, and MetadataProvider shares. This leaf creates:

- `internal/objectmodel/model.go` — pinned structs (`Object`, `BucketInfo`,
  `ListPage`, `ListParams`, `GetOptions`, `PutOptions`, `CapabilitySet`)
  plus ETag and metadata-key normalization helpers.
- `internal/objectmodel/errors.go` — the `Error` type (implements `error`)
  with a fixed code taxonomy and constructor helpers carrying canonical S3
  HTTP status defaults.
- `internal/objectmodel/model_test.go` and
  `internal/objectmodel/errors_test.go` — table-driven stdlib tests.

The package is pure vocabulary: no HTTP, no XML, no I/O, no `package main`
imports. Serialization (headers ↔ Object) is leaf 02's job.

## Context

zeta-object is a single-binary S3-compatible server, `package main` across ~30
root .go files, Go module `zeta-object` (go 1.25.x per go.mod). Object metadata
currently lives in `ObjectMetadata` (types.go:48) with fields
`ContentType, ContentLength, ETag, CustomMetadata, LastModified, StoragePath`.
`StoragePath` is filesystem-internal and must NOT leak into the neutral model.
The gateway initiative will add alternate backends/frontends that all need
this shared vocabulary.

Key existing code to understand (read-only; do not modify in this leaf):

- `types.go` (line ~48) — `ObjectMetadata` struct: the legacy shape the
  neutral `Object` replaces. Note `CustomMetadata` values are currently
  stored WITH their full `x-amz-meta-` header name as key
  (object_handlers.go:139 stores `headerName` directly); the neutral model
  stores lower-case keys with the prefix stripped. The conversion bridging
  this is leaf 02's job — this leaf only owns the key-normalization helper.
- `object_handlers.go:306` — `evaluatePreconditions`: the existing
  HTTP-coupled conditional logic. The neutral `GetOptions`/`PutOptions`
  condition fields mirror its inputs. Do NOT refactor it in this leaf.
- `xml.go` — `errorToXML`/`writeS3Error`: existing S3 error serialization.
  The `Error` type here is what a later tree maps from; keep mapping OUT of
  this package.
- Versioning is not implemented (501 on versionId); `CapabilitySet.Versioning`
  exists so backends can declare support.

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/objectmodel/model.go
package objectmodel

// PINNED — field names, types, and order must match exactly.

type Object struct {
	Key          string
	Size         int64
	ETag         string            // opaque strong validator, S3 quoted form
	LastModified time.Time
	ContentType  string
	Metadata     map[string]string // user metadata; lower-case keys, NO x-amz-meta- prefix
}

type BucketInfo struct {
	Name      string
	CreatedAt time.Time
}

type ListPage struct {
	Objects        []Object
	CommonPrefixes []string
	IsTruncated    bool
	NextToken      string
}

type ListParams struct {
	Prefix            string
	Delimiter         string
	StartAfter        string
	ContinuationToken string
	MaxKeys           int
}

type GetOptions struct {
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   time.Time
	IfUnmodifiedSince time.Time
	Range             string
}

type PutOptions struct {
	ContentType string
	Metadata    map[string]string
	IfMatch     string
	IfNoneMatch string
}

type CapabilitySet struct {
	Multipart         bool
	Versioning        bool
	Immutable         bool
	MetadataProviders []string
}

// ETag helpers
func NormalizeETag(etag string) string // strips surrounding double quotes -> bare form
func QuotedETag(etag string) string    // returns S3 quoted wire form (adds quotes if absent)
func ETagsMatch(a, b string) bool      // quote-insensitive comparison

// Metadata key helpers
func NormalizeMetadataKey(k string) string // lower-case; strips "x-amz-meta-" prefix (case-insensitive)
func MetadataHeaderName(key string) string // "x-amz-meta-" + key
```

```go
// File: internal/objectmodel/errors.go
package objectmodel

type Error struct {
	Code       string // e.g. "NoSuchKey", "NoSuchBucket", "PreconditionFailed"
	Message    string
	HTTPStatus int // canonical S3 default; frontends may override per-protocol
}

func (e *Error) Error() string

func NewError(code, message string, status int) *Error

// Constructors with canonical S3 status defaults:
func ErrNoSuchKey(key string) *Error           // 404
func ErrNoSuchBucket(bucket string) *Error     // 404
func ErrPreconditionFailed() *Error            // 412
func ErrNotModified() *Error                   // 304
func ErrBucketAlreadyExists(bucket string) *Error // 409
func ErrNotImplemented(op string) *Error       // 501
func ErrInvalidArgument(msg string) *Error     // 400
func ErrAccessDenied() *Error                  // 403
func ErrInternalError(msg string) *Error       // 500

// Code constants (exact S3 code strings):
const (
	CodeNoSuchKey           = "NoSuchKey"
	CodeNoSuchBucket        = "NoSuchBucket"
	CodePreconditionFailed  = "PreconditionFailed"
	CodeNotModified         = "NotModified"
	CodeBucketAlreadyExists = "BucketAlreadyExists"
	CodeNotImplemented      = "NotImplemented"
	CodeInvalidArgument     = "InvalidArgument"
	CodeAccessDenied        = "AccessDenied"
	CodeInternalError       = "InternalError"
)
```

### What This Leaf Consumes

Nothing — this is the bottom of the dependency graph. Stdlib only
(`time`, `strings`; `fmt` optional for error messages).

## Tasks

### Task 1: Package scaffolding + pinned structs

**Objective:** Create `internal/objectmodel/model.go` with the pinned types
exactly as specified above.

**Files:**
- Create: `internal/objectmodel/model.go`
- Test: `internal/objectmodel/model_test.go`

**Step 1: Write failing test**

```go
package objectmodel

import (
	"testing"
	"time"
)

func TestPinnedStructShapes(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"Object", Object{Key: "a", Size: 1, ETag: `"d41d8..."`, LastModified: now,
			ContentType: "text/plain", Metadata: map[string]string{"color": "red"}},
			Object{Key: "a", Size: 1, ETag: `"d41d8..."`, LastModified: now,
				ContentType: "text/plain", Metadata: map[string]string{"color": "red"}}},
		{"BucketInfo", BucketInfo{Name: "b", CreatedAt: now}, BucketInfo{Name: "b", CreatedAt: now}},
		{"ListPage", ListPage{Objects: []Object{{Key: "a"}}, CommonPrefixes: []string{"p/"},
			IsTruncated: true, NextToken: "tok"},
			ListPage{Objects: []Object{{Key: "a"}}, CommonPrefixes: []string{"p/"},
				IsTruncated: true, NextToken: "tok"}},
		{"ListParams", ListParams{Prefix: "p/", Delimiter: "/", StartAfter: "p/x",
			ContinuationToken: "t", MaxKeys: 10},
			ListParams{Prefix: "p/", Delimiter: "/", StartAfter: "p/x",
				ContinuationToken: "t", MaxKeys: 10}},
		{"GetOptions", GetOptions{IfMatch: `"e"`, IfNoneMatch: "*", Range: "bytes=0-1"},
			GetOptions{IfMatch: `"e"`, IfNoneMatch: "*", Range: "bytes=0-1"}},
		{"PutOptions", PutOptions{ContentType: "text/plain", Metadata: map[string]string{"a": "b"},
			IfMatch: `"e"`}, PutOptions{ContentType: "text/plain", Metadata: map[string]string{"a": "b"},
			IfMatch: `"e"`}},
		{"CapabilitySet", CapabilitySet{Multipart: true, MetadataProviders: []string{"zfs"}},
			CapabilitySet{Multipart: true, MetadataProviders: []string{"zfs"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%+v != %+v", tc.got, tc.want)
			}
		})
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run TestPinnedStructShapes -v`
Expected: FAIL — package has no such types (compile error is the failure mode).

**Step 3: Write minimal implementation**

Create `model.go` with the pinned structs verbatim from the Interface
Contracts section, including the pinned comments (`// opaque strong
validator, S3 quoted form`, `// user metadata; lower-case keys, NO
x-amz-meta- prefix`) and a package doc comment:

```go
// Package objectmodel defines the backend-neutral object model shared by all
// zeta-object Backend implementations, Frontend protocol plugins, and
// MetadataProviders. It carries no HTTP, XML, or storage dependencies.
package objectmodel
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run TestPinnedStructShapes -v`
Expected: PASS

### Task 2: ETag normalization helpers

**Objective:** Quote-stripping, quoting, and quote-insensitive comparison of
S3 ETags.

**Files:**
- Modify: `internal/objectmodel/model.go` (add helpers)
- Test: `internal/objectmodel/model_test.go`

**Step 1: Write failing test**

```go
func TestNormalizeETag(t *testing.T) {
	tests := []struct{ in, want string }{
		{`"d41d8cd98f00b204e9800998ecf8427e"`, "d41d8cd98f00b204e9800998ecf8427e"},
		{"d41d8cd98f00b204e9800998ecf8427e", "d41d8cd98f00b204e9800998ecf8427e"},
		{`""`, ""},
		{"", ""},
		{`W/"weak123"`, `W/"weak123"`}, // W/ prefix preserved verbatim; only symmetric surrounding quotes stripped
	}
	for _, tc := range tests {
		if got := NormalizeETag(tc.in); got != tc.want {
			t.Errorf("NormalizeETag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestQuotedETag(t *testing.T) {
	tests := []struct{ in, want string }{
		{"d41d8cd98f00b204e9800998ecf8427e", `"d41d8cd98f00b204e9800998ecf8427e"`},
		{`"d41d8cd98f00b204e9800998ecf8427e"`, `"d41d8cd98f00b204e9800998ecf8427e"`},
		{"", `""`},
	}
	for _, tc := range tests {
		if got := QuotedETag(tc.in); got != tc.want {
			t.Errorf("QuotedETag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestETagsMatch(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{`"abc"`, "abc", true},
		{`"abc"`, `"abc"`, true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"", "", true},
	}
	for _, tc := range tests {
		if got := ETagsMatch(tc.a, tc.b); got != tc.want {
			t.Errorf("ETagsMatch(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run 'TestNormalizeETag|TestQuotedETag|TestETagsMatch' -v`
Expected: FAIL — undefined helpers.

**Step 3: Write minimal implementation**

```go
// NormalizeETag strips a surrounding pair of double quotes, returning the
// bare validator form. Input without quotes is returned unchanged.
func NormalizeETag(etag string) string {
	if len(etag) >= 2 && strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) {
		return etag[1 : len(etag)-1]
	}
	return etag
}

// QuotedETag returns the S3 quoted wire form, adding quotes if absent.
func QuotedETag(etag string) string {
	if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 2 {
		return etag
	}
	return `"` + etag + `"`
}

// ETagsMatch compares two ETags quote-insensitively.
func ETagsMatch(a, b string) bool {
	return NormalizeETag(a) == NormalizeETag(b)
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run 'TestNormalizeETag|TestQuotedETag|TestETagsMatch' -v`
Expected: PASS

### Task 3: Metadata key normalization helpers

**Objective:** Lower-case, prefix-stripped user-metadata keys and the reverse
header-name mapping.

**Files:**
- Modify: `internal/objectmodel/model.go` (add helpers)
- Test: `internal/objectmodel/model_test.go`

**Step 1: Write failing test**

```go
func TestNormalizeMetadataKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"X-Amz-Meta-Color", "color"},
		{"x-amz-meta-color", "color"},
		{"Color", "color"},
		{"color", "color"},
		{"", ""},
		{"x-amz-meta-", ""},
	}
	for _, tc := range tests {
		if got := NormalizeMetadataKey(tc.in); got != tc.want {
			t.Errorf("NormalizeMetadataKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMetadataHeaderName(t *testing.T) {
	if got, want := MetadataHeaderName("Color"), "x-amz-meta-color"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run 'TestNormalizeMetadataKey|TestMetadataHeaderName' -v`
Expected: FAIL — undefined helpers.

**Step 3: Write minimal implementation**

```go
// metaHeaderPrefix is the S3 user-metadata header prefix, lower-case.
const metaHeaderPrefix = "x-amz-meta-"

// NormalizeMetadataKey lower-cases the key and strips a leading
// x-amz-meta- prefix (case-insensitive), yielding the canonical in-memory
// form used by Object.Metadata.
func NormalizeMetadataKey(k string) string {
	lowered := strings.ToLower(k)
	return strings.TrimPrefix(lowered, metaHeaderPrefix)
}

// MetadataHeaderName returns the wire header name for a canonical key.
func MetadataHeaderName(key string) string {
	return metaHeaderPrefix + strings.ToLower(key)
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run 'TestNormalizeMetadataKey|TestMetadataHeaderName' -v`
Expected: PASS

### Task 4: Error type and taxonomy

**Objective:** `Error` implementing `error`, code constants, constructors
with canonical S3 HTTP status defaults.

**Files:**
- Create: `internal/objectmodel/errors.go`
- Test: `internal/objectmodel/errors_test.go`

**Step 1: Write failing test**

```go
package objectmodel

import (
	"errors"
	"io"
	"testing"
)

func TestErrorImplementsError(t *testing.T) {
	var e error = ErrNoSuchKey("photos/cat.jpg")
	if !errors.As(e, new(*Error)) {
		t.Fatal("ErrNoSuchKey must return *Error usable as error")
	}
	if e.Error() == "" {
		t.Fatal("Error() must be non-empty")
	}
	var target *Error
	if !errors.As(io.EOF, &target) {
		// sanity: non-Error errors don't convert; keeps errors.As usage honest
		_ = target
	}
}

func TestErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name   string
		got    *Error
		code   string
		status int
	}{
		{"NoSuchKey", ErrNoSuchKey("k"), CodeNoSuchKey, 404},
		{"NoSuchBucket", ErrNoSuchBucket("b"), CodeNoSuchBucket, 404},
		{"PreconditionFailed", ErrPreconditionFailed(), CodePreconditionFailed, 412},
		{"NotModified", ErrNotModified(), CodeNotModified, 304},
		{"BucketAlreadyExists", ErrBucketAlreadyExists("b"), CodeBucketAlreadyExists, 409},
		{"NotImplemented", ErrNotImplemented("versioning"), CodeNotImplemented, 501},
		{"InvalidArgument", ErrInvalidArgument("bad"), CodeInvalidArgument, 400},
		{"AccessDenied", ErrAccessDenied(), CodeAccessDenied, 403},
		{"InternalError", ErrInternalError("boom"), CodeInternalError, 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.Code != tc.code {
				t.Errorf("Code = %q, want %q", tc.got.Code, tc.code)
			}
			if tc.got.HTTPStatus != tc.status {
				t.Errorf("HTTPStatus = %d, want %d", tc.got.HTTPStatus, tc.status)
			}
			if tc.got.Message == "" {
				t.Error("Message should be populated by constructors")
			}
		})
	}
}

func TestNewErrorCustomStatus(t *testing.T) {
	e := NewError(CodeNoSuchKey, "custom", 499)
	if e.HTTPStatus != 499 || e.Code != CodeNoSuchKey {
		t.Errorf("NewError must honor explicit status: %+v", e)
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run 'TestError|TestNewError' -v`
Expected: FAIL — errors.go does not exist.

**Step 3: Write minimal implementation**

```go
package objectmodel

import "fmt"

// Error is the backend-neutral error taxonomy. Frontends map Code onto their
// protocol's error representation; HTTPStatus carries the canonical S3
// default for the S3 frontend.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Code constants use the exact S3 error-code strings.
const (
	CodeNoSuchKey           = "NoSuchKey"
	CodeNoSuchBucket        = "NoSuchBucket"
	CodePreconditionFailed  = "PreconditionFailed"
	CodeNotModified         = "NotModified"
	CodeBucketAlreadyExists = "BucketAlreadyExists"
	CodeNotImplemented      = "NotImplemented"
	CodeInvalidArgument     = "InvalidArgument"
	CodeAccessDenied        = "AccessDenied"
	CodeInternalError       = "InternalError"
)

// NewError builds a custom error; prefer the Err* constructors for known codes.
func NewError(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, HTTPStatus: status}
}

func ErrNoSuchKey(key string) *Error {
	return NewError(CodeNoSuchKey, fmt.Sprintf("The specified key does not exist: %s", key), 404)
}

func ErrNoSuchBucket(bucket string) *Error {
	return NewError(CodeNoSuchBucket, fmt.Sprintf("The specified bucket does not exist: %s", bucket), 404)
}

func ErrPreconditionFailed() *Error {
	return NewError(CodePreconditionFailed, "At least one of the pre-conditions you specified did not hold", 412)
}

func ErrNotModified() *Error {
	return NewError(CodeNotModified, "Not Modified", 304)
}

func ErrBucketAlreadyExists(bucket string) *Error {
	return NewError(CodeBucketAlreadyExists, fmt.Sprintf("The requested bucket name already exists: %s", bucket), 409)
}

func ErrNotImplemented(op string) *Error {
	return NewError(CodeNotImplemented, fmt.Sprintf("A header or query you provided implies functionality that is not implemented: %s", op), 501)
}

func ErrInvalidArgument(msg string) *Error {
	return NewError(CodeInvalidArgument, msg, 400)
}

func ErrAccessDenied() *Error {
	return NewError(CodeAccessDenied, "Access Denied", 403)
}

func ErrInternalError(msg string) *Error {
	return NewError(CodeInternalError, msg, 500)
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run 'TestError|TestNewError' -v`
Expected: PASS

### Task 5: Full package verification

**Objective:** Whole-package green, formatted, vetted, stdlib-only.

**Files:** none new.

**Steps:**

1. Run: `go test ./internal/objectmodel/ -v -count=1` — Expected: ALL PASS
2. Run: `go vet ./internal/objectmodel/` — Expected: clean
3. Run: `gofmt -l internal/objectmodel/` — Expected: no output
4. Run: `go list -deps ./internal/objectmodel` and confirm every listed
   package is stdlib (no `zeta-object` main dependency) — Expected: stdlib only
5. Report files created, test results, and any deviations.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and tests passing
- [ ] Interface contracts (above) satisfied exactly — pinned struct field
      names, types, and order match; pinned comments preserved verbatim
- [ ] All files at exact specified paths (`internal/objectmodel/model.go`,
      `errors.go`, `model_test.go`, `errors_test.go`)
- [ ] No deviations from spec (or deviations documented below)
- [ ] No scope creep — no serialization/headers work (that is leaf 02's)
- [ ] Package imports stdlib only; no `package main` imports; no testify
- [ ] **Do NOT commit.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Every test in the task is present and passing
- [ ] Interface contracts match exactly (signatures, types, file paths);
      pinned structs byte-match the orchestrator's Contract 1/2
- [ ] Code follows project conventions (naming, error handling, structure)
- [ ] No bugs, no security issues
- [ ] No scope creep beyond specified tasks (especially: no header
      serialization, no ObjectMetadata conversion — those belong to leaf 02)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The neutral package must stay wire-format-agnostic: no `net/http`, no
  `encoding/xml`, no `encoding/json` tags. Leaf 02 owns serialization.
- Existing `ObjectMetadata.CustomMetadata` keys keep the `x-amz-meta-` prefix
  today (object_handlers.go:139 stores the raw header name). The bridging
  conversion is leaf 02's `FromLegacy`/`ToLegacy`; this leaf only supplies
  `NormalizeMetadataKey`.
- `Error` implements `error` via pointer receiver — always return `*Error`
  from constructors so `errors.As` works consistently.
- Do NOT refactor `evaluatePreconditions` (object_handlers.go:306) or touch
  any `package main` file in this leaf. Migration of main onto this package
  belongs to the backend-interface sibling tree.
