# Configurable SigV4 Region - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Your job: (1) dispatch implementation agents, (2) review their work,
> (3) re-dispatch if incomplete, (4) track completion.
> Do NOT implement code yourself. All implementation happens in leaf agents.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 3 leaf documents under this node
- **Scope:** Make the SigV4 scope region configurable (`region` config key,
  default `us-east-1`) in the header and presigned verification paths,
  closing zeta-object issue #10.

## Goal

SigV4 credential scopes are hardcoded `us-east-1`; clients signing for
another region fail verification with a misleading SignatureDoesNotMatch.
This tree adds a server-level `region` config key (default `us-east-1`,
set at config load like every other default), threads it through the
header-form and presigned-form verification, and reports it from
GetBucketLocation.

## Architecture

One constant dies, one config field lives. Locate every `"us-east-1"` and
every `REGION` in the SigV4 path (`internal/frontend/s3/sigv4.go`,
`auth_adapter.go`, `bucket_handlers.go` GetBucketLocation). The identity
registry is shared across frontends; the region change lands in the SigV4
core only. Config key: `region` (string, default `us-east-1`), validated
lowercase at load.

## Interface Contracts

### Contract 1: Region accessor (FROZEN for this tree)

```go
// File: internal/frontend/s3/region.go
package s3

// regionOf returns the SigV4 region this server verifies against.
// Wires from the server config at startup; tests set it via
// SetRegion. Default "us-east-1".
func regionOf() string

// SetRegion sets the verification region (config wiring entry;
// empty restores the default). Not safe for concurrent use with
// in-flight requests: call at startup only.
func SetRegion(r string)
```

Owner: `01-region-config.md`. Consumers: `02-sigv4-wiring.md`.

### Contract 2: Scope derivation

SigV4 scope = `<datestamp>/<region>/s3/aws4_request` - the region token
comes from regionOf() in BOTH the header path and the presigned path.
The permissive unset-mode behavior: when the configured region is the
default `us-east-1` AND the client's scope region parses as a valid
region token (matches `[a-z0-9-]+`), accept the request and log a
one-line notice with the client's region (backward-compat escape hatch);
otherwise strict compare. When the config sets an explicit region,
strict compare always.

Owner: `02-sigv4-wiring.md`. Consumers: `03-tests-e2e.md`.

## Child Document Index

| # | Document | Type | Dependencies | Est. Context | Concurrency |
|---|----------|------|-------------|-------------|-------------|
| 01 | 01-region-config.md | leaf | none | ~25K | A |
| 02 | 02-sigv4-wiring.md | leaf | 01 | ~35K | B (after 01) |
| 03 | 03-tests-e2e.md | leaf | 02 | ~35K | C (after 02) |

Sequential. This is the smallest tree; leaves are small by design.

## Dispatch Protocol

For each child document, in dependency order:

1. Read the leaf document fully.
2. Dispatch one implementation agent with the leaf's full text plus:
   repo root `/Users/caimlas/git/mini-s3`, module
   `github.com/bhodgens/zeta-object`, run `make test` and
   `make lint NEW_FROM_REV=HEAD` before reporting. Include: "Do NOT run
   git commit. Do NOT run git add. Write code, run tests, report
   results. The orchestrator handles all git operations."
3. Review the report in-session against the leaf's Review Checklist.
4. Re-dispatch only failing parts (audit `git status` first).
5. Commit after review passes, message referencing the leaf.
6. Update the Completion Tracking Table.

## Coding Conventions

- Go, stdlib-first; no new dependencies.
- User-facing prose: hyphens not em-dashes; plain language.
- Error messages: prefix `s3: ` where the package convention is.
- Config defaults at config load, one place owns them.
- Do not touch the frozen MetadataProvider interface or the .meta
  sidecar shape.

## Completion Tracking Table

| Leaf | State | Review | Commit | Notes |
|------|-------|--------|--------|-------|
| 01-region-config.md | pending | - | - | |
| 02-sigv4-wiring.md | pending | - | - | |
| 03-tests-e2e.md | pending | - | - | |

## Integration Test Plan

1. `make test` green; `make lint NEW_FROM_REV=HEAD` 0 issues.
2. `make e2e` green including the new region case (skips if the harness
   cannot run).
3. Manual: boto3 with region eu-west-1 against a config with
   `region: eu-west-1` verifies (deferred, needs client host).

## Review Checklist

- [ ] Contracts 1-2 verbatim across leaves.
- [ ] Default behavior byte-identical to today for us-east-1 clients.
- [ ] README: `region` key documented; Known Limitations entry updated.
- [ ] No new dependencies; frozen interfaces untouched.

## Open Questions

None - design was fixed in zeta-object#10.

## Test Gate

<!-- Tests-first gate (hierarchical-planning skill v1.3.0, patched 2026-10-24).
     Coverage matrix derived mechanically from leaf task headers; the control
     agent (orchestrator, in-session) refines rows and fills test names at
     Phase 0 dispatch. No implementation leaf dispatches until every row is
     VALIDATED. Delta protocol: lesser of plan/tests updates to the greater.
     Recipe: skill hierarchical-planning -> references/tests-first-gate.md -->

Branch scope: Configurable SigV4 region: 'region' config key, credential-scope verification wiring, tests + e2e case 30.

| # | Feature (from plan) | Unit test evidence | E2E evidence | Status |
|---|---------------------|--------------------|--------------|--------|
| F1 | Region Config Key | config_frontend_test.go:TestLoadConfig_RegionKey | scripts/e2e/cases/30-region.sh | VALIDATED |
| F2 | SigV4 Wiring | internal/frontend/s3/region_sigv4_test.go:TestRegionMatrix_Header; TestRegionMatrix_StrictMismatchCode | scripts/e2e/cases/30-region.sh | VALIDATED |
| F3 | Region Tests + E2E + README | internal/frontend/s3/region_test.go:TestRegionOf; region_race_test.go:TestRegionConcurrentSetAndRead | scripts/e2e/cases/30-region.sh | VALIDATED |

All 3 rows verified on disk by the survey subagent (grep-verified test
functions + e2e case existence) and spot-checked by the control agent
(8/8 random test funcs FOUND, 5/5 e2e cases exist, 2026-10-25). Baseline:
`make test` exit 0, aggregate coverage 80.5% (floor 79.8%).

Statuses: PENDING | DELTA | VALIDATED

Delta log: 2026-10-25, all rows, control-agent survey — mechanically seeded task-header rows replaced with per-row verified test evidence (3 rows). Tree conforms to its plan per this matrix.
