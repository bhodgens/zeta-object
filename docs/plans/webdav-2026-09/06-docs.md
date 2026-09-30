# WebDAV Docs: config.json.example + README Bucket-Mapping Section - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below. Do NOT commit — the orchestrator handles all
> git operations after review. Do NOT use read_file on existing source
> files — explore with search_files or terminal cat. After writing a file,
> do NOT read it back to verify — write once and stop. After completing,
> report what you built, what files you touched, and any deviations from
> the spec.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Update `config.json.example` with the webdav frontend entry and its keys, and add a README WebDAV section: mounting instructions, the bucket-mapping table, capability degradation list, auth requirements.
- **Dependencies:** 01 (config keys frozen); best after 05 (final wire behavior known, realm string confirmed). Docs-only: no Go code changes.
- **Estimated Context:** 30K
- **Concurrency Group:** F (last)

## Goal

A user can enable WebDAV and mount a drive without reading source:

1. `config.json.example` shows a commented webdav entry with every key and
   its meaning (`type`, `listenAddr`, `bucket`), matching the frozen
   Contract 2 — and the existing comment mentioning "a future WebDAV
   frontend" is updated to point at the real section.
2. README gains a "WebDAV frontend" section: what it is, how to enable,
   both modes, the bucket-mapping table (verbatim from master Contract 3),
   what is rejected and why (the never-silent-emulation list), auth setup,
   mounting instructions for macOS Finder / davfs2 / Windows, and known
   limitations.
3. `make check` / doc-affecting gates stay green; no code churn.

## Context

Read first (cat, not read_file):

- `config.json.example` — JSONC style: comments allowed, commented-out
  optional sections. The existing `frontends` comment already references a
  future webdav entry on `:8444` — replace that forward-reference with real
  documentation.
- `README.md` — structure: intro, architecture diagram mentioning WebDAV as
  tracked (line ~10 and ~232), roadmap (line ~289 lists WebDAV as issue #1).
  This leaf moves WebDAV from "tracked" to "shipped" in those spots.
- `docs/plans/webdav-2026-09/master.md` Contracts 2-5 — the content you
  are documenting. The mapping table is copied verbatim; if you find a
  discrepancy between docs and actual behavior while writing, STOP and
  report it (docs must match shipped behavior, not the plan's aspiration).
- `scripts/e2e/cases/18-webdav.sh` header — the manual mount procedure to
  reuse in the README (single source of truth: README is the detailed one,
  the case header references it).

Facts to document accurately (verify against code, do not guess):
- realm string (`zeta-object`) and that all requests require Basic auth
- davfs2 needs `use_locks 0` (no LOCK in v1)
- Mode A: top-level collections = buckets; MKCOL creates buckets only if
  the Backend exposes bucket creation (state the actual landed behavior
  from leaf 03)
- Mode B: `bucket` key fixes the root; `Capabilities().Buckets == false`
- What WebDAV cannot express here and what happens instead: versioning
  (not exposed), multipart (not exposed), LOCK/UNLOCK (405), Depth-infinity
  PROPFIND/COPY (403), dead properties (PROPFIND 404 per property),
  quotas (absent)
- Port layout: webdav typically on its own TLS port (`:8444` style) or
  sharing the default listener
- TLS is always on (the server is HTTPS-only) — clients must accept the
  cert (self-signed by default; `make certs`)

## Tasks

### Task 1: config.json.example

**Files:**
- Modify: `config.json.example`

Replace the forward-referencing comment in the `frontends` block with:

- The same block showing a real example:
  `"frontends": [ { "type": "s3" }, { "type": "webdav", "listenAddr": ":8444", "bucket": "photos" } ]`
- Per-key comments: `type` (required; `webdav`), `listenAddr` (optional,
  own TLS port vs shared default), `bucket` (optional; present ⇒
  single-bucket mode where `/` IS that bucket; absent ⇒ top-level
  collections are buckets; unknown keys in an entry abort startup)
- A one-line pointer to the README WebDAV section for the full mapping

Keep JSONC comment style and comment-wrapping consistent with the file.

### Task 2: README WebDAV section

**Files:**
- Modify: `README.md`

Add a `## WebDAV frontend` section (place near the S3/frontend content —
find the right anchor by reading the TOC/headers) containing, in order:

1. **What it is:** mount the server as a drive from macOS Finder, Linux
   (davfs2/gvfs), Windows; implemented per RFC 4918 (class 1 subset) over
   the neutral object model; methods list.
2. **Enabling:** config snippet (both modes), TLS + cert note, credential
   source (the auth config from the auth tree — cite the actual mechanism
   as landed, e.g. env/config users via the Basic authenticator).
3. **Bucket mapping table** — verbatim from master Contract 3 (both mode
   columns), plus the existence rule (collections are virtual prefixes;
   MKCOL writes no marker; empty MKCOL-only directories are invisible to
   S3 clients).
4. **Capability degradation table** — request ⇒ response (LOCK 405,
   Depth-infinity 403, unknown property 404-in-propstat, collection COPY
   403, MKCOL-with-body 415, bucket DELETE 403), with the one-line rule:
   never silent emulation.
5. **Auth:** all methods require valid Basic credentials (401 otherwise);
   grants map Read/Write to read/write verbs; read-only identities can
   mount and browse.
6. **Mounting:** Finder `Cmd+K` steps; `mount_webdav` command; davfs2
   mount + `use_locks 0` fstab option; Windows `net use` one-liner; note
   that self-signed certs require trusting the cert or client-side flag.
7. **Limitations:** no locking, no versioning, no quotas, no dead
   properties; pointer to issue for LOCK follow-up if the roadmap lists one.

### Task 3: Stale-reference sweep

**Files:**
- Modify: `README.md` (lines ~10, ~232, ~289 or wherever the sweep finds them)
- Grep-check only: `grep -rn "future WebDAV\|a future WebDAV\|WebDAV/FTP/ownCloud" README.md config.json.example docs/`

Update the roadmap/architecture mentions: WebDAV moves from "tracked" to
"shipped (issue #1)"; keep FTP/ownCloud as tracked. Do NOT touch other plan
trees' docs.

### Task 4: Gates

```
make build && make vet && make test     # untouched code still green
make check                               # whatever doc gates exist run here
grep -c "webdav" config.json.example README.md   # sanity: content present
```

Docs-only change: no Go diffs, no e2e changes.

## Self-Verification Checklist

- [ ] config.json.example: webdav entry documented with all three keys; forward-reference comment replaced; JSONC style consistent
- [ ] README: all seven subsections present, in order
- [ ] Mapping table matches master Contract 3 VERBATIM (both mode columns)
- [ ] Degradation table matches shipped behavior — each row verified against code/case 18, not the plan alone
- [ ] Realm string, davfs2 `use_locks 0`, TLS-only, credential mechanism all stated as LANDED (not as planned)
- [ ] Roadmap mentions updated (WebDAV shipped; FTP/ownCloud still tracked)
- [ ] No code changes; all gates green
- [ ] Files touched: exactly `config.json.example` and `README.md` (+ sweep hits in README only)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Task done; docs match shipped behavior (spot-check 3 degradation rows against code)
- [ ] Mapping table verbatim from Contract 3
- [ ] A user with only README + config.json.example can enable, mount, read, write, delete
- [ ] No promises about unshipped features (LOCK etc. listed as limitations)
- [ ] No code files touched

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- This leaf's quality bar is "the manual mount test in leaf 05 can be
  performed from the README alone" — if the README misses a step that
  procedure needs, the docs are incomplete.
- If you find the auth-2026-09 credential mechanism differs from what leaf
  04 actually wired, document LEAF 04's reality and flag the discrepancy in
  your report — docs follow code.
