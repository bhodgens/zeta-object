# Zero-Auth Dev Mode with Loud Logging - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** `auth.mode: "none"` — an explicit, opt-in, LOUDLY-logged bypass
  of authentication, satisfying issue requirement (d): zero-auth dev mode
  stays available but must be loudly logged. Today's behavior (default
  `zetaadmin` pair) is NOT zero-auth — it stays the default and is untouched.
- **Dependencies:** leaf 01 (owns the `auth.mode` config key + validation;
  this leaf owns the behavior).
- **Estimated Context:** 50K
- **Concurrency Group:** B

## Goal

Operators running zeta-object locally for a quick trial want to skip
credential setup entirely — including the default `zetaadmin` pair, which is
still an auth check clients must sign with. This leaf adds `auth.mode:
"none"`: every request authenticates as a loud, fixed anonymous identity with
wildcard readwrite grants. "Loud" is contractual:

1. Startup prints a multi-line banner that the server is running with
   authentication DISABLED.
2. Every request logs a WARN line naming the anonymous principal — visible by
   default (stdlib `log`), not hidden behind a debug flag.

Safety rails:

- `auth.mode` absent/`""` = normal auth (registry required — leaf 01 already
  guarantees the env pair exists, so "normal" always has at least one
  identity).
- `auth.mode: "none"` + validation error in `identities` still aborts startup
  (dev mode is not an excuse for silently ignoring config mistakes).
- Dev mode is a REGISTRY-level bypass, not per-frontend: any frontend
  (S3 today, WebDAV/FTP later) that asks the seam authenticates the same way,
  keeping the protocol-neutrality story intact.

## Context

Key facts:

- Config: `auth.mode` key landed in leaf 01 (`AuthConfig.Mode`, validated to
  `""`/`"none"`). Package main already builds the registry via
  `buildIdentityRegistry()` and installs it into the s3 frontend via
  `s3.InstallIdentityRegistry` (leaf 02) / `InstallDefaultCredentialSource`
  (legacy).
- The S3 frontend's dispatch authenticates through its internal adapter; the
  cleanest injection point that serves ALL frontends is a
  `DevAuthenticator` installed as the registry's authenticator (or installed
  wherever each frontend pulls its `Authenticator()`), plus a dev-mode flag
  consulted by grant checks (dev identity = wildcard, so grants pass anyway).
- Loud logging: stdlib `log` (the repo's only logger — see
  `config.go` credential warnings for house style). Startup banner ≈3 lines,
  impossible to miss; per-request line at WARN text (`log.Printf("WARNING:
  ...")` — stdlib log has no levels; the word WARNING in caps is the house
  pattern from existing `log.Printf("Warning: ...")` calls).
- Existing default (`zetaadmin`/`zetaadmin` when env unset) must NOT change:
  dev mode is opt-in via config, not the env-unset default. Document that
  distinction — the issue says "zero-auth dev mode stays available"; today's
  availability = signed requests against zetaadmin. `auth.mode: "none"` is
  the NEW, louder escape hatch. README wording is leaf 07's job.

Key files:
- [master.md](../master.md) — Contract 6
- internal/auth/registry.go / credentials.go — where the authenticator chain lives
- config.go — `AuthConfig.Mode` (leaf 01), startup sequence
- s3_wiring.go — where package main installs auth into the frontend

## Interface Contracts (From Parent)

```go
// File: internal/auth/devmode.go (new)
package auth

// DevAuthenticator authenticates every request as the fixed anonymous
// identity with wildcard readwrite grants. It never fails.
// Loudness contract: construction emits the startup banner; each successful
// Authenticate emits one request-scoped warning line. No silent mode exists.
type DevAuthenticator struct{ /* logger */ }
func NewDevAuthenticator(l *log.Logger) *DevAuthenticator
func (d *DevAuthenticator) Banner()        // startup: multi-line, unmistakable
func (d *DevAuthenticator) Authenticate(r *http.Request) (Identity, error)
// Returns Identity{AccessKeyID: "anonymous", BucketGrants: map[string]Grant{"*": {Read: true, Write: true}}}

// Compile-time: var _ Authenticator = (*DevAuthenticator)(nil)
```

Package main wiring (`s3_wiring.go` or main.go startup): when
`serverConfig.Auth.Mode == "none"`, install `DevAuthenticator` as the S3
frontend's authenticator source (via a new narrow hook or by registering a
dev identity in the registry — pick ONE mechanism, prefer the authenticator
hook so the banner/log path is shared by all frontends later).

## Tasks

### Task 1: `DevAuthenticator`

**Files:**
- Create: `internal/auth/devmode.go`
- Test: `internal/auth/devmode_test.go`

**Step 1: Write failing tests:**

- `Authenticate` on an arbitrary request → anonymous identity, wildcard
  readwrite grants, nil error. Verify `CanRead`/`CanWrite` true for any
  bucket string (use `auth.Identity` methods from leaf 01).
- `Banner()` writes ≥3 lines to the injected logger containing (case-
  insensitive) "AUTHENTICATION DISABLED" and "development only".
- `Authenticate` writes exactly one line containing "WARNING" + "anonymous".
- Interface assertion: satisfies `auth.Authenticator`.
- Logger injectable (no global log dependency → testable via `bytes.Buffer`).

**Step 2:** `go test ./internal/auth/ -run TestDev -v` → FAIL.

**Step 3: Implement** (constructor takes `*log.Logger`, defaults to
`log.Default()` via a nil check).

**Step 4:** same command → PASS.

### Task 2: Config → wiring (`auth.mode: "none"`)

**Files:**
- Modify: `s3_wiring.go` (install dev authenticator when mode is none)
- Modify: `config.go` ONLY if the leaf-01 validation needs the behavior hook
  (it should not — validation is leaf 01's)
- Test: `config_devmode_test.go` (new, package main) + extend
  `internal/frontend/s3` wiring test if the install path needs one

**Step 1: Write failing tests (package main):**

- Config with `{"auth": {"mode": "none"}}` → startup path installs the dev
  authenticator (observable: the injected-logger banner lines appear in a
  captured log, and an UNSIGNED request to the wired frontend authenticates —
  drive via the s3 export_test surface or an httptest round-trip using a
  deliberately unsigned/garbage-signature request).
- Config with mode unset → unsigned request still rejected
  `AuthorizationHeaderMissing` (default stays authenticated).
- Mode `"none"` + invalid `identities` → startup error (both rails active).

**Step 2:** FAIL → **Step 3:** implement wiring → **Step 4:** PASS.

### Task 3: Coverage + gates

`make test-cover-enforce`; re-measure floors per AGENTS.md if moved (comment
+ ratchet note). `make vet`, `make lint NEW_FROM_REV=<rev>`, `make fmt`.

## Self-Verification Checklist

- [ ] `go test ./internal/auth/ -v` and package-main tests green
- [ ] Default (mode unset) behavior byte-identical to pre-tree (existing e2e
      cases 01–17 pass unchanged)
- [ ] Dev mode is opt-in, loud (banner + per-request WARNING), and cannot
      mask config validation errors
- [ ] No secrets involved (anonymous has no secret); nothing logged that
      isn't already public (method/path are already logged by dispatch)
- [ ] `make fmt`, `make vet`, `make lint` clean

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Tasks 1–3 done, tests first, green
- [ ] Contract 6 satisfied exactly (identity shape, loudness contract)
- [ ] Scope held: no README/example edits (leaf 07), no e2e case (leaf 06)
- [ ] Single bypass mechanism (authenticator hook), not sprinkled checks

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- Leaf 06's e2e case asserts the banner is greppable in server logs — keep
  `Banner()` output stable and single-line-per-fact.
- If wiring discovers the s3 frontend pulls its authenticator at construction
  rather than via a hook, extend the existing seam pattern (mirroring
  `InstallIdentityRegistry`) rather than special-casing dispatch — future
  frontends must inherit dev mode for free.
