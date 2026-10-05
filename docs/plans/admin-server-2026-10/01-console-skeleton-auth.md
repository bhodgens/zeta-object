# Admin Server: Console Skeleton, Config, Session Auth - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the new binary, its fail-loud configuration with a loopback
  guard, the operator-token session with CSRF, the embed of the UI asset
  directory, the Makefile target, and the CI coverage-floor row.
- **Dependencies:** none
- **Estimated Context:** 60K
- **Concurrency Group:** A

## Goal

`make build` produces a console binary that refuses to start with a bad
configuration, binds loopback, and serves only the asset set and the
sign-in page to an unauthenticated caller. This leaf owns the skeleton;
the proxy routes are leaf 05 and the assets are leaf 03.

## Context

- The repo root package is the gateway (`main.go`); the module is
  `github.com/bhodgens/zeta-object`. The Makefile builds it as
  `zeta-object-server` (`BINARY_NAME`, target `build` around line 66).
- The gateway's admin surface (tree `management-api-2026-10`) is a
  loopback mTLS JSON API; this console is the browser-facing half.
- `.gitignore` already ignores the built binaries; add the console binary
  name there too.
- CI has a per-package coverage-floor table in
  `.github/workflows/check.yml`; a new package needs a row.
- `internal/auth` shows the repo's credential-comparison style
  (constant-time); reuse the approach for the operator token.

## Interface Contracts (From Parent)

### Contract 1: console configuration (you produce this)

JSON, decoded with `DisallowUnknownFields` (fail-loud, matching the
gateway's config style). Keys: `listenAddr`, `gatewayUrl`, `caFile`,
`clientCert`, `clientKey`, `operatorToken`, `allowNonLoopback`.

- An empty `operatorToken`, `gatewayUrl`, or any of the three TLS file
  paths is a startup abort naming the missing key.
- `listenAddr` must resolve to a loopback address (127.0.0.1, ::1,
  localhost) unless `allowNonLoopback` is true; otherwise abort naming the
  address. Port 0 is allowed (tests).
- Environment override for the config path: `ZETAOBJECT_ADMIN_CONFIG`
  (fall back to a documented default file name), matching how the gateway
  resolves `ZETAOBJECT_CONFIG`.

### Contract 3: session and CSRF (you produce this)

- Cookie `zeta_session`: `HttpOnly`, `SameSite=Strict`, `Secure`, value an
  HMAC-signed payload of expiry plus a random id; the HMAC key is random
  per process start so a restart invalidates every session.
- The session carries a CSRF token; mutating requests must present it in
  the `X-CSRF-Token` header.
- Pinned constants: session lifetime and idle expiry. No persistent state
  of any kind.

## Tasks

### Task 1: binary skeleton and config

**Files:**
- Create: `cmd/admin-server/main.go` (load config, build the server, run
  it with graceful shutdown on SIGINT/SIGTERM)
- Create: `internal/adminserver/config.go`, `config_test.go`

**Step 1: Write failing tests** (table-driven): a valid config loads; an
unknown key aborts; each missing required key aborts naming the key; a
non-loopback `listenAddr` aborts unless `allowNonLoopback`; a loopback
address and port 0 are accepted.

**Step 2:** `go test ./internal/adminserver/ -run TestConfig -count=1` ->
FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 2: session and CSRF

**Files:** Create `internal/adminserver/session.go`, `session_test.go`
Modify: `config.go` if the token comparison needs a helper.

**Step 1: Write failing tests:** a correct operator token mints a session
cookie with the three required attributes; a wrong token is refused (and
compared in constant time - assert with a timing-insensitive check that
the code path uses `subtle`); a tampered cookie is rejected; an expired
cookie is rejected; a restart-equivalent (new HMAC key) invalidates an old
cookie; a mutating request without the CSRF header is 403; with the wrong
token 403; with the right one it passes.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 3: embedded assets and the unauthenticated surface

**Files:**
- Create: `internal/adminserver/static.go`, `static_test.go`
- Create: `internal/adminserver/web/index.html` (placeholder shell if leaf
  03 has not landed yet: a minimal page that names the console)
- Modify: `cmd/admin-server/main.go` (wire the mux)

**Step 1: Write failing tests** (httptest):
- `GET /` returns 200 `text/html` with no session required;
- `GET /assets/<known file>` returns the embedded bytes with a sane
  content type; an unknown asset is 404;
- every `/api/...` path without a session returns 401 with the JSON error
  envelope (the routes themselves arrive in leaf 05; assert the guard is
  in front of them, for example with a placeholder handler registered in
  the same mux and replaced later);
- the login page is reachable without a session.

**Step 2:** FAIL. **Step 3: Implement** with `go:embed` over
`internal/adminserver/web/`. **Step 4:** PASS.

### Task 4: Makefile, ignore list, floor row

**Files:**
- Modify: `Makefile` (a `admin-server` target; `build` builds BOTH
  binaries; `clean` removes both)
- Modify: `.gitignore` (the console binary name)
- Modify: `.github/workflows/check.yml` (a floor row for the new package,
  measured and rounded down, in the existing comment style)

**Step 1:** `make build` must produce both binaries; run it.
**Step 2:** measure coverage for `./internal/adminserver/` and set the
floor accordingly. **Step 3:** verify `make test` still covers the new
package via `./...`.

**Step 4:** `make build` green; both files exist on disk.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./internal/adminserver/ -count=1` green
- [ ] `make build` produces both binaries; `make clean` removes both
- [ ] The console writes NO file at runtime (state is memory only)
- [ ] No secret, token, or certificate bytes are logged
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH respected: `internal/frontend/`, `config_store.go`,
      `s3_wiring.go`, `internal/bucketmanager/`, `internal/auth/`,
      `scripts/`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 1 satisfied: key names, fail-loud rules, loopback guard
- [ ] Contract 3 satisfied: cookie attributes, per-process HMAC key, CSRF
      header name, no persistent state
- [ ] Every unauthenticated surface is exactly the asset set and the login
      page - nothing else
- [ ] The gateway's own files are untouched

Output: APPROVED or specific gaps with file:line.

## Notes

- Do not put the console's code in the root package: it is a second binary
  and the root package is the gateway.
- `go:embed` requires the asset directory to exist at build time; leaf 03
  owns the real assets, so ship a minimal placeholder now and let it be
  replaced.
