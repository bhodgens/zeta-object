# WebDAV Auth: 401 Challenge + Grant Enforcement - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Wire the frontend `Authenticator()` into the webdav handler chain: 401 + `WWW-Authenticate: Basic` challenge for missing/invalid credentials, and `Grant.Read`/`Grant.Write` enforcement against the effective bucket for every method.
- **Dependencies:** 01 (dispatch skeleton), 02 + 03 (real handlers to protect). **HARD prerequisite: the auth-2026-09 tree (GitHub issue #4) has landed its BasicAuthenticator** — `internal/auth` contains an Authenticator implementation parsing `Authorization: Basic user:pass` into `Identity{AccessKeyID, BucketGrants map[string]Grant}`. If it has not landed, this leaf is BLOCKED — do not write a substitute credential store.
- **Estimated Context:** 50K
- **Concurrency Group:** D (after 02/03)

## Goal

After this leaf:

1. EVERY request to the webdav handler — including OPTIONS and PROPFIND —
   without valid credentials receives `401` with
   `WWW-Authenticate: Basic realm="zeta-object"` and an empty or error-XML
   body. No method bypasses the challenge.
2. Credentials validated by the auth tree's BasicAuthenticator yield an
   `Identity`; enforcement uses ONLY `Identity.BucketGrants`:
   - `Grant.Read` false ⇒ 403 on GET, HEAD, PROPFIND, COPY (source), MOVE (source)
   - `Grant.Write` false ⇒ 403 on PUT, DELETE, MKCOL, COPY (destination), MOVE (destination)
3. Read-only identities can mount and browse; every write attempt gets 403.
4. The 401-vs-403 distinction is exact: 401 = "who are you" (absent/bad
   credentials), 403 = "I know you, and no" (valid identity, missing grant).
5. This leaf adds NO auth types, NO credential storage, NO config for users —
   the BasicAuthenticator is constructed and injected by main exactly like
   the s3 frontend's adapter.

## Context

Read before implementing:

- `internal/auth/auth.go` — frozen `Authenticator`/`Identity`/`Grant`
- `internal/auth/` Basic-auth adapter from auth-2026-09 (grep `-rn "Basic"
  internal/auth/`) — its constructor, its error semantics (which errors mean
  "no/invalid credentials" vs internal failure), and how main constructs it
- `internal/frontend/s3/auth_adapter.go` + `auth_adapter_test.go` — the
  reference pattern for per-frontend auth wiring and its tests
- Leaf-01 `dispatch.go` — the chain insert point: auth runs BEFORE method
  dispatch (401s must not leak which methods exist; `Allow` on 405 comes
  only after successful auth)

Effective-bucket rule (master Contract 6): mode A = first path segment;
mode B = configured bucket. Enforcement order inside serveHTTP:

1. `Authenticator().Authenticate(r)` — error ⇒ 401 + challenge; pass
   Identity into the request context (`context.WithValue` with an unexported
   key type) or thread it explicitly to dispatch — pick one, document.
2. Resolve resource (leaf-01 parser). Root collection `/` in mode A ⇒
   require Read on AT LEAST ONE bucket (listing buckets reveals names, so
   "no grants at all" ⇒ 403 on root); mode B root ⇒ Read on the configured
   bucket.
3. Method → required grant per the table above; resource-specific buckets
   (COPY/MOVE have source AND destination) each checked with their own grant.
4. Then method dispatch.

Authentication failures that are NOT credential failures (e.g. the
authenticator itself errors internally) must map to 500, not 401 —
distinguish via the adapter's error types (discovered in-session).

## Interface Contracts (From Parent)

Master Contract 6 — consumed, not designed. Binding:

- This package adds zero new auth types.
- 401 responses ALWAYS carry `WWW-Authenticate: Basic realm="zeta-object"`.
- Enforcement covers all nine implemented methods plus the 405 family
  (an unauthenticated LOCK gets 401, not 405 — challenge first).
- Root-collection grant rule as stated in Context (pin in tests).

## Tasks

### Task 1: 401 challenge path

**Objective:** Anonymous and bad-credential requests 401 on every method.

**Files:**
- Modify: `internal/frontend/webdav/dispatch.go` (insert auth stage)
- Create: `internal/frontend/webdav/auth_test.go`

**Step 1: Write failing tests** (stub backend; REAL BasicAuthenticator from
`internal/auth` with test credentials):

- No `Authorization` header ⇒ 401 + `WWW-Authenticate:
  Basic realm="zeta-object"` — table over ALL of OPTIONS, PROPFIND, GET,
  HEAD, PUT, DELETE, MKCOL, COPY, MOVE, LOCK (LOCK 401s, does not 405)
- Garbage header (`Authorization: Basic !notbase64!`) ⇒ 401
- Wrong password ⇒ 401
- Correct credentials but auth test needs a configured user — construct the
  adapter per its landed constructor; if it sources users from config/env,
  wire it in the test exactly as main does
- 401 body: empty or `<D:error>`; `Content-Length` present; NO `Allow` header

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 2: Grant enforcement matrix

**Objective:** Identity-scoped access is exact.

**Files:**
- Modify: `internal/frontend/webdav/dispatch.go` (grant gate) or new `authz.go`
- Modify: `internal/frontend/webdav/auth_test.go`

**Step 1: Write failing tests** — full matrix, table-driven, identity with
`BucketGrants{"photos": {Read: true, Write: false}}`:

| Request | Expected |
|---|---|
| PROPFIND /photos/ | 207 |
| GET /photos/a.txt (stub has it) | 200 |
| HEAD /photos/a.txt | 200 |
| COPY /photos/a.txt → /photos/b.txt | 403 (dst Write missing) |
| MOVE /photos/a.txt → /backup/ (backup granted RW, photos read-only) | 403 (src Write missing for MOVE) |
| PUT /photos/x.txt | 403 |
| DELETE /photos/a.txt | 403 |
| MKCOL /photos/sub/ | 403 |

Then the RW-on-photos identity: all of the above writes to `/photos/...` ⇒
their normal statuses (201/204/207). Then:

- Identity with NO grants: root PROPFIND ⇒ 403; any bucket path ⇒ 403
- Mode B (`Config{Bucket:"photos"}`) with read-only identity: root PROPFIND
  ⇒ 207, PUT `/x.txt` ⇒ 403 (effective bucket is the configured one)
- COPY with Read on source bucket and Write on destination bucket ⇒ 201
  (both grants checked independently — the cross-bucket positive case)
- OPTIONS with valid read-only credentials ⇒ 200 (auth ok, OPTIONS needs no grant)
- Authenticated-but-unknown bucket path (`/nope/f.txt`, mode A, identity
  without grants on `nope`) ⇒ 403 (grant check precedes existence —
  existence leakage through 404 is a grant bypass)

**Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 3: Main wiring

**Objective:** The server constructs and injects the BasicAuthenticator for
the webdav frontend.

**Files:**
- Modify: the main-side webdav factory (from leaf 01's wiring) to construct
  the auth tree's BasicAuthenticator and inject it (follow the s3
  frontend's adapter-wiring pattern exactly)
- Test: follow the landed pattern for s3 adapter wiring tests; at minimum a
  construction test (server builds with a webdav config entry and the
  adapter; duplicate-construction or misconfig fails loud)

**Step 1: failing test. Step 2: FAIL. Step 3: implement. Step 4: green.**

### Task 4: Full gate

```
go test ./internal/frontend/webdav/ -v
go test ./... -count=1 && make test && make test-race
make vet && make fmt-check && make lint NEW_FROM_REV=<rev>
make e2e   # unchanged cases green; wire-level 401 case lands in leaf 05
```

## Self-Verification Checklist

- [ ] All tasks implemented; gates green
- [ ] 401 + WWW-Authenticate on EVERY method for anonymous/bad creds, including LOCK (401 before 405)
- [ ] 403 for valid identity missing grants; 401-vs-403 distinction exact
- [ ] Full matrix tested (read-only identity: 3 reads pass, 5 writes 403)
- [ ] Cross-bucket COPY checks source Read AND destination Write independently
- [ ] Grant check precedes existence (no 404-vs-403 leakage)
- [ ] Effective bucket: mode A first segment, mode B configured bucket; root rules pinned
- [ ] NO new auth types, no credential storage, no user config in this package
- [ ] Auth-internal errors ⇒ 500, not 401
- [ ] gofmt clean, no debug artifacts; all files at exact paths

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Task implemented; TDD respected
- [ ] Only `auth.Authenticator`/`Identity`/`Grant` consumed — frozen shapes untouched
- [ ] BasicAuthenticator is the auth tree's implementation, injected via main; nothing reimplemented here
- [ ] Matrix complete including mode B and the no-grants identity
- [ ] Challenge-first ordering proven (LOCK ⇒ 401 not 405)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The realm string `zeta-object` is frozen here — leaf 06's docs quote it;
  changing it later breaks saved client credentials.
- Finder caches auth per mount session; a regression that 401s mid-session
  shows up as endless credential prompts in the manual mount test (leaf 05's
  procedure) — keep the challenge on absent/invalid only, never on
  missing-grant (that is 403).
- Do not log credentials or full Authorization headers anywhere in this
  package — errors may name the username from Identity.AccessKeyID at most.
