# 07 - Config Example, README, License Record Placement - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Docs leaf: no behavior change. Do NOT commit — the orchestrator handles all
> git operations after review.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** Ship the user-visible documentation surface for the two new
  frontends: `config.json.example` keys, README sections, and the license
  audit record's final placement/linkage.
- **Dependencies:** 03, 04 landed (final key names known); 02's audit doc exists.
- **Estimated Context:** 30K
- **Concurrency Group:** D (parallel with 06)

## Goal

A user reading README + config.json.example can enable FTP/FTPS and SFTP
without reading source: every option key, defaults, TLS reuse story, host-key
behavior, and the license policy note.

## Exact files

- Modify: `config.json.example` — extend the commented `frontends` block:

  ```jsonc
  // "frontends": [
  //   { "type": "s3" },
  //   { "type": "ftp", "listenAddr": ":2121",
  //     "options": { "passivePortMin": "50000", "passivePortMax": "50100" } },
  //   { "type": "sftp", "listenAddr": ":2022",
  //     "options": { "hostKeyFile": "certs/host_ed25519" } }
  // ]
  ```

  Comments must state: FTP/FTPS reuses top-level `certFile`/`keyFile`
  (AUTH TLS explicit); SFTP host key auto-generates on first start at
  `hostKeyFile` if absent; non-HTTP frontends REQUIRE their own `listenAddr`
  (they cannot share the HTTPS mux); unknown option keys abort startup.

- Modify: `README.md` — new "Protocols" / "File transfer frontends" section:
  - what each frontend maps (STOR/RETR/LIST/DELE ↔ Put/Get/List/Delete),
  - FTPS: AUTH TLS, cert reuse, client example (`curl --ftp-ssl` / lftp),
  - SFTP: key + password auth, host-key pinning advice, client example
    (`sftp -P` / rclone `:sftp:`),
  - capability limits: no conditional reads, no multipart, no rename
    guarantee (per Contract C), errors you'll see (550 / SSH_FX_*),
  - auth: one credential pair today, multi-identity + bucket grants via the
    auth feature (link to its docs when landed),
  - License & dependencies note: links `docs/licenses/THIRD-PARTY-LICENSES.md`
    and states the policy sentence (GPL/AGPL never linked; deps MIT/BSD —
    verify wording against the audit doc, don't restate from memory).

- Verify only (no edit unless wrong): `docs/licenses/THIRD-PARTY-LICENSES.md`
  exists from leaf 02, versions match current `go.mod`.

## Test-first steps

Docs have no unit tests; the gate is consistency checks (run these):

1. Every `options` key in config.json.example/README exists in the factories'
   validation code (`grep` the key strings in `internal/frontend/ftp` and
   `internal/frontend/sftp`) — a documented-but-unimplemented key is a bug.
2. Every option key in code appears in the docs (reverse grep).
3. `python3 -c "import json,re..."`-style comment-strip sanity check that
   config.json.example's uncommented JSON still parses (the file uses
   JSONC-style comments; only the non-comment portion must stay valid —
   match whatever `loadConfig` tolerates today; if loadConfig supports
   comments, full-file parse is fine).
4. README links resolve (`docs/licenses/...` path exists).
5. `make check` green (no code touched; guards against accidental edits).

## Done-when

- config.json.example shows both frontends with all their option keys.
- README has the frontends section incl. client examples, capability limits,
  and the license-audit link.
- Consistency greps pass both directions; no code files touched.
