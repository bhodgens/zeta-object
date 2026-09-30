# WebDAV Read Path: OPTIONS / PROPFIND / GET / HEAD - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Implement the WebDAV read surface: OPTIONS DAV-header response, GET/HEAD object retrieval (ETag/Last-Modified headers, conditional reads), and PROPFIND Depth 0/1 with collection listing via `List` prefix/delimiter and RFC 4918 `207 Multi-Status` XML encoding.
- **Dependencies:** 01-package-skeleton-config.md (dispatch skeleton, Config, mode mapping). External: `backend.Backend`, `objectmodel` (LANDED). **GATED on auth-2026-09 landing its BasicAuthenticator** — this leaf's tests write real `Authorization: Basic` headers (see master HARD ORDERING).
- **Estimated Context:** 90K
- **Concurrency Group:** B (after 01)

## Goal

A client can point Finder/davfs2 at the server and SEE the file tree:

1. `OPTIONS /` returns 200, `DAV: 1`, `Allow:` with the implemented set,
   `MS-Author-Via: DAV`.
2. `GET /photos/a.jpg` streams the object with `Content-Length`,
   `Content-Type`, `ETag` (quoted strong form), `Last-Modified` (RFC 1123);
   `HEAD` returns identical headers, no body. `If-None-Match` matching ⇒ 304.
3. `PROPFIND` with `Depth: 0` returns the resource itself; `Depth: 1`
   returns the resource plus its immediate children (files + one-entry
   collections for common prefixes). Response is a `207 Multi-Status`
   `<D:multistatus>` document with correct `<D:response>` per resource and
   the property mapping from master Contract 5.
4. Missing files ⇒ 404 with `<D:error>` body; `Depth: infinity` ⇒ 403
   `propfind-finite-depth`; malformed request body ⇒ 400.

## Context

This leaf replaces the leaf-01 stubs for OPTIONS/GET/HEAD/PROPFIND. The
storage data comes exclusively from:

- `backend.Get(ctx, bucket, key, objectmodel.GetOptions)` → `(io.ReadCloser, objectmodel.Object, error)` — supports `IfNoneMatch` via GetOptions
- `backend.Stat(ctx, bucket, key)` → `(objectmodel.Object, error)` — for single-resource PROPFIND and HEAD
- `backend.List(ctx, bucket, objectmodel.ListParams)` → `(objectmodel.ListPage, error)` — collection children: `Prefix` = collection prefix, `Delimiter="/"`; `ListPage.CommonPrefixes` are child collections, `ListPage.Objects` are child files
- `backend.Buckets(ctx)` — mode-A root Depth 1 children

Reference implementations for style: `internal/frontend/s3/object_handlers.go`
(GET/HEAD headers), `internal/frontend/s3/xml.go` (prolog + marshaling
discipline). WebDAV XML uses the `DAV:` namespace
(`xmlns:D="DAV:"`), `xml.Header` prolog, `Content-Type: application/xml;
charset="utf-8"` on 207 bodies.

Property mapping is master Contract 5 — implement it as ONE exported,
side-effect-free function so leaf 06's docs and the future ownCloud tree
can reference it:

```go
// internal/frontend/webdav/props.go
// func objectProps(o objectmodel.Object, isCollection bool) []xml prop structs
```

Collection existence rule (master Contract 3): a prefix `p/` is a collection
iff `List(bucket, {Prefix: p/, Delimiter: "/", MaxKeys: 1})` yields a common
prefix or ≥1 key under it, OR the parent exists and MKCOL just created it
(no marker object — existence is virtual). GET on a collection: 200, empty
body, `Content-Type: httpd/unix-directory` (Finder probes this).

## Interface Contracts (From Parent)

- Master Contracts 3 (mapping), 4 (method table), 5 (property mapping).
- Binding status codes: PROPFIND success 207; GET 200 / 304 / 404; HEAD
  same as GET minus body; OPTIONS 200; Depth-infinity PROPFIND 403.
- All reads flow through ctx-aware Backend calls; honor `r.Context()`.
- Only `internal/backend.Backend` + `internal/objectmodel` — no `os.`.

## Tasks

### Task 1: URL/resource existence resolution + GET/HEAD

**Objective:** GET/HEAD serve objects and collections with correct headers.

**Files:**
- Create: `internal/frontend/webdav/get.go` (handleGET/handleHEAD, shared body logic)
- Create: `internal/frontend/webdav/get_test.go`

**Step 1: Write failing tests** (stub backend; table-driven):

- GET existing object ⇒ 200, `ETag` = `objectmodel.QuotedETag(o.ETag)`,
  `Last-Modified` RFC 1123, `Content-Type` (or `application/octet-stream`
  when empty), `Content-Length` = Size, body bytes match
- HEAD ⇒ same headers, empty body, no `Transfer-Encoding: chunked` surprise
- GET missing key ⇒ 404; Stat error `CodeNoSuchBucket` ⇒ 404
- GET collection (`/photos/` with listed children) ⇒ 200, empty body,
  `Content-Type: httpd/unix-directory`
- GET `/photos` (no slash) that exists only as prefix ⇒ 301/308-style
  redirect to trailing slash OR 404 — pick ONE, document in code comment,
  pin in test (recommendation: 404; clients always use trailing slash from PROPFIND)
- Conditional: `If-None-Match` with current ETag ⇒ 304 (through
  `GetOptions.IfNoneMatch`, proving Backend-driven conditionals)
- Mode B: same object tests with `Config{Bucket:"b"}` and URL paths
  re-rooted (table dimension)

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 2: PROPFIND Depth 0

**Objective:** Single-resource propfind correct.

**Files:**
- Create: `internal/frontend/webdav/propfind.go`
- Create: `internal/frontend/webdav/propfind_test.go`

**Step 1: Failing tests:**

- `PROPFIND /photos/a.txt`, `Depth: 0`, `allprop` body (or NO body — treat
  missing body as allprop per RFC 4918 §9.1) ⇒ 207, exactly one
  `<D:response>`, `<D:href>/photos/a.txt</D:href>`, all five Contract-5
  properties present with correct values (golden XML comparison — inline
  string, `strings.TrimSpace`-normalized)
- PROPFIND on missing resource ⇒ 404
- PROPFIND on collection Depth 0 ⇒ 207 one response, `resourcetype` =
  `<D:collection/>`, `getcontenttype` = `httpd/unix-directory`, NO
  `getcontentlength`/`getetag` elements
- `Depth` header values other than `0`/`1` (e.g. `2`, `infinity`) ⇒ 403 with
  `<D:propfind-finite-depth/>` precondition
- Malformed XML body (e.g. `<broken`) ⇒ 400

**Step 2: FAIL. Step 3: implement (Depth 0 only — Depth 1 is Task 3).**

### Task 3: PROPFIND Depth 1 + collection listing

**Objective:** Directory listings render.

**Files:**
- Modify: `internal/frontend/webdav/propfind.go`
- Modify: `internal/frontend/webdav/propfind_test.go`

**Step 1: Failing tests:**

- Mode A `PROPFIND /` Depth 1 ⇒ parent response + one response per bucket
  (from `Buckets()`), each `resourcetype: collection`, hrefs with trailing slash
- Mode A `PROPFIND /photos/` Depth 1 ⇒ response for `/photos/` + files from
  `List(Prefix:"", Delimiter:"/")` + one response per `CommonPrefixes` entry
- Mode B `PROPFIND /` Depth 1 ⇒ root lists the configured bucket's
  top-level entries (prefix "" under bucket B)
- Nested: `/photos/2024/` Depth 1 lists only `2024/` immediate children —
  assert no grandchildren leak (Delimiter behavior)
- Href encoding: a key with a space (`my file.txt`) renders percent-encoded
  in `<D:href>` (`my%20file.txt`) — pin with test
- List returning `IsTruncated` with MaxKeys unset: request pages until done
  (stub a 2-page backend) — a truncated listing that silently drops entries
  is a correctness bug, not an acceptable degradation

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 4: OPTIONS

**Files:**
- Modify: `internal/frontend/webdav/dispatch.go` (or `options.go`)
- Test in `dispatch_test.go`

Tests: `OPTIONS *` or `OPTIONS /path` ⇒ 200, empty body, headers `DAV: 1`,
`Allow: OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, MKCOL, COPY, MOVE`,
`MS-Author-Via: DAV`, `Content-Length: 0`.

### Task 5: Full gate

```
go test ./internal/frontend/webdav/ -v
go test ./... -count=1 && make test && make test-race
make vet && make fmt-check && make lint NEW_FROM_REV=<rev>
```

Expected: green; note leaf 05 adds the wire-level e2e — this leaf's gate is
unit-level only, but `make e2e` must still pass unchanged (run it).

## Self-Verification Checklist

- [ ] All tasks implemented; gates green
- [ ] PROPFIND golden XML: namespace, prolog, href encoding, propstat blocks exact
- [ ] Contract 5 property mapping via ONE exported function; collections vs files correct
- [ ] Depth 0/1 only; infinity ⇒ 403 propfind-finite-depth; malformed body ⇒ 400
- [ ] GET/HEAD headers exact; conditional 304 via GetOptions; no os. access
- [ ] Mode A and B tables both present
- [ ] Truncated listing fully paged — no dropped entries
- [ ] No debug artifacts, no TODOs, gofmt clean
- [ ] All files at exact specified paths

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Task implemented; TDD respected
- [ ] Contracts 3/4/5 satisfied; status codes exact (207/304/400/403/404/200)
- [ ] Reads only via Backend; ctx honored; no filesystem access
- [ ] Golden-XML tests exist and are strict (not substring-lax on the whole document)
- [ ] Both modes covered
- [ ] No write-path code smuggled in (PUT/MKCOL/etc. remain stubs)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- `xml.Marshal` will not emit `xmlns:D="DAV:"` on children reliably — pin
  the namespace via struct tags on the root element
  (`XMLName xml.Name \`xml:"D multistatus"\`` + `xmlns:D` attr) and
  golden-compare a full 207 body; do not hand-concatenate strings.
- davfs2 sends `PROPFIND` with a `<D:prop>` naming specific properties; your
  parser must handle `allprop`, `propname`, and `prop` (named) — unknown
  named properties go in a second `<D:propstat>` with status 404 per master
  Contract 5. Finder mostly uses allprop.
- `Last-Modified` must be RFC 1123 in GMT (`time.Format(http.TimeFormat)`) —
  Finder parses it with `http.ParseTime`-equivalent logic.
- Zero-byte objects are legal (PUT of empty file) — `getcontentlength` = 0
  must render (do not omit empty elements for the five mapped props).
