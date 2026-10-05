# Admin Server: Documentation - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Do NOT commit - the orchestrator handles all git operations after
> review. Explore with terminal cat; do not use read_file on existing
> source files. After completing, report what you built, what files you
> touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** README section, a console config example, and the
  compatibility-matrix note.
- **Dependencies:** 04, 05 (behavior must exist before it is documented)
- **Estimated Context:** 40K
- **Concurrency Group:** C

## Goal

An operator can discover, configure, run, and safely use the console from
the documentation alone.

## Context

- `README.md` has a Management API section (added by the
  management-api-2026-10 tree) that documents the gateway's admin surface,
  its mTLS credential, the loopback default, and the runtime
  configuration model. The console sits in front of that and must be
  introduced after it.
- `config.json.example` documents the GATEWAY's configuration. The console
  has its OWN config file; do not mix them.
- `docs/protocol-compatibility.md` is the per-operation honest matrix;
  the console is not an S3 protocol surface, so it gets a short
  cross-reference rather than protocol rows.

## Tasks

### Task 1: console config example

**Files:** Create `config.admin-server.example.json` (repo root, beside
`config.json.example`)

**Requirements:** the seven keys from Contract 1 with comments explaining
each, the loopback default and `allowNonLoopback`, a note that
`operatorToken` must be a long random string and that an empty token
aborts startup, and a note that the certificate files are the gateway's
administrative client credentials.

### Task 2: README section

**Files:** Modify: `README.md`

Content requirements: what the console is (a separate process, a browser
UI, a JSON proxy of the Management API); why it is separate (the browser
never holds a client certificate); how to run it (`make build`, the binary
name, the config path and the `ZETAOBJECT_ADMIN_CONFIG` override); the
sign-in flow; the four tabs and what each does; the theme (derived from
the logo palette, dark default, light toggle); the honest statements: it
binds loopback by default, it stores no state on disk, sessions die on
restart, and the destructive purge requires typed confirmation and is the
only irreversible action in the UI. Add a feature-highlights bullet.
Sweep for statements made stale by this tree.

### Task 3: compatibility matrix cross-reference

**Files:** Modify: `docs/protocol-compatibility.md`

**Requirements:** in the Management API section, add a short note and one
row-like line that the web console proxies that surface 1:1 and adds no
operations of its own, with a proof link to e2e case 36
(`scripts/e2e/cases/36-admin-console.sh`).

## Self-Verification Checklist

- [ ] Every documented config key and route matches the implementation
- [ ] `config.admin-server.example.json` parses after comment stripping
- [ ] Hyphens only (grep the diff for em-dashes: zero)
- [ ] The two config files are not conflated anywhere
- [ ] DO-NOT-TOUCH: every `.go` file, `internal/adminserver/web/`,
      `scripts/`, `go.sum`, `config.json.example`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Documented key names, binary name, and env var match the code
- [ ] Claims about state, sessions, loopback, and the purge match reality
- [ ] The proof link points at the real case file

Output: APPROVED or specific gaps with file:line.

## Notes

- The console's own config file must be clearly distinguished from the
  gateway's; confusing the two would send an operator down a wrong path.
