# 04 - SFTP Frontend - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** `internal/frontend/sftp` — SFTP subsystem over `golang.org/x/crypto/ssh`
  + `github.com/pkg/sftp`, host-key config, public-key + password auth to the
  identity model, per master Contract C.
- **Dependencies:** 01-nonhttp-listener-adapter.md, 02-dependency-license-audit.md
  (BSD-2-Clause gate PASSED for pkg/sftp; BSD-3-Clause for x/crypto).
- **Estimated Context:** 90K
- **Concurrency Group:** B (parallel with leaf 03)

## Goal

After this leaf, config entry `{"type":"sftp","listenAddr":":2022","options":{...}}`
starts an SSH server whose SFTP subsystem maps onto `Backend`. Password and
public-key auth both work against local adapter interfaces (leaf 05 replaces
their implementations with the auth-tree-backed adapter — the protocol code
does not change).

## Auth seam note (dependency on the auth tree)

The SFTP key-adapter MAY be delivered by the auth tree (issue #4: system-level
public-key association per identity). This leaf codes against a local narrow
interface and provides a test fake plus a static/env fallback:

```go
// internal/frontend/sftp/auth.go
type PasswordVerifier interface{ Verify(user, password string) (Identity, bool) }
type PublicKeyChecker interface {
    // Accept reports the identity (AccessKeyID + BucketGrants) authorized for key.
    Accept(pubKey ssh.PublicKey) (Identity, bool)
}
```

If the auth tree has landed when this leaf executes, implement these over its
landed lookup functions INSTEAD of the fallback and drop the fallback code —
check `internal/auth` for the landed multi-identity surface first. Do not
reimplement key association here.

## Config surface (owner of these option keys)

```json
{ "type": "sftp", "listenAddr": ":2022",
  "options": {
    "hostKeyFile": "certs/host_ed25519",   // REQUIRED; startup aborts if missing/unreadable
    "allowPasswordAuth": "true"            // default "true"; "false" = pubkey only
  } }
```

Host key: if `hostKeyFile` absent, the server generates an ed25519 key at that
default path on first start and reuses it (document in leaf 07; generation is
scoped to `certs/`-style path under the configured default — never a random
temp key per boot, clients pin host keys).

## Exact files

- New: `internal/frontend/sftp/frontend.go` — `sftp.New(b backend.Backend, opts ...)`,
  `Frontend` + `NonHTTPFrontend`; `Handler()` stub (501, never mounted).
- New: `internal/frontend/sftp/sshserver.go` — ssh.ServerConfig, key exchange,
  subsystem("sftp") channel handling: accept session channels, route the
  `sftp` subsystem request to `sftp.NewServer` over the driver.
- New: `internal/frontend/sftp/driver.go` — `sftp.Handlers` triple
  (FileGet/FilePut/FileCmd/FileList) over Backend (Contract C).
- New: `internal/frontend/sftp/auth.go` — the two interfaces above + static
  fallbacks (env credential pair for password; `authorized_keys`-style file
  option NOT in v1 — pubkey fake/test-only until leaf 05/auth tree).
- New: `internal/frontend/sftp/errors.go` — objectmodel.Error →
  `sftp.StatusCode` (SSH_FX_NO_SUCH_FILE, SSH_FX_PERMISSION_DENIED,
  SSH_FX_OP_UNSUPPORTED) mapping; ctx cancel → SSH_FX_FAILURE with close.
- New: `internal/frontend/sftp/hostkey.go` — load-or-generate host key.
- New: `internal/frontend/sftp/factory.go` — option validation; registered as
  `"sftp"` in `frontends.go`.
- Tests: `internal/frontend/sftp/{frontend_test.go,driver_test.go,conformance_test.go,auth_test.go}`.
- Modify: `frontends.go` (factory entry `"sftp"`), `frontends_test.go` (known types).

## Driver mapping details (binding)

- Reads: `Get` with empty `GetOptions` (no conditional reads cap). A client
  requesting extended stat attributes still gets `Stat` data only.
- Writes: SFTP writes are unbounded-size via `FilePut` (p->v5 write handles).
  Size for `Backend.Put` is not always known up front — consult the landed
  backend contract: if `Put` requires size, buffer to a temp file in the
  dataDir and stream with final size; document the choice in the driver
  doc comment. Never lie about size.
- rename → see Contract C RNTO row. setstat/fsetstat → SSH_FX_PERMISSION_DENIED
  (chmod/chown/utimes have no object-model meaning). symlink/readlink →
  SSH_FX_OP_UNSUPPORTED. mkdir/rmdir → marker-key Put/Delete (Contract C).
- readdir of `/` → `Buckets()` rendered as directory entries.

## Test-first steps

1. Failing test: `TestConformance` — `frontend.RunConformanceSuite` over a
   temp-dir fs backend.
2. Failing test: `TestSFTPRoundTripInProcess` — generate host key in t.TempDir,
   start frontend on `127.0.0.1:0`, dial with `golang.org/x/crypto/ssh`
   client (password auth against `StaticVerifier`), open the `sftp` subsystem
   with `sftp.NewClient`; `Create`+`Write`+`Close` → assert Backend.Put;
   `Open`+`Read` → bytes match; `Remove` → Backend.Delete.
3. Failing test: `TestPublicKeyAuth` — client authenticates with an Ed25519
   key against a fake `PublicKeyChecker`; identity recorded; grant-denied
   write (fake returns identity with no Write grant... note: enforcement is
   leaf 05, so v1 asserts the checker is CONSULTED and its identity reaches
   the driver context; negative path lands with leaf 05).
4. Failing test: `TestListDirMapsToList` — `client.ReadDir("/bkt")` →
   fake Backend asserts `List{Prefix:"bkt/", Delimiter:"/"}`; buckets listed
   at root.
5. Failing test: `TestUnsupportedOps` — `client.Symlink` → error
   `SSH_FX_OP_UNSUPPORTED`; `client.Chmod` → SSH_FX_PERMISSION_DENIED.
6. Failing test: `TestHostKeyPersistence` — first start generates host key
   file; second start reuses it (same fingerprint).
7. Failing test (factory): unknown option key errors loudly; missing
   listenAddr errors (leaf 01 shared-mux rejection).
8. Implement until green; `make test`, `make lint`.

## Done-when

- All tests green including conformance; registered as `"sftp"`.
- Wire round-trip proven in-process with real `ssh` + `sftp` client libraries
  (the CLI-level e2e is leaf 06).
- No SFTP code calls anything below `backend.Backend`.
- Auth-adapter seams (`PasswordVerifier`/`PublicKeyChecker`) are the ONLY
  places credentials are checked.
