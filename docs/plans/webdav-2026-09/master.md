# WebDAV Protocol Frontend (internal/frontend/webdav) - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root; consumes the landed frontend-interface-2026-09 tree and the GitHub issue `frontend: WebDAV` = issue #1)
- **Children:** 6 leaf documents under this node
- **Scope:** Add a WebDAV protocol frontend implementing `frontend.Frontend` in `internal/frontend/webdav/`, backed by the neutral object model through `internal/backend.Backend`, with HTTP Basic auth via the auth-2026-09 adapter, an e2e case, and user-facing docs.
- **GitHub issue:** #1 (`docs/plans/issue-webdav-frontend.md`)

## HARD ORDERING: this tree sequences AFTER auth-2026-09

**Leaf 01 may be dispatched at any time; leaves 02-05 have auth-dependent tests
and are BLOCKED until the auth-2026-09 tree (GitHub issue #4,
`docs/plans/issue-auth-pluggable.md`) has landed its Basic-auth adapter** —
that is, until `internal/auth` exposes an `Authenticator` implementation that
parses HTTP Basic credentials and resolves them to
`Identity{AccessKeyID, BucketGrants map[string]Grant}`.

This tree CONSUMES that seam and MUST NOT redesign it:

- It does not add fields to `auth.Identity` or `auth.Grant`.
- It does not invent its own credential store, config keys for users, or
  password lookup — credential resolution belongs to the BasicAuthenticator
  owned by auth-2026-09.
- If, at dispatch time, the Basic-auth adapter has not landed, leaf 04 is
  BLOCKED (not improvisable), and by dependency 02/03/05 (whose tests assert
  the 401 path) are BLOCKED too. Report BLOCKED; do not fake auth.

Verify before dispatch: `grep -rn "Basic" internal/auth/` shows an
Authenticator implementation, and `make test` is green on main.

## Goal

zeta-object speaks exactly one wire protocol today: S3. The frontend seam
(`internal/frontend/frontend.go`) and its registry/config wiring are landed;
this tree adds the second frontend: WebDAV (RFC 4918, class 1 subset), so
macOS Finder, Linux davfs2/gvfs, and Windows can mount the server as a drive.

The frontend implements exactly: OPTIONS, PROPFIND (Depth 0/1), GET, HEAD,
PUT, DELETE, MKCOL, COPY, MOVE. Every storage touch goes through the
`backend.Backend` interface — this package contains no filesystem access and
no S3 XML. WebDAV collections map onto bucket/prefix semantics (Contract 3
below is the authoritative mapping and is documented verbatim in the README).

Two binding semantic rules, inherited from the frontend-interface master:

1. **Never silent emulation.** Where WebDAV cannot express something the
   request implies (Depth infinity COPY of a collection, LOCK, dead
   properties, quotas), the frontend rejects or degrades at the seam with a
   protocol-appropriate HTTP status — never a fake success.
   `Capabilities()` advertises exactly what is expressible.
2. **Auth at the seam.** Every request (no exceptions, including OPTIONS and
   PROPFIND) flows through `Authenticator()`; missing or invalid credentials
   get `401` + `WWW-Authenticate: Basic`. Valid identity, missing grant, get
   `403`.

## Architecture

One package, six leaves, one vertical slice each:

- **Leaf 01** creates `internal/frontend/webdav` (package skeleton, `New`
  constructor, `Frontend` interface implementation, capability declaration)
  and the registry/config wiring: the `"webdav"` factory in the frontend
  factory map, the per-entry config keys (`listenAddr` already supported;
  new `bucket` key for single-bucket mode — decision in Contract 2), and
  method dispatch with 405 for everything not in the implemented set.
- **Leaf 02** builds the read path: OPTIONS (DAV header), GET/HEAD, and
  PROPFIND Depth 0/1 — URL-to-(bucket,key) mapping, collection listing via
  `List` prefix/delimiter, `objectmodel.Object` → WebDAV property mapping,
  and `207 Multi-Status` XML encoding.
- **Leaf 03** builds the write path: PUT, MKCOL, DELETE, COPY, MOVE —
  including the full `objectmodel.Error` → HTTP status mapping (404/409/412/
  423-family decisions), Overwrite/Depth header semantics, and ETag
  conditionals on PUT.
- **Leaf 04** wires auth: 401 challenge for anonymous, Basic validation via
  the auth-2026-09 BasicAuthenticator, and `Grant.Read`/`Grant.Write`
  enforcement per effective bucket (read-only identities can mount and
  read but every write gets 403).
- **Leaf 05** instantiates the Frontend conformance suite (`internal/frontend/
  conformance.go`) against the webdav frontend and adds e2e case
  `scripts/e2e/cases/18-webdav.sh` (curl-based verb coverage + 401 path).
  Mount-level testing (Finder, davfs2) is documented as a manual procedure —
  explicitly out of CI, with instructions in the case header comment.
- **Leaf 06** lands user-facing docs: `config.json.example` webdav entry,
  README WebDAV section including the bucket-mapping table and
  capability-degradation list.

Control flow after this tree: `main.go` → registry → webdav `Handler()` on
its configured listener (its own port, or shared — per the landed multi-
listener wiring from frontend-interface leaf 03) → webdav request pipeline
(auth → dispatch → handler) → `backend.Backend` → storage.

## Interface Contracts

### Contract 1: `webdav.Frontend` construction and declaration (FROZEN)

```go
// File: internal/frontend/webdav/frontend.go
package webdav

// New constructs the WebDAV frontend.
//   - be is the Backend for ALL storage access (required, non-nil).
//   - cfg carries the per-frontend config entry (Contract 2).
func New(be backend.Backend, cfg Config) (*Frontend, error)

type Config struct {
    // Bucket non-empty => single-bucket mode (Contract 3, mode B).
    // Empty => multi-bucket mode (mode A): top-level collections = buckets.
    Bucket string
}

func (f *Frontend) Name() string { return "webdav" }
func (f *Frontend) Handler() http.Handler
func (f *Frontend) Authenticator() auth.Authenticator
func (f *Frontend) Capabilities() frontend.ProtocolCaps

// Capabilities() returns, in both modes:
//   Buckets:          true  in multi-bucket mode, false in single-bucket mode
//   Versioning:       false (WebDAV cannot express it; versioning probes 404/405)
//   ConditionalReads: true  (GET/HEAD honor If-None-Match via GetOptions)
//   Multipart:        false (no WebDAV expression)
//   PresignedURLs:    false (no WebDAV expression)
// Owner: 01-package-skeleton-config.md
// Consumers: 02, 03, 04, 05, and the registry/main wiring
```

`Name()` returns exactly `"webdav"` (registry key; config `"type"` value).

### Contract 2: Config contract (FROZEN)

The landed `frontends` config array gains per-entry fields:

```jsonc
"frontends": [
  { "type": "webdav", "listenAddr": ":8444", "bucket": "photos" }
]
```

- `"type": "webdav"` — required, selects this frontend via the factory map.
- `"listenAddr"` — optional, unchanged semantics from frontend-interface
  leaf 03 (own TLS listener vs shared default).
- `"bucket"` — optional string. **Decision (binding):** present and
  non-empty ⇒ single-bucket mode: `/` IS that bucket's root, no bucket
  concept is exposed, MKCOL/DELETE on the root collection is rejected (403),
  and `Capabilities().Buckets == false`. Absent/empty ⇒ multi-bucket mode:
  top-level collections are the server's buckets (`backend.Buckets()`),
  `Capabilities().Buckets == true`.
- Unknown keys inside the webdav entry abort startup (fail-loud, matching
  the unknown-frontend-type behavior proven in e2e case 16).

### Contract 3: Bucket/collection mapping (FROZEN — this table ships verbatim in the README)

| WebDAV resource | Mode A (multi-bucket) | Mode B (single-bucket `bucket: B`) |
|---|---|---|
| `/` (root collection) | synthetic collection; Depth 1 children = buckets via `Buckets()` | collection rooted at bucket `B`; Depth 1 children = keys/prefixes of `B` with prefix `` |
| `/b/` (collection) | bucket `b`; children via `List(b, Prefix="", Delimiter="/")` | prefix collection `b/` in `B` (if `b/` has no keys/prefixes ⇒ 404) |
| `/b/dir/` (collection) | prefix `dir/` in `b` via `List(Prefix="dir/", Delimiter="/")` | same, bucket always `B` |
| `/b/dir/file.txt` | object key `dir/file.txt` in `b` | object key `b/dir/file.txt` in `B` |
| MKCOL `/new` | create bucket `new` (`PutBucket` equivalent via the Backend surface the backend-interface tree exposes) | 403 (bucket selection fixed by config) |
| MKCOL `/b/a/bc/` | valid only if parent `/b/a/` exists as a collection or bucket; collections are VIRTUAL (a prefix with zero objects "exists" only if listed as a common prefix or the parent exists); on success 200, nothing is written | same rule within `B` |
| MKCOL `/x/y` where `/x/` missing | 409 Conflict (RFC 4918 §9.3.1) | same |
| Trailing slash | collections end with `/`; a GET of a collection returns its listing (Depth-0 PROPFIND body semantics are for PROPFIND only) | same |

Key-existence semantics: a plain key `dir` and a prefix `dir/` are distinct;
a resource is a COLLECTION iff `List( Prefix=p, Delimiter="/", MaxKeys=1 )`
returns it as a common prefix or contains keys under `p` with `p` as a
directory prefix. A resource is a FILE iff `Stat(bucket, key)` (no trailing
slash) succeeds. Precedence: collection wins on 404-vs-405 decisions only
where both could match.

### Contract 4: Method/error mapping (FROZEN)

Dispatch table after auth (leaf 01 stubs all of these; 02/03 fill in):

| Method | Status on success | Notable errors |
|---|---|---|
| OPTIONS | 200 + `DAV: 1` + `Allow:` list | — |
| PROPFIND | 207 Multi-Status | Depth 0/1 only; `Depth: infinity` ⇒ 403 with `propfind-finite-depth` precondition body; bad XML ⇒ 400; missing resource ⇒ 404 |
| GET / HEAD | 200 (GET streams body; HEAD identical headers, no body) | NoSuchKey ⇒ 404; If-None-Match match ⇒ 304 (GET/HEAD); collection GET ⇒ 200 empty body (collections have no representation) |
| PUT | 201 (created) / 204 (overwrote) | parent prefix absent is NOT an error (S3 semantics); `If-Match` mismatch ⇒ 412; `If-None-Match: *` on existing ⇒ 412; target is a collection ⇒ 405 |
| DELETE | 204 | missing ⇒ 404; collection ⇒ recursive delete of all keys under the prefix (Depth-infinity delete is expressible: List + Delete loop; 207 not required); root collection ⇒ 403 |
| MKCOL | 201 | exists ⇒ 405; missing parent ⇒ 409; request body present ⇒ 415 (unsupported media type per RFC 4918 §9.3.5); mode B on root ⇒ 403 |
| COPY / MOVE | 201 (created) / 204 (overwrote) | source missing ⇒ 404; destination parent missing ⇒ 409; `Overwrite: F` and destination exists ⇒ 412; collection COPY/MOVE at Depth infinity ⇒ 403 (cannot be expressed — never a partial fake copy); MOVE adds Delete of source after successful copy |
| LOCK / UNLOCK / PROPPATCH / any other | 405 Method Not Allowed + `Allow:` header | v1 scope has no locking; this is a REJECTION, not emulation; davfs2/Finder retry behavior is covered by the manual mount procedure (leaf 05) |

All non-2xx bodies: RFC 4918 error XML (`<D:error>` with precondition
elements such as `<D:propfind-finite-depth/>`), `Content-Type:
application/xml; charset="utf-8"`. `objectmodel.Error` codes map:
NoSuchKey/NoSuchBucket ⇒ 404, BucketAlreadyExists ⇒ 405 (on MKCOL),
PreconditionFailed ⇒ 412, AccessDenied ⇒ 403, InvalidArgument ⇒ 400,
NotImplemented ⇒ 405/501 per table above.

### Contract 5: Property mapping (FROZEN)

`objectmodel.Object` → PROPFIND `<D:prop>` entries (all in the
`DAV:` namespace, `allprop` request):

| WebDAV property | Source | Notes |
|---|---|---|
| `getcontentlength` | `Object.Size` | files only; absent on collections |
| `getcontenttype` | `Object.ContentType` | default `application/octet-stream` when empty; collections report `httpd/unix-directory` |
| `getetag` | `objectmodel.QuotedETag(Object.ETag)` | quoted strong form, files only |
| `getlastmodified` | `Object.LastModified` | RFC 1123, GMT (`http.ParseTime`-compatible) |
| `resourcetype` | `<D:collection/>` for collections, empty `<D:resourcetype/>` for files | |

No dead properties, no `getowner`, no quotas — a PROPFIND naming unknown
properties gets them under `<D:propstat>` with `404` status per property
block (RFC 4918 §9.1.2), never a synthesized value.

### Contract 6: Auth contract (consumed, not designed)

```go
// internal/auth (auth-2026-09 tree, issue #4) provides:
//   auth.Authenticator with a Basic-auth implementation:
//     - parses Authorization: Basic user:pass
//     - resolves to Identity{AccessKeyID, BucketGrants map[string]Grant}
//     - error on missing/invalid credentials
// This tree adds NO auth types. It:
//   1. wraps Authenticator() in the handler chain, BEFORE method dispatch
//   2. missing/invalid credentials => 401 + WWW-Authenticate: Basic realm="zeta-object"
//   3. valid identity, effective bucket lacking Grant.Read  => 403 (GET/HEAD/PROPFIND/COPY src)
//   4. valid identity, effective bucket lacking Grant.Write => 403 (PUT/DELETE/MKCOL/COPY dst/MOVE)
//   5. the "effective bucket" is the mode-A first path segment, or the
//      configured bucket in mode B; the root collection requires Read on ANY
//      granted bucket to list (see leaf 04 for the precise root rule)
// Owner: auth-2026-09 tree (the adapter) + 04-auth-grants.md (the enforcement)
```

### Contract 7: Backend seam (external, unchanged)

```go
// internal/backend.Backend — the ONLY storage surface this package touches:
//   Get, Put, Delete, Stat, List, Buckets, Capabilities
// Zero os./ioutil./filepath filesystem access inside internal/frontend/webdav/.
// If a needed operation is missing from Backend, STOP and report BLOCKED —
// do not add filesystem calls and do not extend Backend from this tree.
// Owner: backend-interface-2026-09 tree. Consumer: 02, 03.
```

## Child Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | [01-package-skeleton-config.md](01-package-skeleton-config.md) | leaf | frontend-interface tree (landed); Config parse in package main | 60K | A |
| 02 | [02-read-path-propfind-get.md](02-read-path-propfind-get.md) | leaf | 01 | 90K | B |
| 03 | [03-write-path-copy-move.md](03-write-path-copy-move.md) | leaf | 01 | 90K | C |
| 04 | [04-auth-grants.md](04-auth-grants.md) | leaf | 01, auth-2026-09 (BasicAuthenticator landed) | 50K | D |
| 05 | [05-conformance-e2e.md](05-conformance-e2e.md) | leaf | 02, 03, 04 | 70K | E |
| 06 | [06-docs.md](06-docs.md) | leaf | 01 (config keys decided); best after 05 | 30K | F |

**Concurrency groups:** strictly sequential (A → B → C → D → E → F). 02 and
03 touch the same dispatch/error files and both need 01; 04's tests exercise
routes from 02/03; 05 is the whole-tree gate; 06 documents the landed
surface. Do not parallelize: the leaves share `dispatch.go`/`errors.go`, and
the review cost exceeds the latency saving.

**External dependencies (whole tree):**
- frontend-interface-2026-09 — LANDED (Frontend seam, registry, config
  wiring, conformance suite). This tree builds on it; it does not reopen it.
- backend-interface-2026-09 + object-model-2026-09 — LANDED (Backend
  interface, neutral model). Consumers, not deliverables.
- **auth-2026-09 (issue #4) — GATING for leaves 02-05.** See HARD ORDERING
  above.

## Dispatch Protocol

### Phase 0: Verify ordering prerequisite

1. Confirm the auth-2026-09 Basic-auth adapter is landed: `grep -rn "Basic"
   internal/auth/` finds an Authenticator implementation; `make test` green.
2. If not landed: dispatch leaf 01 only (it has no auth-dependent tests) and
   hold 02-06, reporting BLOCKED. Do not invent auth.

### Phase 1: Dispatch Leaf 01

1. **Read** [01-package-skeleton-config.md](01-package-skeleton-config.md) and
   dispatch via `delegate_task`:
   - Goal: "Implement all tasks from 01-package-skeleton-config.md"
   - Context: full leaf text + Contracts 1, 2, 4 from this master + Coding
     Conventions block + the pinned `Frontend`/`ProtocolCaps` contract INLINED
   - Include: "Do NOT commit. Do NOT run git add. Write code, run tests,
     report results only."
   - Include: "Do NOT use read_file on existing source files — explore with
     search_files or terminal cat instead. If you read a file, never feed its
     output into write_file."

### Phase 2: Review Leaf 01, Dispatch Leaf 02

1. **Orchestrator reviews leaf 01 in-session** (main model, NOT a delegated
   reviewer): contracts match exactly, `go test ./internal/frontend/webdav/`
   passes, webdav factory reachable from config wiring, `gofmt` clean. Max 3
   re-dispatch cycles, then escalate.
2. **Read** [02-read-path-propfind-get.md](02-read-path-propfind-get.md) and
   dispatch with the same boilerplate, additionally inlining Contracts 3, 5
   and the `backend.Backend` + `objectmodel` signatures.

### Phase 3: Review Leaf 02, Dispatch Leaf 03

1. **Review in-session:** PROPFIND golden-XML tests pass; read path never
   touches `os.`; mode A and mode B both tested via test table.
2. **Read** [03-write-path-copy-move.md](03-write-path-copy-move.md) and
   dispatch likewise (Contracts 3, 4 inlined).

### Phase 4: Review Leaf 03, Dispatch Leaf 04

1. **Review in-session:** error-mapping table tests pass; Overwrite/Depth
   semantics proven; no partial collection copies.
2. **Read** [04-auth-grants.md](04-auth-grants.md) and dispatch (Contract 6
   inlined). If auth-2026-09 has still not landed by now, report BLOCKED.

### Phase 5: Review Leaf 04, Dispatch Leaf 05

1. **Review in-session:** anonymous ⇒ 401 on every method; read-only
   identity ⇒ 403 on every write; grants enforced per effective bucket.
2. **Read** [05-conformance-e2e.md](05-conformance-e2e.md) and dispatch.
   This leaf runs `make e2e` — the AGENTS.md hard rule gate for the whole
   feature.

### Phase 6: Review Leaf 05, Dispatch Leaf 06

1. **Review in-session:** conformance suite green for webdav; case 18 green
   under `make e2e`; mount instructions present in the case header.
2. **Read** [06-docs.md](06-docs.md) and dispatch.

### Phase 7: Integration Review

1. **Orchestrator runs integration review in-session:**
   - `make build`, `make vet`, `make fmt-check`, `make test`, `make
     test-race` all pass
   - `make test-cover-enforce` — if aggregate coverage dipped below the
     floor because new untested wiring shipped, this is a leaf-05 failure;
     fix before proceeding (AGENTS.md floor rule)
   - `make e2e` green INCLUDING new case 18
   - `grep -rn "os\.\|ioutil\." internal/frontend/webdav/` returns no
     filesystem access outside the Backend seam (path-string manipulation
     and errors.As/tests excepted)
   - `grep -rn "internal/frontend/s3" internal/frontend/webdav/` returns
     nothing — the webdav frontend shares nothing with the s3 frontend
   - S3 behavior untouched: pre-existing e2e cases 01-17 all still green
2. If gaps: re-dispatch the owning leaf with contract-fix instructions,
   re-review.
3. If pass: run `gofmt` over changed files, commit per leaf with conventional
   messages (`feat(webdav): ...`), update the tracking table to COMPLETE.

## Review Checklist

The orchestrator (main model) verifies each child in-session:

- [ ] All tasks from the leaf document are implemented
- [ ] Interface contracts from this master are satisfied exactly (names, signatures, file paths)
- [ ] Frozen external contracts untouched: `frontend.Frontend`, `ProtocolCaps`, `auth.Identity`/`Grant`, `backend.Backend`
- [ ] All specified files created/modified at exact paths
- [ ] Tests written and passing (TDD followed): PROPFIND golden XML, error-map table, auth matrix
- [ ] `internal/frontend/webdav` calls only `backend.Backend` — no direct `os.`/filesystem access
- [ ] Capability shortfalls produce protocol-appropriate status codes (Contract 4 table) — no silent emulation
- [ ] Mode A and mode B are BOTH covered wherever mode matters
- [ ] Code follows project conventions (see Coding Conventions below)
- [ ] No scope creep: no LOCK/UNLOCK, no dead properties, no auth store, no S3 changes
- [ ] No debug artifacts: no print debugging, no TODOs, no placeholder values, no commented-out code
- [ ] No line-number corruption: no `     N|` prefixes baked into source files

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go 1.25 (module `github.com/bhodgens/zeta-object`), stdlib only — no new dependencies (no golang.org/x/net/webdav; the mapping work IS the deliverable)
- **Style:** gofmt + goimports with `-local github.com/bhodgens/zeta-object` (`make fmt` before reporting)
- **Naming:** exported = PascalCase with doc comments; unexported = camelCase; package name `webdav`
- **Imports:** stdlib group, then module-internal; no unused imports
- **Errors:** errors as values; wrap with `%w`; switch on `*objectmodel.Error` identity at the frontend boundary
- **Tests:** stdlib `testing`, table-driven, `_test.go` alongside source; no testify; golden-file or inline-golden XML comparisons for multistatus bodies
- **Gates:** `make vet`, `make lint NEW_FROM_REV=<rev>`, `make test` must pass per leaf; `make test-cover-enforce` floor holds; e2e case required before the tree completes (AGENTS.md rule)

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-package-skeleton-config | PENDING | 0 | |
| 02-read-path-propfind-get | PENDING | 0 | |
| 03-write-path-copy-move | PENDING | 0 | |
| 04-auth-grants | PENDING | 0 | |
| 05-conformance-e2e | PENDING | 0 | |
| 06-docs | PENDING | 0 | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. **Unit + conformance:** `go test ./internal/frontend/webdav/ -v` —
   dispatch table, URL mapping (both modes), PROPFIND golden XML, error
   mapping, auth matrix, and `frontend.RunConformanceSuite(t, f, opts)` for
   the webdav frontend.
2. **Full suite:** `make test` then `make test-race` — zero failures;
   coverage floor holds.
3. **Harness:** `make e2e` — case 18 (webdav) green AND all pre-existing
   cases 01-17 unchanged-green (proves S3 behavior untouched).
4. **Cross-boundary contract checks:**
   - Registry round-trip: config `frontends: [{"type":"webdav","bucket":"x"}]`
     starts, `Lookup("webdav")` returns it, `Name()=="webdav"`.
   - Mode switch: same binary, config with/without `"bucket"` —
     `Capabilities().Buckets` flips, PROPFIND `/` children change shape.
   - 401 wire check: `curl -k https://host:port/` without credentials ⇒ 401
     with `WWW-Authenticate: Basic`.
5. **Manual mount test (documented, NOT CI):** macOS Finder `Cmd+K` and
   davfs2 mount against a local TLS instance — procedure lives in the case
   18 header comment and the README (leaf 06). CI never mounts drives.
6. **Static gates:** `make build`, `make vet`, `make fmt-check`, `make lint`,
   plus the filesystem-access grep from Phase 7.

## Structural Completeness Check (Before Dispatch)

Before dispatching any child, confirm this master contains every required
section: Meta, Goal, Architecture, Interface Contracts, Child Index, Dispatch
Protocol, Review Checklist, Coding Conventions, Completion Tracking Table,
Integration Test Plan, Notes. If any is missing, fill it in first.

## Notes

- **The auth dependency is real and hard.** Leaves 02-05 assert the 401
  behavior in their test-first steps; testing a route that always 401s
  without the BasicAuthenticator means the whole tree stalls behind leaf 04.
  Sequence per HARD ORDERING.
- **Single-bucket mode is a decided contract (Contract 2), not an open
  question.** Implementers must not add a third mode or auto-detection.
- **Collections are virtual prefixes.** MKCOL never writes a marker object;
  a collection "exists" iff its parent path exists and it either lists as a
  common prefix or contains keys. This keeps S3 and WebDAV views of the same
  data consistent — an empty directory that exists only because MKCOL
  succeeded is NOT visible to S3 clients, which is acceptable and documented
  (leaf 06).
- **No golang.org/x/net/webdav.** The stdlib-only convention stands; the
  value of this frontend is the explicit mapping (Contracts 3-5), which the
  x/net package would hide behind its own model.
- **LOCK/UNLOCK is out of scope by the issue body** and rejected with 405.
  If a real client proves to require locks (Finder write failures are the
  known risk), that is a FOLLOW-UP tree — do not grow a lock implementation
  inside these leaves.
- **Riskiest leaf is 02** (multistatus XML correctness against picky
  clients). The golden-XML tests plus the manual mount procedure are the
  authority; Finder/davfs2 parsing bugs surface there first.
- Future frontends (ownCloud = WebDAV superset, issue #3) will consume this
  package's property/error mapping — keep those helpers exported and
  side-effect-free so the ownCloud tree can reuse them.
