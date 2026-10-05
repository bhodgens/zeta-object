# Admin Server: UI Screens - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Do NOT commit - the orchestrator handles all git operations after
> review. Explore with terminal cat; do not use read_file on existing
> source files. After completing, report what you built, what files you
> touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the four screens and the sign-in behaviour: dashboard,
  configuration, buckets, danger zone.
- **Dependencies:** 03 (the shell's DOM hooks), Contract 2 (the endpoint
  surface)
- **Estimated Context:** 60K
- **Concurrency Group:** B

## Goal

Each tab does real work against the console's proxied API and reports
honestly what the gateway said: no invented values, no swallowed errors,
and a confirmation step before the one destructive action.

## Context

- The shell (leaf 03) provides `window.Console` with `render`, `toast`,
  `modal`, `api`, `theme`, and `escapeHtml`, and the DOM hooks `#view`,
  `#rail`, `#toast`, `#modal`, `#theme-toggle`, `#signout`.
- The endpoint surface is Contract 2 in the master: `/api/status`,
  `/api/config`, `/api/config/save`, `/api/auth/reload`, `/api/buckets`,
  `/api/buckets/{name}`, `/api/buckets/{name}/settings`, `/api/purge`.
  Bodies and statuses pass through from the gateway unchanged.
- The gateway's shapes: `/status` carries version, uptime, listeners,
  frontends, backends, restart-required keys and metadata provider
  availability; `/config` carries the effective configuration with
  secrets masked plus the restart-required list; `/buckets` is a list of
  buckets with backend and tunables; a bucket entry carries `isDataset`;
  a dataset-backed delete answers 409 with code
  `DatasetBucketNotDeletable`.
- Read `internal/frontend/admin/routes.go` and `admin_wiring.go` to learn
  the exact JSON field names. Do NOT guess them.

## Interface Contracts (From Parent)

Each screen is a module attached to `window.Console.screens` with a
`mount(view)` function: `dashboard`, `config`, `buckets`, `danger` - the
same four values as the rail's `data-screen` attributes.

## Tasks

### Task 1: dashboard

**Files:** Create `internal/adminserver/web/screens/dashboard.js`

**Requirements:** on mount, call `/api/status` and render: version, uptime
(formatted), the listener list, the frontend list (marking the admin
frontend), the backend list with capabilities, the restart-required keys
as a banner when non-empty, and the metadata provider state as
available plus reason (never a fabricated percentage or count). A refresh
control re-fetches. A failed call renders the gateway's message verbatim
with a retry control.

### Task 2: configuration

**Files:** Create `internal/adminserver/web/screens/config.js`

**Requirements:** load `/api/config`; render the configuration as grouped
editable fields, with every secret field read-only and visibly masked;
seed a small set of hot-apply keys for editing (region, zfs_versioning,
zfs_versioning_reflink_retention) and make the restart-required keys
visibly read-only with an explanation; an `Apply` control sends a partial
patch to `PUT /api/config` and then shows the returned `applied` list as a
success toast and the `restartRequired` list as a persistent notice; a
`Save to file` control calls `POST /api/config/save`; a `Reload
identities` control calls `POST /api/auth/reload`. An invalid patch must
surface the gateway's validator message and must NOT appear to have
applied anything.

### Task 3: buckets

**Files:** Create `internal/adminserver/web/screens/buckets.js`

**Requirements:** list buckets from `/api/buckets` with backend, tunables,
and a dataset marker; a create control (name field) calling
`POST /api/buckets`; a row action opening a detail panel from
`GET /api/buckets/{name}`; the detail panel edits `auditReads` and
`reflinkRetention` through `PUT /api/buckets/{name}/settings`; a delete
action that first calls `GET /api/buckets/{name}`, and when the bucket is
a dataset, explains that the API does not destroy datasets and does NOT
offer the delete when the gateway would refuse it (show the gateway's 409
message if it still occurs). Deleting a plain bucket goes through the
confirmation modal.

### Task 4: danger zone and sign-in

**Files:** Create `internal/adminserver/web/screens/danger.js`,
Create `internal/adminserver/web/signin.js`

**Requirements:** danger zone explains, plainly, that purge destroys the
event history AND the permanent gap/loss record, and requires typing the
dataset name into the confirmation modal before the request is sent;
success and failure both render the gateway's own message. Sign-in posts
the operator token to `POST /login`, then loads the shell; a 401 from any
later call returns the user to sign-in with a toast.

## Self-Verification Checklist

- [ ] All four screens and the sign-in behaviour implemented, wired to the
      real endpoint names
- [ ] No invented data anywhere: fields the gateway does not send are
      absent, not faked
- [ ] Every failure path shows the gateway's message verbatim
- [ ] The destructive action requires the typed confirmation
- [ ] A dataset-backed bucket never offers a delete that would be refused
- [ ] No external requests; no framework; no stray `console.log`
- [ ] DO-NOT-TOUCH: every `.go` file, `app.css` and the shell's DOM hooks
      (leaf 03 owns them; read only), `Makefile`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Field names used by the JS match the gateway's JSON exactly
      (spot-check against routes.go / admin_wiring.go)
- [ ] Hot-apply versus restart-required is rendered as the gateway reports
      it, not as the UI assumes
- [ ] Loading, empty, and error states exist for every screen
- [ ] Keyboard reachable controls throughout

Output: APPROVED or specific gaps with file:line.

## Notes

- The screens are plain ES modules loaded by the shell; keep each file
  self-contained and small enough to read in one pass.
- If a gateway field is missing, render "unknown" rather than a guess;
  the honest-reporting rule from the gateway applies to the console too.
