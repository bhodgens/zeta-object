# Admin Server: E2E Case 36 - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Do NOT commit - the orchestrator handles all git operations after
> review. Explore existing cases with terminal cat; do not use read_file
> on existing source files. After completing, report what you built, what
> files you touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** a wire-level e2e case for the console, driving its real
  surface against a real gateway admin listener.
- **Dependencies:** 05 (the proxy must exist)
- **Estimated Context:** 50K
- **Concurrency Group:** C

## Goal

Prove, without a browser, that the console works end to end: it refuses
unauthenticated calls, signs an operator in, proxies each management route
to the gateway, passes the gateway's statuses and bodies through, and
serves the UI assets. Skips gracefully when its fixtures are unavailable.

## Context

- `scripts/e2e/run-e2e.sh` ALREADY generates a CA, a client certificate
  with a pinned common name, and an admin frontends entry on a loopback
  port (added by the management-api tree). Reuse those fixtures; do not
  invent new ones.
- Case numbering is at 35, so this is
  `scripts/e2e/cases/36-admin-console.sh`.
- `scripts/e2e/lib.sh` provides the assert helpers and the skip shape.
- The console needs its own config file at run time; write it into the
  case's work directory and start the console binary with
  `ZETAOBJECT_ADMIN_CONFIG` pointing at it.

## Interface Contracts (From Parent)

Contract 2 is the wire truth. The case must assert, with body checks not
status-only:

1. `GET /` without a session returns the shell and contains the rail tabs
   (`data-screen="dashboard"`, `config`, `buckets`, `danger`).
2. `GET /assets/app.css` returns the theme tokens (assert the accent
   colour literal is present).
3. Every `/api/...` route without a session returns 401 with the JSON
   error envelope.
4. `POST /login` with a wrong token is 401; with the right token it sets
   the session cookie.
5. With the session: `GET /api/status` returns the gateway's status JSON
   (assert a field the gateway really sends); `GET /api/config` returns no
   unmasked secret; `POST /api/buckets` creates a plain bucket, `GET
   /api/buckets` lists it, and `DELETE /api/buckets/{name}` removes it
   (the case asserts the directory exists and then is gone, as case 35
   does).
6. A mutating call without the CSRF header is 403.
7. The dataset-delete refusal passes through when the environment has ZFS
   (skip that group otherwise): 409 with `DatasetBucketNotDeletable` in
   the body.
8. `POST /logout` clears the session: a following `/api/status` is 401.

## Tasks

### Task 1: the case

**Files:** Create `scripts/e2e/cases/36-admin-console.sh`
Modify: `scripts/e2e/run-e2e.sh` only if the console binary or its config
must be staged (keep the change minimal and guarded).

**Steps:** source `lib.sh`; skip the whole case gracefully when the admin
fixtures or the console binary are unavailable (print the standard SKIP
line and exit 0); start the console on a free loopback port with a
generated config; run the eight assert groups; use a cookie jar file for
the session; clean up the console process and the work directory in an
EXIT trap.

### Task 2: run it

**Steps:** `bash -n` clean; then `make e2e` fully green with case 36
executing its asserts (and its ZFS group skipping where there is no ZFS);
report the case verdict line and the suite tally.

## Self-Verification Checklist

- [ ] `bash -n` clean; `make e2e` green with case 36 running
- [ ] Body asserts for the 401 envelope, the theme token, and the
      `DatasetBucketNotDeletable` pass-through
- [ ] The CSRF 403 is asserted, not assumed
- [ ] Logout really invalidates the session (asserted)
- [ ] No leftovers: no console process, no cookies, no temp configs, no
      buckets
- [ ] DO-NOT-TOUCH: every `.go` file, `scripts/zfs-validate/`, `README.md`,
      `docs/`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] All eight groups present; the 401 and 403 asserts prove the gate
- [ ] The case reuses the existing certificate fixtures rather than
      generating competing ones
- [ ] Cleanup cannot leave a listener or a bucket behind

Output: APPROVED or specific gaps with file:line.

## Notes

- No browser is used: the case drives the console's own HTTP surface.
  Browser rendering is a manual check for the operator, and the UI's
  correctness is the assets plus these routes.
- If the console binary needs to be built by the harness, build it the way
  the harness already builds the gateway (same GOOS/flags), and skip if
  the build is unavailable rather than failing the whole suite.
