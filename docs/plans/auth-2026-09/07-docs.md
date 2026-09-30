# Docs Update: config.json.example, README Auth Section, Issue Close-Out - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below. Do NOT commit — the orchestrator handles all git
> operations after review. After completing, report what you changed, what
> files you touched, and any deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** Documentation for the new auth surface and the issue's
  decision-record criterion: `config.json.example`, README auth section +
  env table + limitations/roadmap, cross-links from `internal/auth` doc
  comments if stale, and the decision summary posted to GitHub issue #4.
- **Dependencies:** leaves 01–06 merged (documenting real, verified behavior —
  not intentions). Dispatch last.
- **Estimated Context:** 30K
- **Concurrency Group:** D

## Goal

A user upgrading zeta-object must be able to: (1) learn from `config.json.example`
alone how to declare multiple identities with per-bucket grants; (2) read the
README auth section and understand the model (multi-identity, protocol-neutral,
Basic-for-HTTP/keys-for-SFTP shape, env-pair migration, dev mode); (3) find
issue #4 closed with the Option-1 decision and its rationale recorded.

Docs must match EXACTLY what landed: key names, grant vocabulary
(`readonly`/`readwrite`), identity name rules, error behavior. Verify each
claim against the code before writing it — docs that drift from behavior are
worse than no docs.

## Context

Key files:

- `config.json.example` — JSONC-style with `//` comments; documents every
  config key (see the `frontends`/`backends` blocks for the house style:
  what it does, defaults, backward-compat note). The credentials footer at
  the bottom currently says credentials are env-only — that section changes.
- `README.md` — auth today: line ~34 (features list: "Auth — AWS Signature
  Version 4 ..."), section at line ~70 ("single global credential pair"),
  env var table at ~125 (`ZETAOBJECT_ACCESS_KEY`/`ZETAOBJECT_SECRET_KEY`),
  limitations (~281: "Single credential pair; no per-user auth") and roadmap
  (~290: "Pluggable authentication ... (#4)"). All five spots get touched.
- `docs/plans/issue-auth-pluggable.md` — the issue text this tree implements.
- House doc style: plain, declarative, concrete examples, explicit
  backward-compat notes, no marketing.

## Interface Contracts (From Parent)

No Go surface. Deliverables are exact file edits + one `gh issue comment`.

## Tasks

### Task 1: `config.json.example`

**Files:**
- Modify: `config.json.example`

Add (matching house comment style):

1. An `"identities"` block with 2–3 commented examples: a readwrite wildcard
   identity, a readonly-on-one-bucket identity, one showing
   `sshPublicKeys` (noting it is consumed by the future SFTP frontend).
   Comments must state: identity `name` is for logs only; Basic-auth username
   == `accessKey`, password == `secretKey`; grant vocabulary
   `readonly`|`readwrite`; `"*"` wildcard; absent `grants` ⇒ full readwrite.
2. An `"auth"` block: `""` (default, required auth) vs `"none"` (dev only,
   loudly logged).
3. Update the trailing credentials footer: env pair still works and is ALWAYS
   merged as the wildcard `"env"` identity; duplicate access keys abort
   startup; `MINIS3_*` fallback unchanged.

### Task 2: README auth section

**Files:**
- Modify: `README.md`

1. Features bullet (~line 34): extend the Auth line — SigV4 + multi-identity
   + per-bucket grants; Basic auth adapter available for HTTP frontends.
2. Auth section (~line 70): replace "single global credential pair" prose
   with the model: identities from `config.json` `identities` + always-present
   env identity; grants (`readonly`/`readwrite`, per-bucket, `*` wildcard);
   enforcement (403 `AccessDenied`, ListBuckets filtering); migration note
   (existing env deployments unchanged); dev mode (`auth.mode: "none"`, loud,
   dev-only).
3. Env table (~line 125): keep both vars, note they now define the wildcard
   env identity rather than "the" credentials.
4. Limitations (~281): replace the single-pair limitation with what remains
   (e.g. no ACLs/policies beyond readwrite buckets, no key rotation/expiry, no
   OAuth — see roadmap).
5. Roadmap (~290): mark pluggable auth DONE (link issue #4 / this plan tree);
   leave the remaining frontend issues.
6. Repo-tree comment (`auth/` at ~252): update to describe the real package
   (registry, adapters, dev mode).

### Task 3: Decision recorded on issue #4

**Files:** none in repo (GitHub comment).

Draft the decision comment (Option 1, rationale per master.md Decision
section, what shipped: registry/adapters/grants/dev-mode/e2e, migration
guarantee, pointers to `docs/plans/auth-2026-09/` and README section).
Post it with:

```
gh issue comment 4 --repo bhodgens/zeta-object --body-file <draft>
```

If `gh` is unavailable or the comment fails, save the draft to
`docs/plans/auth-2026-09/issue-4-decision.md` and report the blocker —
do not fabricate a successful post; the orchestrator verifies the comment URL.

## Self-Verification Checklist

- [ ] Every documented key/value/error verified against the merged code
      (grep the implementation; don't document from the plan text)
- [ ] `config.json.example` stays valid JSONC (comment style matches
      existing blocks); a config built from the example parses (spot-check by
      hand against leaf-01 parsing tests)
- [ ] All five README touch-points updated consistently; no contradictions
      between section and env table
- [ ] Issue comment posted (URL in report) or draft saved + blocker reported
- [ ] No code changes (this leaf is docs-only; `git status` shows only
      config.json.example, README.md, optionally the draft file)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Example config + README match implemented behavior key-for-key
- [ ] Migration and dev-mode caveats stated plainly (env pair always works;
      dev mode loud + opt-in)
- [ ] Issue #4 decision criterion satisfied (comment or saved draft)
- [ ] House style: declarative, concrete, no marketing filler

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The plan tree (`docs/plans/auth-2026-09/`) is the durable record; the
  README/example are the user-facing record; the issue comment closes the
  decision criterion. Keep all three consistent with each other.
