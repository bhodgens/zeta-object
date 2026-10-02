# Range Parsing + Coalescing - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Pure parsing/normalization/coalescing of multi-span Range
  headers (Contract 1). No HTTP, no storage.
- **Dependencies:** none.
- **Estimated Context:** ~30K (explore 6K + generate 12K + iterate 6K + overhead 6K)
- **Concurrency Group:** A

## Goal

Create `internal/frontend/s3/rangespan.go`: ParseMultiRange,
CoalesceRanges, Span, MultiRangePartsMax - pure functions with exhaustive
table tests. This is the correctness core; the HTTP framing leaf builds
on it blindly.

## Context

Package `internal/frontend/s3`. Find the existing single-range handling
(grep `Range` / `Content-Range` in object_handlers.go) to match
established edge-case behavior (416 conditions, suffix forms) - do not
change it; this leaf only ADDS the multi-span parser. RFC 9110 14.1.1:
`Range: bytes=<spec>` with spec `first-last | -suffix | first-`;
multiple specs comma-separated; whitespace tolerated.

Semantics decided (issue #9):
- Header absent or `bytes=` with zero valid specs -> (nil, false).
- Individually-unsatisfiable specs (start >= size) are DROPPED, not fatal.
- Suffix form `-N`: last N bytes; N == 0 -> dropped; N > size -> whole
  object span (0, size).
- Open-ended form `N-`: N to size-1; N >= size -> dropped.
- Normalized spans are half-open [Start, End); End is EXCLUSIVE; clamp
  End to size.
- Coalesce: sort ascending by Start; merge when next.Start <= cur.End
  (overlap OR adjacency); after merge, if count > maxParts ->
  (nil, false) meaning "serve full body".
- Mixed valid+invalid specs: keep the valid ones (drop invalid).

## Interface Contracts (From Parent)

Contract 1 verbatim: Span, ParseMultiRange, CoalesceRanges,
MultiRangePartsMax = 100.

## Tasks

### 1. TDD: ParseMultiRange

Table-driven `rangespan_test.go`. Cover (size=1000 unless noted):
- absent header -> nil, false
- "bytes=0-99" -> [(0,100)]
- "bytes=0-99,200-299,400-499" -> 3 spans in order
- whitespace variants "bytes= 0-99 , 200-299 "
- suffix "-50" -> (950,1000); "-1000" -> (0,1000); "-0" dropped
- open "100-" -> (100,1000); "999-" -> (999,1000); "1000-" dropped
- "1500-1600" dropped (start >= size)
- "bytes=abc" -> nil, false; "bytes=-" -> nil, false
- mixed "1500-1600,0-99,abc,300-399" -> [(0,100),(300,400)]
- other units "items=0-5" -> nil, false (only bytes supported)
- size=0: any spec -> dropped -> nil, false
2. Run (fail), implement, run (pass).

### 2. TDD: CoalesceRanges

- empty -> empty slice
- unordered input -> sorted output
- overlap "0-100","50-150" -> (0,150)
- adjacency "0-100","100-200" -> (0,200)
- gap preserved "0-99","200-299" -> 2 spans
- nested "0-1000","10-20" -> (0,1000)
- over cap: 101 disjoint spans, maxParts=100 -> nil, false
- exactly at cap: 100 disjoint -> kept
2. Run (fail), implement, run (pass).

### 3. Gate

`make test && make lint NEW_FROM_REV=HEAD` green.

## Interface Contract (Exposed to Siblings)

Contract 1 exactly. No other exports.

## Self-Verification Checklist

- [ ] Table tests cover every bullet above (aim 40+ subtests).
- [ ] make test && make lint NEW_FROM_REV=HEAD green.
- [ ] No changes to existing single-range code.

## Review Checklist (for review agent)

- [ ] Contract 1 verbatim.
- [ ] Half-open spans everywhere (check for off-by-one at End).
- [ ] Dropped-vs-fatal distinction per spec (tests prove).
- [ ] No allocation pathology (spans reused, no per-spec maps).

## Do NOT commit

The orchestrator stages and commits after review.
