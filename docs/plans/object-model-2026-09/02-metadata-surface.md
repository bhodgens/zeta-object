# Metadata Surface & Serialization - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Canonical S3 metadata surface as code: header serialization helpers (headers ↔ Object), legacy ObjectMetadata conversion, and parity-test helpers reusable by the metadata-zfs and backend-interface trees.
- **Dependencies:** 01-canonical-types.md — this leaf's code imports `objectmodel.Object`, the ETag helpers, and the metadata-key helpers from leaf 01's `model.go`. Must be dispatched only after leaf 01 reaches REVIEWED.
- **Estimated Context:** 45K (exploration + generation + iteration + overhead)
- **Concurrency Group:** B
- **Do NOT commit.**

## Goal

Make the canonical S3 metadata surface executable and testable. This leaf
delivers, in `internal/objectmodel/`:

- `metadata.go` — serialization between HTTP headers and `Object`
  (`ObjectFromHeaders`, `ObjectToMetadataHeaders`), and conversion between
  `package main`'s legacy `ObjectMetadata` (types.go:48) and the neutral
  `Object` (`FromLegacy`/`ToLegacy`).
- `parity.go` — a small, importable test helper package surface
  (`HeaderSnapshot`, `SnapshotHeaders`, `AssertHeaderParity`) that sibling
  trees use to assert FS-backed vs alternate-backend responses are
  byte-identical at the header level.
- `metadata_test.go` — table-driven stdlib tests including round-trip and
  legacy-conversion tables.

The leaf encodes the canonical surface as the single place where the
`x-amz-meta-` prefix rule is applied on the wire, so no backend or provider
ever reimplements it.

## Context

mini-s3 is a single-binary Go S3 server (`package main`, ~30 root .go files,
module `mini-s3`, go 1.25.x). Today, header handling is inline in handlers:
object_handlers.go:139 loops over request headers collecting
`x-amz-meta-*` into `ObjectMetadata.CustomMetadata` — keeping the FULL
header name (prefix included) as the map key. The neutral model instead
stores lower-case keys with the prefix stripped, and this leaf owns both
directions of that bridge.

Key existing code to understand (read-only; do not modify in this leaf):

- `types.go` (~line 48) — `ObjectMetadata{ContentType string;
  ContentLength int64; ETag string; CustomMetadata map[string]string;
  LastModified time.Time; StoragePath string}`. The legacy struct this leaf
  converts from/to. `StoragePath` is filesystem-internal and NEVER surfaces
  in the neutral model.
- `object_handlers.go:139` — the current inline x-amz-meta collection loop
  (prefix kept in keys, joined values with `", "`). The parse helper here
  must accept both prefixed and unprefixed keys and lower-case them; value
  joining is the caller's concern for multi-value headers.
- `object_handlers.go:306` — `evaluatePreconditions` (If-Match,
  If-None-Match, If-Modified-Since, If-Unmodified-Since): shows the exact
  conditional semantics the neutral surface must keep representable. Not
  modified by this leaf.
- Leaf 01 output: `internal/objectmodel/model.go` and `errors.go` — the
  types and helpers this leaf builds on. Read them with terminal cat.

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/objectmodel/metadata.go
package objectmodel

// ObjectFromHeaders builds an Object's metadata-bearing fields from request
// headers. get is a header lookup (e.g. r.Header.Get); keys enumerates the
// raw header names to consider for user metadata (e.g. from a range over
// r.Header). User-metadata keys are normalized: lower-case, x-amz-meta-
// prefix stripped. Size and LastModified are caller-supplied (body length
// and server clock are not header concerns).
func ObjectFromHeaders(key string, size int64, lastModified time.Time,
	get func(string) string, rawMetaKeys []string) Object

// ObjectToMetadataHeaders renders the canonical S3 metadata surface for an
// object onto response headers: Content-Type, Content-Length, ETag (quoted
// form), Last-Modified (http.TimeFormat), and x-amz-meta-* for each
// Metadata entry (prefix added, key lower-cased).
func ObjectToMetadataHeaders(o Object) map[string][]string

// ParseMetadataHeaders normalizes raw header names into canonical
// user-metadata keys. Header values are joined with ", " when a header
// carries multiple values (values argument is parallel to keys).
func ParseMetadataHeaders(rawKeys, rawValues []string) map[string]string

// Legacy conversion — bridges package main's ObjectMetadata (types.go:48)
// onto the neutral model. The legacy shape is mirrored here (no import of
// package main, which would be a cycle). StoragePath is filesystem-internal
// and NEVER surfaced: FromLegacy drops it; ToLegacy leaves it zero — the
// caller (backend adapter) sets it.

type LegacyObjectMetadata struct {
	ContentType    string
	ContentLength  int64
	ETag           string
	CustomMetadata map[string]string // keys may carry x-amz-meta- prefix (legacy on-disk form)
	LastModified   time.Time
}

func FromLegacy(key string, m LegacyObjectMetadata) Object
func ToLegacy(o Object) LegacyObjectMetadata
```

```go
// File: internal/objectmodel/parity.go
package objectmodel

// HeaderSnapshot is a protocol-neutral capture of the canonical metadata
// surface of one response, comparable with == after normalization.
type HeaderSnapshot struct {
	ContentType   string
	ContentLength string
	ETag          string
	LastModified  string
	UserMetadata  map[string]string // canonical keys (lower-case, no prefix)
}

// SnapshotHeaders builds a HeaderSnapshot from a header lookup. get is a
// case-insensitive header getter (e.g. r.Header.Get or
// http.Header.Get on a recorded response).
func SnapshotHeaders(get func(string) string) HeaderSnapshot

// AssertHeaderParity reports whether two snapshots carry identical canonical
// metadata. Returns a human-readable diff description; empty string = parity.
// Used by sibling trees as the drift gate: same requests against FS-backed
// vs ZFS-backed buckets must produce equal snapshots (headers) and equal
// bodies (compared by the caller).
func AssertHeaderParity(a, b HeaderSnapshot) string
```

### What This Leaf Consumes

```go
// From 01-canonical-types.md (already implemented):
type Object struct { Key string; Size int64; ETag string; LastModified time.Time;
	ContentType string; Metadata map[string]string }
func NormalizeETag(etag string) string
func QuotedETag(etag string) string
func NormalizeMetadataKey(k string) string
func MetadataHeaderName(key string) string
const CodeInvalidArgument = "InvalidArgument"
func ErrInvalidArgument(msg string) *Error
```

## Tasks

### Task 1: ParseMetadataHeaders — header names → canonical user metadata

**Objective:** Normalize raw x-amz-meta header names to canonical keys,
preserving the legacy value semantics.

**Files:**
- Create: `internal/objectmodel/metadata.go`
- Test: `internal/objectmodel/metadata_test.go`

**Step 1: Write failing test**

```go
package objectmodel

import "testing"

func TestParseMetadataHeaders(t *testing.T) {
	tests := []struct {
		name   string
		keys   []string
		values []string
		want   map[string]string
	}{
		{
			name:   "prefixed mixed case",
			keys:   []string{"X-Amz-Meta-Color", "x-amz-meta-Owner"},
			values: []string{"red", "alice"},
			want:   map[string]string{"color": "red", "owner": "alice"},
		},
		{
			name:   "unprefixed passthrough lowercased",
			keys:   []string{"Color"},
			values: []string{"blue"},
			want:   map[string]string{"color": "blue"},
		},
		{
			name:   "multi-value joined",
			keys:   []string{"x-amz-meta-tags"},
			values: []string{"a", "b"},
			want:   map[string]string{"tags": "a, b"},
		},
		{
			name:   "empty input",
			keys:   nil,
			values: nil,
			want:   map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMetadataHeaders(tc.keys, tc.values)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run TestParseMetadataHeaders -v`
Expected: FAIL — metadata.go does not exist.

**Step 3: Write minimal implementation**

```go
package objectmodel

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseMetadataHeaders normalizes raw header names into canonical
// user-metadata keys (lower-case, x-amz-meta- prefix stripped). Parallel
// slices: rawValues[i] is the value(s) for rawKeys[i]; multiple values are
// joined with ", " to match existing handler behavior.
func ParseMetadataHeaders(rawKeys, rawValues []string) map[string]string {
	meta := make(map[string]string)
	for i, rawKey := range rawKeys {
		key := NormalizeMetadataKey(rawKey)
		if key == "" {
			continue
		}
		vals := strings.Split(rawValues[i], ", ")
		if len(vals) > 1 {
			meta[key] = strings.Join(vals, ", ")
		} else {
			meta[key] = rawValues[i]
		}
	}
	return meta
}
```

(Note: the join branch is deliberately explicit even though it round-trips
the split; it documents the multi-value contract and keeps a single join
point. `net/http`, `strconv`, `time` imports land with Tasks 2-3.)

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run TestParseMetadataHeaders -v`
Expected: PASS

### Task 2: ObjectFromHeaders / ObjectToMetadataHeaders — the canonical surface

**Objective:** Encode the canonical S3 metadata surface in both directions.

**Files:**
- Modify: `internal/objectmodel/metadata.go`
- Test: `internal/objectmodel/metadata_test.go`

**Step 1: Write failing test**

```go
func TestObjectFromHeaders(t *testing.T) {
	get := func(k string) string {
		m := map[string]string{"Content-Type": "text/plain", "x-amz-meta-color": "red"}
		return m[k]
	}
	o := ObjectFromHeaders("a.txt", 5, time.Unix(1700000000, 0).UTC(),
		get, []string{"X-Amz-Meta-Color"})
	if o.Key != "a.txt" || o.Size != 5 || o.ContentType != "text/plain" {
		t.Errorf("scalar fields wrong: %+v", o)
	}
	if o.Metadata["color"] != "red" {
		t.Errorf("user metadata wrong: %v", o.Metadata)
	}
	if _, has := o.Metadata["x-amz-meta-color"]; has {
		t.Error("prefix must be stripped in canonical keys")
	}
}

func TestObjectToMetadataHeaders(t *testing.T) {
	o := Object{
		Key: "a.txt", Size: 5,
		ETag:         "d41d8cd98f00b204e9800998ecf8427e",
		LastModified: time.Unix(1700000000, 0).UTC(),
		ContentType:  "text/plain",
		Metadata:     map[string]string{"Color": "red"},
	}
	h := ObjectToMetadataHeaders(o)
	if h["Content-Type"][0] != "text/plain" {
		t.Errorf("Content-Type = %v", h["Content-Type"])
	}
	if h["Content-Length"][0] != "5" {
		t.Errorf("Content-Length = %v", h["Content-Length"])
	}
	if h["ETag"][0] != `"d41d8cd98f00b204e9800998ecf8427e"` {
		t.Errorf("ETag must be quoted form, got %v", h["ETag"])
	}
	if h["Last-Modified"][0] != time.Unix(1700000000, 0).UTC().Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %v", h["Last-Modified"])
	}
	if h["x-amz-meta-color"][0] != "red" {
		t.Errorf("x-amz-meta-color = %v", h["x-amz-meta-color"])
	}
}

func TestMetadataSurfaceRoundTrip(t *testing.T) {
	want := map[string]string{"color": "red", "owner": "alice"}
	o := Object{Key: "k", Metadata: want}
	h := ObjectToMetadataHeaders(o)
	var keys, values []string
	for k, v := range h {
		keys, values = append(keys, k), append(values, v[0])
	}
	got := ParseMetadataHeaders(keys, values)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("round-trip %q = %q, want %q", k, got[k], v)
		}
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run 'TestObjectFromHeaders|TestObjectToMetadataHeaders|TestMetadataSurfaceRoundTrip' -v`
Expected: FAIL — functions undefined.

**Step 3: Write minimal implementation**

```go
// ObjectFromHeaders builds an Object's metadata-bearing fields from request
// headers. get is a header lookup (e.g. r.Header.Get); rawMetaKeys enumerates
// raw header names carrying user metadata (e.g. collected by ranging over
// r.Header). User-metadata keys are normalized: lower-case, x-amz-meta-
// prefix stripped.
func ObjectFromHeaders(key string, size int64, lastModified time.Time,
	get func(string) string, rawMetaKeys []string) Object {
	rawValues := make([]string, len(rawMetaKeys))
	for i, k := range rawMetaKeys {
		rawValues[i] = get(k)
	}
	return Object{
		Key:          key,
		Size:         size,
		LastModified: lastModified,
		ContentType:  get("Content-Type"),
		Metadata:     ParseMetadataHeaders(rawMetaKeys, rawValues),
	}
}

// ObjectToMetadataHeaders renders the canonical S3 metadata surface:
// Content-Type, Content-Length, ETag (quoted), Last-Modified
// (http.TimeFormat), and x-amz-meta-* per Metadata entry.
func ObjectToMetadataHeaders(o Object) map[string][]string {
	h := map[string][]string{
		"Content-Type":   {o.ContentType},
		"Content-Length": {strconv.FormatInt(o.Size, 10)},
		"ETag":           {QuotedETag(o.ETag)},
		"Last-Modified":  {o.LastModified.UTC().Format(http.TimeFormat)},
	}
	for k, v := range o.Metadata {
		h[MetadataHeaderName(k)] = []string{v}
	}
	return h
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run 'TestObjectFromHeaders|TestObjectToMetadataHeaders|TestMetadataSurfaceRoundTrip' -v`
Expected: PASS

### Task 3: Legacy ObjectMetadata conversion

**Objective:** Bridge `types.go` ObjectMetadata (values, not the struct —
no import of package main) onto the neutral model. StoragePath never
surfaces.

**Files:**
- Modify: `internal/objectmodel/metadata.go`
- Test: `internal/objectmodel/metadata_test.go`

**Step 1: Write failing test**

```go
func TestFromLegacy(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	m := LegacyObjectMetadata{
		ContentType:    "application/json",
		ContentLength:  42,
		ETag:           "abc123",
		CustomMetadata: map[string]string{"x-amz-meta-color": "red", "plain": "blue"},
		LastModified:   now,
		StoragePath:    "/data/bucket/.metadata/a.meta", // must be dropped
	}
	o := FromLegacy("a", m)
	if o.Key != "a" || o.Size != 42 || o.ContentType != "application/json" || o.ETag != "abc123" {
		t.Errorf("scalar mapping wrong: %+v", o)
	}
	if !o.LastModified.Equal(now) {
		t.Errorf("LastModified = %v, want %v", o.LastModified, now)
	}
	if o.Metadata["color"] != "red" {
		t.Errorf("prefixed legacy key must be normalized: %v", o.Metadata)
	}
	if o.Metadata["plain"] != "blue" {
		t.Errorf("unprefixed legacy key must be lower-cased and kept: %v", o.Metadata)
	}
}

func TestToLegacyDropsStoragePath(t *testing.T) {
	o := Object{Key: "a", Size: 42, ETag: `"abc123"`, ContentType: "text/plain",
		LastModified: time.Unix(1700000000, 0).UTC(),
		Metadata:     map[string]string{"color": "red"}}
	m := ToLegacy(o)
	if m.ContentLength != 42 || m.ContentType != "text/plain" || m.ETag != `"abc123"` {
		t.Errorf("mapping wrong: %+v", m)
	}
	if m.StoragePath != "" {
		t.Errorf("StoragePath must stay zero from neutral model (backend sets it), got %q", m.StoragePath)
	}
	if m.CustomMetadata["x-amz-meta-color"] != "red" {
		t.Errorf("ToLegacy must re-add prefix (legacy on-disk form): %v", m.CustomMetadata)
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run 'TestFromLegacy|TestToLegacyDropsStoragePath' -v`
Expected: FAIL — LegacyObjectMetadata and converters undefined.

**Step 3: Write minimal implementation**

```go
// LegacyObjectMetadata mirrors package main's ObjectMetadata (types.go:48)
// without importing it (that would create a main-dependency cycle).
type LegacyObjectMetadata struct {
	ContentType    string
	ContentLength  int64
	ETag           string
	CustomMetadata map[string]string // keys may carry the x-amz-meta- prefix (legacy on-disk form)
	LastModified   time.Time
	StoragePath    string // filesystem-internal; NEVER surfaced on the neutral model
}

// FromLegacy converts a legacy metadata value into the neutral model.
// Mapping: contentType↔ContentType, contentLength↔Size, eTag↔ETag,
// customMetadata↔Metadata, lastModified↔LastModified. StoragePath is
// filesystem-internal and is dropped.
func FromLegacy(key string, m LegacyObjectMetadata) Object {
	meta := make(map[string]string, len(m.CustomMetadata))
	for k, v := range m.CustomMetadata {
		meta[NormalizeMetadataKey(k)] = v
	}
	return Object{
		Key:          key,
		Size:         m.ContentLength,
		ETag:         m.ETag,
		LastModified: m.LastModified,
		ContentType:  m.ContentType,
		Metadata:     meta,
	}
}

// ToLegacy reconstructs the legacy value. ETag keeps whatever form the
// Object carries (callers quote for the wire). CustomMetadata keys get the
// x-amz-meta- prefix re-added (legacy on-disk form). StoragePath is left
// zero: the backend adapter sets it.
func ToLegacy(o Object) LegacyObjectMetadata {
	custom := make(map[string]string, len(o.Metadata))
	for k, v := range o.Metadata {
		custom[MetadataHeaderName(k)] = v
	}
	return LegacyObjectMetadata{
		ContentType:    o.ContentType,
		ContentLength:  o.Size,
		ETag:           o.ETag,
		CustomMetadata: custom,
		LastModified:   o.LastModified,
	}
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run 'TestFromLegacy|TestToLegacyDropsStoragePath' -v`
Expected: PASS

### Task 4: Parity test helpers (drift gate for sibling trees)

**Objective:** The reusable FS-vs-alternate-backend header parity check
consumed by metadata-zfs-2026-09 as its parity gate.

**Files:**
- Create: `internal/objectmodel/parity.go`
- Test: `internal/objectmodel/parity_test.go`

**Step 1: Write failing test**

```go
func TestSnapshotHeaders(t *testing.T) {
	get := func(k string) string {
		m := map[string]string{
			"Content-Type":     "text/plain",
			"Content-Length":   "5",
			"ETag":             `"abc"`,
			"Last-Modified":    "Mon, 02 Jan 2006 15:04:05 GMT",
			"x-amz-meta-color": "red",
		}
		return m[k]
	}
	s := SnapshotHeaders(get, []string{"x-amz-meta-color"})
	if s.ContentType != "text/plain" || s.ContentLength != "5" || s.ETag != `"abc"` {
		t.Errorf("snapshot wrong: %+v", s)
	}
	if s.UserMetadata["color"] != "red" {
		t.Errorf("user metadata keys must be canonical: %v", s.UserMetadata)
	}
}

func TestAssertHeaderParity(t *testing.T) {
	a := HeaderSnapshot{ContentType: "text/plain", ContentLength: "5",
		ETag: `"abc"`, LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
		UserMetadata: map[string]string{"color": "red"}}
	identical := a
	if diff := AssertHeaderParity(a, identical); diff != "" {
		t.Errorf("identical snapshots must pass, got diff: %s", diff)
	}
	b := a
	b.ETag = `"different"`
	if diff := AssertHeaderParity(a, b); diff == "" {
		t.Error("differing ETag must be reported")
	}
	c := a
	c.UserMetadata = map[string]string{"color": "blue"}
	if diff := AssertHeaderParity(a, c); diff == "" {
		t.Error("differing user metadata must be reported")
	}
}
```

**Step 2: Run test to verify failure**

Run: `go test ./internal/objectmodel/ -run 'TestSnapshotHeaders|TestAssertHeaderParity' -v`
Expected: FAIL — parity.go does not exist.

**Step 3: Write minimal implementation**

`SnapshotHeaders` must capture user-metadata headers too, but a header
getter cannot enumerate names — so it takes the raw user-metadata header
names as a parameter alongside the getter:

```go
// SnapshotHeaders builds a HeaderSnapshot via a case-insensitive header
// getter. metaKeys enumerates the raw user-metadata header names to capture
// (e.g. collected by ranging over a recorded response's Header map).
func SnapshotHeaders(get func(string) string, metaKeys []string) HeaderSnapshot {
	meta := ParseMetadataHeaders(metaKeys, valuesFor(get, metaKeys))
	return HeaderSnapshot{
		ContentType:   get("Content-Type"),
		ContentLength: get("Content-Length"),
		ETag:          get("ETag"),
		LastModified:  get("Last-Modified"),
		UserMetadata:  meta,
	}
}

func valuesFor(get func(string) string, keys []string) []string {
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = get(k)
	}
	return vals
}

// AssertHeaderParity compares two snapshots field-by-field. Returns an empty
// string on parity, otherwise a "\n"-joined description of every difference.
// This is the drift gate for the metadata-zfs tree: same requests against
// FS-backed vs ZFS-backed buckets must yield equal snapshots.
func AssertHeaderParity(a, b HeaderSnapshot) string {
	var diffs []string
	if a.ContentType != b.ContentType {
		diffs = append(diffs, fmt.Sprintf("Content-Type: %q vs %q", a.ContentType, b.ContentType))
	}
	if a.ContentLength != b.ContentLength {
		diffs = append(diffs, fmt.Sprintf("Content-Length: %q vs %q", a.ContentLength, b.ContentLength))
	}
	if a.ETag != b.ETag {
		diffs = append(diffs, fmt.Sprintf("ETag: %q vs %q", a.ETag, b.ETag))
	}
	if a.LastModified != b.LastModified {
		diffs = append(diffs, fmt.Sprintf("Last-Modified: %q vs %q", a.LastModified, b.LastModified))
	}
	for k, v := range a.UserMetadata {
		if b.UserMetadata[k] != v {
			diffs = append(diffs, fmt.Sprintf("x-amz-meta-%s: %q vs %q", k, v, b.UserMetadata[k]))
		}
	}
	for k, v := range b.UserMetadata {
		if _, ok := a.UserMetadata[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("x-amz-meta-%s: absent vs %q", k, v))
		}
	}
	return strings.Join(diffs, "\n")
}
```

**Step 4: Run test to verify pass**

Run: `go test ./internal/objectmodel/ -run 'TestSnapshotHeaders|TestAssertHeaderParity' -v`
Expected: PASS

### Task 5: Full package verification

**Objective:** Whole-package green with leaf 01's code, formatted, vetted,
stdlib-only, no main imports.

**Files:** none new.

**Steps:**

1. Run: `go test ./internal/objectmodel/ -v -count=1` — Expected: ALL PASS
   (leaf 01 tests still green too)
2. Run: `go vet ./internal/objectmodel/` — Expected: clean
3. Run: `gofmt -l internal/objectmodel/` — Expected: no output
4. Run: `go list -deps ./internal/objectmodel` — Expected: stdlib only
   (`net/http` and `fmt`/`strings` are stdlib and allowed; anything under
   `mini-s3` is a violation)
5. Report files created, test results, deviations.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented and tests passing
- [ ] Interface contracts (above) satisfied exactly — signatures match,
      StoragePath dropped in FromLegacy, zero in ToLegacy output
- [ ] All files at exact specified paths (`metadata.go`, `parity.go`,
      `metadata_test.go`, `parity_test.go` — plus leaf 01's files intact)
- [ ] No deviations from spec (or deviations documented below)
- [ ] No scope creep — no changes to `package main`, no new dependencies,
      no precondition refactor
- [ ] Canonical surface definition from the parent (Contract 4) is honored:
      Content-Type, Content-Length, ETag, Last-Modified, x-amz-meta-* only
- [ ] **Do NOT commit.** The orchestrator handles all git operations after review.

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

The review agent will verify against this leaf document:

- [ ] Every Task above is implemented
- [ ] Every test in the task is present and passing
- [ ] Interface contracts match exactly (signatures, types, file paths)
- [ ] The PARITY RULE holds in code: parity helpers compare exactly the
      canonical surface (Content-Type, Content-Length, ETag, Last-Modified,
      x-amz-meta-*) and nothing else
- [ ] Code follows project conventions (naming, error handling, structure)
- [ ] No bugs, no security issues
- [ ] No scope creep beyond specified tasks

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- **Value-joining convention:** existing handlers join multi-value headers
  with `", "` (object_handlers.go:139 stores `strings.Join(headerValues,
  ", ")`). `ParseMetadataHeaders` preserves that so the FS backend's
  on-disk metadata stays byte-compatible during later migration.
- **Legacy on-disk keys keep the prefix.** `FromLegacy` normalizes prefixed
  keys; `ToLegacy` re-adds them. This asymmetry is intentional: the neutral
  model is prefix-free, the legacy disk format is not. Do not "fix" it.
- **ETag form:** `Object.ETag` is the S3 quoted form per the pinned comment;
  legacy `ObjectMetadata.ETag` is stored bare. The converters pass ETag
  through unchanged — quote/strip explicitly with `QuotedETag`/`NormalizeETag`
  at call sites where the form differs, and note that `FromLegacy` output
  carries bare ETags from legacy data. Sibling trees must not assume one
  form without checking; this is the known sharpest edge in the conversion.
- `SnapshotHeaders` needs the raw meta-key list because `http.Header.Get`
  can't enumerate; callers range over `resp.Header` for
  `strings.HasPrefix(lower(name), "x-amz-meta-")` and pass those names in.
- Bodies are compared by the caller (parity helper is headers-only by
  design; the drift gate in metadata-zfs-2026-09 asserts identical
  headers+body, using this helper for the header half).
- `net/http` is imported only for `http.TimeFormat` — a constant, not a
  handler dependency; the package stays free of request/response types.
