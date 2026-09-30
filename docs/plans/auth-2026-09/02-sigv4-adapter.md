# SigV4 Adapter on the Identity Registry (S3 Frontend) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Rewire the S3 frontend's SigV4 authentication
  (`internal/frontend/s3/auth_adapter.go`) from the single-pair
  `CredentialSource` to the multi-identity `auth.IdentityRegistry` (leaf 01),
  return full Identities (grants populated), and enforce bucket grants in
  `dispatch.go` before handlers run.
- **Dependencies:** leaf 01 (`internal/auth` registry + `CanRead`/`CanWrite`).
- **Estimated Context:** 80K
- **Concurrency Group:** B (after 01; parallel with 03/04/05)

## Goal

Today `sigv4Authenticator` resolves the signing secret via
`auth.CredentialSource` (single pair, installed by package main's
`mainCredentialSource` in s3_wiring.go) and returns
`Identity{AccessKeyID: accessKeyID}` with **nil** `BucketGrants` — every
authenticated client is the same omnipotent principal. After this leaf:

1. `s3.InstallIdentityRegistry(reg)` becomes the credential source of record;
   SigV4 access key IDs resolve through `LookupByAccessKey`, so different keys
   are different identities.
2. Successful auth returns the FULL identity (grants populated) — the adapter
   stops throwing grants away.
3. `dispatch.go serveHTTP` enforces grants: a readonly identity gets `AccessDenied`
   (403, S3 error bytes) on any write op; bucket-scoped identities get 403 on
   other buckets; `ListBuckets` filters to granted buckets for non-wildcard
   identities. Wildcard (env pair / `*` grants) sees exactly today's behavior.

The SigV4 verification logic itself (canonical requests, chunked streaming,
presigned expiry, the 9 hardening fixes) is **not touched** — only where the
secret comes from and what the success path returns.

## Context

Key files to understand before implementing:

- [master.md](../master.md) — Contract 3 is this leaf's spec
- `internal/frontend/s3/auth_adapter.go` — `sigv4Authenticator`,
  `credentialSecret`, `installDefaultCredentialSource`, the
  `authFailureError` triple (code/message/status) rendered by `writeAuthFailure`.
  `authenticateRequest`/`authenticatePresignedRequest` return
  `(Identity, *authFailureError, bool)`.
- `internal/frontend/s3/seam.go` — hook plumbing (`InstallDefaultCredentialSource`,
  `credentialSourceFor`, `hookMu`); add `InstallIdentityRegistry` next to them,
  same mutex pattern.
- `internal/frontend/s3/dispatch.go` — `serveHTTP`: parse path → authenticate →
  ACL stub → route by service/bucket/object level. Grant checks slot in right
  after authentication, before the ACL stub.
- `internal/frontend/s3/frontend.go` — `Frontend{creds, authz}`;
  `Authenticator()` returns the adapter.
- `s3_wiring.go` (package main) — `mainCredentialSource{}` and
  `installS3Seams()`; this is where the leaf-01 registry gets installed.
- Existing regression gate: `internal/frontend/s3/sigv4_test.go`,
  `sigv4_presigned_test.go`, `sigv4_chunked_test.go`,
  `auth_adapter_test.go` + e2e cases 01–13. These MUST stay green unchanged —
  that is the behavior-preservation proof for the wildcard/env path.

Error semantics (S3, AWS-compatible):
- Write op, read-only grant → `AccessDenied`, 403, message
  "Access Denied".
- Bucket outside grants → `AccessDenied`, 403 (do NOT reveal existence with
  `NoSuchBucket`).
- Service-level `ListBuckets` with scoped identity → filtered list (not 403).
- Unknown access key → unchanged `InvalidAccessKeyId` 403.

Write-op classification (minimum set, in dispatch.go): PUT/POST/DELETE at
bucket level (create/delete bucket, versioning, actions) and object level
(put, copy, multipart initiate/upload/complete, delete batch). GET/HEAD are
reads. `?acl` remains the existing stub.

## Interface Contracts (From Parent)

```go
// internal/frontend/s3/seam.go — new hook (alongside InstallDefaultCredentialSource):
func InstallIdentityRegistry(reg auth.IdentityRegistry)

// internal/frontend/s3/auth_adapter.go — changed resolution:
// credentialSecret prefers the installed registry's LookupByAccessKey
// (returning secret + full Identity), falls back to the legacy
// CredentialSource when no registry is installed (tests, transitional).
// Success returns Identity with BucketGrants populated from the registry.

// dispatch.go — after authentication:
// id, ok := authenticated Identity from the adapter
// write-wanting request + !id.CanWrite(bucket) → writeAuthFailure AccessDenied
// read request + !id.CanRead(bucket) → writeAuthFailure AccessDenied
// bucket == "" (service level): ListBuckets filters by grants; other service
// ops require wildcard or any grant? No — ListBuckets filters; ListBuckets is
// the only service-level op today.
```

`s3_wiring.go`: `installS3Seams()` builds the leaf-01 registry
(`buildIdentityRegistry()`) and calls `s3.InstallIdentityRegistry(reg)` IN
ADDITION to the existing `InstallDefaultCredentialSource` (kept as fallback).

## Tasks

### Task 1: Registry-backed credential resolution

**Objective:** `credentialSecret` (and the presigned twin) resolve through the
registry; success carries grants.

**Files:**
- Modify: `internal/frontend/s3/seam.go` (hook + accessor)
- Modify: `internal/frontend/s3/auth_adapter.go` (resolution + Identity plumbing)
- Test: `internal/frontend/s3/auth_adapter_test.go` (extend)

**Step 1: Write failing tests.** Table-driven:

- Registry with identity `AKBOT` (readonly on bucket `b1`, via leaf-01
  constructor): signed request with `AKBOT` authenticates and the returned
  Identity has `CanRead("b1")==true`, `CanWrite("b1")==false`.
- Same registry: unknown key → `InvalidAccessKeyId` (unchanged bytes).
- No registry installed → legacy CredentialSource path still works
  (existing tests already prove; add one explicit fallback test).
- Presigned request with a registry identity → grants populated.
- Use the existing sigv4 test helpers (`sigv4_test_helpers_test.go`) for
  signing; do NOT write a new signer.

**Step 2:** `go test ./internal/frontend/s3/ -run TestSigV4Registry -v` → FAIL.

**Step 3: Implement.** Add the hook + mutex-guarded accessor; change
`credentialSecret` to prefer the registry; thread the resolved `Identity`
into the success returns of `authenticateRequest` /
`authenticatePresignedRequest` (signature already returns Identity — populate
it).

**Step 4:** same command → PASS, then the FULL existing S3 suite
(`go test ./internal/frontend/s3/ -v`) → PASS unchanged.

### Task 2: Grant enforcement in dispatch

**Objective:** Write/read checks after auth, before handlers; service-level
filtering for ListBuckets.

**Files:**
- Modify: `internal/frontend/s3/dispatch.go`
- Modify: `internal/frontend/s3/object_handlers.go`,
  `bucket_handlers.go` ONLY if the ListBuckets filtering needs a hook
  (prefer computing the filter in dispatch and passing it down / via context)
- Test: `internal/frontend/s3/dispatch_grants_test.go` (new)

**Step 1: Write failing tests** (httptest against the real Frontend over the
seam, stub/fake backend):

- Readonly identity: PUT object → 403 `AccessDenied`; GET object → 200.
- Scoped identity (only `b1`): GET on `b2` → 403 `AccessDenied` (not NoSuchBucket).
- Scoped identity: ListBuckets → only `b1` in the XML.
- Wildcard identity (env-pair shape): everything allowed — byte-identical to
  current behavior (this is the back-compat guard).
- Multipart initiate (POST ?uploads) counts as write → 403 for readonly.
- Delete bucket / delete object → write path.

**Step 2:** run → FAIL.

**Step 3: Implement.** Add an `authorize(f *Frontend, id auth.Identity,
bucket string, r *http.Request)` step in `serveHTTP` after auth; classify
write ops per the Context table (switch on method+query, one function, unit-
testable on its own). ListBuckets filtering: dispatch computes allowed
buckets and passes them to the service handler.

**Step 4:** run → PASS; full `go test ./internal/frontend/s3/ ./... -count=1`
green.

### Task 3: Wire the registry in package main

**Files:**
- Modify: `s3_wiring.go` (install leaf-01 registry; keep legacy source fallback)

**Step 1:** extend `main_frontends_test.go` (or a new `s3_wiring_auth_test.go`)
first: with `identities` in config, the s3 frontend authenticates a
non-env access key end-to-end; without, behavior unchanged.

**Step 2:** FAIL → implement (call `buildIdentityRegistry()` from leaf 01,
install, run) → PASS.

### Task 4: Coverage floors + gates

Run `make test-cover-enforce`. If floors moved (code added to
`internal/frontend/s3` and `internal/auth`), re-measure and update per
AGENTS.md (measured value rounded DOWN, ratchet comment). Then run
`make vet`, `make lint NEW_FROM_REV=<rev>`, `make fmt`.

## Self-Verification Checklist

- [ ] Existing SigV4 suites (`sigv4_test.go`, `sigv4_presigned_test.go`,
      `sigv4_chunked_test.go`, `auth_adapter_test.go`) pass UNMODIFIED — the
      wildcard path is byte-identical (if you edited those tests to make them
      pass, STOP: the extraction is wrong)
- [ ] E2E cases 01–13 still pass (`make e2e`) — the env/default path is unchanged
- [ ] New tests prove: multi-identity auth, readonly 403, scoped-bucket 403,
      ListBuckets filtering, fallback without registry
- [ ] SigV4 crypto/canonical logic untouched (diff review: only resolution +
      success-Identity + dispatch authorization changed)
- [ ] No secrets logged; unknown-key logs unchanged
- [ ] Coverage floors re-measured if moved
- [ ] `make fmt`, `make vet`, `make lint` clean

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Contracts from master Contract 3 satisfied exactly
- [ ] Frozen shapes untouched (`Authenticator` interface, `Identity` fields)
- [ ] No credential table or grant logic outside `internal/auth` — the S3 side
      only translates registry results into S3 errors
- [ ] All Task 1–3 tests present and green; legacy suites untouched
- [ ] Write-op classification complete for today's surface (incl. multipart,
      actions, delete batch)

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- This is the riskiest leaf in the tree (hot, security-sensitive code).
  Behavior preservation is proven by the untouched legacy suites + e2e, not by
  inspection.
- The legacy `CredentialSource` fallback stays until the SFTP/WebDAV frontends
  land and wiring is proven — removal is a later cleanup, not this leaf.
- Leaf 03 depends on the Identity-with-grants success path but not on S3
  internals; land this before 06's e2e case.
