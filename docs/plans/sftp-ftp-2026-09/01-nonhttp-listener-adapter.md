# 01 - Non-HTTP Listener Adapter + FrontendConfig Options - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. After writing a file, do NOT read
> it back to verify — write once and stop.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Minimal backward-compatible seam extension so raw-TCP frontends
  (FTP, SFTP) can host their own listener lifecycle; per-frontend `options`
  config map.
- **Dependencies:** none (auth-independent; may start before the auth tree lands).
- **Estimated Context:** 60K
- **Concurrency Group:** A (can run parallel with leaf 02)

## Goal

After this leaf:

1. `internal/frontend` exports the optional `NonHTTPFrontend` interface
   (Contract A in master.md). The frozen `Frontend` interface is untouched.
2. `main.go`'s dedicated-listener path type-asserts to `NonHTTPFrontend`:
   implementers get a raw `net.Listener` + `Serve` goroutine wired into the
   existing graceful-shutdown fan; non-implementers keep today's
   `http.Server` path byte-for-byte.
3. A `NonHTTPFrontend` configured WITHOUT `listenAddr` (i.e. shared-mux
   candidate) aborts startup with a clear error.
4. `FrontendConfig` gains `Options map[string]string` (omitempty) — decode
   only; key validation belongs to the owning factory (leaf 03/04).

## Exact files

- Modify: `internal/frontend/frontend.go` — add `NonHTTPFrontend` (additive only).
- New: `internal/frontend/nonhttp_test.go` — interface-compliance + doc tests.
- Modify: `config.go` — `FrontendConfig.Options` field.
- Modify: `config_frontend_test.go` — options decode round-trip cases.
- Modify: `frontends.go` — `mountFrontends` rejects a shared-mux mount of a
  `NonHTTPFrontend`; expose the split so `main.go` can see both kinds.
- Modify: `main.go` — dedicated-listener loop gains the non-HTTP branch
  (net.Listen + go Serve, tracked alongside `extraServers` for drain).
- New: `frontends_nonhttp_test.go` (package main) — fake NonHTTPFrontend
  proves: raw listener started, served, drained on SIGTERM path; shared-mux
  rejection error text; plain frontend unchanged.

## Test-first steps

1. Failing test: `TestNonHTTPFrontendInterfaceCompliance` — a fake type
   embedding `frontend.Frontend` + the three extra methods satisfies
   `frontend.NonHTTPFrontend`; a plain frontend does not (compile-time asserts).
2. Failing test (package main): `startupPlan` with a fake NonHTTP frontend
   WITHOUT listenAddr returns an error mentioning the frontend name and
   "requires its own listenAddr".
3. Failing test (package main): fake NonHTTP frontend WITH listenAddr —
   after starting the plan's non-HTTP listeners, a TCP dial connects and the
   fake's Serve saw the conn; graceful shutdown closes the listener and Serve
   returns.
4. Failing test (config): `{"type":"x","listenAddr":":9000","options":{"a":"b"}}`
   decodes; absent options decodes to nil.
5. Implement minimally until green.

## Done-when

- `go test ./...` green; existing s3-only startup behavior tests untouched
  and passing (backward compatibility proven by them, not by assertion).
- `make lint` clean.
- No change to `internal/frontend.Frontend` method set or `Registry`.
