# HTTP/3 (QUIC) Frontend + Listener Seam - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec. REPORT the pinned quic-go version
> and the exact API shapes you used (listener construction, server
> serving, graceful stop) - leaf 03's probe and leaf 04's harness pin
> the same shapes.

## Meta

- **Parent:** ./master.md
- **Scope:** the QUIC listener seam extension, the `h3` frontend
  package (wraps the webdav handler, serves it over HTTP/3), the
  factory entry, the UDP serving path in main, and the Alt-Svc
  advertisement middleware.
- **Dependencies:** none (disjoint files from leaf 01)
- **Estimated Context:** 80K
- **Concurrency Group:** A

## Goal

A config entry `{"type":"h3","listenAddr":"0.0.0.0:9443","bucket":"x"}`
starts an HTTP/3 server on that UDP address serving the same WebDAV
semantics (same auth, same bucket re-rooting) over QUIC, using the
process cert pair. Existing TCP frontends advertise it with
`alt-svc: h3="<port>"; persist=1` so h3-capable clients migrate
automatically and fall back to TCP when UDP is blocked.

## Context

- The frozen frontend seam is `internal/frontend/frontend.go`:
  `Frontend` (lines 14-19), the optional-interface precedents
  `NonHTTPFrontend` (lines 35-45) and `TLSListenerFrontend` (lines
  57-68). You ADD `QUICListenerFrontend` following the same doc-comment
  pattern. The `Frontend` interface itself gains NOTHING.
- `frontends.go` (root package): `frontendFactories` map at line 43;
  the webdav factory at lines 52-62 shows the exact webdav constructor
  call you re-use (Basic authenticator over `identityRegistry`,
  `webdav.WithLockStoreRoot(getBucketPath)`); `validateOptions` at
  lines 127-139; `listenerSpec` at 300-308; `mountFrontends` at
  377-414 with the TLS-listener rejection pattern at 385-387.
- `main_server.go`: the dedicated-listener path opens TCP TLS
  listeners (the `ListenAndServeTLS` shape; TLSListenerFrontend
  frontends get the pre-built-config branch). Your UDP branch goes
  beside it, following its goroutine/shutdown/error patterns.
- Dependency: `github.com/quic-go/quic-go` (pure Go, no cgo) and its
  `http3` subpackage. Pick the CURRENT stable release; PIN THE EXACT
  VERSION in go.mod. Verify the static-binary property at the end:
  `CGO_ENABLED=0 go build ./...` must succeed - a cgo dependency is a
  FAILED leaf.
- HTTP/3 requires TLS 1.3; the quic-go TLS config path enforces it.
  Reuse the process `certFile`/`keyFile` pair - read how the admin
  frontend or main builds a tls.Config from the pair (search_files
  `tls.LoadX509KeyPair`) and follow that pattern.
- Coverage: your changes touch the ROOT package (`frontends.go`,
  `main_server.go`). The root-package coverage floor is 79; if
  measured coverage drops below the floor, write the missing tests -
  never lower the floor (AGENTS.md).

## Interface Contracts (From Parent)

### Contract 2: the QUIC listener seam (you produce this)

```go
// internal/frontend/frontend.go (ADD; the Frontend interface above it is
// UNTOUCHED):
//
// QUICListenerFrontend is an OPTIONAL extension for frontends served
// over QUIC (HTTP/3). The caller (package main) opens a UDP socket on
// Addr() and serves HTTP/3 with the pre-built TLS config from
// TLSConfig(). The precedent is TLSListenerFrontend above it.
type QUICListenerFrontend interface {
    Frontend
    // Addr returns this frontend's dedicated UDP listen address from
    // its config ("" => config error, rejected at construction). An
    // empty address is NEVER a shared-mux fallback.
    Addr() string
    // TLSConfig returns the QUIC listener's TLS configuration, built
    // from the process cert pair. The caller uses it as-is.
    TLSConfig() (*tls.Config, error)
}
```

### Contract 3: the h3 frontend (you produce this)

```go
// internal/frontend/h3 (NEW package):
// Name() is "h3". The frontend WRAPS a webdav frontend built with the
// same constructor and options the "webdav" factory entry uses
// (Basic auth over the identity registry, lock store root). It is a
// QUICListenerFrontend, never a shared-mux frontend: Handler() exists
// for the seam but is never mux-mounted.
// Capabilities() delegates to the wrapped webdav frontend.
```

- Factory entry `frontendFactories["h3"]`. Option keys validated
  fail-loud through `validateOptions`: v1 has NONE. An unknown option
  key aborts startup naming the key and the known (empty) set.
- Known-types strings: adding the `h3` type changes the startup
  error's known list. GREP and update every pinned copy: search for
  the literal `known: [` across `*_test.go` and `scripts/e2e/`
  (the copies inside `internal/frontend/ftp/frontend.go` and
  `internal/frontend/sftp/frontend.go` are those files' own error
  text - update only if a test pins the GLOBAL set they appear in).
  Every pinned literal gains `h3` IN THIS LEAF.

### Contract 5 (partial, you produce the middleware): Alt-Svc

- When any mounted frontend implements `QUICListenerFrontend`, main
  wraps each HTTP-serving frontend's handler so every response
  carries `alt-svc: h3=":<h3-port>"; persist=1` (RFC 7838). Purely
  additive response headers; the h3 frontend's own responses do not
  carry it. Port comes from the h3 frontend's Addr().

### Contract 4: dependency and build discipline (you produce this)

- quic-go pinned exact in `go.mod`; `go mod tidy` clean;
  `CGO_ENABLED=0 go build ./...` green. Report the version.

## Tasks

### Task 1: the QUIC listener seam

**Objective:** a frontend can own a UDP/QUIC listener.

**Files:**
- Modify: `internal/frontend/frontend.go` (add the interface only)
- Modify: `frontends.go` (`mountFrontends` rejects a
  `QUICListenerFrontend` without its own `listenAddr`, error naming
  the rule, same style as the TLSListenerFrontend rejection at
  lines 385-387)
- Modify: `main_server.go` (a QUICListenerFrontend spec is served on
  UDP via quic-go with the pre-built config - never through the TCP
  `ListenAndServeTLS` path; follow the existing dedicated-listener
  goroutine/shutdown/error patterns)

**Step 1: Write failing tests** (root package, beside the existing
`mountFrontends` tests - find them with search_files):
- a stub `QUICListenerFrontend` configured without `listenAddr` is
  rejected by `mountFrontends` with an error naming the rule;
- a mounted h3 frontend produces a UDP listener spec carrying its
  Addr and TLS config (assert on the spec, no real socket in unit
  tests);
- `mountFrontends` classifies it as needing a dedicated listener
  (never shared-mux) even with an empty addr present.

**Step 2:** `go test . -run TestQUICListener -count=1` -> FAIL.

**Step 3: Implement** the interface, the rejection, and the spec
plumbing. The UDP serving itself is Task 4 (integration there);
here the spec must carry everything main needs.

**Step 4:** PASS.

### Task 2: the h3 frontend package

**Objective:** the `h3` type exists, wraps webdav, and validates
loudly.

**Files:**
- Create: `internal/frontend/h3/frontend.go` (the frontend: Name/
  Handler/Authenticator/Capabilities delegating to the wrapped webdav;
  Addr/TLSConfig for the seam)
- Create: `internal/frontend/h3/frontend_test.go`
- Modify: `frontends.go` (factory entry + option validation + the
  pinned known-types strings from Contract 3)

**Step 1: Write failing tests:**
- construction requires `bucket` (a missing bucket is an error naming
  the requirement - the webdav single-bucket re-root needs it);
- `Name() == "h3"`; `Authenticator()` is non-nil and is the Basic
  authenticator; `Capabilities()` equals the wrapped webdav's
  (Buckets false in single-bucket mode, ConditionalReads true);
- unknown option key aborts construction naming the key and the
  known empty set;
- empty `listenAddr` aborts construction (never a shared-mux
  fallback);
- `TLSConfig()` returns a config with the process pair loaded,
  MinVersion TLS 1.3, and no client-cert requirement (webdav auth is
  Basic over h3, not mTLS - assert `ClientAuth ==
  tls.NoClientCert`);
- Addr() round-trips the config value.

**Step 2:** `go test ./internal/frontend/h3/ -count=1` -> FAIL.

**Step 3: Implement.** The h3 frontend builds its wrapped webdav with
the SAME constructor call the webdav factory entry uses (copy the
shape from `frontends.go:52-62`; the identity registry is nil-safe:
like webdav, construction errors with a named message when
`identityRegistry == nil`). The http3.Server wiring (Task 4) takes
`Handler()` output of the wrapped webdav; the h3 frontend's own
`Handler()` returns the same handler for seam completeness.

**Step 4:** PASS; factory entry added; grep-updated known-types
strings all carry `h3` (grep `known: \[` yourself and list what you
updated in the report).

### Task 3: Alt-Svc advertisement

**Objective:** TCP frontends point h3-capable clients at the UDP
endpoint.

**Files:**
- Create: `altsvc.go` (root package: the middleware - a small
  http.Handler wrapper adding the header)
- Test: `altsvc_test.go`
- Modify: `main_server.go` (mount the middleware on HTTP frontends
  when an h3 frontend is mounted; port from the h3 Addr)

**Step 1: Write failing tests:**
- with an h3 frontend mounted, a request to a (stub) TCP frontend
  response carries `Alt-Svc: h3="<port>"; persist=1`;
- without an h3 frontend mounted, no Alt-Svc header appears;
- the h3 frontend's own handler response carries NO Alt-Svc header;
- an existing Alt-Svc value (defensive) is preserved/appended, never
  clobbered silently (choose append-with-comma and pin it).

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 4: serving HTTP/3 (integration)

**Objective:** the listener actually serves.

**Files:**
- Modify: `internal/frontend/h3/frontend.go` (or a `serve.go` in the
  package: build the `http3.Server` with the wrapped handler and the
  TLS config; expose the quic-go listener construction main calls)
- Modify: `main_server.go` (the UDP branch from Task 1 opens the
  quic-go listener and serves it; shutdown honors the same graceful
  stop channel the TCP listeners use)
- Test: `internal/frontend/h3/serve_test.go` - a REAL loopback test:
  start the h3 frontend on `127.0.0.1:0` (ephemeral port) with a
  stub backend, speak HTTP/3 to it with the quic-go CLIENT side (the
  library ships one), assert: a PUT then a GET round-trips the bytes;
  an unauthenticated GET is 401; a Range GET returns 206 (the header
  only - leaf 01 owns the semantics; if leaf 01 has not landed, a
  200 is the expected value and the test says so in a comment the
  orchestrator updates).

**Step 1: Write failing tests** (the loopback round-trip above).
**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS; full root
+ h3 suites green.

### Task 5: dependency and build discipline

- `go mod tidy`; record the pinned quic-go version;
- `CGO_ENABLED=0 go build ./...` green (the sqlite-approval probe
  pattern);
- `go test ./... -count=1` green (webdav package included - if leaf
  01 is mid-flight in the same worktree, run YOUR packages plus
  `go build ./...` and say so in the report; the orchestrator runs
  the full suite at review).

## Self-Verification Checklist

- [ ] All tasks implemented; h3 + root suites green; `go build ./...`
      green
- [ ] `internal/frontend/frontend.go` Frontend interface UNTOUCHED
      (interface diff is additive only: QUICListenerFrontend)
- [ ] No cgo: CGO_ENABLED=0 build green; quic-go version pinned exact
- [ ] `mountFrontends` rejects an h3 frontend without listenAddr
- [ ] Alt-Svc present on TCP responses only when h3 is configured;
      absent on the h3 frontend's own responses
- [ ] Every pinned `known: [` literal updated with `h3` (list them in
      the report)
- [ ] Basic auth over h3 (no client-cert requirement on the h3
      listener)
- [ ] `gofmt -l` clean on changed files; `make lint NEW_FROM_REV=HEAD`
      0 findings
- [ ] DO-NOT-TOUCH respected: `internal/frontend/webdav/get.go` (leaf
      01 owns it), `internal/frontend/s3/` (except reading),
      `scripts/zfs-validate/run-zfs-validation.sh`,
      `go.sum` changes limited to the quic-go addition

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 2 satisfied: interface name, method set, empty-Addr
      rule, the rejection error text names the rule
- [ ] Contract 3 satisfied: Name "h3", webdav wrapping with the same
      constructor options, Capabilities delegation, empty options set
- [ ] Contract 5 satisfied: Alt-Svc middleware shape and presence
      rules pinned by tests
- [ ] Contract 4 satisfied: exact quic-go pin, CGO_ENABLED=0 probe
      recorded in the report
- [ ] The UDP serving path never goes through TCP ListenAndServeTLS
      (grep the diff for the branch)
- [ ] quic-go version + API shapes (listener construction, serve,
      stop) recorded in the report for leaf 03/04

Output: APPROVED or specific gaps with file:line.

## Notes

- **Why a new interface and not TLSListenerFrontend:** that
  interface's caller path opens a TCP TLS listener; QUIC needs a UDP
  packet conn and the quic-go listener. Wedging UDP through the TCP
  path is the rejected naive fix.
- **Why webdav under h3 and not S3:** the cache client protocol
  decision (user, 2026-10-04). S3-over-h3 is a future increment; the
  frontend shape (wrap any HTTP handler) does not prevent it.
- **quic-go API churn is real:** do NOT pin "latest" loosely - exact
  version in go.mod, and name the three API calls in the report.
  If the current release changed a shape this leaf text assumes,
  follow the library and report the deviation; never vendor an old
  API shape over the library's current one.
- The graceful-stop path matters: quic-go servers hold UDP sockets
  that the e2e harness must not leak. Wire the stop channel the same
  way the TCP listeners do, and add the h3 listener to whatever
  cleanup the harness startup uses.
