# Admin Server: Proxy Routes, Session Gate, Static Serving - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the console's HTTP surface: the session gate, login and
  logout, the `/api/...` proxy, and static asset serving.
- **Dependencies:** 01 (config, session, embed), 02 (gateway client)
- **Estimated Context:** 60K
- **Concurrency Group:** B

## Goal

Contract 2 realised: an unauthenticated caller sees only the asset set and
the sign-in page; an authenticated caller gets a transparent proxy of the
gateway's management routes, with the gateway's statuses and bodies passed
through unchanged.

## Context

- Leaf 01 provides the config loader, the session/CSRF code, and the
  embedded asset set (root `GET /`, `/assets/{file}`, `GET /login`).
- Leaf 02 provides `internal/adminserver/gateway.Client` with one method
  per management route and a typed error carrying status, code, and
  message.
- The gateway's routes and shapes: `docs/plans/management-api-2026-10/master.md`
  Contract 5, implemented in `internal/frontend/admin/routes.go`.

## Interface Contracts (From Parent)

### Contract 2 (binding, restated)

Static: `GET /`, `GET /assets/{file}`, `GET /login`, `POST /login`,
`POST /logout`.

Proxy: `GET /api/status`, `GET /api/config`, `PUT /api/config`,
`POST /api/config/save`, `POST /api/auth/reload`, `GET /api/buckets`,
`POST /api/buckets`, `GET /api/buckets/{name}`,
`DELETE /api/buckets/{name}`, `PUT /api/buckets/{name}/settings`,
`POST /api/purge`.

- The proxy strips the `/api` prefix, forwards method, query, and body
  unchanged, and returns the gateway's status and body unchanged.
- Every `/api/...` route requires a valid session (401 with the JSON error
  envelope) and the CSRF header on mutating methods (403 when missing or
  wrong).
- An unknown `/api/...` path is 404 with the JSON envelope; a wrong method
  on a known path is 405 with the envelope.
- A gateway transport failure is 502 with the envelope carrying the reason
  (the console could not reach the gateway; it must not pretend the
  gateway answered).

## Tasks

### Task 1: the session gate

**Files:** Create `internal/adminserver/server.go`, `server_test.go`
Modify: `cmd/admin-server/main.go` if the wiring changes.

**Step 1: Write failing tests** (httptest, with a stub gateway):
- with no session, every `/api/...` route answers 401 with the envelope;
- with a valid session but no CSRF header, a mutating route answers 403;
- with both, the request reaches the gateway client;
- `POST /login` with the right token sets the cookie and returns 200;
  with the wrong token 401 and no cookie;
- `POST /logout` clears the cookie;
- the asset set and the login page remain reachable without a session.

**Step 2:** `go test ./internal/adminserver/ -run TestServer -count=1` ->
FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 2: the proxy

**Files:** modify the two files from Task 1.

**Step 1: Write failing tests** (one per Contract 2 row): the correct
gateway method and path are called; the response status and body are
returned byte-identically, including a 409 with code
`DatasetBucketNotDeletable` and a 400 with a validator message; a query
string survives; a request body survives unchanged; the gateway's
`Content-Type` is preserved when it is JSON.

**Step 2:** FAIL. **Step 3: Implement** one handler per route calling the
matching client method; no business logic, no reshaped JSON. Map the
typed gateway error to its status and envelope; a transport error becomes
502 with the envelope and the reason.
**Step 4:** PASS.

### Task 2b: console listener TLS and cookie policy (added during execution)

**Objective:** close a defect found in leaf 01: the console served plain
HTTP while stamping cookies `Secure`, which Safari rejects even on
loopback.

**Files:** `internal/adminserver/config.go`, `config_test.go`,
`server.go`, `session.go`, `session_test.go`, `cmd/admin-server/main.go`

**Requirements:**
- Console config gains OPTIONAL `certFile` / `keyFile` (its own server
  certificate, distinct from the mTLS client pair).
- Both set: serve HTTPS with `MinVersion` TLS 1.2; every cookie `Secure`.
- Neither set: serve plain HTTP, and `allowNonLoopback` must be false - a
  startup abort otherwise naming the reason; cookies `Secure` only when
  the request arrived over TLS (`r.TLS != nil`).
- The cookie attribute decision must be per response, driven by whether
  that response is on TLS, so both modes are correct.

**Step 1: Write failing tests:** TLS mode serves HTTPS and every cookie
carries `Secure`; plain loopback mode omits `Secure` on an HTTP request;
`allowNonLoopback` with no certificate refuses to start; `allowNonLoopback`
with a certificate starts and serves TLS.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

Also pin the login request shape: the sign-in page posts JSON with a
`token` field and offers a form fallback, so `POST /login` must accept the
token from a JSON body OR a form field named `token`.

### Task 3: routing and 404/405

**Files:** modify the two files from Task 1.

**Step 1: Write failing tests:** an unknown `/api/...` path is 404 with
the envelope; a wrong method on a known path is 405 with the envelope and
an `Allow` header; `GET /assets/../../etc/passwd` (and encoded variants)
is 404 and never escapes the embedded set.

**Step 2:** FAIL. **Step 3: Implement** a small explicit router rather
than a mux pattern that cannot express 405; keep the asset path resolver
strictly within the embedded FS.
**Step 4:** PASS.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./internal/adminserver/ -count=1` green
- [ ] Unauthenticated surface is exactly the assets and the login page
- [ ] Methods, queries, and bodies forwarded unchanged; statuses and
      bodies returned unchanged
- [ ] A gateway transport failure is 502 with a reason, never a fabricated
      success
- [ ] No path traversal out of the embedded asset set
- [ ] No token, cookie secret, or certificate material in any log line
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] Both listener modes correct: TLS stamps `Secure`, plain loopback does not
- [ ] `allowNonLoopback` without a certificate refuses to start
- [ ] `POST /login` accepts the token as JSON `{token}` or as a form field
- [ ] DO-NOT-TOUCH: `internal/frontend/`, `internal/adminserver/gateway/`
      (consume only), `internal/adminserver/web/` (leaf 03/04),
      `config_store.go`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 2 implemented row for row, including the 404/405/502 shapes
- [ ] The proxy adds nothing and removes nothing
- [ ] Session and CSRF checks run before any gateway call
- [ ] Static serving cannot escape the embed FS

Output: APPROVED or specific gaps with file:line.

## Notes

- Resist adding convenience endpoints: the console's value is that it is
  a faithful window onto the gateway's own surface.
- If a gateway method has no client counterpart because leaf 02 lagged,
  report it rather than calling the gateway with a hand-rolled request.
