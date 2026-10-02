# Dispatch Wiring + E2E - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Route multi-span Range in the GET handler to the multipart
  writer; e2e case; README limitation removal.
- **Dependencies:** 01, 02.
- **Estimated Context:** ~35K (explore 12K + generate 10K + iterate 8K + overhead 5K)
- **Concurrency Group:** C

## Goal

GET with a multi-span Range header serves 206 multipart/byteranges;
single-span and absent headers behave EXACTLY as today; over-cap spans
serve 200 full body. AGENTS.md e2e rule satisfied with a new case.

## Context

- object_handlers.go GET path: find where the single Range header is
  parsed today (grep `Range` / `Content-Range`); the insertion point is
  immediately before that logic. Order of precedence:
  1. ParseMultiRange(header, size) -> ok and len(spans) > 1 -> multipart
     path (fetch closure wraps the same read primitive the single-range
     path uses - read how it obtains bytes first).
  2. ok and len(spans) == 1 -> existing single-range path UNCHANGED.
  3. !ok -> existing behavior (full body / 416) UNCHANGED.
  4. CoalesceRanges(spans, MultiRangePartsMax) -> nil -> 200 full body.
- fetch closure: wrap the backend read for [off, end). If the existing
  path reads via backend.Get + section reader, mirror that; if it has a
  dedicated ranged-read helper, call it. Report which.
- README Known Limitations (~line 693): delete the multi-range line.
- E2E: new numbered case (highest + 1). The e2e harness serves over
  HTTPS with SigV4; find how existing cases issue ranged GETs (case 17
  or the probe client in zfs-validate is a template). Simpler: many e2e
  cases use aws CLI or curl with --aws-sigv4 - check case sources first
  and reuse the established pattern.

## Tasks

### 1. TDD: dispatch unit tests

Handler-level tests (mirror existing object-handler test setup):
- PUT a 4KB object; GET with `Range: bytes=0-99,200-299` -> 206,
  two parts, correct bytes and Content-Range headers.
- Single span `bytes=0-99` -> 206 with Content-Range (existing shape,
  NOT multipart).
- 101 disjoint spans -> 200 full body (cap).
- Malformed `Range: bytes=abc` -> existing behavior.
- Existing ranged-read tests still pass (no behavior change).

### 2. E2E case

`<n+1>-multirange.sh` (highest number + 1): PUT a multi-KB object via
the established client; GET with a 3-span Range header; assert 206,
boundary Content-Type, and (grep-able) all three spans' Content-Range
headers present. Cleanup pairing per convention.

### 3. README

Remove the "Multi-range GET is unsupported" Known Limitations line.

### 4. Final gate

`make test && make lint NEW_FROM_REV=HEAD && make e2e` (report
pre-existing unrelated failures as such - webdav/owncloud cases documented
in docs/validation-zmetad-2026-10-01.md).

## Self-Verification Checklist

- [ ] Handler unit tests green; single-range unchanged.
- [ ] e2e case green in the suite.
- [ ] README limitation removed.

## Review Checklist (for review agent)

- [ ] Precedence order implemented exactly (multi -> single -> fallback).
- [ ] fetch closure uses the storage read primitive, no duplication.
- [ ] e2e conventions (BKT, cleanup) followed.
- [ ] No regression in existing Range tests (run them).

## Do NOT commit

The orchestrator stages and commits after review.
