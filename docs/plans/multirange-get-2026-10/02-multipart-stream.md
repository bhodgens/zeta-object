# Multipart Byteranges Streaming - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** `multipart/byteranges` response writer: boundary framing,
  per-span streaming from the storage read primitive, header hygiene.
- **Dependencies:** 01-range-parse.md (Span type).
- **Estimated Context:** ~40K (explore 12K + generate 14K + iterate 8K + overhead 6K)
- **Concurrency Group:** B

## Goal

Create `internal/frontend/s3/rangemulti.go` with WriteMultipartByteranges
(Contract 2). The function is storage-agnostic: it takes a fetch callback
so tests can drive it with in-memory bytes and the dispatch leaf wires it
to the real storage read.

## Context

- Find the existing single-range serve path in object_handlers.go: the
  read primitive it uses (likely a backend Get with an offset/limit or
  an io.Reader seek pattern) and how it sets 206/Content-Range. Extract
  NOTHING in this leaf - leaf 03 does the dispatch wiring. Your fetch
  contract: fetch(off, end) -> io.ReadCloser for [off, end).
- RFC 9110 / RFC 2046 multipart/byteranges: parts separated by
  `--boundary\r\n` + part headers (`Content-Range: bytes <start>-<end-1>/<size>`,
  optional `Content-Type`) + `\r\n` + bytes; final `--boundary--\r\n`.
- Read primitive discovery matters: if the current single-range path
  reads via a small helper, fetch can wrap it directly; if reading is
  inline in the handler, fetch should call the storage layer directly
  (backend.Get then io.NewSectionReader-style) - keep it allocation-light
  and streaming. Report what you found.

## Interface Contracts (From Parent)

Contract 2 verbatim: WriteMultipartByteranges(w, key, size, contentType,
spans, fetch) error. Boundary: crypto/rand 16 bytes hex. On fetch error
mid-stream: the response is already committed - return the error after
best-effort abort (close the connection via http.NewResponseController
SetWriteDeadline/FlushError if available, else just return); document
this limitation in the function comment.

## Tasks

### 1. TDD: framing with a fake fetch

`rangemulti_test.go` with an in-memory fetch (bytes.NewReader over a
1KB pattern):
1. Two spans -> 206; Content-Type header
   `multipart/byteranges; boundary=<hex>`; parse the body by splitting
   on the boundary from the header: 2 parts, each with correct
   `Content-Range: bytes <s>-<e>/<size>` and byte-identical payload
   slices; terminator `--<boundary>--` present.
2. Boundary is valid CRLF-safe hex (no `-` inside is fine; assert it
   never appears in the payloads - with 32-char hex and arbitrary
   payloads there is a vanishing collision chance; assert format only).
3. Empty spans slice -> caller bug; return an error without writing
   (test asserts error + empty body).
4. contentType "" -> parts carry no Content-Type header; non-empty ->
   every part carries it.
5. Single span in the slice still renders multipart (dispatch
   normalizes single-span to the old path, but the writer must be
   correct for one span anyway).
6. Streaming check: fetch is called lazily in span order (recording
   fetch: assert call order ascending and that part N's fetch happens
   before part N+1's bytes are written - simplest: assert call order).
2. Run (fail), implement, run (pass).

### 2. TDD: fetch error mid-stream

1. fetch succeeds for part 1, errors for part 2 -> WriteMultipartByteranges
   returns the error; test asserts no panic and that part 1 bytes were
   flushed.
2. Run (fail if the implementation fails early), implement, pass.

### 3. Gate

`make test && make lint NEW_FROM_REV=HEAD` green.

## Interface Contract (Exposed to Siblings)

Contract 2 exactly. Boundary generation unexported.

## Self-Verification Checklist

- [ ] All framing subtests green; make test/lint green.
- [ ] No whole-object buffering (fetch-per-span only).
- [ ] Mid-stream fetch error handled (documented, no panic).

## Review Checklist (for review agent)

- [ ] Contract 2 verbatim.
- [ ] Part headers exactly `Content-Range` (+ optional `Content-Type`);
      no Transfer-Encoding games - Go's chunking handles it.
- [ ] io.Copy (bounded per span) not io.ReadAll.
- [ ] Response cannot be retried after header write (comment states it).

## Do NOT commit

The orchestrator stages and commits after review.
