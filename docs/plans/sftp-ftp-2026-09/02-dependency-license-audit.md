# 02 - Dependency License Audit - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> This leaf is a gate, not a feature: NO protocol code ships until it is done.
> Do NOT commit — the orchestrator handles all git operations after review.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Execute the license gate for every third-party dependency this
  tree introduces and record the audit reproducibly. Policy (user hard rule):
  GPL/AGPL acceptable ONLY as subprocess deps — never vendored, never linked
  into shipped binaries.
- **Dependencies:** none (auth-independent; may start before the auth tree lands).
- **Estimated Context:** 40K
- **Concurrency Group:** A (parallel with leaf 01)

## Goal

A committed audit document that (a) states each dependency's license from the
LICENSE file read out of the local module cache (authoritative), (b) records
the exact command to reproduce it, and (c) records the version pinned in
`go.mod`. Online reconnaissance from planning (2026-09): ftpserverlib = MIT,
pkg/sftp = BSD-2-Clause, golang.org/x/crypto = BSD-3-Clause,
jlaffaye/ftp = MIT. The module-cache read is the record; the online note is
context only.

## Dependencies to gate

| Module | Role | Shipped? |
|---|---|---|
| github.com/fclairamb/ftpserverlib | FTP/FTPS server protocol | linked into binary |
| github.com/pkg/sftp | SFTP subsystem | linked into binary |
| golang.org/x/crypto | SSH transport (pkg/sftp dep) | linked into binary (transitive) |
| github.com/jlaffaye/ftp | FTP CLIENT for tests only | test-only, not shipped — still gate it (cheap) |

## Exact files

- New: `docs/licenses/THIRD-PARTY-LICENSES.md` — the audit record (see template below).
- Modify: `go.mod`/`go.sum` — pin the four modules at current stable versions
  (`go get module@latest` then `go mod tidy`).
- Modify: `.gitignore` — if the jlaffaye test client lands under a vendored
  test-tools dir instead of module form, ensure it is NOT under `vendor/`
  (project `vendor/` holds runtime deps; keep the policy line simple: all
  four go through the module cache, none hand-vendored).

## Test-first steps

1. Run `go get` for the four modules; `go mod tidy`.
2. For each module, run:
   `cat "$(go env GOMODCACHE)/$(go list -m -f '{{.Path}}@{{.Version}}' <mod> | tr ':' '!')"/LICENSE`
   (Go module-cache paths escape colons on case-insensitive filesystems —
   use `go list -m -f` output verbatim; escape `:` → `!`). Record:
   license type, copyright line, SPDX identifier if stated.
3. Classification step (explicit in the doc): permissive (MIT/BSD/ISC/Apache-2.0)
   → approved for linkage; GPL/AGPL/LGPL → REJECTED as linked dep, escalate
   to the user. Any MISSING or non-standard LICENSE file → treat as rejected
   until clarified.
4. Write `docs/licenses/THIRD-PARTY-LICENSES.md` from the observed output —
   no values from memory or the planning notes alone.
5. Verify reproducibility: re-run the command in a clean shell, diff against
   the doc's recorded output.

## Audit record template

```markdown
# Third-Party License Audit — FTP/SFTP frontend tree (2026-09)

Policy: GPL/AGPL only as subprocess deps; never vendored or linked into
shipped binaries. Source of truth: LICENSE file in the module cache.

| Module | Version (go.mod) | License | SPDX | Copyright | Checked |
|---|---|---|---|---|---|
| github.com/fclairamb/ftpserverlib | vX.Y.Z | <from LICENSE> | ... | ... | <date> |
...

Reproduce:
    go list -m -f '{{.Path}}@{{.Version}}' github.com/pkg/sftp
    cat "$(go env GOMODCACHE)/<path-escaped>"'/LICENSE'

Reviewed-by: <PR link / commit>
```

## Done-when

- `docs/licenses/THIRD-PARTY-LICENSES.md` committed with all four modules,
  license strings read from module-cache LICENSE files (not from memory),
  versions matching `go.mod`, and a reproducible command block.
- All four classified permissive; if any is not, STOP and escalate — the
  dependent leaves (03/04) must not start that protocol.
- `go build ./...` still green after the `go.mod` change.
