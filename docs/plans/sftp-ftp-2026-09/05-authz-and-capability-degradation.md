# 05 - Grant Enforcement + Capability Degradation - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Wire `Identity.BucketGrants` into both frontends' driver calls
  (read vs write per bucket) and finish the capability-degradation mapping
  (no conditional reads, no multipart → protocol-appropriate responses).
  Replace leaves 03/04's local verifier/checker fakes with the auth-tree-
  backed adapter.
- **Dependencies:** 03-ftp-ftp-frontend.md, 04-sftp-frontend.md, and the
  **auth tree (`docs/plans/auth-2026-09/`, issue #4) LANDED**. Verify the
  landed `internal/auth` surface before writing any code here.
- **Estimated Context:** 70K
- **Concurrency Group:** C

## Goal

After this leaf:

1. Every storage-touching operation in both frontends is authorized: the
   authenticated `Identity` (from leaf 03's USER/PASS, leaf 04's pubkey or
   password) must hold `Grant.Read` for GET/LIST/RETR/reads and
   `Grant.Write` for PUT/STOR/DELE/RMD/MKD/writes — on the target bucket.
2. The FTP `PasswordVerifier` and SFTP `PasswordVerifier`/`PublicKeyChecker`
   implementations delegate to the landed auth model, including the
   SFTP public-key→identity association (if the auth tree delivered it
   already, leaf 04 may have wired it — this leaf makes it binding + tested).
3. Degradation mapping is complete and tested: `Capabilities()` says
   `ConditionalReads:false, Multipart:false, PresignedURLs:false,
   Versioning:false`; nothing emulates them. FTP naturally lacks the verbs;
   SFTP answers `SSH_FX_OP_UNSUPPORTED` for unrepresentable ops and
   `SSH_FX_PERMISSION_DENIED` for denials; FTP denials answer `550` (action
   not taken) with 530 for auth failures.

## Exact files

- New: `internal/frontend/authz.go` (shared, package frontend) — small helper:
  `RequireGrant(id Identity, bucket string, write bool) error` typed so both
  frontends map the error to their wire form. (Keep it in package frontend,
  not a new package, to avoid import churn.)
- Modify: `internal/frontend/ftp/driver.go` — grant check before every
  Backend call; map denial → 550 reply; USER/PASS failure → 530 (already
  there) now sourced from the real verifier.
- Modify: `internal/frontend/ftp/auth.go` — real verifier over the landed
  auth surface; delete `StaticVerifier` fallback.
- Modify: `internal/frontend/sftp/driver.go` — grant checks; denial →
  SSH_FX_PERMISSION_DENIED.
- Modify: `internal/frontend/sftp/auth.go` — real pubkey checker (auth
  tree's association) + real password verifier; drop fallbacks.
- Tests: `internal/frontend/ftp/authz_test.go`,
  `internal/frontend/sftp/authz_test.go`, updates to `auth_test.go` files.

## Test-first steps

1. Failing test (ftp): fake Backend + verifier returning an Identity with
   Read-only grant on bucket `b`: RETR OK (200-class/226); STOR → 550;
   DELE → 550; LIST OK. Write grant on a second bucket: STOR there OK.
2. Failing test (ftp): no grant on bucket `c`: every op → 550, and the fake
   Backend records ZERO calls for bucket `c` (authorization precedes I/O).
3. Failing test (sftp): same matrix via in-process ssh client: read-only
   identity can Get/ReadDir but Create → SSH_FX_PERMISSION_DENIED; Remove →
   same; write identity round-trips.
4. Failing test (sftp pubkey): pubkey checker backed by the landed auth
   association (or its landed equivalent) resolves the right Identity;
   unknown key → auth failure (SSH disconnect, no subsystem opened).
5. Failing test (degradation): SFTP extended/unsupported requests return
   SSH_FX_OP_UNSUPPORTED (assert via client error type); FTP answers to
   unsupported-site commands with 502 — add driver-level unit assertions so
   the mapping cannot silently regress to a success code.
6. Conformance rerun: both frontends still pass
   `frontend.RunConformanceSuite` — now with auth wired (suite's
   capability-gated checks prove no silent emulation).

## Done-when

- Authorization matrix tests green for both frontends; zero Backend calls on
  denied paths.
- No `StaticVerifier`/fake auth code remains in non-test files.
- `internal/auth` is the only source of identity/verdicts (grep: no
  credential comparison in frontend/ftp or frontend/sftp).
- `make test`, `make lint` green.
