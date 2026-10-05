# Management API: E2E Case 35 - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Do NOT commit - the orchestrator handles all git operations after
> review. Explore existing cases with terminal cat; do not use read_file
> on existing source files. After completing, report what you built, what
> files you touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** a wire-level e2e case for the management API, including
  certificate generation in the harness.
- **Dependencies:** 04 (the routes must exist)
- **Estimated Context:** 50K
- **Concurrency Group:** D

## Goal

The AGENTS.md e2e rule is satisfied: the management API ships with a case
that exercises authentication, configuration changes, bucket operations,
the dataset-delete refusal, and the audit record - and skips gracefully
where its prerequisites are missing.

## Context

- `scripts/e2e/run-e2e.sh` builds the server, generates a self-signed
  server certificate with openssl (`run-e2e.sh:36`), writes a temp
  config (around lines 62 and 131), starts the server on a free port, and
  runs every `scripts/e2e/cases/*.sh` in lexical order. Numbering is
  currently at 34, so this case is `35-management-api.sh`.
- `scripts/e2e/lib.sh` provides the assert helpers, the `BKT=` bucket
  convention, and the graceful-skip shape (see case 28 for the
  conditional-skip pattern, case 34 for a skip tied to a server feature).
- Cases must clean up after themselves and must not leak datasets or
  directories into shared locations.
- The management listener is loopback-only by default, which is exactly
  what a local test harness needs.

## Interface Contracts (From Parent)

Contract 5 is the wire truth: route paths, statuses, the JSON error
envelope, the `DatasetBucketNotDeletable` code, and the audit record.

## Tasks

### Task 1: certificate fixtures in the harness

**Objective:** the harness can produce a CA, a client certificate, and a
client certificate from a DIFFERENT CA (to test rejection).

**Files:** Modify: `scripts/e2e/run-e2e.sh`

**Steps:**
1. Generate in the work directory: a CA key and certificate, a client
   key and certificate signed by that CA (Common Name pinned, for example
   `e2e-admin`), and a second CA with its own client certificate.
2. Add the admin `frontends` entry to the generated config: type
   `admin`, its own `listenAddr` on a free port, `clientCAFile` pointing
   at the first CA.
3. Skip the whole case gracefully when `openssl` is unavailable (print
   the standard SKIP line and exit 0).

### Task 2: the case

**Objective:** the six assert groups below.

**Files:** Create: `scripts/e2e/cases/35-management-api.sh`

Assert groups (use body asserts, not status-only):
1. **Auth:** no client certificate -> 401; certificate from the wrong CA
   -> 401 (the TLS handshake may fail outright, which is also acceptable:
   accept either a handshake failure or a 401, and assert that the body
   carries no certificate detail); valid certificate -> 200 on `/status`.
2. **Status and config read:** `/status` reports the frontends list
   including `admin`; `/config` returns JSON in which the literal
   `secretKey` value from the config NEVER appears.
3. **Config write:** `PUT /config` changing a hot key (for example
   `region`) reports it under `applied`; `PUT /config` changing a
   restart-required key (for example `dataDir`) reports it under
   `restartRequired` and NOT under `applied`; `PUT /config` with an
   invalid key -> 400 with the validator message and no change.
4. **Buckets:** create a plain bucket via the API -> success and the
   directory exists; it appears in `GET /buckets`; delete it -> success
   and the directory is gone.
5. **Snapshot/dataset refusal:** with a dataset-backed bucket (requires
   ZFS; skip this group gracefully on a non-ZFS host) `DELETE
   /buckets/{name}` -> 409 with `DatasetBucketNotDeletable` in the body,
   and the dataset still exists afterwards.
6. **Audit:** the audit file contains a record for a management request
   with the eight contract keys, `op` equal to `admin`, and the principal
   equal to the client certificate Common Name.

**Files:** Create the case; source `lib.sh` exactly as the neighbouring
cases do.

**Steps:** `bash -n` clean; then `make e2e` with case 35 running (or
skipping per its guards) and 0 failures in the rest of the suite.

## Self-Verification Checklist

- [ ] `bash -n scripts/e2e/cases/35-management-api.sh` clean
- [ ] `make e2e` fully green; case 35's verdict line shows its asserts
- [ ] Body asserts present for the 409 code, the audit keys, and the
      masked configuration
- [ ] Graceful skips: openssl missing, and the ZFS-only group
      (verify the SKIP lines appear when run without those prerequisites)
- [ ] No leftovers: no temp certificates, buckets, or datasets left behind
- [ ] DO-NOT-TOUCH: all `.go` files, `scripts/zfs-validate/run-zfs-validation.sh`,
      `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Case covers all six groups, including the dataset-delete refusal
- [ ] Cleanup pairing: create and delete are paired; the EXIT trap cleans up
- [ ] The 401 asserts prove the credential is required (not merely that a
      path 404s)
- [ ] The audit assert pins the eight keys AND the principal

Output: APPROVED or specific gaps with file:line.

## Notes

- A wrong-CA client certificate usually fails the TLS handshake instead
  of reaching the handler; the case must accept that outcome explicitly
  rather than treating it as an error.
- The ZFS group is the one that matters most (it is the destructive-scope
  decision); it must be skipped only for a missing ZFS dataDir, never
  silently passed.
