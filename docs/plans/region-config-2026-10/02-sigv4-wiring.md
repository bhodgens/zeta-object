# SigV4 Wiring - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** Thread regionOf() through the SigV4 header and presigned
  verification paths; GetBucketLocation reports the configured region;
  s3_wiring.go calls SetRegion.
- **Dependencies:** 01-region-config.md.
- **Estimated Context:** ~35K (explore 12K + generate 10K + iterate 8K + overhead 5K)
- **Concurrency Group:** B

## Goal

Every place that hardcodes `us-east-1` in signature verification or scope
parsing now uses `regionOf()` (leaf 01). Behavior:

- Config sets an explicit region (anything, including the default, via
  SetRegion with non-"" after config load): STRICT compare - a client
  scope from a different region fails with SignatureDoesNotMatch whose
  message names the expected region.
- Default mode (SetRegion never called OR called with ""): permissive -
  accept any well-formed client region token `[a-z0-9-]+`, log a one-line
  notice naming the client's region. This preserves today's behavior for
  existing clients while defaulting to us-east-1.

The distinction: `SetRegion("")` = default/permissive;
`SetRegion("us-east-1")` (explicit config value) = strict. Region.go's
regionOf() returns the string either way; add an unexported
`regionExplicit() bool` accessor in the same file (leaf 01's file, small
additive edit allowed here - it is this leaf's dependency seam).

## Context

- internal/frontend/s3/sigv4.go - SigV4 core: string-to-sign, scope
  parsing, chunked upload signature verification. Find the us-east-1
  constant(s) and the scope-parse site.
- internal/frontend/s3/auth_adapter.go - authenticatePresignedRequest
  (~line 304): presigned verification, same scope handling.
- bucket_handlers.go:296-339 - GetBucketLocation: reports a location
  constraint; make it report the configured region.
- s3_wiring.go (repo root) - where serverConfig flows into s3.*
  Install* calls; add the SetRegion call next to them.
- main.go / wherever config lands at startup - trace the dataDir flow.

## Interface Contracts (From Parent)

Contract 2 (scope derivation) + the additive
`regionExplicit() bool` in region.go. All signature-relevant sites use
regionOf().

## Tasks

### 1. TDD: regionExplicit + strict/permissive semantics

1. Extend region_test.go: regionExplicit() false by default and after
   SetRegion(""); true after SetRegion("anything").
2. Run (fail), implement (same file), run (pass).

### 2. TDD: header-form verification

1. Find the existing SigV4 test file (sigv4_test.go or similar); add
   cases: explicit region eu-west-1 + client scope eu-west-1 -> verify
   OK; explicit eu-west-1 + client us-east-1 -> fail with message
   containing "eu-west-1" (the expected region); default mode + client
   us-east-1 -> OK (today's behavior); default mode + client eu-west-1
   -> OK (permissive) with the notice logged.
2. Run (fail), rewire sigv4.go scope construction/compare to regionOf()
   + regionExplicit(), run (pass).

### 3. TDD: presigned verification

1. Same matrix for authenticatePresignedRequest (find the presigned
   test site; add to it).
2. Run (fail), rewire, run (pass).

### 4. GetBucketLocation + wiring

1. bucket_handlers.go: report the configured region as the location
   constraint (find the current literal; keep the response shape).
2. s3_wiring.go: `s3.SetRegion(serverConfig.Region)` next to the other
   config-driven Install calls (trace how serverConfig reaches that
   file first - read before editing).
3. `make test` green.

## Self-Verification Checklist

- [ ] `rg -c 'us-east-1' internal/frontend/s3/ --glob '*.go'` - only
      region.go's default constant and tests remain.
- [ ] make test && make lint NEW_FROM_REV=HEAD green.
- [ ] Both header and presigned paths honor regionOf().
- [ ] GetBucketLocation reports the configured region.

## Review Checklist (for review agent)

- [ ] No signature-relevant us-east-1 literal survives outside
      region.go/tests.
- [ ] Strict vs permissive semantics exactly as Contract 2 (tests prove
      all four matrix cells for both header and presigned).
- [ ] Failure message names the expected region (test asserts).
- [ ] The notice log line fires once per request in permissive mode, not
      per-retry spam (grep the implementation).
- [ ] Default-mode behavior unchanged for existing us-east-1 clients.

## Do NOT commit

The orchestrator stages and commits after review.
