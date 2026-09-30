# Pluggable Authentication (GH issue #4) - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root) — implements GitHub issue #4 (`auth: pluggable authentication architecture`, tracked as `docs/plans/issue-auth-pluggable.md`)
- **Children:** 7 leaf documents under this node
- **Scope:** Multi-identity, per-identity bucket grants, protocol-neutral credential adapters (SigV4 now, Basic now, SFTP public-key hook), zero-auth dev mode with loud logging, migration/back-compat, and docs.

## Decision (recorded per issue acceptance criterion)

**Option 1: system-level key association**, with interfaces shaped so Option 2
(OAuth/OIDC) and Option 3 (plugin/subprocess authenticator) can slot in later.

Rationale (mirrors the issue body): zeta-object is a self-hosted, offline-friendly
single binary. Option 1 needs no new protocols, no network calls at auth time,
and ~2–3 days. OAuth/OIDC cannot express S3 SigV4 anyway (S3 clients sign with
long-lived keys), and a plugin subsystem is unjustified until an external auth
source (LDAP etc.) becomes a requirement. The seam that keeps 2 and 3 open is
`auth.Authenticator` (frozen v1 shape) plus the new `auth.IdentityRegistry`
interface (leaf 01): both are interfaces, so an OIDC-backed or subprocess-backed
registry is a drop-in implementation, not a rewrite.

The frontend-interface-2026-09 tree pinned the v1 adapter shape
`Authenticator.Authenticate(r) (Identity, error)` and `Identity{AccessKeyID,
BucketGrants}`. This tree **fleshes out** that placeholder without renaming or
removing anything: `Identity.BucketGrants` finally gets meaning, and every
frontend maps its wire credentials onto the SAME registry lookup + grant check.

## Goal

Today the server has exactly one credential pair for every client
(`config.go:277` `serverCredentials`, loaded from `ZETAOBJECT_ACCESS_KEY` /
`ZETAOBJECT_SECRET_KEY` with `MINIS3_*` fallback, default `minioadmin`/
`minioadmin`). `internal/frontend/s3/auth_adapter.go` resolves the signing
secret through `auth.CredentialSource` (`internal/auth/credentials.go`) and
returns `Identity{AccessKeyID}` with **nil** grants; nothing enforces
`Grant.Read/Write`. There is no way to distinguish clients, no per-identity
permissions, and no story for non-S3 frontends.

After this tree:

1. A **multi-identity registry** in `internal/auth` maps credentials →
   `Identity` (with populated `BucketGrants`) and answers authorization
   questions (`CanRead`/`CanWrite`) with a `*` wildcard bucket.
2. The **S3 frontend** resolves SigV4 access key IDs through the registry, so
   different keys authenticate as different identities and grants are enforced
   in `dispatch.go` before any handler runs.
3. A **Basic-auth adapter + shared grant helper** serve HTTP frontends without
   SigV4 (WebDAV/ownCloud/FTP-over-HTTP shape today; the real frontends land
   per their own GH issues — this tree ships the adapter and proves it from a
   second wire surface).
4. An **SFTP public-key adapter hook** exists interface-only, ready for the
   SFTP frontend issue.
5. **Zero-auth dev mode** (`auth.mode: "none"`) stays available and logs
   loudly at startup and per request.
6. **Migration is invisible**: deployments with only the env pair behave
   byte-identically to today (the env pair becomes a wildcard-grant identity).
7. Docs and `config.json.example` describe the new config surface; the AGENTS.md
   e2e rule is satisfied in the same change (leaf 06).

Acceptance criteria from the issue, and where this tree proves them:

| Issue criterion | Proven by |
|---|---|
| Decision recorded with rationale | This master, Decision section; leaf 07 posts it to the issue + README |
| Multi-identity config format, backward compatible | Leaf 01 (schema + loading), leaf 06 (tests) |
| Identity+grant enforcement tested from ≥2 frontends (protocol-neutral) | Leaf 03 (Basic wire surface) + leaf 02 (S3), cross-checked in leaf 06's e2e case |
| Migration path: env-pair deployments keep working | Leaf 01 (merge rule) + leaf 06 (back-compat tests + e2e) |

## Architecture

One shared core, thin protocol adapters:

```
config.json / env
      │  (leaf 01: parse + validate + merge; package main config.go)
      ▼
auth.IdentityRegistry          ← the ONE credential→identity decision
      │  LookupByAccessKey / LookupByBasicCredential / LookupByPublicKey
      ▼
auth.Identity{AccessKeyID, BucketGrants}
      │  CanRead(bucket) / CanWrite(bucket)   ← the ONE grant decision
      ├──────────────────────────────┐
      ▼                              ▼
internal/frontend/s3            internal/auth (Basic adapter, shared)
sigv4Authenticator              basicAuthenticator + frontends.Authorize
(leaf 02: registry-backed       (leaf 03: used by any HTTP frontend;
 CredentialSource + grant        leaf 04: PublicKeyAuthenticator hook
 checks in dispatch.go)          for the future SFTP frontend)
      │
      ▼
leaf 05: auth.mode="none" bypasses both adapters, loudly
leaf 06: migration/back-compat tests + scripts/e2e/cases/18-auth-identities.sh
leaf 07: docs (config.json.example, README) + decision posted to issue #4
```

Key rule binding on every leaf: **the credential→identity decision and the
grant decision each live in exactly one place (`internal/auth`). Frontend
adapters only translate wire formats (SigV4 header, Basic header, SSH public
key) into registry lookups and render protocol-appropriate errors.** No
frontend may hold credential tables or grant logic.

Frozen contracts (frontend-interface-2026-09 master Contracts 1–2) are
untouched: `Frontend`, `ProtocolCaps`, `auth.Authenticator`,
`auth.Identity`, `auth.Grant`, `auth.CredentialSource` all keep their exact
names, shapes, and file locations. This tree only adds types and wires them.

## Interface Contracts

### Contract 1: `IdentityRegistry` + lookup keys (leaf 01)

```go
// File: internal/auth/registry.go (new)
package auth

// IdentityRegistry resolves wire credentials to identities. This is the
// interface an OAuth/OIDC (option 2) or subprocess (option 3) backend would
// implement — option 1 ships the static, config-backed implementation.
type IdentityRegistry interface {
    // LookupByAccessKey: SigV4 access key ID → identity.
    LookupByAccessKey(accessKeyID string) (Identity, bool)
    // LookupByBasicCredential: Basic username+password → identity.
    // Constant-time password comparison; password == secret key by design.
    LookupByBasicCredential(username, password string) (Identity, bool)
    // LookupByPublicKey: authorized_key string → identity (SFTP later).
    LookupByPublicKey(authorizedKey string) (Identity, bool)
}

// MultiRegistry fans a parsed config (+ env fallback pair) into lookups.
// Constructed by package main at startup (leaf 01), installed into the s3
// frontend and any future frontend (leaf 02/03 wiring).
func NewMultiRegistry(identities []IdentityConfig) (*MultiRegistry, error)

// Grant queries on Identity (file: internal/auth/auth.go — methods ADDED,
// struct shape unchanged):
func (id Identity) CanRead(bucket string) bool   // wildcard "*"
func (id Identity) CanWrite(bucket string) bool  // wildcard "*"
// Write implies Read.

// Owner: 01-identity-model-config.md
// Consumers: 02 (s3 adapter), 03 (basic adapter + enforcement), 04 (key hook)
```

### Contract 2: multi-identity config schema (leaf 01)

```jsonc
// config.json extension — BACKWARD COMPATIBLE: key absent ⇒ exactly today's
// behavior (single env pair, wildcard grants).
"identities": [
  {
    "name": "ci-bot",                       // required, unique, logged only
    "accessKey": "AKIDZETACIBOT01",         // required, unique
    "secretKey": "…",                       // required
    "grants": {                             // optional; absent ⇒ full readwrite
      "*": "readwrite",                     // wildcard allowed
      "photos": "readonly"                  // "readonly" | "readwrite"
    },
    "sshPublicKeys": []                     // optional; SFTP frontend consumes later
  }
],
"auth": { "mode": "" }                      // "" (default, required) | "none" (leaf 05)
```

Merge rule (the migration contract): the env pair
(`ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY`, `MINIS3_*` fallback, default
`minioadmin`) is **always** present in the registry as identity name `"env"` with
wildcard readwrite grants — identical to today's effective behavior. Configured
`identities` are added alongside. Duplicate access keys (config-vs-config or
config-vs-env) abort startup fail-loud. Validation errors abort startup — never
a silent fallback (same semantic as unknown-backend in case 15).

### Contract 3: S3 adapter consumes the registry (leaf 02)

```go
// Files: internal/frontend/s3/seam.go (InstallIdentityRegistry),
// internal/frontend/s3/auth_adapter.go (grants on Identity), s3_wiring.go (main).
// s3.InstallIdentityRegistry(reg auth.IdentityRegistry) becomes the credential
// source of record: credentialSecret() resolves via LookupByAccessKey.
// authenticateRequest/authenticatePresignedRequest return the FULL Identity
// (grants populated) instead of Identity{AccessKeyID: …}.
// dispatch.go serveHTTP: after auth, authorize bucket/object ops against
// Identity.CanRead/CanWrite → AccessDenied S3 error (403) before dispatch.
// Service-level (ListBuckets) filters to granted buckets when the identity
// is not wildcard.
```

`auth.CredentialSource` is NOT removed — `IdentityRegistry` satisfies the need
and richer; the old interface remains for compatibility (and `MultiRegistry`
also satisfies it via `SecretKey`).

### Contract 4: Basic adapter + shared enforcement (leaf 03)

```go
// File: internal/auth/basic.go (new)
type BasicAuthenticator struct{ /* registry */ }
// Implements auth.Authenticator: parses Authorization: Basic, constant-time
// credential check via LookupByBasicCredential, returns Identity with grants.

// File: internal/frontend/authz.go (new)
// AuthorizeRequest maps an Identity + request facts (bucket, wants-write) to
// nil or an authorization error. Shared by ALL HTTP frontends so the grant
// decision is protocol-neutral; non-HTTP frontends (FTP/SFTP) call
// Identity.CanRead/CanWrite directly.
func AuthorizeRequest(id auth.Identity, bucket string, write bool) error
```

Protocol-neutrality proof (issue acceptance criterion): leaf 03 exercises the
SAME `MultiRegistry` through two wire surfaces — the S3 frontend (SigV4) and a
test-only Basic-auth HTTP frontend driven by `httptest` — asserting the same
identity and the same grant allow/deny outcomes. Leaf 06 repeats one slice of
it at e2e level.

### Contract 5: SFTP public-key hook (leaf 04, interface-only)

```go
// File: internal/auth/publickey.go (new)
// PublicKeyAuthenticator is the seam the future SFTP frontend consumes.
// Implemented by MultiRegistry.LookupByPublicKey in leaf 01; leaf 04 adds the
// canonicalization rules (authorized_keys format, fingerprint logging) and
// tests. No SFTP frontend is built in this tree.
type PublicKeyAuthenticator interface {
    AuthenticatePublicKey(authorizedKey string) (Identity, error)
}
```

### Contract 6: zero-auth dev mode (leaf 05)

```go
// File: internal/auth/devmode.go (new)
// DevAuthenticator returns Identity{AccessKeyID: "anonymous", wildcard
// readwrite grants} for every request when auth.mode == "none".
// Loud = startup banner + one WARN line per request (rate-guarded is fine,
// but the line must appear; tests assert the log output).
```

## Child Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | [01-identity-model-config.md](01-identity-model-config.md) | leaf | none | 70K | A |
| 02 | [02-sigv4-adapter.md](02-sigv4-adapter.md) | leaf | 01 | 80K | B |
| 03 | [03-basic-auth-grants.md](03-basic-auth-grants.md) | leaf | 01 | 70K | B |
| 04 | [04-sftp-key-hook.md](04-sftp-key-hook.md) | leaf | 01 | 40K | B |
| 05 | [05-zero-auth-dev-mode.md](05-zero-auth-dev-mode.md) | leaf | 01 | 50K | B |
| 06 | [06-migration-e2e.md](06-migration-e2e.md) | leaf | 01, 02, 03, 05 | 70K | C |
| 07 | [07-docs.md](07-docs.md) | leaf | 01–06 | 30K | D |

**Concurrency groups:** A first (registry + config is the dependency of
everything). B leaves (02/03/04/05) are independent of each other once 01
lands — dispatch in parallel or any order. C (06) integrates B's work and is
the AGENTS.md e2e gate. D (07) last, after the config surface is final.

**External dependencies (whole tree):** none new — builds entirely on the
landed frontend-interface-2026-09 seam (`Frontend`, `auth` v1 shapes) and the
existing e2e harness.

## Dispatch Protocol

1. **Dispatch Leaf 01** via `delegate_task` with the full leaf text + Coding
   Conventions + frozen-contract note. Boilerplate: "Do NOT commit. Do NOT run
   git add. Write code, run tests, report results only. Do NOT use read_file on
   existing source files — explore with search_files or terminal cat."
2. **Review leaf 01 in-session** (main model, not a delegated reviewer):
   contracts match, `make test` green, back-compat tests prove env-only
   behavior unchanged. Max 3 re-dispatch cycles, then escalate.
3. **Dispatch B leaves (02–05)** — may be parallel; same boilerplate. Each
   context must INLINE the frozen `auth` v1 shapes and the Contract 2 config
   schema from this master.
4. **Review B leaves in-session**, then **dispatch Leaf 06** (integration +
   e2e). Its context must note: `make e2e` runs ALL cases; the new case must
   keep the create/cleanup pairing and `BKT=` convention.
5. **Review leaf 06**, then **dispatch Leaf 07** (docs + issue comment).
6. **Integration review** (orchestrator, in-session):
   - `make check` (build + vet + fmt + lint + test + cover floor) green
   - `make e2e` green including the new `18-auth-identities.sh`
   - Backward compat: run with only env vars set (or unset → minioadmin
     default) and confirm behavior/logs match pre-tree
   - grep check: no credential table or grant logic outside `internal/auth`
     (`grep -rn "BucketGrants" --include='*.go' | grep -v internal/auth |
     grep -v _test` returns only seam-plumbing lines)
   - Coverage floors: leaves 01/02 moved/added package code — verify
     `make test-cover-enforce` passes and floors were re-measured per AGENTS.md
7. Commit per leaf with conventional messages (`feat(auth): ...`). Update the
   tracking table. Post the decision summary to issue #4 (leaf 07).

## Review Checklist

The orchestrator verifies each child in-session:

- [ ] All tasks from the leaf implemented; tests written first and passing
- [ ] Frozen contracts untouched: `Frontend`, `ProtocolCaps`,
      `auth.Authenticator`, `auth.Identity`, `auth.Grant`,
      `auth.CredentialSource` — shapes unchanged (methods MAY be added to
      `Identity` per Contract 1)
- [ ] Single-decision rule holds: credential tables and grant logic only in
      `internal/auth`; adapters only translate wire formats
- [ ] Fail-loud config validation (abort startup, never silent fallback)
- [ ] Constant-time comparisons for all secret/password checks
- [ ] Secrets never logged (names and access key IDs may be; secret keys and
      Basic passwords never)
- [ ] AGENTS.md e2e rule: user-facing config surface (`identities`, `auth`)
      covered by `scripts/e2e/cases/18-auth-identities.sh` in the same change
- [ ] Coverage floors re-measured where code moved packages (AGENTS.md rule)
- [ ] `gofmt` clean, no debug artifacts, no line-number corruption

Output per leaf: APPROVED or list of specific gaps.

## Coding Conventions

- **Language:** Go 1.25 (module `github.com/bhodgens/zeta-object`), stdlib only — no new dependencies
- **Style:** gofmt + goimports with `-local github.com/bhodgens/zeta-object` (`make fmt`)
- **Naming:** exported = PascalCase with doc comments; packages lowercase single words
- **Errors:** errors as values; wrap with `%w`; fail-loud at startup for config problems
- **Tests:** stdlib `testing`, table-driven, `_test.go` alongside source; no testify
- **Gates per leaf:** `make vet`, `make lint NEW_FROM_REV=<rev>`, `make test`,
  `make test-cover-enforce`; e2e leaves additionally `make e2e`

## Completion Tracking Table

| Child | Status | Iterations | Review Notes |
|-------|--------|------------|-------------|
| 01-identity-model-config | PENDING | 0 | |
| 02-sigv4-adapter | PENDING | 0 | |
| 03-basic-auth-grants | PENDING | 0 | |
| 04-sftp-key-hook | PENDING | 0 | |
| 05-zero-auth-dev-mode | PENDING | 0 | |
| 06-migration-e2e | PENDING | 0 | |
| 07-docs | PENDING | 0 | |

Status values: PENDING | IN_PROGRESS | IMPLEMENTED | REVIEWED | COMPLETE | BLOCKED

## Integration Test Plan

1. **Unit:** `go test ./internal/auth/ ./internal/frontend/... -v` — registry
   lookups, grant math (wildcard, write⊃read), adapter tables, dev-mode logs.
2. **Cross-frontend neutrality (the issue's ≥2-frontend criterion):** leaf 03's
   test drives one `MultiRegistry` through (a) the real S3 frontend via SigV4
   requests and (b) a Basic-auth HTTP test frontend; identical identities and
   grant outcomes must result.
3. **Migration:** leaf 06 — env-only config (no `identities` key) behaves
   exactly as today: same successful requests, same `InvalidAccessKeyId` on
   unknown keys, no new log lines on the happy path. Env pair + identities
   merge; duplicates abort.
4. **E2E:** `make e2e` — new `scripts/e2e/cases/18-auth-identities.sh` covers
   multi-identity + readonly-grant denial + back-compat env-pair case + dev-mode
   loud banner (private-server pattern, like case 15).
5. **Static gates:** `make check`; coverage floor verification per AGENTS.md.

## Coverage Floors (moving-with-code rule)

AGENTS.md hard rule: floors move with code, re-measured in the SAME change,
set to the measured value rounded **down** with a ratchet-back comment.
This tree mostly ADDS packages (`internal/auth` grows, all new code is
well-tested), so the aggregate floor should rise, not fall — but leaf 02 moves
grant-check responsibility from nowhere to `dispatch.go` and leaf 01 changes
`config.go`. Each of leaves 01, 02, and 06 MUST:

1. Run `make test-cover-enforce` before reporting.
2. If `COVER_MIN` (Makefile) or any per-package floor comment no longer
   reflects measured reality, update the floor in the same change with the
   comment format `// measured <date> after <change>; ratchet up when <x> lands`.
3. Never lower a floor without a measured number in the comment.

## Structural Completeness Check (Before Dispatch)

Confirm this master contains: Meta, Decision, Goal, Architecture, Interface
Contracts, Child Index, Dispatch Protocol, Review Checklist, Coding
Conventions, Completion Tracking Table, Integration Test Plan, Coverage
Floors, Notes. All present.

## Notes

- **Frozen v1 shapes are load-bearing.** `frontend-interface-2026-09` pinned
  `Authenticator.Authenticate(r) (Identity, error)`; two trees (conformance,
  coverage floors) reference it. Add methods/types; never rename or remove.
- **Secrets hygiene:** access key IDs and identity names are log-safe; secret
  keys, Basic passwords, and private keys are never logged, never in test
  fixtures that get committed (use obviously-fake values like
  `wJalrXUtnFEMIfake`), and never appear in e2e case output.
- **Out of scope:** the WebDAV, FTP/FTPS, SFTP, and ownCloud FRONTENDS
  themselves (their own GH issues). This tree ships the adapters and hooks they
  will consume; leaf 03's Basic adapter is proven from a test frontend, not a
  shipped one. OAuth/OIDC and plugin/subprocess backends are future work that
  `IdentityRegistry` deliberately leaves room for.
- **Zero-auth stays opt-in.** `auth.mode: "none"` must never become the default
  and must be impossible to combine silently with a misconfigured registry —
  if `identities` fail validation, startup aborts even in dev mode.
- **Riskiest leaf is 02.** It touches `sigv4Authenticator`, the hottest and
  most security-sensitive code in the repo (hardening-2026-09 fixed 9 issues
  there). The regression gate is the existing SigV4 test suite
  (`internal/frontend/s3/sigv4_test.go`, `auth_adapter_test.go`) plus e2e cases
  01–13 — if any of those move, leaf 02 is wrong, full stop.
