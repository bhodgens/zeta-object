# Management API: Admin Frontend + mTLS Listener - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the TLS-listener seam extension, the `admin` frontend
  package (transport + client-certificate authentication), the loopback
  guard, and the factory entry.
- **Dependencies:** none
- **Estimated Context:** 70K
- **Concurrency Group:** A

## Goal

An `admin` frontend that listens on its own address with TLS client
certificate verification, extracts the certificate principal, and serves
a JSON surface. This leaf delivers transport and authentication only:
the route implementations for configuration and buckets arrive in leaf
04 through the injected `Services` value.

## Context

- `internal/frontend/frontend.go` is the frozen protocol seam. The
  optional-interface precedent is `NonHTTPFrontend` (`frontend.go:28-44`):
  an optional extension the frozen interface knows nothing about, handled
  by main. Follow that pattern exactly.
- `frontends.go:41-123` is the factory table; `:249-253` defines
  `listenerSpec`; `:322-343` `mountFrontends` splits shared-mux mounts
  from dedicated listeners and REJECTS a second shared mount.
- `main_server.go:55-65` starts each dedicated listener with
  `es.ListenAndServeTLS(serverConfig.CertFile, serverConfig.KeyFile)`.
  That call builds its own TLS config from the cert pair, so it cannot
  express client-certificate verification. It must be replaced for
  TLS-config frontends by a listener served with a pre-built config.
- `main.go:45-58` `newServer` shows the TLS hardening baseline
  (`MinVersion: tls.VersionTLS12`); the admin listener must match it.
- e2e certificate generation exists at `scripts/e2e/run-e2e.sh:36`
  (openssl self-signed); leaf 06 extends it with a CA plus a client
  certificate.

## Interface Contracts (From Parent)

### Contract 1: the listener seam (you produce this)

```go
// internal/frontend/frontend.go (ADD; the Frontend interface above it is
// UNTOUCHED):
type TLSListenerFrontend interface {
	Frontend
	Addr() string
	TLSConfig() (*tls.Config, error)
}
```

`Addr()` empty is a construction error, never a shared-mux fallback.

### Contract 2: the admin frontend (you produce this)

```go
// internal/frontend/admin (NEW package):
func New(opts Options) (frontend.Frontend, error)

type Options struct {
	ListenAddr       string
	ClientCAFile     string        // REQUIRED; PEM bundle
	AdminPrincipals  []string      // optional allow-list of CNs
	AllowNonLoopback bool
	Services         Services      // injected by main (leaf 04 fills it)
}

// Services is the route surface. Every field may be nil in this leaf;
// a nil service answers 503 {"error":{"code":"NotImplemented",...}}.
type Services struct {
	Status        func(ctx context.Context) (StatusReport, error)
	GetConfig     func(ctx context.Context) (json.RawMessage, error)
	PutConfig     func(ctx context.Context, patch json.RawMessage) (ConfigApplyResult, error)
	SaveConfig    func(ctx context.Context) error
	ReloadAuth    func(ctx context.Context) error
	ListBuckets   func(ctx context.Context) (json.RawMessage, error)
	CreateBucket  func(ctx context.Context, name string) error
	DeleteBucket  func(ctx context.Context, name string) error
	BucketDetail  func(ctx context.Context, name string) (json.RawMessage, error)
	BucketSettings func(ctx context.Context, name string, patch json.RawMessage) error
	Purge         func(ctx context.Context, dataset string) error
}
```

Routes served in THIS leaf: `GET /status` (from `Services.Status`, else
503) and a 404 JSON shape for everything else. Leaf 04 supplies the
remaining services and routes.

### Contract 3: factory entry and options (you produce this)

- `frontendFactories["admin"]` builds the frontend from
  `FrontendConfig`. Option keys, validated fail-loud through
  `validateOptions` (`frontends.go:127-139`):
  `clientCAFile` (required), `adminPrincipals` (optional, comma
  separated), `allowNonLoopback` (optional, `"true"`).
- Unknown option key -> startup error naming the key and the known set.
- `clientCAFile` missing or unreadable -> startup error naming the path.
- The CA file is read at startup AND on every `POST /auth/reload`
  (leaf 04 calls the same reload entry), so revocation does not require
  a restart.

## Tasks

### Task 1: the TLS listener seam

**Objective:** a frontend can own its listener's TLS configuration.

**Files:**
- Modify: `internal/frontend/frontend.go` (add the interface only)
- Modify: `frontends.go` (`listenerSpec` carries the frontend; reject a
  `TLSListenerFrontend` without its own `listenAddr`)
- Modify: `main_server.go` (dedicated-listener goroutine serves a
  pre-built TLS config when the frontend supplies one)

**Step 1: Write failing tests** (package main or the existing
`frontends_nonhttp_test.go` style):
- a stub `TLSListenerFrontend` configured without `listenAddr` is
  rejected by `mountFrontends` with an error naming the rule;
- its `TLSConfig()` is carried into the `listenerSpec`;
- `Addr()` empty is rejected at construction.

**Step 2:** `go test . -run TestTLSListener -count=1` -> FAIL.

**Step 3: Implement.** The listener must be served with the pre-built
config (for example `srv.ServeTLS(l, "", "")` after
`tls.NewListener(l, cfg)`), never `ListenAndServeTLS(cert, key)`, which
would silently drop `ClientCAs` and `ClientAuth`.

**Step 4:** PASS; the whole root package green.

### Task 2: certificate verification and the principal

**Objective:** a request is authenticated by the client certificate or
it is 401.

**Files:**
- Create: `internal/frontend/admin/mtls.go`
- Test: `internal/frontend/admin/mtls_test.go`

**Step 1: Write failing tests** (generate certs in-test with
`crypto/x509` self-signed fixtures - no openssl dependency in unit tests):
- no client certificate -> 401, JSON body, no transport detail leaked;
- certificate signed by a DIFFERENT CA -> 401;
- expired certificate -> 401;
- valid certificate -> 200, and the principal is the Subject CN;
- valid certificate with an EMPTY CN -> 401 (fail closed);
- allow-list configured and CN absent from it -> 403;
- allow-list configured and CN present -> 200.

**Step 2:** `go test ./internal/frontend/admin/ -run TestMTLS -count=1` -> FAIL.

**Step 3: Implement** `TLSConfig()` building:
```go
&tls.Config{
	MinVersion: tls.VersionTLS12,
	ClientAuth: tls.RequireAndVerifyClientCert,
	ClientCAs:  pool, // from ClientCAFile
	Certificates: []tls.Certificate{serverCert}, // the process pair
}
```
and the handler wrapper that reads `r.TLS.PeerCertificates[0]`.

**Step 4:** PASS.

### Task 3: loopback guard

**Objective:** the admin listener is not reachable off-host by accident.

**Files:**
- Create: `internal/frontend/admin/listen.go` (guard function)
- Test: `internal/frontend/admin/listen_test.go`

**Step 1: Write failing tests:** `127.0.0.1:9000`, `[::1]:9000` and
`localhost:9000` pass; `0.0.0.0:9000`, `:9000`, `10.0.0.5:9000` fail
without the option and pass with `AllowNonLoopback`; an unresolvable
host fails.

**Step 2:** FAIL. **Step 3: Implement** (resolve the host with
`net.LookupIP` and require every result to be loopback).

**Step 4:** PASS.

### Task 4: frontend skeleton, factory entry, 503 defaults

**Objective:** the `admin` type is configurable and serves `GET /status`.

**Files:**
- Create: `internal/frontend/admin/admin.go` (Name/Handler/Capabilities,
  route table, 404 and 503 JSON shapes)
- Create: `internal/frontend/admin/routes.go` (route dispatch only)
- Test: `internal/frontend/admin/admin_test.go`
- Modify: `frontends.go` (factory entry + option validation)

**Step 1: Write failing tests:**
- `Name() == "admin"`, `Capabilities().Buckets == true`;
- unknown option key aborts construction naming the key;
- missing `clientCAFile` aborts construction naming the path;
- non-loopback address without the option aborts construction;
- `GET /status` with a nil Status service -> 503 JSON
  `{"error":{"code":"NotImplemented",...}}`;
- an unknown path -> 404 with the same JSON error envelope.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS; full package
suite green.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./internal/frontend/admin/ . -count=1` green
- [ ] `internal/frontend/frontend.go` Frontend interface UNTOUCHED (interface diff is additive only)
- [ ] No route is served without a verified client certificate
- [ ] Loopback guard enforced at construction, not at request time
- [ ] `gofmt -l` clean on changed files; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH respected: `internal/frontend/s3/bucket_handlers.go`,
      `internal/frontend/s3/zfsdatasets.go`, `main.go`,
      `s3_wiring.go` (leaf 02 owns it), `scripts/zfs-validate/run-zfs-validation.sh`,
      `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 1 satisfied: interface name, method set, and the rule that
      an empty Addr is a startup error
- [ ] Contract 2 satisfied: Options fields, Services field names, 503 and
      404 envelope shapes
- [ ] Contract 3 satisfied: the three option keys and their validation
- [ ] `ServeTLS` is called with the pre-built config (grep the diff:
      `ListenAndServeTLS` must not appear on the admin path)
- [ ] Certificate failures return 401 and leak nothing

Output: APPROVED or specific gaps with file:line.

## Notes

- Client certificates cannot be verified by `ListenAndServeTLS(cert, key)`;
  that is the whole reason for Contract 1. A reviewer should check this
  specifically.
- The server certificate for the admin listener is the process pair
  (`serverConfig.CertFile`/`KeyFile`). Operators may later want a
  separate pair; that is deliberately not in scope.
- Do not add a route that bypasses authentication for convenience
  (not even a health check). Leaf 04 documents the route table.
