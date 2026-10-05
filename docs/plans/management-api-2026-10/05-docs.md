# Management API: Documentation - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Do NOT commit - the orchestrator handles all git operations after
> review. Do NOT use read_file on existing source files - explore with
> search_files or terminal cat. After completing, report what you built,
> what files you touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** README section, the compatibility matrix rows, and the
  config example entry.
- **Dependencies:** 04 (behavior must exist before it is documented)
- **Estimated Context:** 40K
- **Concurrency Group:** D

## Goal

An operator can discover, configure, and safely use the management API
from the documentation alone, and the honest compatibility matrix covers
the new surface.

## Context

- `README.md` documents config keys in a table (around lines 198-212),
  features in a highlights list, and has an auth section around lines
  129-184 and a Purge section at 599-615.
- `docs/protocol-compatibility.md` is the per-operation matrix: a
  "Legend", then sections per protocol with tables of
  `| Operation | Status | Notes / proof |`. Add a new section for the
  management API. Rows cite a proof: an e2e case and/or a validation doc.
- `config.json.example` documents every key with a comment; the
  `frontends` array is documented there.
- Repo copy style: plain language, mechanism first, hyphens (never
  em-dashes), no marketing words.

## Interface Contracts (From Parent)

Nothing to build; the deliverable is text that must match Contract 5's
route table and the loopback and dataset-delete decisions exactly.

## Tasks

### Task 1: README section

**Objective:** the feature is discoverable and honestly described.

**Files:** Modify: `README.md`

Content requirements:
- New section "Management API" near the auth section. State plainly:
  it is a separate listener with a JSON shape (NOT the S3 API), it is
  authenticated by a TLS client certificate, and it binds to loopback
  unless explicitly told otherwise.
- A worked example: the `frontends` entry, a `clientCAFile`, the
  `adminPrincipals` allow-list, and a curl invocation using a client
  certificate.
- The route table from Contract 5, with the JSON error envelope.
- The configuration model, stated honestly: changes apply at runtime
  where a runtime path exists; everything else is reported as
  restart-required; `POST /config/save` persists the live configuration
  to the config file; the file is a snapshot, memory is authoritative
  while the process runs.
- The destructive-scope statement: the API cannot destroy ZFS datasets;
  dataset destruction stays a host-level operator action. Deleting a
  dataset-backed bucket through the API answers 409.
- The purge warning: purge destroys event history AND the permanent
  gap/loss record.
- Key rotation: replacing the CA file plus `POST /auth/reload` revokes
  old client certificates without a restart.
- A feature-highlights bullet.
- Sweep the README for now-stale claims (for example a statement that
  purge has no endpoint, or that identities are the only runtime change)
  and fix them in this commit.

### Task 2: compatibility matrix

**Objective:** the honest matrix covers the new surface.

**Files:** Modify: `docs/protocol-compatibility.md`

- Add a "Management API" section with one row per route (or one row per
  route group if the table style prefers it), each with Status
  "Implemented (opt-in)", the wire status, and a proof link to the e2e
  case 35 and the leaf 07 validation doc path.
- Note in the DeleteBucket row or the new section that the management
  path refuses dataset-backed buckets while the S3 path keeps its
  existing behavior - the two surfaces differ deliberately.

### Task 3: config example

**Objective:** the configuration shape is copy-pasteable.

**Files:** Modify: `config.json.example`

- Add a commented `frontends` entry for the admin type with the three
  option keys and a note that the listener is loopback-only by default.

## Self-Verification Checklist

- [ ] README claims match Contract 5 exactly (paths, statuses, codes)
- [ ] No secret, key path, or certificate material is published in the docs
- [ ] Hyphens only (grep the diff for em-dashes: zero)
- [ ] `config.json.example` still parses as JSON after comment stripping
- [ ] Stale-claim sweep done (purge, runtime-change statements)
- [ ] DO-NOT-TOUCH: all `.go` files, `scripts/`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Every documented route exists in the code with the documented status
- [ ] The configuration model paragraph matches leaf 02's actual behavior
      (hot-apply set, restart-required set)
- [ ] The dataset-delete statement matches leaf 03/04 behavior
- [ ] Matrix rows carry proof links

Output: APPROVED or specific gaps with file:line.

## Notes

- Do not describe the API as "secure by default" without the loopback
  caveat: a non-loopback bind requires an explicit option, and that is
  the operator's decision to make knowingly.
