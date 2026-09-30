# WebDAV Package Skeleton + Registry/Config Wiring - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Create `internal/frontend/webdav` implementing `frontend.Frontend` (constructor, Name/Handler/Authenticator/Capabilities, method dispatch with 405 for unimplemented methods, mode A/B selection) and wire the `"webdav"` type into the frontend factory map with the `bucket` config key (single-bucket mode decision).
- **Dependencies:** frontend-interface-2026-09 tree (LANDED: `Frontend`, `ProtocolCaps`, `Registry`, config `frontends` key, multi-listener wiring). No sibling leaf. NOT gated on auth (Authenticator() may return the auth-2026-09 adapter wired by main; this leaf does not test its behavior).
- **Estimated Context:** 60K
- **Concurrency Group:** A (first)

## Goal

After this leaf:

1. `internal/frontend/webdav` exists, compiles, and implements the frozen
   `Frontend` contract: `New(backend.Backend, Config) (*Frontend, error)`,
   `Name()=="webdav"`, `Capabilities()` exactly per master Contract 1
   (mode-dependent `Buckets` flag), `Handler()` returning a dispatching
   handler, `Authenticator()` returning the injected `auth.Authenticator`.
2. A config entry `{"type": "webdav", "listenAddr": ":8444", "bucket":
   "photos"}` starts the server with the webdav frontend mounted (own
   listener); `{"type": "webdav"}` starts it in multi-bucket mode sharing
   the default listener. Unknown keys in a webdav entry abort startup.
3. Method dispatch exists with the FULL final routing skeleton: OPTIONS/
   PROPFIND/GET/HEAD/PUT/DELETE/MKCOL/COPY/MOVE reach per-method handler
   methods (stubbed: 501 body "not implemented in this leaf" is FORBIDDEN —
   stubs return the correct shape where trivially known, otherwise 405);
   LOCK/UNLOCK/PROPPATCH/unknown methods get 405 + `Allow:` header.
4. `make test` and `make e2e` stay green; S3 behavior untouched.

## Context

The frontend seam is landed. Study these before implementing (cat, not
read_file):

- `internal/frontend/frontend.go` — frozen `Frontend` + `ProtocolCaps`
- `internal/frontend/registry.go` — `Register/Lookup/All`, duplicate-name error
- `internal/frontend/caps.go` — `ErrCapability`/`IsCapabilityError`
- `internal/frontend/s3/frontend.go` — the reference implementation shape
  (Name/Handler/Authenticator/Capabilities pattern)
- `internal/backend/backend.go` — the ONLY storage surface
- `internal/objectmodel/model.go`, `errors.go` — neutral model + error codes
- `main.go` + config loading — where the `frontends` array is decoded and
  the factory map selects constructors (landed by frontend-interface leaf 03;
  find the factory map with `grep -rn '"s3"' main.go config*.go internal/`)
- `scripts/e2e/cases/16-frontends-config.sh` — the fail-loud pattern for
  unknown frontend types your config validation must match
- `internal/auth/auth.go` — `Authenticator`/`Identity`/`Grant` (v1 shape;
  auth-2026-09 will provide the Basic implementation — you only accept one)

WebDAV is HTTP; the frontend is a plain `http.Handler`. URL semantics:
collections end with `/`; the parsed (bucket, key) derivation lives in ONE
function (master Contract 3) because every method needs it.

## Interface Contracts (From Parent)

Implement master Contracts 1 and 2 exactly. Restated, binding:

```go
// File: internal/frontend/webdav/frontend.go
package webdav

func New(be backend.Backend, cfg Config) (*Frontend, error) // be required non-nil => error otherwise
func (f *Frontend) Name() string                            // exactly "webdav"
func (f *Frontend) Handler() http.Handler
func (f *Frontend) Authenticator() auth.Authenticator
func (f *Frontend) Capabilities() frontend.ProtocolCaps
// Buckets: true only in multi-bucket mode (cfg.Bucket == "")
// Versioning/Multipart/PresignedURLs: always false
// ConditionalReads: true

type Config struct {
    Bucket string // non-empty => single-bucket mode
}
```

Config (main-side): webdav entries accept keys `type`, `listenAddr`,
`bucket` ONLY; anything else aborts startup with an error naming the key.
The factory map grows a `"webdav"` entry constructing `webdav.New(be, cfg)`
and registering it. Registration happens through the existing
`frontend.Registry` — no registry changes.

Dispatch skeleton (files suggested, names binding):

```go
// internal/frontend/webdav/dispatch.go
// serveHTTP: parse URL -> resource descriptor; then method switch.
// Per master Contract 4: unknown/LOCK/UNLOCK/PROPPATCH => 405 + Allow header.
```

## Tasks

### Task 1: Package skeleton + Frontend implementation

**Objective:** `internal/frontend/webdav` passes the frozen-contract tests.

**Files:**
- Create: `internal/frontend/webdav/frontend.go`
- Create: `internal/frontend/webdav/config.go` (Config + validation)
- Create: `internal/frontend/webdav/frontend_test.go`

**Step 1: Write failing tests**

Table-driven tests over `New`:

- `nil` backend ⇒ non-nil error from `New`
- `Name()` == `"webdav"`
- Mode A (`Config{}`): `Capabilities()` == `{Buckets:true, ConditionalReads:true}` with the rest false
- Mode B (`Config{Bucket:"photos"}`): `Capabilities()` == `{ConditionalReads:true}` with the rest false (Buckets:false)
- `Authenticator()` returns the injected authenticator (pass a stub implementing `auth.Authenticator`)
- Empty `Config.Bucket` string containing only whitespace ⇒ error (mode must be deliberate)
- A valid instance registered into `frontend.NewRegistry()` round-trips `Lookup("webdav")`

**Step 2: Run** `go test ./internal/frontend/webdav/ -v` — verify FAIL (package absent).

**Step 3: Implement minimally** until green. `Handler()` may return a
handler that only 405s for now; dispatch depth is Task 3.

**Step 4:** `go test ./internal/frontend/webdav/ -v` green; `make vet` clean.

### Task 2: Config wiring + registry factory

**Objective:** `{"type":"webdav",...}` config entries construct, register,
and mount the frontend.

**Files:**
- Modify: the main-side config struct + frontend factory map (locate via
  `grep -rn 'frontends\|"s3"' main.go config*.go`; exact filenames discovered
  in-session, do not guess)
- Create: `config_wiring_test.go` in the package where the factory lives
  (follow how the s3 frontend's registration is tested today)

**Step 1: Write failing tests**

- Factory map contains `"webdav"`; calling it with a test backend + entry
  `{"type":"webdav","bucket":"b"}` yields a frontend with `Name()=="webdav"`
  and mode-B caps
- Entry with unknown key `{"type":"webdav","bogus":1}` ⇒ construction error naming `"bogus"`
- Entry missing `type` still errors with the existing unknown-type path (unchanged)
- `{"type":"webdav","listenAddr":"127.0.0.1:0"}` constructs without error
- Duplicate registration (`Register` twice) returns the registry's duplicate-name error (regression guard)

**Step 2: Run to verify FAIL. Step 3: implement. Step 4: green.**

Additionally verify the full server still boots with the landed default
config: `go build -o /tmp/zeta-object . && /tmp/zeta-object --help || true`
(compiles = wiring did not break main).

### Task 3: Method dispatch skeleton

**Objective:** Every request reaches the right per-method handler or 405.

**Files:**
- Create: `internal/frontend/webdav/dispatch.go` — `serveHTTP`, URL→resource
  parsing (the single mapping function from master Contract 3:
  mode A: first segment = bucket or synthetic-root; mode B: everything under
  the configured bucket; trailing-slash ⇒ collection flag)
- Create: `internal/frontend/webdav/dispatch_test.go`

**Step 1: Write failing tests** (httptest + stub backend + stub
authenticator permitting all):

- OPTIONS request reaches `handleOPTIONS` (assert via exported-for-test
  hook or by observing the `Allow` header through the stub)
- GET/HEAD/PUT/DELETE/MKCOL/COPY/MOVE/PROPFIND each route to a distinct
  handler method — assert via a recording stub embedded in the frontend
  (exported test hook pattern: `export_test_surface.go` like the s3 package)
- `LOCK`, `UNLOCK`, `PROPPATCH`, `VERSION-CONTROL`, gibberish method ⇒ 405
  with `Allow: OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, MKCOL, COPY, MOVE`
- URL `/` in mode A parses as root-collection; `/photos/` in mode B parses
  as prefix-collection `photos/` with bucket `photos` — table of
  (mode, URL) → (bucket, key, isCollection) cases including trailing slash,
  double slash (`//` normalized), and percent-encoding (`%20` stays encoded
  in the key per Go's `r.URL.Path` semantics — pin actual behavior in the test)

**Step 2: FAIL. Step 3: implement dispatch + stub handlers. Step 4: green.**

### Task 4: Full gate

```
go test ./... -count=1
make test && make test-race
make vet && make fmt-check && make lint NEW_FROM_REV=<rev>
make e2e
```

Expected: ALL PASS; pre-existing e2e cases unchanged.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] All tasks implemented; all gates green
- [ ] `internal/frontend/webdav` contains zero `os.`/`ioutil.` filesystem calls
- [ ] Frozen contracts intact: `Frontend`, `ProtocolCaps`, `auth` types, `backend.Backend` untouched
- [ ] `Name()` returns exactly `"webdav"`; `Capabilities()` per Contract 1 in both modes
- [ ] Config: `bucket` key works; unknown key fails loud; absent `frontends` behavior unchanged
- [ ] 405 + Allow for LOCK/UNLOCK/PROPPATCH/unknown — never silent emulation
- [ ] Conventions: stdlib only (NO golang.org/x/net/webdav), gofmt clean, errors as values
- [ ] No debug artifacts, no TODOs, no line-number corruption
- [ ] All files at exact specified paths

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Task implemented; tests exist and pass (TDD order respected)
- [ ] Contracts 1 and 2 satisfied exactly — mode flip changes only `Buckets`
- [ ] Factory/registration wired through existing registry; no registry edits
- [ ] Dispatch table per Contract 4; 405 family correct with Allow header
- [ ] No auth behavior tested here (leaf 04 owns that) — stub authenticator only
- [ ] No S3 package imports; no filesystem access; stdlib only
- [ ] `make e2e` green; S3 cases untouched

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The mode-B "empty/whitespace Bucket" validation is deliberate: silent
  fallback from a typo (`"bucket": " "`) to mode A would expose the whole
  server as buckets — a fail-loud startup beats a surprising mount.
- Keep the URL→resource function pure (no I/O) so leaves 02/03/04 can unit
  test it independently; put it in `paths.go` if it outgrows dispatch.go.
- Do NOT implement OPTIONS/PROPFIND bodies yet — 02 owns them. The OPTIONS
  stub may already emit the `DAV: 1` header; it must not yet answer PROPFIND.
