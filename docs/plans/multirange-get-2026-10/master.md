# Multi-Range GET - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 3 leaf documents under this node
- **Scope:** RFC 9110 multi-span Range support on the S3 GET path:
  parse multi-span headers, serve `multipart/byteranges` (streamed),
  coalesce overlapping/adjacent spans, cap part count. Closes
  zeta-object issue #9.

## Goal

Today only a single Range span works; multi-span requests are rejected.
This tree implements compliant multi-range responses and removes the
README Known Limitations entry.

## Architecture

Three layers, one leaf each:

1. **Parsing + coalescing** (pure functions, no HTTP): parse the Range
   header per RFC 9110 into spans, validate against size, normalize
   (suffix/last-N forms), coalesce overlapping/adjacent spans, cap at
   100 parts (over cap -> full body). Pure and exhaustively testable.
2. **Multipart framing + streaming** (HTTP writer): emit
   `multipart/byteranges` with a generated boundary; read each span
   from the storage layer in ascending offset order and stream it; never
   buffer whole objects. Reuse the existing single-range read primitive
   per span.
3. **Dispatch + docs**: route multi-span (vs single-span) in the GET
   handler, README limitation removal, e2e case.

Interface storage needs: an existing "read [off, off+len) of key"
primitive (the single-range path already uses one - find it; if it is
inline in the handler, extract it in leaf 02, do NOT duplicate).

## Interface Contracts

### Contract 1: Span parsing + coalescing (FROZEN)

```go
// File: internal/frontend/s3/rangespan.go
package s3

// Span is one half-open byte range [Start, End) after normalization
// against the object size.
type Span struct{ Start, End int64 }

// ParseMultiRange parses a Range header per RFC 9110 14.1.1/14.1.2:
// comma-separated byte-range-specs, "bytes=-N" suffix form, tolerant of
// whitespace. Returns nil, false when the header is absent/malformed
// (caller falls back to single-range or full-body paths as today).
// Malformed = no valid spec at all; individually-invalid specs are
// DROPPED per spec (an unsatisfiable span is dropped, not fatal).
func ParseMultiRange(header string, size int64) ([]Span, bool)

// CoalesceRanges sorts spans ascending, merges overlapping/adjacent
// (next.Start <= cur.End), and enforces maxParts: when the merged count
// exceeds maxParts, return nil, false (caller serves the full body).
// Empty input -> empty slice.
func CoalesceRanges(spans []Span, maxParts int) ([]Span, bool)

// MultiRangePartsMax is the part cap (100).
const MultiRangePartsMax = 100
```

Owner: `01-range-parse.md`. Consumers: `02-multipart-stream.md`.

### Contract 2: Multipart response (FROZEN)

```go
// File: internal/frontend/s3/rangemulti.go
package s3

// WriteMultipartByteranges streams a 206 multipart/byteranges response.
// fetch(off, end) must return an io.ReadCloser for the byte span
// [off, end) of the object (caller closes). Boundary is generated
// (crypto/rand hex). Content-Type of the object rides in each part
// when contentType != "". Headers already written by the caller:
// none - this function writes everything (status, Content-Type with
// boundary). Never materializes the whole object.
func WriteMultipartByteranges(w http.ResponseWriter, key string, size int64,
    contentType string, spans []Span,
    fetch func(off, end int64) (io.ReadCloser, error)) error
```

Owner: `02-multipart-stream.md`. Consumers: `03-dispatch-e2e.md`.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-range-parse.md | leaf | none | ~30K | A |
| 02 | 02-multipart-stream.md | leaf | 01 | ~40K | B (after 01) |
| 03 | 03-dispatch-e2e.md | leaf | 02 | ~35K | C (after 02) |

## Dispatch Protocol

Same protocol as the region tree: sequential dispatch, leaf text +
repo context + no-commit instruction, in-session review, commit after
review, tracking table updated.

## Coding Conventions

- Go, stdlib-first; no new dependencies.
- Errors prefix `s3: `.
- Streaming discipline: no whole-object buffering anywhere on this path.
- User-facing prose: hyphens not em-dashes.

## Completion Tracking Table

| Leaf | State | Review | Commit | Notes |
|------|-------|--------|--------|-------|
| 01-range-parse.md | pending | - | - | |
| 02-multipart-stream.md | pending | - | - | |
| 03-dispatch-e2e.md | pending | - | - | |

## Integration Test Plan

1. make test / lint NEW_FROM_REV green.
2. make e2e green including the new multi-range case.
3. Manual spot-check (curl with a 3-span Range against a dev server)
   optional; e2e covers it.

## Review Checklist

- [ ] Contracts verbatim.
- [ ] No whole-object buffering (review the streaming path).
- [ ] Single-range behavior unchanged (existing tests still pass).
- [ ] README limitation removed.

## Open Questions

None - design fixed in zeta-object#9.
