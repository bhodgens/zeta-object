# QUIC/HTTP-3 Transport + WebDAV Range - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 4 leaf documents (flat)
- **Scope:** Two server capabilities that together serve the zeta-cache
  client (the $HOME cache daemon, FUSE-only v1 on Linux and macOS):
  (1) Range GET (206) on the WebDAV frontend, so the cache daemon can
  hydrate partial files; (2) an HTTP/3 (QUIC) transport listener that
  serves the same WebDAV handler over QUIC, advertised via `Alt-Svc`,
  for wifi and distant-link clients where QUIC's loss recovery and
  stream multiplexing are worth real latency.

## User decisions (FINAL, 2026-10-04)

1. **QUIC is in scope.** The gains over wifi and distant links justify
   the transport work. Transport only: HTTP/3 (RFC 9114) carrying the
   EXISTING WebDAV semantics. No new file protocol, no custom
   application protocol over raw QUIC streams.
2. **Client protocol is WebDAV.** zeta-cache (the future FUSE cache
   daemon) speaks WebDAV against zeta-object, not S3: directories,
   mtime fidelity, and atomic MOVE match a filesystem workload; S3
   remains the interop surface (rclone/mc/boto3) unchanged.
3. **Client platform scope:** FUSE-only v1 on Linux and macOS. The
   iCloud-grade macOS FileProvider extension is future work recorded in
   issue #14 (docs/plans/issue-fileprovider-macos-client.md).
4. **Client authentication is a CERTIFICATE (mTLS), user decision
   2026-10-04.** The h3 frontend verifies a client certificate
   against a configured CA (`RequireAndVerifyClientCert`); the
   certificate CN is the principal and maps into the existing
   identity registry, so bucket grants and audit attribution work
   unchanged. Per-device certificates (CN = device name). No
   passwords on the cache path at all. This mirrors the admin
   surface's credential model; unlike admin, unknown CNs are an
   identity-registry question (401), and the endpoint is not
   loopback-restricted. Alt-Svc (RFC 7838) stays the only discovery
   mechanism - no special negotiation beyond the TLS handshake.
   Plain-TCP webdav keeps Basic auth for ad-hoc clients (Finder,
   rclone); the two credential models coexist per frontend entry.

## Goal

- A WebDAV GET/HEAD with a valid `Range` header answers 206 with the
  requested bytes and a `Content-Range` header, using the SAME range
  grammar and semantics the S3 frontend already pins (multirange case
  29, RFC 9110 classification).
- A configured `h3` frontend serves the WebDAV handler over HTTP/3 on
  its own UDP listener, reusing the process cert pair, and the
  existing HTTP(S) frontends advertise it with an `Alt-Svc` response
  header so an h3-capable client migrates automatically and falls back
  to TCP when UDP is blocked.
- Honest protocol-compatibility and README documentation for both
  capabilities, and a live-ZFS validation run closing the AGENTS.md
  rule (the change touches the data plane).

Non-goals (explicitly deferred): SMB-over-QUIC and NFS-over-QUIC (no
cross-platform client story / not implemented anywhere); raw-QUIC
custom protocols; 0-RTT early data (replay-safety analysis deferred;
the v1 listener uses QUIC v1 with standard 1-RTT); S3-over-h3 (the h3
frontend serves the webdav handler only in v1); the zeta-cache client
itself (separate effort).

## Architecture

- **WebDAV Range (leaf 01).** The webdav frontend's `serveObject`
  (`internal/frontend/webdav/get.go:60-92`) today answers 200 with the
  full body and never reads the `Range` header. The S3 frontend already
  owns the full range machinery: `ParseMultiRange` and `CoalesceRanges`
  in `internal/frontend/s3/rangespan.go` (exported, pinned by unit
  tables and e2e case 29 with the RFC 9110 malformed vs
  unsatisfiable classification). Leaf 01 moves nothing: the webdav
  package imports the s3 package's range parser (both are
  `internal/`, an import does not touch the frozen Backend seam), or,
  if importing s3 from webdav creates a cycle risk, the parser moves
  to a new leaf package `internal/rangespan` with the s3 package
  re-pointed at it and its tests moved in the SAME leaf. Preference
  order: try the direct import first; move only on a real cycle.
  Response shapes mirror the S3 frontend exactly: single-span 206 with
  `Content-Range: bytes a-b/size`; multi-span 206
  `multipart/byteranges` via the S3 `serveMultiRangeIfNeeded` pattern;
  syntactic-malformed header ignored (200 full body); all-unsatisfiable
  416 with `Content-Range: bytes */size`. `Capabilities()` already
  declares `ConditionalReads: true` for webdav
  (`internal/frontend/webdav/frontend.go:127`); that flag stays
  truthful and gains range support under the same umbrella.
- **HTTP/3 frontend (leaf 02).** Dependency `quic-go/quic-go` (pure
  Go, no cgo - static-binary property holds; verify with a
  CGO_ENABLED=0 probe build in the leaf, the same discipline as the
  sqlite approval). A NEW optional interface on the frozen frontend
  seam, `QUICListenerFrontend`, mirrors the existing
  `TLSListenerFrontend` precedent (`internal/frontend/frontend.go:57`):
  the frozen `Frontend` interface gains NO methods. The `h3` frontend
  package builds a webdav frontend internally (same constructor call
  the `webdav` factory entry uses in `frontends.go:52-62`) and serves
  it through `http3.Server` over a QUIC listener on its own UDP
  address. `main_server.go` gains the UDP branch: when a frontend
  implements `QUICListenerFrontend`, main opens a
  `quic.EarlyListener` (or `http3` equivalent) with the frontend's
  pre-built `*tls.Config` instead of a TCP TLS listener. Alt-Svc:
  when an h3 frontend is mounted, main wraps every HTTP frontend
  handler with a small middleware adding
  `alt-svc: h3=":<h3-port>"` (RFC 7838) - additive headers only, no
  frontend code changes for advertisement.
- **Charter compliance.** Nothing persistent is added. QUIC connection
  state is in-memory transport state, not server-owned storage; no new
  sidecar, index, or database. The zfs events / metadata surfaces are
  untouched.
- **Config surface.** One new frontend type `h3` with the same
  `FrontendConfig` shape (`type`, `listenAddr` - UDP host:port,
  `bucket`, `options`). No new option keys in v1: the process
  `certFile`/`keyFile` pair is reused (HTTP/3 requires TLS 1.3; the
  MinVersion pin comes from the h3 package). An explicit `frontends`
  array that adds `h3` must also carry the explicit `s3`/`webdav`
  entries it wants (the existing rule that an explicit list disables
  the default S3 mount) - the harness config in leaf 03 shows it.

## Interface Contracts

### Contract 1: WebDAV Range semantics (leaf 01, consumed by leaf 03 docs)

```go
// internal/frontend/webdav/get.go (behavior, not new API):
// A GET/HEAD on a FILE resource with a Range header:
//   - single satisfiable span            -> 206, Content-Range, span bytes
//   - multiple satisfiable specs         -> 206 multipart/byteranges
//                                           (S3 serveMultiRangeIfNeeded shape)
//   - syntactic-malformed header         -> 200 full body (header ignored,
//                                           RFC 9110, matches S3 case 29)
//   - all-unsatisfiable                  -> 416, Content-Range: bytes */size
// A Range on a COLLECTION resource is ignored (200, empty body) -
// collections have no bytes. HEAD mirrors GET status/headers, no body.
// If-None-Match evaluation happens BEFORE Range: a 304 never carries a body
// (RFC 9110 13.1.2 precedence: If-None-Match wins).
```

- The parser is `s3.ParseMultiRange` / `s3.CoalesceRanges` (or the
  moved `internal/rangespan` twin - the move is allowed only with the
  s3 tests moved in the same leaf and the s3 package's behavior
  byte-identical, proven by its existing unit tables passing
  unmodified).
- The webdav get_test.go table gains: single span, suffix span
  (`bytes=-N`), open-ended span (`bytes=N-`), malformed (200),
  unsatisfiable (416), multi-span multipart, range-on-collection (200
  empty), If-None-Match + Range together (304 wins), HEAD with Range
  (206, no body), Content-Length equals span length.
- The frozen `internal/backend/backend.go` seam is untouched: Get
  streams via `io.Seeker`-free copy; if the fs backend Get cannot
  seek, leaf 01 reads the span by streaming and discarding the prefix
  OR adds an offset/length read at the EXISTING `GetOptions` shape if
  one exists - it must NOT add methods to `Backend`. Read the fs
  backend's Get signature first and pick the honest option; report
  the choice.

### Contract 2: QUIC listener seam (leaf 02, consumed by main)

```go
// internal/frontend/frontend.go (ADD; Frontend itself is untouched):
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

- `mountFrontends` (`frontends.go:377-414`) REJECTS a
  `QUICListenerFrontend` configured without its own `listenAddr`
  (same loud rule as `TLSListenerFrontend`, same error style naming
  the rule).
- `main_server.go`: a `QUICListenerFrontend` is served on UDP via the
  quic-go listener API with the pre-built config - never through the
  TCP `ListenAndServeTLS` path. The goroutine, shutdown, and startup
  error handling follow the dedicated-listener pattern already there.

### Contract 3: the h3 frontend (leaf 02, consumed by leaf 03)

```go
// internal/frontend/h3 (NEW package):
// Name() is "h3". The frontend WRAPS a webdav frontend built with the
// same constructor and options the "webdav" factory entry uses
// (Basic auth over the identity registry, lock store root). It is a
// QUICListenerFrontend, not a shared-mux frontend: Handler() exists
// for the seam but is never mux-mounted.
// Capabilities() delegates to the wrapped webdav frontend.
```

- Factory entry `frontendFactories["h3"]` in `frontends.go`. Option
  keys validated fail-loud through the existing `validateOptions`
  helper: `clientCAFile` (REQUIRED, PEM bundle of the trusted client
  CA(s) - read at startup; an unreadable path aborts startup naming
  the file). No other option keys in v1. An unknown option key
  aborts startup naming the key and the known set.
- mTLS: the h3 TLS config sets `RequireAndVerifyClientCert` with
  ClientCAs from `clientCAFile` (the pattern
  `internal/frontend/admin/mtls.go` already proves). The principal is
  the certificate Subject CN, resolved through the identity registry
  by the `CertAuthenticator` (`internal/auth/cert.go`): known CN ->
  that identity's bucket grants; unknown or empty CN -> 401
  fail-closed. Per-device certificates: the operator issues one cert
  per device, CN = device name.
- The loopback guard does NOT apply to h3 (that is an admin-surface
  control; the h3 listener serves off-host cache clients by design -
  authorization is the certificate + bucket grants).
- Known-types strings: adding the `h3` type changes the startup error's
  known list. GREP and update every pinned copy: the literal
  `known: [` appears in root-package tests and in
  `scripts/e2e/` (the `internal/frontend/ftp/frontend.go` and
  `internal/frontend/sftp/frontend.go` copies are their own files'
  error text - check whether they enumerate the global set and update
  only if the test pins the global set). Every pinned literal gets the
  new type IN THE SAME LEAF.
- Alt-Svc middleware: when any mounted frontend is a
  `QUICListenerFrontend`, main wraps each HTTP-serving frontend's
  handler so every response carries
  `alt-svc: h3=":<h3-port>"; persist=1`. Purely additive headers. The
  h3 frontend's own responses do not carry the header. A unit test
  pins: header present on webdav-over-TCP responses when h3 is
  configured, absent when it is not.

### Contract 4: dependency and build discipline (leaf 02)

- `quic-go/quic-go` is added to `go.mod` with a PINNED version (pin
  exact, not minor-range: the quic-go API has churned across
  versions). The leaf records the chosen version and the API shapes it
  uses (listener construction, server serving, graceful stop) in its
  report, because the orchestrator will pin the same shapes in leaf 03
  and 04 probe code.
- After `go mod tidy`, the leaf proves the static-binary property:
  `CGO_ENABLED=0 go build ./...` succeeds (the sqlite-approval probe
  pattern). A cgo dependency is a FAILED leaf, full stop.

### Contract 5: e2e + docs (leaf 03)

- NEW e2e case `scripts/e2e/cases/37-h3-webdav.sh` following the
  harness conventions (`BKT=` bucket convention, create/cleanup
  pairing, lib.sh assert helpers). The case needs an HTTP/3 CLIENT:
  stock curl rarely ships h3 enabled, so the case compiles and runs a
  small Go probe at `scripts/e2e/h3probe/main.go` (`go run`, the
  toolchain is present because the harness builds the server). The
  probe speaks the pinned quic-go API from Contract 4 and performs:
  PUT, full GET, single-span Range GET (206 + Content-Range assert),
  PROPFIND depth-1 - the probe presents a per-device CLIENT
  CERTIFICATE over h3 (leaf 02 pins the clientCAFile option; the
  harness generates a CA + one client cert the way case 35's mTLS
  setup does), a no-cert connection fails the handshake, and the
  TCP-mode 401 covers Basic auth rejection - asserting
  status lines and key headers, printing a PASS/FAIL tally, exiting
  non-zero on failure.
- The harness config for the case carries an EXPLICIT frontends array:
  webdav (TCP), s3 (default mount entry), and h3 - per the
  explicit-array-disables-default rule in the repo skill.
- Graceful skip: if `go` is unavailable at case runtime the case
  SKIPs (the harness itself needs go to build the server, so in
  practice it always runs).
- Docs in the same leaf: README transport section (mechanism before
  name: QUIC is UDP-based transport with built-in encryption; HTTP/3
  is HTTP over QUIC; Alt-Svc is how a client discovers the UDP
  endpoint), `docs/protocol-compatibility.md` rows for webdav Range
  (implemented, with the case 37 + case proof links) and h3 transport
  (implemented, degrades-to-TCP semantics documented), and the
  frontend-type lists in README config examples that enumerate
  types. House copy rules: hyphens not em-dashes, no marketing
  words; grep your own diff for em-dashes before reporting.

### Contract 6: live validation (leaf 04)

- The change touches the data plane (a new listener serving object
  data), so AGENTS.md's ZFS validation rule applies. Extend
  `scripts/zfs-validate/run-zfs-validation.sh` with an h3 section:
  server config on zfs-meta gains the h3 frontend (UDP port),
  `checks.py` gains h3probe-driven checks mirroring the e2e case
  asserts PLUS the metadata round-trip (`?events`,
  `?events&versions`) fetched OVER HTTP/3, compared against
  `zfs events -j` ground truth as the existing sections do.
- The webdav Range checks ride the same run: a Range GET against the
  ZFS-backed bucket asserted byte-for-byte against the full-body
  slice.
- Bracket one char of every pkill pattern (repo skill rule); the
  server process name and UDP port cleanup follow the existing
  harness patterns. ALL sections green including pre-existing ones;
  report the exact tally; max 3 fix+rerun cycles then report.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-webdav-range.md | leaf | none | 60K | A |
| 02 | 02-h3-frontend.md | leaf | none (disjoint files from 01) | 80K | A |
| 03 | 03-e2e-case-docs.md | leaf | 01, 02 | 60K | B |
| 04 | 04-live-validation.md | leaf | 03 | 40K | C (live host) |

**Concurrency groups:** A: 01 and 02 simultaneously (disjoint files:
01 owns `internal/frontend/webdav/get.go` + a possible new
`internal/rangespan` package; 02 owns `go.mod`/`go.sum`,
`internal/frontend/frontend.go`, `frontends.go`, `main_server.go`,
and the new `internal/frontend/h3` package). If leaf 01 must move
rangespan, the move touches `internal/frontend/s3/` - still disjoint
from 02. B: 03 after both (probe pins leaf 02's quic-go API shapes;
docs pin leaf 01's semantics). C: 04 last, on zfs-meta -
`run-zfs-validation.sh` is the highest-collision file in the repo:
before dispatching, check `git log --oneline -3 -- 
scripts/zfs-validate/run-zfs-validation.sh` and `git status` for
sibling hunks; if a sibling owns it, WAIT or run leaf 04 in-session.

## Dispatch Protocol

For each concurrency group, in dependency order:

### Phase 1: Dispatch Concurrency Group A

Dispatch 01 and 02 simultaneously via `delegate_task`:

1. **Read** the leaf, paste the FULL leaf text plus the contracts it
   consumes and produces. Include the repo Coding Conventions block
   below. Include: "Do NOT commit. Do NOT run git add."
2. DO-NOT-TOUCH list (sibling WIP may be dirty in the worktree):
   `scripts/zfs-validate/run-zfs-validation.sh`,
   `internal/frontend/s3/reflinkversions.go`,
   `internal/frontend/s3/versioning_handlers.go`, any file in the
   sibling leaf's ownership list above. Stage explicit paths only.
3. quic-go API shapes are pinned by leaf 02's report, not guessed by
   leaf 03 - do not dispatch 03 until 02's dependency-version and API
   report is in the master's tracking table.

### Phase 2: Review and Commit Each Child

After each agent returns, the orchestrator reviews in-session:

1. Read the changed files; check against the leaf spec and contracts.
2. RE-RUN the gates in the parent (subagent gate reports are
   self-claims): `go test ./... -count=1`, `make lint
   NEW_FROM_REV=HEAD` (0 findings), `gofmt -l` clean on changed files.
   Leaf 02 additionally: `CGO_ENABLED=0 go build ./...` in the parent.
3. Commit explicit paths only (never `git add -A`).
4. Gaps -> re-dispatch with findings; max 3 cycles, then escalate.

### Phase 3: Dispatch Group B, then C

- Leaf 03 after 01 and 02 are COMMITTED.
- Leaf 04 after 03 is committed, on zfs-meta, solo (the harness
  recreates testpool/zval; two suites on the host at once corrupt the
  run).

### Phase 4: Integration Review

1. Full gates: `make test && make lint NEW_FROM_REV=HEAD && make e2e
   && make parity-test`.
2. `grep -rcE '^\s+[0-9]+\|' --include='*.go' .` returns zero
   (no line-number corruption).
3. Frozen seams untouched: `git diff --stat` must not list
   `internal/backend/backend.go` or `internal/metadata/metadata.go`;
   the `Frontend` interface in `internal/frontend/frontend.go` is
   additive-only (only `QUICListenerFrontend` added).
4. Update the tracking table; report.

## Review Checklist

- [ ] All tasks from each leaf document are implemented
- [ ] Contracts 1-6 satisfied exactly (range status codes, interface
      names, option keys, Alt-Svc header, probe asserts)
- [ ] `internal/backend/backend.go` and `internal/metadata/metadata.go`
      UNTOUCHED
- [ ] `internal/frontend/frontend.go` Frontend interface UNTOUCHED
      (only the additive QUICListenerFrontend)
- [ ] S3 range behavior byte-identical (existing s3 range tests and
      case 29 unmodified and passing)
- [ ] webdav conditional-read precedence: If-None-Match before Range,
      304 carries no body
- [ ] No cgo: CGO_ENABLED=0 build green; quic-go version pinned
- [ ] Alt-Svc present on HTTP responses only when h3 is configured
- [ ] Tests written and passing (TDD followed); coverage floors
      re-measured if code moved packages (the rangespan move, if it
      happened, is the case to check)
- [ ] No scope creep, no debug artifacts, no line-number corruption

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go (repo go.mod version); exported PascalCase,
  unexported camelCase; imports grouped stdlib / third-party / local.
- **Errors:** wrap with `%w`; sentinel vars for wire-mapped conditions;
  no `panic` in library paths; ignored-error and empty-branch forms are
  HOOK-BANNED (`_ = err`, `if err != nil {}`).
- **Go 1.27 modernize forms:** `errors.AsType[T]`, `new(expr)`,
  `for i := range n`, `wg.Go`.
- **Testing:** table-driven; seams are package-level vars the tests
  replace; tests live in the package under test.
- **Formatting:** `gofmt` before reporting; `make lint
  NEW_FROM_REV=HEAD` must be 0 (bare `make lint` shows sibling
  findings).
- **Static binary:** NO cgo, ever.
- **Secrets:** never log a secret; never return one in a body.

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-webdav-range | PENDING | | |
| 02-h3-frontend | PENDING | | |
| 03-e2e-case-docs | PENDING | | |
| 04-live-validation | PENDING | | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. `make test` - full unit suite green (new: webdav range table,
   QUIC seam rejection tests, h3 construction/factory tests, Alt-Svc
   middleware tests).
2. `make lint NEW_FROM_REV=HEAD` - 0 findings.
3. `make e2e` - full suite green; case 37 runs the h3probe against
   the case server and SKIPs only where go is unavailable.
4. `make parity-test` - metadata parity untouched.
5. Live: leaf 04's harness section on zfs-meta - HTTP/3 against the
   real server including the metadata round-trip over h3, and the
   ZFS-backed Range GET byte-check; ALL checks green including
   pre-existing sections; exact tally reported.

## Structural Completeness Check (Before Dispatch)

Run: `python3 ~/.hermes/skills/software-development/hierarchical-planning/scripts/check_template_compliance.py docs/plans --strict-leaves`
(against `docs/plans` - the PARENT; a flat-leaf tree scanned alone is
misread as a forest root).

## Open Questions

- None blocking. The scoping decisions were made by the user
  2026-10-04 (see User decisions above).
- Deferred (not in scope): S3-over-h3; 0-RTT early data; SMB/NFS-over-
  QUIC; the zeta-cache client itself; per-listener certificate pairs.

## Notes

- **Sibling WIP is live in this worktree.** `git status` at authoring
  time shows untracked: `docs/plans/zfs-bucket-datasets-2026-10/`,
  `internal/frontend/webdav/dbg_test.go`, `scripts/e2e/run-one.sh`,
  `testdata/fuzz/FuzzGetCanonicalURI/`, `.claude/`. Stage explicit
  paths; verify `git diff --cached` hunk-by-hunk before every commit;
  never sweep sibling files.
- **Why UDP listen is a new seam and not TLSListenerFrontend:** that
  interface's caller path opens a TCP TLS listener; a QUIC frontend
  needs a UDP packet conn and the quic-go listener. Reusing the
  interface would wedge a UDP listener through a TCP code path. The
  optional-interface precedent makes the new interface the cheap,
  honest shape.
- **Why webdav and not S3 for the h3 frontend:** the cache client
  decision (user, 2026-10-04) is WebDAV. Serving S3 over h3 is a
  future increment that adds a second wrapped frontend; nothing in
  this tree's shapes prevents it.
- **The `dbg_test.go` file in webdav is UNTRACKED sibling WIP** -
  leaf 01 must not delete, stage, or depend on it.
- **Coverage floors:** root package floor is 79 (ratcheted
  77 -> 79 by 7d82e3f). Leaf 02 edits `main_server.go` and
  `frontends.go` in the root package: if measured coverage drops
  below the floor, write the missing tests - never lower the floor
  (AGENTS.md).
- **QUIC on wifi, stated plainly for the docs leaf:** QUIC recovers
  from packet loss without stalling every other transfer (TCP's
  head-of-line blocking is the thing it removes), and connection
  establishment merges transport and TLS setup into one round trip.
  On a lossy wifi link or a high-latency WAN path, many small
  concurrent reads (a cache daemon hydrating a directory) complete
  sooner. On a clean LAN the gain is small; the feature is aimed at
  wifi and distant links.
