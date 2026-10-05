# WebDAV Range GET (206) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** Range (206) support on the WebDAV frontend's GET/HEAD for
  FILE resources, reusing the S3 frontend's range machinery.
- **Dependencies:** none
- **Estimated Context:** 60K
- **Concurrency Group:** A

## Goal

A WebDAV GET with a `Range` header behaves exactly like the S3
frontend's Range handling: 206 + `Content-Range` for satisfiable
spans, RFC 9110 classification for malformed and unsatisfiable
headers, multipart/byteranges for multi-span requests. Collections
ignore Range. Conditional-read precedence: If-None-Match is evaluated
BEFORE Range; a 304 carries no body.

## Context

- `internal/frontend/webdav/get.go` is the whole surface you edit.
  `handleGET` (lines 20-55) dispatches by kind; `serveCollectionHeaders`
  answers collections; `serveObject` (lines 60-92) today sets
  Content-Length to the FULL size, writes 200, and `io.Copy`s the
  whole body. It never reads the `Range` header.
- If-None-Match is already evaluated inside `serveObject` (line 65:
  `etagMatchesAny(inm, etag)` -> 304). Keep that position: conditional
  first, then range.
- The S3 frontend owns the range grammar:
  `internal/frontend/s3/rangespan.go` exports `ParseMultiRange(header
  string, size int64) ([]Span, bool)` (line 32),
  `parseByteRangeSpec` (line 79, unexported), and
  `CoalesceRanges(spans []Span, maxParts int) ([]Span, bool)` (line
  131). The RFC 9110 classification (syntactic-malformed -> whole
  header ignored; interpretable-unsatisfiable -> 416) is pinned by
  its unit tables and e2e case 29 (`scripts/e2e/cases/29-*.sh`).
  Find the S3 frontend's serve-side range functions with
  `search_files` (`serveMultiRange`, `Content-Range`) and mirror their
  response shapes - the byte-level wire form must match what S3
  already ships.
- The fs backend `Get` returns a reader over the whole object. The
  frozen `internal/backend/backend.go` Backend interface MUST NOT gain
  methods. Check the `GetOptions`/`Get` signature first
  (`search_files` in `internal/backend/`); if an offset/limit or seek
  option already exists, use it; otherwise stream-discard the prefix
  bytes before copying the span (correct for v1 sizes; report the
  choice and its cost).
- **Known cycles risk:** `internal/frontend/s3` and
  `internal/frontend/webdav` are sibling packages; importing s3 from
  webdav is legal UNLESS s3 imports webdav somewhere (grep first). If
  a cycle exists, move `rangespan.go` plus its test file to a new
  package `internal/rangespan`, re-point the s3 package's imports,
  and move the tests in the SAME leaf - s3 behavior must stay
  byte-identical, proven by its existing unit tables passing
  UNMODIFIED. If you move the package, you MUST re-measure the
  affected coverage floors (AGENTS.md rule: floors move with code) -
  find them in the Makefile and CI workflow comments and set each to
  the measured value rounded down, with a comment saying when to
  ratchet up.

## Interface Contracts (From Parent)

### Contract 1: WebDAV Range semantics (you produce this)

```go
// internal/frontend/webdav/get.go (behavior, not new exported API):
// A GET/HEAD on a FILE resource with a Range header:
//   - single satisfiable span    -> 206, Content-Range: bytes a-b/size,
//                                   body = exactly the span,
//                                   Content-Length = span length
//   - multiple satisfiable specs -> 206 multipart/byteranges with the
//                                   same boundary/part shape S3 serves
//   - syntactic-malformed header -> 200 full body (header ignored)
//   - all-unsatisfiable          -> 416, Content-Range: bytes */size
// A Range on a COLLECTION resource is ignored (200, empty body).
// HEAD mirrors GET status/headers with no body.
// If-None-Match wins over Range: a matching INM answers 304, no body,
// regardless of any valid Range header.
```

## Tasks

### Task 1: the parser seam

**Objective:** webdav uses the proven range parser with no duplicated
grammar code.

**Files:**
- Modify: `internal/frontend/webdav/get.go` (import and call only)
- Possibly move: `internal/frontend/s3/rangespan.go` + its test ->
  `internal/rangespan/` (ONLY if the import cycles)

**Step 1:** Grep for a cycle (`import` of webdav anywhere under
`internal/frontend/s3/`). No cycle: `import "github.com/bhodgens/
zeta-object/internal/frontend/s3"` from webdav and call
`s3.ParseMultiRange` / `s3.CoalesceRanges`. Cycle: do the package
move exactly as Context describes.

**Step 2 (only if moved):** the s3 package's own range tests run
unmodified from their new home (`go test ./internal/rangespan/
./internal/frontend/s3/ -count=1` green) and no s3 file other than
the import lines changed.

### Task 2: single-span and unsatisfiable/malformed handling

**Objective:** the main table, TDD.

**Files:**
- Modify: `internal/frontend/webdav/get.go` (`serveObject` grows the
  range branch before the 200 write; HEAD shares the path)
- Test: `internal/frontend/webdav/get_test.go` (extend the existing
  table)

**Step 1: Write failing tests** (the stub backend test double the
package already uses - find it with search_files - needs to serve
seekable content; if its reader is not seekable, wrap with
`bytes.Reader` in the double):
- `Range: bytes=0-4` on a 10-byte object -> 206,
  `Content-Range: bytes 0-4/10`, body `01234`, Content-Length 5;
- suffix `bytes=-3` -> 206, `bytes 7-9/10`, body `789`;
- open-ended `bytes=8-` -> 206, `bytes 8-9/10`, body `89`;
- malformed `bytes=abc` -> 200 full body, no Content-Range;
- unsatisfiable `bytes=50-` on a 10-byte object -> 416,
  `Content-Range: bytes */10`, empty body;
- multi-span `bytes=0-1,5-6` -> 206 with `Content-Type:
  multipart/byteranges; boundary=...` and two parts each carrying its
  own Content-Range and Content-Type;
- HEAD with a valid Range -> 206, correct Content-Length, NO body;
- If-None-Match matching + valid Range -> 304, no body, no
  Content-Range;
- Range on a collection -> 200, empty body (unchanged behavior).

**Step 2:** `go test ./internal/frontend/webdav/ -run Range -count=1`
-> FAIL.

**Step 3: Implement.** Parse the header with the Task 1 parser. Empty
result + malformed classification -> ignore header (200). All-dropped
unsatisfiable -> 416 shape. Single span -> slice the stream (see the
Get decision in Context) and write the headers before the body write,
matching the existing header order. Multi-span -> mirror the S3
multipart response shape (grep s3 for the boundary writer and follow
it).

**Step 4:** PASS; the whole webdav package suite green.

### Task 3: capability truthfulness

**Objective:** Capabilities() reflects reality.

**Files:**
- Check: `internal/frontend/webdav/frontend.go` Capabilities()
  (`ConditionalReads: true` at line 127 - range rides under this
  flag; verify no separate range flag exists in `internal/frontend/
  caps.go` and add none).

**Step 1:** Read `internal/frontend/caps.go` - if ProtocolCaps has a
Range field, set it; if not, do not widen the struct (the frozen
seam rule extends to capability structs: additive only when a
consumer needs it).

**Step 2:** Any change here gets a one-line unit assert; no change
means no code.

### Task 4: gates

**Objective:** the leaf is green and lint-clean.

- `go test ./internal/frontend/webdav/ ./internal/frontend/s3/ . -count=1`
  green (the s3 and root runs prove no collateral damage);
- `gofmt -l` clean on changed files;
- `make lint NEW_FROM_REV=HEAD` -> 0 findings;
- if rangespan moved: floors re-measured with the measured values and
  comments, per AGENTS.md.

## Self-Verification Checklist

- [ ] All tasks implemented; package + s3 + root suites green
- [ ] Backend interface untouched (`git diff --stat` does not list
      `internal/backend/backend.go`)
- [ ] No range grammar re-implemented in webdav (grep: no new
      byte-spec parsing in the webdav package - it delegates)
- [ ] If-None-Match precedence pinned by a test
- [ ] 416 carries `Content-Range: bytes */size`; malformed answers 200
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH respected: `scripts/zfs-validate/run-zfs-validation.sh`,
      `internal/frontend/s3/reflinkversions.go`,
      `internal/frontend/s3/versioning_handlers.go`, `go.mod`/`go.sum`
      (leaf 02 owns them), `main_server.go`, `frontends.go`,
      `internal/frontend/frontend.go`, the untracked
      `internal/frontend/webdav/dbg_test.go` (never delete or stage it)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 1 satisfied: every status-code row of the semantics
      table has a pinning test with the exact header values
- [ ] The parser is the s3 package's (or the moved rangespan twin);
      no grammar duplication
- [ ] If the package moved: s3 tests unmodified and passing from the
      new location; floors updated; `git diff --stat` on s3 shows
      import-line-only changes
- [ ] Content-Length equals the span length, not the object size
- [ ] HEAD with Range sends no body

Output: APPROVED or specific gaps with file:line.

## Notes

- e2e case 29 (`scripts/e2e/cases/29-*.sh`) pins the S3 side. This
  leaf must not need to touch it; if it seems you do, stop and report.
- The orchestrator's leaf 03 adds the wire-level e2e case (37) that
  exercises Range over both TCP and h3 transports. This leaf ships
  unit coverage only; the wire case is not yours.
- Stream-discard for the prefix is acceptable v1 ONLY because the fs
  backend is local disk; if you find GetOptions already supports
  ranges, prefer it and say so in the report.
