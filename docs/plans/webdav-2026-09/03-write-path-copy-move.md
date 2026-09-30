# WebDAV Write Path: PUT / MKCOL / DELETE / COPY / MOVE - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Implement the WebDAV write surface: PUT (create/overwrite + ETag conditionals), MKCOL (bucket/prefix creation with parent-existence rule), DELETE (file and recursive collection), COPY/MOVE (file-level, Overwrite/Depth semantics), and the complete `objectmodel.Error` → HTTP status mapping.
- **Dependencies:** 01-package-skeleton-config.md (dispatch, Config). Strongly benefits from 02 (existence/existence-detection helpers); may be dispatched before 02 ONLY if the existence helper is extracted first — orchestrator decides; default order is 02 → 03. **GATED on auth-2026-09 BasicAuthenticator** (tests use real Basic headers; see master HARD ORDERING).
- **Estimated Context:** 90K
- **Concurrency Group:** C (after 02)

## Goal

A mounted client can write:

1. `PUT /photos/new.txt` with a body ⇒ 201 (created) or 204 (overwrote an
   existing object); bytes land via `backend.Put`.
2. `MKCOL /photos/2024/` ⇒ 201 when parent exists; 409 when it doesn't;
   405 when the target already exists; 403 on the mode-B root collection.
3. `DELETE /photos/a.txt` ⇒ 204; `DELETE /photos/2024/` ⇒ 204 and every
   key under `photos/2024/` is gone (recursive via List + Delete loop);
   missing ⇒ 404.
4. `COPY /photos/a.txt` to `/backup/a.txt` with `Destination:` header ⇒ 201;
   `MOVE` additionally deletes the source. `Overwrite: F` on an existing
   destination ⇒ 412. Collection COPY/MOVE (Depth infinity) ⇒ 403 —
   never a partial fake copy.
5. `objectmodel.Error` codes map deterministically to HTTP statuses (master
   Contract 4) through one error-rendering function with RFC 4918 `<D:error>`
   bodies.

## Context

This leaf replaces leaf-01 stubs for the five write methods. Storage surface:

- `backend.Put(ctx, bucket, key, data io.Reader, size int64, objectmodel.PutOptions)` — `PutOptions.ContentType` from the request `Content-Type` header; `IfMatch`/`IfNoneMatch` feed ETag conditionals
- `backend.Delete(ctx, bucket, key)` — single key
- `backend.List(...)` — enumerate keys under a prefix for recursive DELETE (page until `IsTruncated` false, then delete; delete-then-relist-until-empty loop guards against listing races)
- `backend.Stat(...)` — destination-existence checks for Overwrite semantics

Destination parsing: the `Destination` header is a full URL or absolute
path (`https://host:port/backup/a.txt` or `/backup/a.txt`) — parse with
`url.Parse`, take `.Path`, run the SAME URL→(bucket,key) resolver from leaf
01. A Destination that escapes to another bucket in mode A is legal (it is
a cross-bucket copy — two Backend calls, no backend support needed);
in mode B a Destination that would leave the configured bucket ⇒ 403.

Lock-null semantics (issue wording "lock-null/ETag semantics"): v1 has NO
LOCK (405 per Contract 4), therefore lock-null resources do not exist and
MUST NOT be emulated. The deliverable is the ETag semantics: `PUT` returns
`ETag` header on 201/204; `If-Match`/`If-None-Match: *` preconditions map
to 412 via `objectmodel.ErrPreconditionFailed`; `If-None-Match: *` on an
existing object ⇒ 412 (do NOT create). DAV's `Overwrite` header is the
COPY/MOVE analogue of If-Match.

## Interface Contracts (From Parent)

Master Contract 4 is the authority — the full method/error table. Binding
highlights re-checked by tests below:

| Condition | Status |
|---|---|
| PUT create | 201 + ETag |
| PUT overwrite | 204 + ETag |
| PUT to a collection URL (trailing slash) | 405 |
| PUT `If-Match` mismatch / `If-None-Match: *` on existing | 412 |
| MKCOL success | 201, empty body |
| MKCOL on existing | 405 |
| MKCOL missing parent | 409 |
| MKCOL with non-empty request body | 415 |
| MKCOL root in mode B | 403 |
| DELETE success | 204 |
| DELETE missing | 404 |
| DELETE root collection (either mode) | 403 |
| COPY/MOVE file success | 201 (new dst) / 204 (overwrote) |
| COPY/MOVE missing source | 404 |
| COPY/MOVE missing destination parent | 409 |
| COPY/MOVE `Overwrite: F` + dst exists | 412 |
| COPY/MOVE collection (Depth infinity) | 403 |
| MOVE missing Destination header | 400 |

All non-2xx: RFC 4918 error XML, `Content-Type: application/xml;
charset="utf-8"`, one shared render function:

```go
// internal/frontend/webdav/errors.go
// func writeDavError(w http.ResponseWriter, status int, precondition string) 
// func davStatus(oe *objectmodel.Error) int  // the ONLY place objectmodel codes map to statuses
```

## Tasks

### Task 1: Error mapping + render function

**Objective:** The objectmodel→status map exists and is table-tested.

**Files:**
- Create: `internal/frontend/webdav/errors.go`
- Create: `internal/frontend/webdav/errors_test.go`

**Step 1: Failing tests** — table over every `objectmodel` code used by the
Backend (NoSuchKey, NoSuchBucket, PreconditionFailed, BucketAlreadyExists,
NotImplemented, InvalidArgument, AccessDenied, InternalError) asserting the
master Contract 4 mapping (404/404/412/405/501→405-or-501 per table/400/403/
500) AND that the rendered body is `<D:error>` XML with the right
`Content-Type` and status. Unknown codes ⇒ 500 (never silently mapped).

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 2: PUT

**Files:**
- Create: `internal/frontend/webdav/put.go`
- Create: `internal/frontend/webdav/put_test.go`

**Step 1: Failing tests** (stub backend recording Put calls):

- PUT new object ⇒ 201; stub captured (bucket, key, size, ContentType from
  header); response carries `ETag` header = quoted stub-returned ETag
- PUT over existing ⇒ 204 + ETag
- PUT empty body ⇒ 201, size 0 (zero-byte objects are legal)
- PUT to trailing-slash URL ⇒ 405
- PUT `If-Match: "stale"` on existing object ⇒ 412 (through
  `PutOptions.IfMatch` — assert stub received it)
- PUT `If-None-Match: *` on existing ⇒ 412; on missing ⇒ 201
- PUT to bucket-without-grant is NOT this leaf's concern (403 path is leaf 04)
- Mode B re-rooting: `/dir/f.txt` lands as key `dir/f.txt` in configured bucket
- PUT with `Content-Length` absent + chunked body: size must still be
  passed correctly — buffer-or-count strategy, pin behavior in test
  (recommendation: stream with the request's declared length; reject
  chunked-without-length with 400 rather than buffering unboundedly)

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 3: MKCOL

**Files:**
- Create: `internal/frontend/webdav/mkcol.go` + `mkcol_test.go`

**Step 1: Failing tests:**

- Mode A `MKCOL /newbucket/` ⇒ 201; Backend receives the bucket-creation
  call (use whatever the landed backend-interface surface exposes for
  bucket creation; if Backend has NO create-bucket method, MKCOL of a
  top-level collection in mode A must 403 with a documented reason —
  STOP-and-report rule if you believe the seam is missing the method,
  do not extend Backend)
- Mode A/B `MKCOL /photos/2024/` with parent `/photos/` listing non-empty ⇒ 201
- MKCOL missing parent ⇒ 409 (parent existence via the leaf-02 List-based rule)
- MKCOL on existing collection or file ⇒ 405
- MKCOL root `/` ⇒ 405 (mode A) / 403 (mode B)
- MKCOL with body ⇒ 415
- No marker object written: after successful MKCOL, stub's Put call count == 0
  (collections are virtual — master Notes rule)

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 4: DELETE

**Files:**
- Create: `internal/frontend/webdav/delete.go` + `delete_test.go`

**Step 1: Failing tests:**

- DELETE file ⇒ 204; stub got Delete(bucket, key) exactly once
- DELETE missing ⇒ 404 (Stat + List both miss)
- DELETE collection ⇒ 204 AND stub received Delete for every key under the
  prefix (2-page truncated List stub proves full pagination before deletes)
- DELETE root `/` ⇒ 403 both modes
- DELETE of a bucket (mode A `/photos/` when photos is a BUCKET): 403 —
  bucket deletion is not expressible safely over WebDAV; document in code
- Concurrent-put race during recursive delete: delete loop re-lists until
  the prefix is empty or unchanged; pin the loop terminates (bounded
  retries, then 500 with clear message — never a hang)

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 5: COPY / MOVE

**Files:**
- Create: `internal/frontend/webdav/copymove.go` + `copymove_test.go`

**Step 1: Failing tests:**

- COPY file, dst absent ⇒ 201; Backend received Get + Put (streamed, no full
  buffering for large objects — assert via a stub body >buffer size that Put
  got a Reader, not []byte)
- COPY file, dst exists, `Overwrite: T` (or absent — T is the RFC default)
  ⇒ 204
- COPY `Overwrite: F` + dst exists ⇒ 412
- COPY missing source ⇒ 404; dst parent missing ⇒ 409
- COPY missing/garbage Destination header ⇒ 400
- COPY collection with any Depth ⇒ 403 (never partial)
- MOVE = COPY + Delete(source) after success; source gone ⇒ 204/201 as COPY;
  failed copy ⇒ source untouched (assert stub Delete NOT called on 412 path)
- Mode A cross-bucket COPY `/photos/a.txt` → `/backup/a.txt` ⇒ legal, 201
- Mode B Destination escaping the configured bucket ⇒ 403
- `Depth: 0` on file COPY is a no-op distinction (file always copies whole)

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 6: Full gate

```
go test ./internal/frontend/webdav/ -v
go test ./... -count=1 && make test && make test-race
make vet && make fmt-check && make lint NEW_FROM_REV=<rev>
make e2e   # unchanged cases still green
```

## Self-Verification Checklist

- [ ] All tasks implemented; gates green
- [ ] Contract 4 table fully tested — every row has a test
- [ ] One `davStatus` function owns the objectmodel→status mapping
- [ ] No lock-null emulation; LOCK still 405 from leaf 01
- [ ] PUT ETag headers on 201/204; 412 via PutOptions conditionals
- [ ] MKCOL writes no marker object; parent rule enforced
- [ ] Recursive DELETE fully paginates; bucket DELETE 403; root DELETE 403
- [ ] COPY/MOVE Overwrite semantics; collection 403; mode-B escape 403
- [ ] Streaming (no unbounded buffering) for PUT body and COPY transfer
- [ ] No os. access; stdlib only; gofmt clean; no debug artifacts
- [ ] All files at exact specified paths

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Task implemented; TDD respected
- [ ] Error mapping centralized; unknown objectmodel codes ⇒ 500
- [ ] Destination parsing shares the leaf-01 URL resolver (no second parser)
- [ ] Recursive delete pagination + termination bound proven by tests
- [ ] Mode A and mode B covered in every table
- [ ] No read-path regressions (leaf-02 tests still green)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- davfs2 writes files with `PUT` after `MKCOL` of parents — the 409-vs-
  auto-vivify choice matters: RFC 4918 REQUIRES the 409 (no auto-vivify),
  and Finder relies on clients issuing MKCOL first. Do not "helpfully"
  auto-create parents.
- `MOVE` failure atomicity: only delete the source after the destination Put
  returns success. A crash between them leaves a duplicate, which is the
  acceptable failure mode (documented, not emulated away).
- Content-Type on PUT: clients often send `application/x-www-form-urlencoded`
  or empty for unknown types — store what was sent (empty ⇒ backend default),
  never guess by extension. PROPFIND later reports the stored value
  (`application/octet-stream` when empty) — that asymmetry is per Contract 5.
