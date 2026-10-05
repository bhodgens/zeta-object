# Admin Server (web console) - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 7 leaf documents (flat)
- **Scope:** a separate Go binary that serves a browser-based console for
  the management API. It holds the gateway's administrative client
  certificate, exposes a small JSON surface that proxies the gateway's
  management routes 1:1, authenticates the browser with an operator token
  and an in-memory session, and serves an embedded single-page UI whose
  theme is derived from the project logo.

## Why a separate process (design decision, 2026-10-04)

The management API is a loopback, mTLS, JSON surface (tree
`management-api-2026-10`). A browser cannot be expected to hold and
present a client certificate, so the console is its own process:

- it holds the client certificate and private key (`clientCert` /
  `clientKey`) plus the trusted CA (`caFile`) and speaks mTLS to the
  gateway's admin listener;
- it authenticates the OPERATOR, not the machine: a token from its own
  config, exchanged for an in-memory session cookie;
- it binds loopback by default, exactly like the gateway's admin listener;
- it keeps ZERO persistent state (session state is memory, per the
  charter); the token and certificate are the operator's own files.

Rejected alternative: serving the UI from the gateway's own admin listener
(the browser would have to present a client certificate, and the UI would
be coupled to the gateway's TLS listener). The console being separate also
leaves the door open to pointing one console at several gateways later.

## Goal

An operator opens `https://127.0.0.1:<port>/`, signs in, and can:

- see server status: version, uptime, listeners, frontends, backends,
  restart-required keys, metadata provider availability;
- read the effective configuration, change it, save it, and see which
  changes need a restart;
- manage buckets: list, inspect, create, delete (plain directories; a
  dataset-backed bucket reports the refusal reason), and edit per-bucket
  tunables;
- trigger identity reload and read the outcome;
- run the history purge with a deliberate confirmation step.

Non-goals: managing other protocols' data, a plugin system, a JS build
step or npm dependency, and any persistent console state.

## Architecture

- **Binary:** `cmd/admin-server/main.go` (root package stays the gateway).
  Makefile gains an `admin-server` target; `make build` builds both.
  Static binary, no cgo, unchanged.
- **Package:** `internal/adminserver` owns config, session auth, the proxy,
  and the embedded UI. The UI assets live in
  `internal/adminserver/web/` and ship via `go:embed`, so there is no
  runtime asset path and no CDN dependency.
- **Gateway client:** `internal/adminserver/gateway` is a typed mTLS JSON
  client over the gateway's management routes. It maps the gateway's
  error envelope to console errors unchanged, so the UI shows the
  gateway's own message.
- **UI:** one HTML shell plus hand-written CSS and JavaScript. No
  framework, no bundler, no external requests. Navigation is a persistent
  left rail of tabs; content swaps in place.
- **Theme:** tokens are derived from `logo-icon.png` (see Contract 4). Dark
  is the default because the logo is a dark field; a light variant reuses
  the same accent so both read as the same product.

## Interface Contracts

### Contract 1: console config (leaf 01, consumed by 02 and 05)

```json
{
  "listenAddr": "127.0.0.1:9443",
  "gatewayUrl": "https://127.0.0.1:9708",
  "caFile": "/path/ca.pem",
  "clientCert": "/path/client.pem",
  "clientKey": "/path/client-key.pem",
  "operatorToken": "a long random string",
  "allowNonLoopback": false
}
```

- Fail-loud on load: unknown keys rejected; `listenAddr` must resolve to a
  loopback address unless `allowNonLoopback` is true; the four certificate
  and token fields must all be present and non-empty (an empty
  `operatorToken` is a startup abort, never an unauthenticated console).
- The certificate files are read at startup AND re-read on the console's
  own certificate reload, so rotation does not need a restart.
- **Console listener TLS (added during execution).** The console's own
  listener takes OPTIONAL `certFile` / `keyFile` (its server certificate;
  distinct from `clientCert`/`clientKey`, which are the console's identity
  TO the gateway). Rules: both set means the console serves HTTPS (TLS 1.2
  minimum) and every cookie is `Secure`; neither set means plain HTTP and
  `allowNonLoopback` must be FALSE (a startup abort otherwise - the console
  never serves plain HTTP off-host), with cookies `Secure` only when the
  request arrived over TLS. This exists because a `Secure` cookie over
  plain `http://` is rejected by Safari even on loopback.

### Contract 2: console HTTP surface (leaf 05, consumed by 04)

| Method | Path | Behavior |
|---|---|---|
| GET | `/` | the UI shell (HTML) |
| GET | `/assets/{file}` | embedded CSS, JS, logo images |
| GET | `/login` | the sign-in page |
| POST | `/login` | exchange the operator token for a session cookie |
| POST | `/logout` | clear the session |
| GET | `/api/status` | proxy: gateway `GET /status` |
| GET | `/api/config` | proxy: gateway `GET /config` |
| PUT | `/api/config` | proxy: gateway `PUT /config` |
| POST | `/api/config/save` | proxy: gateway `POST /config/save` |
| POST | `/api/auth/reload` | proxy: gateway `POST /auth/reload` |
| GET | `/api/buckets` | proxy: gateway `GET /buckets` |
| POST | `/api/buckets` | proxy: gateway `POST /buckets` |
| GET | `/api/buckets/{name}` | proxy: gateway `GET /buckets/{name}` |
| DELETE | `/api/buckets/{name}` | proxy: gateway `DELETE /buckets/{name}` |
| PUT | `/api/buckets/{name}/settings` | proxy: gateway `PUT /buckets/{name}/settings` |
| POST | `/api/purge` | proxy: gateway `POST /purge` |

- The `/api/...` prefix is the console's; the proxy rewrites it to the
  gateway path and forwards method, query, and body unchanged. Response
  bodies and status codes pass through unchanged, including the gateway's
  error envelope.
- Every `/api/...` route requires a valid session (401 with a JSON error
  envelope when absent) and the CSRF header (403 when missing or wrong).
- Static assets and the login page need no session.

### Contract 3: session and CSRF (leaf 01)

- Cookie: `zeta_session`, `HttpOnly`, `SameSite=Strict`, `Secure`, value
  is an HMAC-signed payload carrying an expiry and a random id. The HMAC
  key is generated per process start, so a restart invalidates sessions
  (in-memory by design, no persistent state).
- CSRF: the session also carries a CSRF token; the UI echoes it in an
  `X-CSRF-Token` header on every mutating request. A mutating request
  without a matching token is 403.
- Session lifetime is a pinned constant with idle expiry; the UI shows the
  sign-in page when a call returns 401.

### Contract 4: theme tokens (leaf 03, consumed by 04)

Extracted from `logo-icon.png` (560x560, dark field with a glowing
hexagonal emblem) and `logo.jpeg`:

| Token | Value | Note |
|---|---|---|
| `--bg` | `#05080f` | deepest field (logo bulk is `#000018`/`#001818`) |
| `--surface` | `#0b1421` | panels |
| `--surface-2` | `#12202f` | raised rows, inputs |
| `--border` | `#1d3143` | hairlines |
| `--accent` | `#2f8fc4` | the emblem glow (`#2581b6` brightened for contrast) |
| `--accent-strong` | `#43a6dc` | hover and active |
| `--accent-dim` | `#186078` | logo mid-tone, for rails and chips |
| `--text` | `#eef3f7` | near-white from the emblem highlights |
| `--text-dim` | `#9fb3c4` | secondary text |
| `--ok` `--warn` `--danger` | `#35c48f` `#d9a441` `#e0555a` | semantic states, deliberately not brand colours |

- Light variant: `--bg #f6f9fb`, `--surface #ffffff`, `--surface-2 #eef3f7`,
  `--text #0e1a26`, same `--accent`. Toggled by a `data-theme` attribute on
  the root element; the choice is remembered in `localStorage` only.
- Motion: transitions of about 140ms on colour and transform, a visible
  focus ring on every interactive element, and no layout shift when a tab
  changes. Respect `prefers-reduced-motion`.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-console-skeleton-auth.md | leaf | none | 60K | A |
| 02 | 02-gateway-client.md | leaf | none | 50K | A |
| 03 | 03-ui-shell-theme.md | leaf | none | 50K | A |
| 04 | 04-ui-screens.md | leaf | 03 (shell), Contract 2 (endpoints) | 60K | B |
| 05 | 05-proxy-routes.md | leaf | 01, 02 | 60K | B |
| 06 | 06-docs.md | leaf | 04, 05 (behavior) | 40K | C |
| 07 | 07-e2e-case-36.md | leaf | 05 | 50K | C |

**Concurrency groups:** A: 01, 02, 03 simultaneously (disjoint: the
`cmd/admin-server` skeleton plus auth and embed, the gateway client
package, and the static asset set). B: 04 and 05 in parallel (the UI
screens consume the pinned endpoint contract; the proxy implements it).
C: 06 and 07 after 05 (docs and the e2e case touch disjoint files).

## Dispatch Protocol

For each concurrency group, in dependency order:

### Phase 1: Dispatch Concurrency Group A

Dispatch 01, 02 and 03 simultaneously via `delegate_task`:

1. Read the leaf, paste the FULL leaf text plus the contracts it consumes
   and produces. Include the repo Coding Conventions block below.
   Include: "Do NOT commit. Do NOT run git add."
2. DO-NOT-TOUCH list: `go.sum`, `zeta-object-server`, `internal/frontend/`
   (any file), `internal/bucketmanager/`, `internal/fslock/`,
   `internal/auth/`, `config_store.go`, `s3_wiring.go`,
   `scripts/zfs-validate/run-zfs-validation.sh`. Stage explicit paths
   only.
3. File ownership in group A: leaf 01 owns `cmd/admin-server/`,
   `internal/adminserver/` Go files, the Makefile and the CI floor table;
   leaf 02 owns `internal/adminserver/gateway/`; leaf 03 owns
   `internal/adminserver/web/` (assets). No other leaf may touch those.

### Phase 2: Review and Commit Each Child

After each agent returns, the orchestrator reviews in-session:

1. Read the changed files; check against the leaf spec and contracts.
2. RE-RUN the gates in the parent (subagent gate reports are
   self-claims): `go build ./...`, `go test ./... -count=1`,
   `make lint NEW_FROM_REV=HEAD` (0 findings), `gofmt -l` clean,
   `make e2e` when the leaf touches served behavior.
3. Commit explicit paths only (never `git add -A`).
4. Gaps -> re-dispatch with findings; max 3 cycles, then escalate.

### Phase 3: Dispatch Group B, then C

- Leaf 05 after 01 and 02 are COMMITTED.
- Leaf 04 after 03 is committed (it needs the shell's DOM ids and the
  asset layout).
- Leaves 06 and 07 after 05 is committed.

### Phase 4: Integration Review

1. Full gates: `make test && make lint NEW_FROM_REV=HEAD && make e2e &&
   make parity-test`, plus `make build` proving BOTH binaries build.
2. Confirm the gateway is untouched by this tree: `git diff --stat` must
   not list `internal/frontend/`, `config_store.go`, `s3_wiring.go`,
   `internal/bucketmanager/`, `internal/auth/`.
3. Confirm no console state is persisted: no new files written by the
   console at runtime (session state is memory).
4. Update the tracking table; report.

## Review Checklist

- [ ] All tasks from each leaf document are implemented
- [ ] Contracts 1-4 satisfied exactly (config keys, the `/api/` surface,
      session and CSRF rules, the theme tokens)
- [ ] The console never serves a page or an API response without a
      session, except the asset set and the login page
- [ ] No external network request from the UI (no CDN, no fonts, no
      analytics): `grep` the assets for `http://` and `https://`
- [ ] No client-certificate secret or operator token appears in any
      response body, log line, or committed asset
- [ ] The gateway's admin surface is unchanged (this tree is additive)
- [ ] Both binaries build statically (no cgo)
- [ ] Tests written and passing (TDD followed)
- [ ] No scope creep, no debug artifacts, no line-number corruption

Output: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go (repo go.mod version); exported PascalCase,
  unexported camelCase; imports grouped stdlib / third-party / local.
- **Errors:** wrap with `%w`; no `panic` in library paths;
  ignored-error and empty-branch forms are HOOK-BANNED.
- **Go 1.26 modernize forms:** `errors.AsType[T]`, `new(expr)`,
  `for i := range n`, `wg.Go`.
- **Testing:** table-driven; handlers tested with `httptest`; the gateway
  client tested against a stub TLS server.
- **Formatting:** `gofmt` before reporting; `make lint
  NEW_FROM_REV=HEAD` must be 0.
- **Static binary:** NO cgo, ever. Assets ride `go:embed`.
- **Frontend:** hand-written HTML/CSS/JS only. No framework, no bundler,
  no external request, and every interactive element keyboard reachable.

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-console-skeleton-auth | COMPLETE | 1 | 5c28f22; plain-HTTP+cookie-Secure defect found -> folded into leaf 05 |
| 02-gateway-client | COMPLETE | 1 | 76590ab (also carries the web assets: a failed attempt left them staged) |
| 03-ui-shell-theme | COMPLETE | 1 | in 76590ab; shell hooks verified in parent |
| 04-ui-screens | COMPLETE | 1 | 3cb4cf5; screens under web/assets/screens; it also caught the asset-layout 404 |
| 05-proxy-routes | COMPLETE | 1 | 038d9cc; TLS/cookie policy done + smoke-verified by parent; asset layout fix 4a7cd15 |
| 06-docs | IN_PROGRESS | 0 | dispatched (Group C) |
| 07-e2e-case-36 | IN_PROGRESS | 0 | dispatched (Group C) |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. `make test` - full unit suite green (new: config validation and
   loopback guard, session and CSRF, gateway client, proxy handlers,
   embedded asset serving).
2. `make lint NEW_FROM_REV=HEAD` - 0 findings.
3. `make build` - both `zeta-object-server` and the console binary build.
4. `make e2e` - full suite green; case 36 drives the console against a
   real gateway admin listener, and SKIPS gracefully if its fixtures are
   unavailable.
5. `make parity-test` - metadata parity untouched (the gateway is not
   modified by this tree).

## Why there is no live-ZFS leaf

The AGENTS.md validation rule covers changes to the data plane, the
metadata surface, and the backends. This tree changes none of them: the
console is a client of the management API, and the operations it proxies
(bucket create/delete, configuration changes) were validated on real ZFS
by `docs/validation-management-api-2026-10-04.md`. The console's own
correctness is wire-level, so e2e case 36 is its evidence. If a later
change makes the console touch ZFS directly, that rule applies again.

## Structural Completeness Check (Before Dispatch)

Run: `python3 ~/.hermes/skills/software-development/hierarchical-planning/scripts/check_template_compliance.py docs/plans --strict-leaves`
(against `docs/plans` - the PARENT).

## Open Questions

- None blocking. The process-split decision is recorded above with its
  rejected alternative.

## Notes

- **Reuse the existing fixtures:** `scripts/e2e/run-e2e.sh` already
  generates a CA, a client certificate with a pinned common name, and the
  admin frontends entry on a loopback port (added by the management-api
  tree). Case 36 consumes those rather than inventing new ones.
- **Coverage floors:** adding a package means adding a floor row for it
  (AGENTS.md); measure on the CI-shaped command and round down.
- **Client-side TLS reload API:** the gateway client uses
  `GetClientCertificate` on the CLIENT side; `GetConfigForClient` is a
  server hook and does not exist for a client. The master originally said
  otherwise and the implementing leaf corrected it.
- **The logo files stay where they are** (`logo-icon.png`, `logo.jpeg` at
  the repo root, referenced by README). The console embeds its own copy
  for the favicon and the rail mark; do not move or rewrite the originals.
