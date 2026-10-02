# Region Tests + E2E + README - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** E2E case for a non-default region; README Known Limitations
  update; final gate sweep.
- **Dependencies:** 02-sigv4-wiring.md.
- **Estimated Context:** ~35K (explore 10K + generate 12K + iterate 8K + overhead 5K)
- **Concurrency Group:** C

## Goal

AGENTS.md e2e rule: a config-surface change (`region`) needs an e2e case.
Add `scripts/e2e/cases/` numbered next (check lexical order; expect 25+
- find the highest number and increment), which signs a request for
`eu-west-1` against a server configured with `region: eu-west-1` and
verifies both accept (matching region) and reject (mismatched region,
error names the expected region).

## Context

- The e2e harness (`scripts/e2e/run-e2e.sh`) starts the server from a
  temp config; cases are bash with lib.sh assert helpers; look at case
  17/18 for config-JSON writing patterns and the probe client used for
  SigV4 signing (there is a python probe in the zfs-validate harness;
  e2e cases may have their own - find how existing cases sign requests).
- README Known Limitations (~line 693): "Region pinned to us-east-1" -
  update to reflect configurability (keep a line for the default-mode
  permissive escape hatch).

## Tasks

### 1. E2E case

1. Find the highest-numbered case; create `<n+1>-region.sh`: write a
   temp config with `"region": "eu-west-1"`, start (or reuse) the
   server, sign a HEAD bucket request with the probe client using scope
   region eu-west-1 -> expect 200; sign with us-east-1 -> expect 403
   SignatureDoesNotMatch (assert the error code in the XML body).
2. Follow the BKT/create/cleanup conventions; graceful skip is NOT
   needed here (no ZFS dependency).
3. `make e2e` green (the full suite; expect the pre-existing webdav/
   owncloud failures documented in docs/validation-zmetad-2026-10-01.md
   to still fail - report them as pre-existing, do not fix here).

### 2. README Known Limitations

Replace the region line: configurable via `region` (default
us-east-1); when unset, the default mode accepts any client region
(permissive dev escape hatch) - set `region` for strict production
posture.

### 3. Final gate

`make test`, `make lint NEW_FROM_REV=HEAD`, `make build` green.
Report all results.

## Self-Verification Checklist

- [ ] New e2e case passes in the suite.
- [ ] README limitation updated (no stale "pinned" claim).
- [ ] make test/lint/build green.

## Review Checklist (for review agent)

- [ ] e2e case follows numbered-case conventions (BKT, cleanup pairing).
- [ ] Case asserts BOTH accept and reject paths.
- [ ] README has no contradictory region claims (grep "us-east-1" in
      README - all mentions consistent).

## Do NOT commit

The orchestrator stages and commits after review.
