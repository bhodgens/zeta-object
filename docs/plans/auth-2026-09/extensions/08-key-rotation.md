# Key Rotation / Revocation Without Persistent Server-Owned State - Design Leaf

> **Status:** DESIGN ONLY — no code in this leaf. This document proposes the
> extension; implementation happens in its own change after review.

## Meta

- **Parent:** [master.md](../master.md) (post-completion extension; the master
  tree itself is COMPLETE per its Implementation Record)
- **Scope:** Rotating and revoking credentials served by the multi-identity
  registry **without restarting the server and without any persistent
  server-owned state** (charter rule, AGENTS.md "dumb gateway/proxy").
- **Dependencies:** the landed pluggable-authentication tree (leaf 01 core:
  `IdentityRegistry` / `MultiRegistry`, `buildIdentityRegistry()`).
- **Estimated Context:** 50K

## Goal

Today a credential change (add a key, rotate a secret, revoke a compromised
key) requires a full server restart:

- `buildIdentityRegistry()` (config.go:369) runs exactly once at startup,
  merging the env pair (config.go:376, always present as identity `"env"`,
  per the migration contract) with the configured `identities` (config.go:377).
- The result is stored in the process-wide `identityRegistry` var
  (config.go:390) and installed into the S3 frontend once
  (s3_wiring.go:74-75 → `s3.InstallIdentityRegistry`, seam.go:239).
- `main.go:71-74` aborts startup when the registry fails to build.

Between requests nothing re-reads config; `MultiRegistry` is immutable after
construction (registry.go:79-83: three maps built in `NewMultiRegistry`,
never mutated). This leaf adds **reload** so config.json remains the sole
source of truth — which is precisely why no persistence is needed: revocation
is *deleting a line from config.json*, and the running server's view is a
cache of that file, never an independent store.

## Constraints

1. **Charter:** no persistent server-owned state with identity association
   (AGENTS.md). Any design that records revocations in a DB/file/journal
   owned by the server is out. Revocation state = the config file itself.
2. **Frozen v1 contracts** (master.md Notes; frontend-interface-2026-09):
   `Authenticator`, `Identity{AccessKeyID, BucketGrants}`, `Grant`,
   `CredentialSource`, and the `IdentityRegistry` interface
   (registry.go:35-45) — add types/methods, never rename/remove.
3. **Stdlib only** (master Coding Conventions; repo ships no third-party deps).
4. **Constant-time comparisons and secret hygiene are unchanged** — reload
   must not create a path where secrets are logged or compared non-constantly
   (registry.go:57-63 hashing discipline).
5. **Fail-loud config validation** at every reload, same as startup
   (config.go:362-368 semantics): a bad reload must never half-apply.

## Options Evaluated

### Option 1 (CHOSEN): SIGHUP-triggered full rebuild + atomic registry swap

On `SIGHUP`, re-run the exact startup sequence — `loadConfig` +
`buildIdentityRegistry()` — against the same config file; on success, swap
the new registry in atomically; on failure, keep serving with the old one
and log the error fail-loud.

Why it wins:

- **One code path.** Reload reuses `buildIdentityRegistry()` verbatim; there
  is no second, weaker validation path to drift from startup. The e2e case
  can even reuse the `launch_expect_fail` pattern (scripts/e2e/lib.sh:122)
  semantics for "bad config is rejected".
- **Zero new dependencies.** `os/signal.Notify` is stdlib. File-watching via
  fsnotify would break the stdlib-only convention; mtime polling needs a
  timer goroutine, adds reload latency, and can fire mid-write (partial
  JSON) — SIGHUP is an explicit operator action on an already-complete file.
- **The seam was built for this.** Frontends already resolve credentials
  *per request* through the installed hook: `identityRegistryFor()` reads
  `identityRegistryHook` under `hookMu` (seam.go:229-250), and the S3 adapter
  calls `reg.LookupByAccessKey(accessKeyID)` on every request
  (auth_adapter.go:266-267); FTP/SFTP/Basic frontends resolve per-connection
  through the same interface (ftp/auth.go:31-40, sftp/auth.go). Swapping the
  value behind the hook is therefore observable by every frontend with **no
  frontend changes at all**.
- **Charter-clean.** No state is persisted anywhere; config.json on disk is
  the record of what is and isn't valid.

### Option 2 (REJECTED for now): file-watching config reload

`fsnotify` = new dependency (violates conventions); stdlib mtime polling =
timer goroutine, reload latency up to the poll interval, and forced handling
of torn reads (editor write-in-place). Additionally, *implicit* reload is
operationally worse than *explicit* reload for a security surface: an
operator editing config for an unrelated reason can accidentally revoke a
key mid-edit. SIGHUP makes the moment of rotation deliberate and loggable.

### Option 3 (REJECTED for now, seam preserved): external / subprocess authenticator

The GH issue's option 3 (docs/plans/issue-auth-pluggable.md:23,
"plugin/subprocess authenticator, hashicorp go-plugin style") remains open
by design: master.md's Decision (lines 17-27) notes `IdentityRegistry` is an
interface precisely so an OIDC- or subprocess-backed registry is a drop-in
implementation. An external authenticator would make rotation the external
system's problem (no reload needed at all). Rejected here because it adds a
process/network dependency at auth time to a self-hosted, offline-friendly
single binary (issue rationale, issue-auth-pluggable.md:20-22), and nothing
in this repo's deployment story needs LDAP yet. **This leaf does not close
that door**: the `ReloadableRegistry` below implements `IdentityRegistry`,
so a future subprocess registry can be installed in its place unchanged.

## Design

### Contract 1: `ReloadableRegistry` (new file: internal/auth/reload.go)

```go
// File: internal/auth/reload.go (new)
package auth

// ReloadableRegistry is an IdentityRegistry wrapper whose inner registry
// can be swapped atomically at runtime (SIGHUP reload). The zero usable
// value is constructed with NewReloadableRegistry; the initial inner
// registry is the startup build, so behavior before the first SIGHUP is
// byte-identical to today.
type ReloadableRegistry struct {
    mu    sync.RWMutex
    inner IdentityRegistry // never nil after construction
}

func NewReloadableRegistry(inner IdentityRegistry) *ReloadableRegistry

// Swap atomically replaces the inner registry. newReg must be non-nil;
// the previous registry is returned so the caller can log/inspect it.
// Swap NEVER validates — validation happened in the builder that produced
// newReg (single-decision rule: NewMultiRegistry remains the only
// validator).
func (r *ReloadableRegistry) Swap(newReg IdentityRegistry) (prev IdentityRegistry)

// Inner returns the current registry (for tests and diagnostics only).

// The three IdentityRegistry methods delegate to the current inner
// registry under mu.RLock, preserving each one's exact semantics
// (constant-time Basic compare, canonicalized public-key lookup):
func (r *ReloadableRegistry) LookupByAccessKey(accessKeyID string) (Identity, bool)
func (r *ReloadableRegistry) LookupByBasicCredential(username, password string) (Identity, bool)
func (r *ReloadableRegistry) LookupByPublicKey(authorizedKey string) (Identity, bool)

// SecretKey delegates too — MultiRegistry's frozen-v1 CredentialSource
// method (registry.go:222-230) must rotate with the rest.
func (r *ReloadableRegistry) SecretKey(accessKeyID string) (string, bool)

// Compile-time: satisfies both frozen seams.
var _ IdentityRegistry = (*ReloadableRegistry)(nil)
var _ CredentialSource = (*ReloadableRegistry)(nil)
```

- **Owner:** this leaf. **Consumers:** package main wiring only; frontends
  keep consuming `IdentityRegistry` unchanged.
- Delegation is per-call under a read lock, so a swap between two requests
  is immediately visible; a swap *during* one request is not observed
  mid-request (the lookup is a single delegated call). In-flight requests
  that already hold an `Identity` value keep its grants for that request —
  bounded by request duration, acceptable and documented.

### Contract 2: reload trigger + builder (package main)

```go
// File: config.go — additions only
// reloadIdentityRegistry re-runs the STARTUP sequence against the same
// config file and swaps the result into the installed ReloadableRegistry.
// Fail-closed: ANY error (read, parse, validate, build) leaves the old
// registry serving and logs one line naming the offender. On success it
// logs the rotated-in identity NAMES only (never secrets).
func reloadIdentityRegistry() error   // loadConfig → buildIdentityRegistry → reg.Swap
```

```go
// File: main.go — after the existing wiring (identityRegistry = reg, main.go:75):
//   identityRegistry is wrapped: reloadable := auth.NewReloadableRegistry(reg)
//   and reloadable (not reg) is installed via s3.InstallIdentityRegistry.
// A goroutine does signal.Notify(sighupCh, syscall.SIGHUP) and calls
// reloadIdentityRegistry() per signal. No new config keys.
```

Semantics:

- **No new config surface.** Rotation is operational (edit file + `kill -HUP`),
  so the AGENTS.md e2e rule is satisfied by the e2e case alone, not by new
  JSON keys.
- **Env pair caveat (documented limitation):** `loadCredentials()` reads
  `ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY` at process start
  (config.go:409-412). A SIGHUP re-runs `loadCredentials()`, but the process
  environment is fixed, so rotating the *env* identity still requires a
  restart. Operators rotate by editing `identities` in config.json. This is
  stated in README (docs leaf) — not hidden.
- **Revocation flow:** remove/replace the identity's entry in config.json →
  SIGHUP → next `LookupByAccessKey` misses → S3 clients get the unchanged
  pre-tree `InvalidAccessKeyId` 403 (auth_adapter.go:145). No tombstones,
  no revocation list, no persistence: the absence of the entry in the file
  IS the revocation.
- **Rotation flow:** add the new identity (new accessKey + secret) → SIGHUP →
  new key works → re-point clients → remove the old entry → SIGHUP. Both
  keys valid in the interim window; no shared moment where the old key is
  dead before the new one works.
- **SIGHUP with a broken config:** old registry keeps serving; startup
  abort semantics (main.go:71-74) intentionally do NOT apply at reload —
  a bad edit must not take a running server down. Log at WARN/ERROR with
  the validator's named-offender error.
- **Non-SIGHUP platforms:** document that SIGHUP is POSIX; on Windows the
  reload goroutine simply never fires (no behavior regression vs. today).

### What is explicitly NOT persisted (charter audit)

- No revocation journal, no "seen keys" cache, no last-rotation timestamp
  anywhere on disk. The only durable artifact is the operator's config.json,
  which predates the server and is not server-owned.
- Registry contents live in process memory only, rebuilt from file on every
  reload — same as startup today.

## E2E Plan (AGENTS.md hard rule)

New case `scripts/e2e/cases/26-auth-rotation.sh`, private-server pattern
(like case 18: own config.json, own port, `ZETAOBJECT_CONFIG` per
scripts/e2e/lib.sh:136, create/cleanup pairing, `BKT=` convention):

- **26a baseline:** server starts with identity `ak-old` (+ env pair);
  SigV4 round-trip with `ak-old` succeeds.
- **26b hot-add:** append identity `ak-new` to config.json → `kill -HUP` →
  within the case's retry window, `ak-new` performs a full PUT/GET
  round-trip **without a restart**; `ak-old` still works (rotation window).
- **26c hot-revoke:** remove `ak-old` → SIGHUP → `ak-old` PUT fails with
  403 `InvalidAccessKeyId`; `ak-new` unaffected.
- **26d fail-closed reload:** write syntactically-valid-but-invalid config
  (duplicate accessKey) → SIGHUP → server logs the named-offender error and
  **keeps serving** with the previous registry (`ak-new` still round-trips).
- **26e torn file:** write truncated JSON → SIGHUP → same fail-closed
  behavior.

Assert helpers from `scripts/e2e/lib.sh`; log assertions on the reload
error/success lines. Unit tests: `internal/auth/reload_test.go` (swap
visibility, delegation of all four methods incl. `SecretKey`, nil-swap
panic/contract, concurrent swap+lookup race via `-race`).

## ZFS Validation

Auth-only change: no backend, metadata, or data-plane code is touched
(lookup happens before dispatch; grants semantics unchanged — that is leaf
09's topic). Per the AGENTS.md scoping, `run-zfs-validation.sh` is not
strictly triggered; however, because the change touches the request path's
credential resolution, the implementing change SHOULD still run the script
once as a regression sweep (cheap, automated).

## Risks

| Risk | Mitigation |
|---|---|
| Swap races with per-request lookups | `RWMutex` in `ReloadableRegistry`; delegation is the only read path; `-race` unit test |
| Half-applied reload on transient error | Fail-closed: build fully into a NEW registry before swap; old registry untouched on any error |
| Operators expect env-pair rotation via SIGHUP | README limitation note; error-free but ineffective reload for env keys is logged (names only) |
| Long-lived SFTP sessions outlive revocation | Documented: SSH auth happens at handshake; revoked keys cannot *start* sessions, existing sessions persist until disconnect (pre-existing property of per-connection auth, not introduced here) |
| Signal goroutine swallows signals during reload | Reload is fast (file read + build); serialize via the signal channel loop, log duration |
| Secret leakage in reload logs | Reload logs identity NAMES and access key IDs only (log-safe per master Notes); secrets never printed |

## Open Decisions (for review)

1. Swap-install order in wiring: wrap at construction (recommended — env
   fallback `CredentialSource` path in auth_adapter.go:276 also rotates) vs.
   wrapping only the registry hook.
2. Whether reload should also re-read the server's listening/bucket config
   (explicitly OUT of scope here — auth only; a broader "hot config" leaf
   would supersede).
