# Identity Model + Multi-Identity Config - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** The shared credential→identity→grant core: `IdentityRegistry` /
  `MultiRegistry` in `internal/auth`, grant query methods on `Identity`, the
  backward-compatible `identities` + `auth` config schema in package main, and
  the env-pair merge rule that preserves today's behavior exactly.
- **Dependencies:** none (base of the chain). Everything else in this tree
  consumes this leaf's registry.
- **Estimated Context:** 70K
- **Concurrency Group:** A (dispatched first)

## Goal

`internal/auth` today is a frozen placeholder: `Authenticator`, `Identity`
(with always-nil `BucketGrants`), `Grant`, and `CredentialSource`
(`internal/auth/credentials.go`) which resolves ONE pair from package main's
`serverCredentials`. This leaf gives the seam its real model:

1. `IdentityRegistry` — the interface all credential kinds resolve through
   (access key, Basic username/password, SSH public key). Option 2 (OIDC) and
   option 3 (plugin) authenticators implement this same interface later.
2. `MultiRegistry` — the config-backed implementation: N identities, each with
   optional grants, wildcard support, constant-time secret comparison.
3. Grant query methods on `Identity` (`CanRead`/`CanWrite`) so the grant
   decision exists in exactly one place.
4. Config: the `identities` array + `auth.mode` key in `ServerConfig`
   (config.go), parsed, validated fail-loud, and merged with the legacy env
   pair — which remains a wildcard-grant identity so existing deployments
   behave identically (the issue's migration criterion).

## Context

Key facts to understand before implementing:

- **Module path:** `github.com/bhodgens/zeta-object` (repo dir still
  `mini-s3`). Existing internal imports use this path.
- **Frozen shapes (do NOT rename/remove):** `internal/auth/auth.go` has
  `Authenticator`, `Identity{AccessKeyID string; BucketGrants map[string]Grant}`,
  `Grant{Read, Write bool}`; `internal/auth/credentials.go` has
  `CredentialSource { SecretKey(accessKeyID string) (string, bool) }`. This
  leaf ADDS types and methods only.
- **Env pair today:** config.go:277 `serverCredentials`, loaded by
  `loadCredentials()` from `ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY`
  (legacy `MINIS3_*` fallback — deprecated, keep working until two minor
  releases; use `credentialFromEnv` as-is), default `zetaadmin`/`zetaadmin`,
  set-but-empty warns and defaults. Do not change this behavior; wrap it.
- **Config parsing:** `ServerConfig` (config.go:24) unmarshals
  dataDir/buckets/listenAddr/certFile/keyFile + `frontends`/`backends`
  (added by earlier trees with the same fail-loud validation pattern this
  leaf copies). `config.json.example` documents every key — the example file
  itself is updated in leaf 07, not here.
- **Secret comparison:** the S3 adapter already uses `hmacEqual`
  (internal/frontend/s3/auth_adapter.go) for signatures. Basic-password
  comparison in this package must also be constant-time — use
  `crypto/subtle.ConstantTimeCompare` on fixed-length hashes (e.g. compare
  SHA-256 of both sides so length never leaks).
- **Fail-loud precedent:** an unknown backend name in `buckets` aborts startup
  (e2e case 15 proves it). Config validation here follows the same rule.

Key files:
- [master.md](../master.md) — Contracts 1–2 are this leaf's spec, verbatim
- internal/auth/auth.go, internal/auth/credentials.go — frozen shapes, extend around
- config.go — ServerConfig, loadCredentials, the frontends/backends parsing pattern

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// File: internal/auth/registry.go (new)
package auth

type IdentityConfig struct {
    Name           string               `json:"name"`            // required, unique
    AccessKey      string               `json:"accessKey"`       // required, unique
    SecretKey      string               `json:"secretKey"`       // required
    Grants         map[string]string    `json:"grants,omitempty"` // bucket -> "readonly"|"readwrite"; absent ⇒ readwrite all
    SSHPublicKeys  []string             `json:"sshPublicKeys,omitempty"`
}

type IdentityRegistry interface {
    LookupByAccessKey(accessKeyID string) (Identity, bool)
    LookupByBasicCredential(username, password string) (Identity, bool)
    LookupByPublicKey(authorizedKey string) (Identity, bool)
}

// NewMultiRegistry validates and builds. Rules (all fail-loud):
//   - name, accessKey, secretKey non-empty
//   - no duplicate names, access keys, or SSH public keys
//   - grant values only "readonly" | "readwrite"; grant key "*" allowed once
//     per identity; other keys are bucket names (not validated against config
//     buckets here — unknown bucket = grant that never matches, logged at build)
func NewMultiRegistry(identities []IdentityConfig) (*MultiRegistry, error)
```

```go
// File: internal/auth/auth.go — ADD only (shape of Identity/Grant unchanged):
// CanRead(bucket): grants["*"] or grants[bucket] is readonly/readwrite.
// CanWrite(bucket): grants["*"] or grants[bucket] is readwrite. Write ⇒ Read.
// Zero-grant identity (nil map): no access to any bucket.
func (id Identity) CanRead(bucket string) bool
func (id Identity) CanWrite(bucket string) bool
```

```go
// File: internal/auth/env.go (new) — legacy-pair bridge, consumed by config.go:
// EnvPair() IdentityConfig returns the env-derived identity (name "env",
// wildcard readwrite) using loadCredentials's values. Exported via an
// export_test surface if main-package tests need it; production wiring calls it
// from package main (config.go), not from this package.
```

Config schema (ServerConfig additions, package main):

```go
// File: config.go — added fields (omitempty everywhere for backward compat):
type ServerConfig struct { /* existing fields unchanged */ 
    Identities []auth.IdentityConfig `json:"identities,omitempty"`
    Auth       AuthConfig            `json:"auth,omitempty"`
}
type AuthConfig struct {
    Mode string `json:"mode,omitempty"` // "" (default) | "none" (leaf 05)
}
```

Merge/startup rule (implement as `buildIdentityRegistry()` in package main,
called from the same place `loadCredentials()` runs):

1. `env := auth.EnvPair()` (always present — migration contract).
2. Append config `identities`. Duplicate access key against env or another
   identity ⇒ startup error naming the offender.
3. Empty `identities` + env pair ⇒ registry with exactly one wildcard identity
   = today's behavior, byte for byte.
4. `auth.mode` values other than `""`/`"none"` ⇒ startup error.

### What This Leaf Consumes

Nothing new — stdlib plus the existing `internal/auth` package it extends.

## Tasks

### Task 1: Grant query methods on Identity

**Objective:** `CanRead`/`CanWrite` with wildcard and write⊃read semantics.

**Files:**
- Modify: `internal/auth/auth.go` (add methods only)
- Test: `internal/auth/grants_test.go`

**Step 1: Write failing tests** (table-driven; cases: nil grants, empty grants,
`*` readonly, `*` readwrite, specific bucket readonly, specific readwrite,
unknown bucket, write implies read, exact-beats-nothing but `*` covers all —
per master Contract 1 semantics: `*` wildcard, Write implies Read).

**Step 2:** `go test ./internal/auth/ -run TestIdentity -v` → FAIL (methods missing).

**Step 3: Implement** the two methods on `Identity` (small, pure, no I/O).

**Step 4:** same command → PASS.

### Task 2: `IdentityRegistry` interface + `MultiRegistry`

**Objective:** The three-way lookup with constant-time secret checks and
fail-loud construction.

**Files:**
- Create: `internal/auth/registry.go`
- Test: `internal/auth/registry_test.go`

**Step 1: Write failing tests.** Table-driven, covering at minimum:

- `NewMultiRegistry` accepts a valid set (env-like pair + 2 configured
  identities, one with `*` readwrite, one with `photos: readonly`).
- Rejects: empty name / empty accessKey / empty secretKey; duplicate name;
  duplicate accessKey; duplicate SSH public key; grant value `"admin"`;
  `*` grant value `"readonly"` is legal (verify), grant key `""` illegal.
- `LookupByAccessKey` hit returns Identity with populated `BucketGrants`
  (translate `readonly`→`Grant{Read:true}`, `readwrite`→`Grant{Read:true,Write:true}`,
  absent grants → `*` readwrite); miss returns `(Identity{}, false)`.
- `LookupByBasicCredential` hit (username=accessKey, password=secretKey) →
  same identity; wrong password → miss; unknown user → miss; miss takes
  constant time regardless (assert via comparing that a wrong-password check
  on an existing user and on a missing user both take the comparison path —
  at minimum, both hash-then-compare, never early-return on user miss;
  test with a timing-insensitive structural check: a nil identity never
  short-circuits before the compare).
- `LookupByPublicKey` matches an exact authorized-key string; mismatch → miss.
- `MultiRegistry` still satisfies `auth.CredentialSource` (compile-time
  assertion + one behavioral test: `SecretKey` returns the pair for a known
  access key) so the existing S3 adapter keeps compiling against it.

**Step 2:** `go test ./internal/auth/ -run TestMultiRegistry -v` → FAIL.

**Step 3: Implement.** Hash secrets with SHA-256 at build time, store hashes,
compare with `subtle.ConstantTimeCompare`. Keep `Grant` translation in ONE
helper. Public keys: store normalized (trimmed) strings; matching is exact
string equality over the normalized form (fingerprint logging is leaf 04).

**Step 4:** same command → PASS.

### Task 3: Config schema + env merge + fail-loud validation

**Objective:** `identities`/`auth` keys parse, validate, and merge with the
legacy env pair; absence changes nothing.

**Files:**
- Modify: `config.go` (ServerConfig fields, `buildIdentityRegistry()`, wire
  into the existing load path where `loadCredentials()` runs)
- Test: `config_identities_test.go` (new, package main)

**Step 1: Write failing tests.** Cover:

- Config JSON with no `identities`/`auth` keys parses exactly as today
  (golden: same ServerConfig fields as before the change) — the migration test.
- Config with 2 identities + env pair builds a 3-identity registry; env
  identity is named `"env"` with wildcard readwrite.
- Duplicate accessKey (config-vs-env, config-vs-config) → `loadConfig`
  path returns an error naming the offending access key / identity name.
- Invalid grant value / empty required field / bad `auth.mode` → error, never
  silent fallback.
- `MINIS3_*`-only environment still produces the env identity (legacy prefix
  path unchanged).
- Set-but-empty env var: warning + default (existing behavior, now via EnvPair).

**Step 2:** `go test -run TestIdentities -v` (package main) → FAIL.

**Step 3: Implement** per the merge rule in Contracts. Reuse the existing
validation-error style (the `frontends`/`backends` parsing introduced one —
match it).

**Step 4:** same command → PASS.

### Task 4: Coverage floor re-measurement (AGENTS.md rule)

**Files:**
- Modify: `Makefile` (COVER_MIN) and/or `.github/workflows/check.yml` floor
  comments — ONLY if measured aggregate moved.

**Steps:** run `make test-cover-enforce`; if the measured total differs from
the floor's assumption, update the floor to measured-rounded-down with the
comment `// measured <YYYY-MM-DD> after auth leaf 01 added internal/auth
registry code; ratchet up when leaves 02/03 tests land`. This package is new
heavily-tested code, so the floor should RISE — record the new number.

## Self-Verification Checklist

Before reporting completion, verify:

- [ ] `go test ./internal/auth/ -v` and `go test -run TestIdentities .` (main) all pass
- [ ] Frozen shapes untouched: `Authenticator`, `Identity` fields, `Grant`,
      `CredentialSource` — methods ADDED to Identity, nothing renamed
- [ ] Back-compat: config without new keys parses identically (golden test)
      and the env-only registry equals today's single wildcard pair
- [ ] All secret comparisons constant-time (subtle on SHA-256 digests)
- [ ] Fail-loud validation: every malformed input aborts with a named error
- [ ] No secret material in logs or test fixtures (fake values only)
- [ ] `gofmt -l internal/auth .` clean at repo root for touched files; `make vet` passes
- [ ] Coverage floor re-measured per Task 4, comment updated if changed

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Every task implemented, tests first, all green
- [ ] Contracts 1–2 from master.md satisfied exactly (names, files, JSON keys)
- [ ] Migration contract proven by tests: env-only = today's behavior
- [ ] Registry is the single credential table; no copies elsewhere
- [ ] Conventions: stdlib only, table-driven tests, errors as values
- [ ] No scope creep: no adapter changes (leaf 02/03), no dev mode (leaf 05)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- `Identity.BucketGrants` stops being nil-for-everyone in THIS leaf at the
  model level; the S3 adapter does not populate it until leaf 02. That gap is
  intentional and sequenced — do not touch `internal/frontend/` here.
- The `username` of a Basic credential is the **access key**, by design
  (one namespace, one registry). Document that in the registry doc comment.
- Leaf 05's `auth.mode` value space is decided here (`""`/`"none"`) so config
  validation is complete from day one; the "none" BEHAVIOR is leaf 05.
