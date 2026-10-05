# Admin Server: Gateway Client - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** a typed mTLS JSON client over the gateway's management API.
- **Dependencies:** none
- **Estimated Context:** 50K
- **Concurrency Group:** A

## Goal

One place that knows how to talk to the gateway's admin listener: build
the mTLS transport from the console config, call each management route,
decode the JSON, and surface the gateway's own error envelope unchanged so
the UI can show the gateway's message verbatim.

## Context

- The gateway's management routes and their shapes are documented in
  `docs/plans/management-api-2026-10/master.md` (Contract 5) and in
  `README.md`'s Management API section; the implementation is
  `internal/frontend/admin/routes.go` and `admin_wiring.go`.
- The error envelope is an object with a top-level `error` key holding
  `code` and `message` strings.
- The gateway verifies the client certificate against its `clientCAFile`
  and uses the certificate's common name as the audit principal, so the
  console's certificate common name is what shows up in the gateway's
  audit log.

## Interface Contracts (From Parent)

Contract 1 (config) supplies `gatewayUrl`, `caFile`, `clientCert`,
`clientKey`. This leaf consumes those and produces:

```go
// internal/adminserver/gateway (NEW package)
func New(cfg Config) (*Client, error)

type Config struct {
    BaseURL    string
    CAFile     string
    ClientCert string
    ClientKey  string
}
```

Methods, one per proxied route: `Status`, `GetConfig`, `PutConfig`,
`SaveConfig`, `ReloadAuth`, `ListBuckets`, `CreateBucket`, `GetBucket`,
`DeleteBucket`, `PutBucketSettings`, `Purge`.

Each returns the decoded body plus an error. A non-2xx response becomes a
typed error carrying the HTTP status, the gateway's `code`, and the
gateway's `message`, so callers can re-emit the envelope unchanged. A
transport failure (bad certificate, refused connection, timeout) is a
distinct error type carrying a human-readable reason and never a panic.

## Tasks

### Task 1: mTLS transport and error mapping

**Files:** Create `internal/adminserver/gateway/client.go`,
`client_test.go`

**Step 1: Write failing tests** against an `httptest` TLS server built
from in-test certificates (`crypto/x509`, no openssl):
- a client built with the CA and client pair connects to a server that
  requires and verifies client certificates;
- a client WITHOUT a certificate is refused by such a server (proving the
  transport really presents the pair);
- a server error body with the error envelope becomes a typed error whose
  code and message round-trip exactly;
- a 401 (missing certificate on the console's side) and a 500 both map to
  the typed error, not to a panic;
- a timeout is reported as a transport error naming the URL.

**Step 2:** `go test ./internal/adminserver/gateway/ -count=1` -> FAIL.
**Step 3: Implement** with `crypto/tls` (`MinVersion` TLS 1.2, the CA
pool, the client pair), a bounded HTTP client timeout, and a JSON error
decoder. **Step 4:** PASS.

### Task 2: the route methods

**Files:** modify the two files from Task 1.

**Step 1: Write failing tests** (table-driven against the stub server):
for each method, assert the request METHOD, PATH, QUERY, and BODY the
client sends, and that the decoded response matches the fixture. Include
the pass-through cases where the gateway answers 409 with a code (bucket
delete refusal) and 400 (config validation), asserting code and message
survive unchanged.

**Step 2:** FAIL. **Step 3: Implement** one thin method per route; no
business logic in this package (no retries, no caching, no reshaping).
**Step 4:** PASS.

### Task 3: certificate reload

**Files:** modify the two files from Task 1.

**Step 1: Write failing tests:** after the certificate files on disk are
replaced and a reload is requested, a NEW request uses the new certificate
(assert the server sees the new common name); a reload with unreadable
files keeps the previous certificate working and reports the error.

**Step 2:** FAIL. **Step 3: Implement** an atomic swap of the TLS config
(the gateway's own admin code uses `GetConfigForClient` for the same
reason - never mutate a config after first use). **Step 4:** PASS.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./internal/adminserver/gateway/ -count=1` green
- [ ] The gateway's error envelope round-trips byte-exactly (assert the
      message, not just the code)
- [ ] No business logic in this package (no retry, no cache, no reshape)
- [ ] The client certificate is never logged or returned
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH: `internal/frontend/` (read the routes as a reference
      only), `internal/adminserver/web/` (leaf 03), `cmd/admin-server/`
      (leaf 01), `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract satisfied: package path, `New` plus `Config`, one method
      per proxied route
- [ ] The typed error carries status, code, and message
- [ ] TLS is verified (CA pool plus client pair); no `InsecureSkipVerify`
- [ ] Certificate reload is atomic and never mutates a config in use

Output: APPROVED or specific gaps with file:line.

## Notes

- Keep this package free of HTTP-server concerns: it is a client, and the
  console's own surface lives in leaf 05.
- Do not add convenience helpers that reshape the gateway's JSON; the UI
  is written against the gateway's shapes.
