# FTP/FTPS + SFTP Frontends - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 7 leaf documents under this node
- **Scope:** Add `ftp` and `sftp` frontends (GitHub issue #2) behind the pluggable
  `Frontend` seam: a minimal backward-compatible seam extension so non-HTTP
  (raw TCP) protocols can host their own listeners, license-gated dependencies,
  protocol drivers mapping file-transfer commands onto `internal/backend.Backend`,
  grant enforcement + capability degradation at the seam, conformance
  instantiation, wire-level e2e coverage, and docs.

## Goal

zeta-object speaks exactly one wire protocol today (S3 over HTTPS, mounted
through `internal/frontend.Frontend.Handler() http.Handler`). A large tool
ecosystem — rclone, WinSCP, lftp, curl, plain `sftp`, cron scripts — speaks
file-transfer protocols but not object APIs. This tree adds:

- **FTP/FTPS frontend** on `github.com/fclairamb/ftpserverlib`
  (explicit TLS via AUTH TLS, reusing `certFile`/`keyFile` config).
- **SFTP frontend** on `github.com/pkg/sftp` over `golang.org/x/crypto/ssh`
  (SSH public-key auth mapping to the identity model per the auth issue #4
  seam; password auth via the same adapter).

Mapping (from issue #2, binding): directory listing → `List` with
prefix/delimiter; STOR/RETR → `Put`/`Get`; DELE/RMD → `Delete`. Capability
degradation at the seam: no conditional reads, no multipart — reject or no-op
with protocol-appropriate responses, never silent emulation.

## Ordering (read this before dispatching)

- **This tree sequences AFTER the auth tree (`docs/plans/auth-2026-09/`,
  GitHub issue #4) lands.** Leaves 03, 04, 05, 06 consume its multi-identity
  seam: `AccessKeyID` lookup, bucket grants, and the SFTP public-key
  association may be delivered BY the auth tree — check its landed contracts
  before writing the auth adapters. Leaves 01 and 02 are auth-independent and
  may start earlier.
- Leaf order: **01 → 02 → (03 ∥ 04) → 05 → 06 → 07.** Leaves 03 and 04 are
  independent of each other; 05 depends on both; 06 depends on all.

## Architecture

**The seam problem.** `internal/frontend.Frontend` is HTTP-shaped:
`Handler() http.Handler`, and `main.go` (`frontends.go`, leaf 03 of
frontend-interface-2026-09) mounts every frontend either on the shared mux or
on a dedicated TLS `http.Server`. FTP and SFTP are raw TCP protocols — they
cannot be expressed as an `http.Handler`. Leaf 01 adds the minimal,
backward-compatible extension: an OPTIONAL interface (`NonHTTPFrontend`)
implemented only by frontends that host their own listener lifecycle. The
`Frontend` interface, `Registry`, and every existing config file stay
untouched; a frontend that does not implement the optional interface mounts
exactly as today.

**Config surface.** `FrontendConfig` gains an `options` map (same pattern as
`BackendCfg.Options`) for protocol-specific settings: FTP passive-port range,
SFTP host-key path. Also leaf 01.

**The frontends.** Leaf 03 (`internal/frontend/ftp`) and leaf 04
(`internal/frontend/sftp`) each pair a license gate (leaf 02's method,
executed FIRST in the leaf) with a protocol driver mapping commands onto
`backend.Backend`. Both register via the existing factory map in
`frontends.go` as types `"ftp"` and `"sftp"` — one config entry each, each
with its own `listenAddr` (required for non-HTTP frontends; the factory
rejects an empty one). `Handler()` still returns a non-nil handler (always
501, documented as never mounted) so the seam contract and conformance
harness keep a total function.

**Auth at the seam.** `auth.Authenticator.Authenticate(r *http.Request)` is
also HTTP-shaped. The FTP/SFTP frontends do NOT call it with a fake request;
leaf 05 defines a narrow protocol-neutral adapter (password verify, public-key
check, grant lookup) implemented over the auth tree's landed identity model.
Until the auth tree lands, leaves 03/04 code against small local interfaces
(`PasswordVerifier`, `PublicKeyChecker`, `GrantChecker`) in their own
packages, with fake implementations in tests — so the protocol work is
reviewable now and the adapter is the only thing that changes when the auth
tree lands.

**License policy (user hard rule, binding on every leaf):** GPL/AGPL
dependencies are acceptable ONLY as separate subprocesses — never vendored,
never linked into shipped binaries. Both candidate deps are permissive
(verified 2026-09 online, re-verified from module cache in leaf 02 as the
authoritative record):

| Dependency | Role | License (verified 2026-09) |
|---|---|---|
| `github.com/fclairamb/ftpserverlib` v0.32.x | FTP/FTPS server protocol | **MIT** (github + pkg.go.dev) |
| `github.com/pkg/sftp` v1.13.x | SFTP subsystem protocol | **BSD-2-Clause** (github + pkg.go.dev) |
| `golang.org/x/crypto` | SSH transport under SFTP | **BSD-3-Clause** |
| `github.com/jlaffaye/ftp` (test-only) | FTP client for in-process tests | **MIT** (confirm at gate) |

Leaf 02 turns this table into a documented, reproducible audit step
(`docs/licenses/THIRD-PARTY-LICENSES.md`) whose source of truth is the
LICENSE file read from the local module cache — the online check above is
reconnaissance, not the record.

## Interface Contracts

### Contract A: optional non-HTTP frontend interface (leaf 01, additive)

```go
// File: internal/frontend/frontend.go (addition; Frontend itself is FROZEN)

// NonHTTPFrontend is an OPTIONAL extension implemented by frontends whose
// wire protocol is not expressible as http.Handler (FTP, SFTP). A frontend
// implementing it MUST NOT be mounted on the shared mux: main() opens a
// plain net.Listener at NonHTTPAddr() and runs Serve until ctx/shutdown.
type NonHTTPFrontend interface {
    Frontend
    NonHTTPAddr() string          // listen address from this frontend's config ("" => config error, rejected at construction)
    Serve(l net.Listener) error   // blocking; returns on Stop() or listener close
    Stop() error                  // graceful: stop accepting, close sessions
}
```

Rules (binding):

- `main.go` type-asserts each dedicated-listener frontend to
  `NonHTTPFrontend`: if it implements the interface it gets a raw
  `net.Listener` + `Serve` goroutine tracked in the existing graceful-shutdown
  fan (parallel to `extraServers`); otherwise the existing `http.Server` path
  runs unchanged. Shared-mux mounting of a `NonHTTPFrontend` is a loud
  startup error.
- Registry, factories, and the absent-`frontends`-config behavior
  (S3-only) are unchanged — exact backward compatibility.

### Contract B: per-frontend options (leaf 01)

```go
// File: config.go — FrontendConfig gains (backward compatible, omitempty):
type FrontendConfig struct {
    Type       string            `json:"type"`
    ListenAddr string            `json:"listenAddr,omitempty"`
    Options    map[string]string `json:"options,omitempty"` // protocol-specific keys; unknown keys are rejected by the owning factory
}
```

### Contract C: backend mapping (binding for leaves 03/04)

| Protocol op | Backend call | Notes |
|---|---|---|
| LIST/NLST/MLSD, SFTP readdir | `List(ctx, bucket, ListParams{Prefix, Delimiter: "/"})` | common prefixes render as directories; buckets are top-level dirs |
| STOR / SFTP write+close | `Put(ctx, bucket, key, r, size, PutOptions{})` | no multipart — single-shot; unknown-size SFTP writes buffer via the backend's streaming rules (verify against landed backend contract) |
| RETR / SFTP read | `Get(ctx, bucket, key, GetOptions{})` | no conditional/Range mapping — reject REST/`If-*`-style requests protocol-appropriately |
| DELE / SFTP remove | `Delete(ctx, bucket, key)` | |
| RMD / SFTP rmdir | `Delete` on the dir marker, else `objectmodel` NoSuchKey → 550 / SSH_FX_NO_SUCH_FILE | directories are List common-prefixes; no separate dir object exists |
| MKD / SFTP mkdir | `Put` of the zero-byte `"dir/"` marker key | consistent with fs layout; verify against objectmodel conventions before implementing |
| CWD/CDUP/PWD, path resolution | pure driver logic over the same `bucket/key` path model | never touches storage |
| STAT/SIZE/MDTM, SFTP stat/fsetstat | `Stat` | `fsetstat` rejected (protocol-appropriate permission denied) |
| RNFR/RNTO, SFTP rename | `Put` copy + `Delete` if the landed backend has no atomic rename; check `backend.Backend` for a Copy/Rename method added by later trees and prefer it | |
| SYMLINK/READLINK | rejected (permission denied / unsupported) | object model has no links |

### Contract D: capability declaration (leaves 03/04/05)

Both frontends declare `ProtocolCaps{Buckets: true, ConditionalReads: false,
Multipart: false, PresignedURLs: false, Versioning: false}`. Degradation
mapping (leaf 05): FTP — no client-visible conditional/multipart verbs exist,
so the mapping is naturally total; any future SITE/rest-command gap answers
`502 Command not implemented`. SFTP — `SSH_FX_OP_UNSUPPORTED` for
unrepresentable requests, `SSH_FX_PERMISSION_DENIED` for grant denials and
rejected setstat/symlink. Protocol-appropriate errors only; never a silent
success that faked the capability.

## Leaf index

| Leaf | Title | Depends on |
|---|---|---|
| [01-nonhttp-listener-adapter.md](01-nonhttp-listener-adapter.md) | Non-HTTP listener adapter + FrontendConfig options | (may start pre-auth-tree) |
| [02-dependency-license-audit.md](02-dependency-license-audit.md) | License gate + audit record for all four deps | (may start pre-auth-tree) |
| [03-ftp-ftp-frontend.md](03-ftp-ftp-frontend.md) | FTP/FTPS frontend on ftpserverlib | 01, 02 |
| [04-sftp-frontend.md](04-sftp-frontend.md) | SFTP frontend on pkg/sftp | 01, 02 |
| [05-authz-and-capability-degradation.md](05-authz-and-capability-degradation.md) | Grant enforcement + capability degradation | 03, 04, auth tree landed |
| [06-conformance-and-e2e.md](06-conformance-and-e2e.md) | Conformance instantiation + wire-level e2e cases | 03, 04, 05 |
| [07-config-and-docs.md](07-config-and-docs.md) | config.json.example, README, license record placement | 03, 04 (docs final pass) |

## Done-when (tree level — maps to issue #2 acceptance)

- [ ] Both frontends registered under `internal/frontend/` (types `"ftp"`,
      `"sftp"` in the factory map), calling only `internal/backend.Backend`.
- [ ] Both pass `frontend.RunConformanceSuite` in their own package tests.
- [ ] `make e2e` green with new cases: FTP round-trip via `curl ftp://`,
      FTPS via `curl --ftp-ssl` (AUTH TLS), SFTP via the system `sftp` CLI
      with key auth, rclone interop case (soft-skip if the binary is absent —
      the curl/sftp cases carry the AGENTS.md hard-rule coverage).
- [ ] `docs/licenses/THIRD-PARTY-LICENSES.md` records each dependency's
      LICENSE read from the module cache, with version pinned at `go.mod`.
- [ ] `make test`, `make lint`, `make check` all green; coverage floors
      re-measured if any code moved between packages (AGENTS.md rule).
- [ ] README documents the new frontends, config keys, and the license
      policy note.
