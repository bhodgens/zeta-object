# Basic-Auth Adapter + Shared Grant Enforcement - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** The `internal/auth` Basic-authenticator (Basic header → identity
  via the registry) and the frontend-shared `AuthorizeRequest` grant helper;
  plus the cross-frontend protocol-neutrality proof the issue demands:
  ONE registry, TWO wire surfaces (SigV4 and Basic), identical grant outcomes.
- **Dependencies:** leaf 01 (registry + grants). Independent of leaf 02's
  S3 changes except for the neutrality proof, which reads leaf 02's wired S3
  frontend (if 02 has not landed, prove via the `sigv4Authenticator` interface
  instead — see Task 3 note).
- **Estimated Context:** 70K
- **Concurrency Group:** B

## Goal

WebDAV, ownCloud, and FTP-auth-over-HTTP clients do not sign SigV4 — they send
`Authorization: Basic`. This leaf builds that adapter once, in
`internal/auth`, so every HTTP frontend shares it instead of growing per-
protocol credential code, and extracts the grant decision into
`internal/frontend.AuthorizeRequest` so the check is provably protocol-neutral
(the issue's "tested from at least two different frontends" criterion).

Concretely:

1. `auth.BasicAuthenticator` implements the frozen `auth.Authenticator`
   interface: parse Basic header → `LookupByBasicCredential(username,
   password)` → `Identity` with grants, or a typed rejection.
2. `frontend.AuthorizeRequest(id, bucket, write)` — the shared helper S3
   dispatch (leaf 02) and future WebDAV/ownCloud frontends call; errors carry
   enough structure for each frontend to render its own wire error.
3. A minimal **test-only** Basic-auth HTTP frontend drives the whole stack
   (Basic adapter → registry → AuthorizeRequest) under `httptest`, proving
   protocol neutrality without shipping a real WebDAV frontend (out of scope —
   its own GH issue consumes this leaf's work).

## Context

Key facts:

- Frozen v1: `auth.Authenticator { Authenticate(r *http.Request) (Identity,
  error) }`. The Basic adapter fits it exactly — that is the point: the
  seam was shaped for this in frontend-interface-2026-09.
- Registry from leaf 01: `LookupByBasicCredential(username, password)` —
  username IS the access key (one namespace), constant-time compared.
- Leaf 02 puts grant checks in the S3 dispatch. To keep the grant decision in
  ONE place, this leaf extracts that logic into `internal/frontend` (package
  shared by all frontends) and leaf 02's dispatch calls it. If leaf 02 landed
  first with inline checks, REFACTOR it to call `AuthorizeRequest` — do not
  tolerate two grant implementations. Coordinate via the orchestrator if both
  leaves run in parallel.
- Basic auth over plain HTTP is forbidden in production (listener is TLS
  already: certFile/keyFile); the adapter itself need not check scheme —
  the server only serves HTTPS. Note it in the doc comment.
- Errors as values: define `auth.ErrBasicMissing`, `auth.ErrBasicMalformed`,
  `auth.ErrBadCredentials` (sentinels or typed) so frontends render
  protocol-appropriate responses (S3: 403 AccessDenied; WebDAV later: 401 +
  `WWW-Authenticate: Basic`).

Key files:
- [master.md](../master.md) — Contracts 1 and 4
- internal/auth/ (leaf 01's registry) — consumed, not modified beyond adding basic.go
- internal/frontend/ — new authz.go (shared helper), new test frontend under
  internal/frontend/internal_test support files (test-only)

## Interface Contracts (From Parent)

```go
// File: internal/auth/basic.go (new)
package auth

// BasicAuthenticator authenticates HTTP Basic credentials against the
// registry. Username is the access key; password is the secret key.
// The server is TLS-only, so credentials cross the wire once, encrypted.
type BasicAuthenticator struct{ /* registry */ }
func NewBasicAuthenticator(reg IdentityRegistry) *BasicAuthenticator
// Implements auth.Authenticator. Rejections are typed:
//   missing header  → ErrBasicMissing
//   non-Basic scheme or bad base64 → ErrBasicMalformed
//   unknown user / wrong password → ErrBadCredentials
func (b *BasicAuthenticator) Authenticate(r *http.Request) (Identity, error)
```

```go
// File: internal/frontend/authz.go (new)
package frontend

// AuthzError is the protocol-neutral authorization rejection. Frontends
// render it however their wire protocol speaks (S3 AccessDenied XML,
// WebDAV 401/403, ...). Never silent emulation.
type AuthzError struct {
    Bucket string
    Write  bool
}
func (e *AuthzError) Error() string

// AuthorizeRequest checks id's grants for bucket access. write=true requires
// CanWrite; write=false requires CanRead. nil means allowed.
func AuthorizeRequest(id auth.Identity, bucket string, write bool) error
```

## Tasks

### Task 1: `BasicAuthenticator`

**Files:**
- Create: `internal/auth/basic.go`
- Test: `internal/auth/basic_test.go`

**Step 1: Write failing tests** (table-driven; construct a small
`MultiRegistry` from leaf 01):

- Valid `Authorization: Basic base64(accessKey:secretKey)` → Identity with
  expected AccessKeyID and grants.
- Missing header → `ErrBasicMissing`.
- `Authorization: Bearer xyz` / garbage base64 / no colon in decoded pair →
  `ErrBasicMalformed`.
- Unknown username → `ErrBadCredentials`; known username + wrong password →
  `ErrBadCredentials` (same sentinel — never distinguish for callers).
- Case: header with extra whitespace tolerated.
- Compile-time assertion `var _ Authenticator = (*BasicAuthenticator)(nil)`.

**Step 2:** `go test ./internal/auth/ -run TestBasic -v` → FAIL.

**Step 3: Implement.** `base64.StdEncoding` decode (accept padded + unpadded
via `RawStdEncoding` fallback), split on FIRST colon (passwords may contain
colons), registry lookup, return typed errors.

**Step 4:** same command → PASS.

### Task 2: Shared `AuthorizeRequest` helper

**Files:**
- Create: `internal/frontend/authz.go`
- Test: `internal/frontend/authz_test.go`
- Modify (if leaf 02 already landed): `internal/frontend/s3/dispatch.go` —
  replace inline grant checks with `AuthorizeRequest` calls (behavior
  identical; existing dispatch_grants_test.go stays green)

**Step 1: Write failing tests:** allowed read/write under `*` readwrite;
read allowed + write denied under readonly; both denied for ungranted bucket;
nil-grants identity denied everywhere; error carries bucket + write flag.

**Step 2:** FAIL. **Step 3:** implement (thin: calls `id.CanRead/CanWrite`;
the value is the single shared decision point, not the logic volume).
**Step 4:** PASS. If refactoring s3/dispatch.go: run the FULL S3 suite after.

### Task 3: Protocol-neutrality proof (two frontends, one registry)

**Objective:** The issue's acceptance criterion, as a durable test.

**Files:**
- Create: `internal/frontend/neutrality_test.go` (package `frontend_test`)

**Step 1: Write the failing test.** Build ONE `MultiRegistry` with:

- `AKRW` — wildcard readwrite
- `AKRO` — `neuro` bucket readonly

Then drive BOTH surfaces:

1. **S3 surface:** the real S3 frontend via `httptest` with SigV4-signed
   requests (reuse `internal/frontend/s3`'s exported test surface / helpers —
   `export_test_surface.go` exists for exactly this; add to it if needed).
2. **Basic surface:** a minimal test-only HTTP frontend (in the test file;
   ~30 lines: authenticates via `BasicAuthenticator`, calls
   `AuthorizeRequest`, 200/403) over `httptest`.

Assert BOTH surfaces produce IDENTICAL outcomes for the same
identity×operation×bucket matrix: AKRW read+write ok on any bucket; AKRO
read ok + write 403-rejected on `neuro`; both ops rejected on other buckets;
bad credentials rejected on both. This table IS the neutrality proof.

Note: if leaf 02 has not merged when this runs, the S3 leg uses
`sigv4Authenticator.Authenticate` + `AuthorizeRequest` directly (still the
real wire adapter); upgrade the test to full-HTTP-S3 when 02 lands. Flag which
mode ran in the report.

**Step 2:** FAIL → **Step 3:** fix whatever the proof exposes (that is its
job) → **Step 4:** PASS.

## Self-Verification Checklist

- [ ] All new tests pass: `go test ./internal/auth/ ./internal/frontend/... -v`
- [ ] Frozen `Authenticator`/`Identity`/`Grant` shapes untouched
- [ ] ONE grant decision (`AuthorizeRequest`); S3 dispatch delegates to it (or
      coordination with leaf 02 is flagged in the report)
- [ ] Bad-username and bad-password failures are indistinguishable to callers
- [ ] No secret material in logs; test fixtures use fake credentials
- [ ] Coverage floors re-checked (`make test-cover-enforce`); AGENTS.md
      re-measure + comment if moved
- [ ] `make fmt`, `make vet`, `make lint` clean

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Tasks 1–3 implemented, tests first, green
- [ ] Basic adapter lives in `internal/auth` (not in any frontend package)
- [ ] Neutrality test covers the full identity×op×bucket matrix on both surfaces
- [ ] No shipped WebDAV/ownCloud frontend (test-only surface) — scope held
- [ ] Errors are typed sentinels frontends can switch on

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The test-only Basic frontend intentionally does NOT join the frontend
  registry or ship in the binary. The WebDAV issue's frontend will be the
  first production consumer; this leaf only guarantees the model is
  protocol-agnostic BEFORE that frontend exists.
- Do not add a `WWW-Authenticate` challenge to any shipped surface here —
  response-shaping is per-frontend and belongs to the frontends' own issues.
