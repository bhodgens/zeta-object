# Management API: Routes, Authorization, Audit - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the management route table wired to the config store and the
  bucket manager, the `admin` audit operation, and the purge and
  auth-reload actions.
- **Dependencies:** 01, 02, 03 COMMITTED (this leaf consumes all three)
- **Estimated Context:** 70K
- **Concurrency Group:** C

## Goal

The full management surface from Contract 5, authenticated by the client
certificate from leaf 01, backed by the store from leaf 02 and the bucket
manager from leaf 03, with every request attributed in the audit log.

## Context

- Leaf 01 produced `internal/frontend/admin` with the `Services` struct
  and the `GET /status` route; leaf 02 produced `ConfigStore` in package
  main; leaf 03 produced package-level `Create`/`Delete`/`Exists`/`List`,
  `DeleteOptions` and `ErrDatasetBucketNotDeletable`.
- The audit writer lives in package s3 (`audit_log.go:30-39` record,
  `:117` the append call site) and is WRITE-ONLY by charter. The admin
  package must not import package s3: export an append entry point from
  package s3 and inject it from main, like the other seams.
- `auth.Op` and `opVocabulary` are at `internal/auth/richgrants.go:30-48`.
- The purge capability is `MetadataProvider.Purge` with the config
  `zmetad_binary`; README:599-615 documents why it has no endpoint today.

## Interface Contracts (From Parent)

### Contract 5: the route table (binding)

| Method | Path | Behavior |
|---|---|---|
| GET | `/status` | version, uptime, listeners, frontends, backends, restart-required keys, metadata provider availability as `{available, reason}` |
| GET | `/config` | effective configuration, secrets masked, plus `restartRequired` |
| PUT | `/config` | partial update; 200 `{applied:[...], restartRequired:[...]}`; 400 on invalid input with the validator message; nothing changes on error |
| POST | `/config/save` | persist to the config file atomically |
| POST | `/auth/reload` | re-run the identity reload path; 200 with the identity NAMES; 500 with the validator error on failure |
| GET | `/buckets` | list with backend and per-bucket tunables |
| POST | `/buckets` | create; body `{"name":"..."}` |
| GET | `/buckets/{name}` | backend, tunables, `isDataset`; NO object count (no index exists) |
| DELETE | `/buckets/{name}` | plain-directory buckets only; dataset-backed -> 409 `DatasetBucketNotDeletable` |
| PUT | `/buckets/{name}/settings` | body `{"auditReads":bool,"reflinkRetention":int}` |
| POST | `/purge` | body `{"dataset":"..."}`; gated on the admin tier |

- Every response is `application/json`. Errors are
  `{"error":{"code":"...","message":"..."}}`.
- No route is reachable without a verified client certificate.
- Every request appends exactly one audit record.

### Contract 6: the audit operation (binding)

- Add `OpAdmin Op = "admin"` to the vocabulary in
  `internal/auth/richgrants.go`. The bucket-grant model is unchanged:
  `Admin` is never granted through `BucketGrants`, and the five existing
  ops keep byte-identical behavior (a bucket grant must never confer
  administrative authority).
- The audit record's EIGHT KEYS are unchanged. Management requests write
  `bucket` = the affected bucket (or empty), `key` = the dataset for
  purge (or empty), `op` = `admin`.

## Tasks

### Task 1: the audit seam and the admin op

**Objective:** management actions are attributable.

**Files:**
- Modify: `internal/auth/richgrants.go` (add `OpAdmin`, extend the
  vocabulary; do NOT change `AuthorizeOp` behavior)
- Modify: `internal/frontend/s3/audit_log.go` (accept the new op value;
  keep the eight keys)
- Create: `internal/frontend/s3/audit_seam.go` (an exported append entry
  point, e.g. `func AppendAudit(class, principal, method, bucket, key, op string, status int, denied bool)`;
  nil writer = no-op)
- Test: `internal/auth/richgrants_test.go`, `internal/frontend/s3/audit_log_test.go`

**Step 1: Write failing tests:**
- `OpAdmin` is in the vocabulary and `AuthorizeOp` returns FALSE for it
  even when the identity has a full wildcard bucket grant (a bucket
  grant is not administrative authority);
- the five existing ops behave exactly as before (existing tests
  unmodified);
- the audit record for a management action has exactly the eight keys.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 2: wire the services in main

**Objective:** the admin frontend gets the store and the manager.

**Files:**
- Create: `admin_wiring.go` (package main: build `admin.Services` from
  the `ConfigStore`, `bucketmanager`, the audit seam, the metadata
  provider and the reload path)
- Modify: `frontends.go` (the `admin` factory passes the services into
  `admin.New`)
- Test: `admin_wiring_test.go`

**Step 1: Write failing tests:** the wiring builds a complete `Services`
(no nil field that a route needs); `Purge` and `ReloadAuth` are wired to
the real functions; the audit function is called once per built service
call (assert with a recording stub).

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 3: the routes

**Objective:** Contract 5 is real.

**Files:**
- Modify: `internal/frontend/admin/routes.go` (the full table)
- Test: `internal/frontend/admin/routes_test.go` (httptest with a
  generated client certificate, or a stub verifier that marks the
  request as authenticated)

**Step 1: Write failing tests** (one per row, plus error paths):
- each route returns its documented status and JSON shape;
- `DELETE /buckets/{name}` on a dataset-backed bucket -> 409 with code
  `DatasetBucketNotDeletable` and a message naming the operator workflow;
- `PUT /config` with an invalid key -> 400, and the store is unchanged;
- `PUT /config` with a restart-required key -> 200 listing it under
  `restartRequired`, NOT under `applied`;
- `GET /config` contains no unmasked secret (assert the literal secret
  from the fixture is absent from the body);
- `POST /purge` calls the purge service for the named dataset and
  returns its result;
- an unknown path -> 404 JSON; a wrong method on a known path -> 405 JSON;
- every request appends exactly one audit record with `op = admin`.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS; the whole
admin package green.

### Task 4: reload re-reads the CA

**Objective:** certificate revocation works without a restart.

**Files:** modify `internal/frontend/admin` + its tests; modify the
`ReloadAuth` service in `admin_wiring.go` to also reload the CA file.

**Step 1: Write failing tests:** after the CA file is rewritten to a
different CA, a certificate from the OLD CA is rejected following a
reload; a certificate from the NEW CA is accepted.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test ./internal/frontend/admin/ ./internal/auth/ ./internal/frontend/s3/ . -count=1` green
- [ ] Every route requires a verified certificate (assert in a test that
      a request with no certificate gets 401 on EVERY route)
- [ ] No dataset destroy is reachable from any route (assert the manager
      is called with `AllowDatasetDestroy: false`)
- [ ] `GET /config` and every log line are secret-free
- [ ] Audit: exactly one record per request, eight keys, `op = admin`
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH: `internal/backend/backend.go`,
      `internal/metadata/metadata.go`, `internal/frontend/s3/zfsdatasets.go`,
      `scripts/zfs-validate/run-zfs-validation.sh`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 5 fully implemented: every row, every status, the JSON
      error envelope
- [ ] Contract 6 satisfied: `OpAdmin` exists, `AuthorizeOp` denies it,
      the eight audit keys are unchanged
- [ ] `admin` package does not import `internal/frontend/s3` (seams are
      injected from main)
- [ ] Purge is destructive and audited: the reviewer confirms it is
      reachable ONLY through the authenticated admin route
- [ ] Reload covers both identities and the client CA

Output: APPROVED or specific gaps with file:line.

## Notes

- Purge destroys event history and the permanent gap/loss record
  (README:609-615). It is exposed here because the operator asked for
  full capability management over an administrative credential, and
  README already names this as the right shape. Docs (leaf 05) must say
  so plainly.
- A 503 from a nil service is a construction bug, not a user-facing
  state: the wiring test in Task 2 exists to prevent it from shipping.
