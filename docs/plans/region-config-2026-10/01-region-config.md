# Region Config Key - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files - explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify - write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** ../master.md
- **Scope:** The `region` config key (default `us-east-1`) + the
  `regionOf()`/`SetRegion()` accessor in the s3 frontend package.
- **Dependencies:** none.
- **Estimated Context:** ~25K (explore 6K + generate 8K + iterate 5K + overhead 6K)
- **Concurrency Group:** A

## Goal

Create `internal/frontend/s3/region.go` with the frozen accessor
(Contract 1), wire the `region` config key through `config.go` with
default/normalization handling, and document the key in README's config
table.

## Context

Config lives in `config.go`: struct fields with json tags, defaults
applied in `defaultServerConfig()` and normalized in `loadConfig`
(search how `zmetad_db_path` does it - same pattern). README config table
is a markdown table around line 199. The s3 frontend package is
`internal/frontend/s3`. Note package-global wiring entries like
`s3.InstallMetadataProvider` exist in `s3_wiring.go` (repo root) - the
SetRegion call site will be added there by leaf 02; this leaf only
provides the accessor and config plumbing.

Key files:
- config.go - config struct + defaults + loadConfig normalization
- config_frontend_test.go - config-parse test patterns
- internal/frontend/s3/ - the frontend package

## Interface Contracts (From Parent)

Contract 1 verbatim (region.go):

```go
func regionOf() string
func SetRegion(r string)
```

Default `us-east-1`. SetRegion("") restores the default. Region values
are lowercased at config load (SigV4 regions are lowercase).

## Tasks

### 1. TDD: region accessor

1. Write `internal/frontend/s3/region_test.go`:
   - default without SetRegion is `us-east-1`
   - SetRegion("eu-west-1") then regionOf() == "eu-west-1"
   - SetRegion("") resets to `us-east-1`
   - SetRegion("EU-WEST-1") lowercases (regionOf() == "eu-west-1")
2. Run (fail), implement `region.go`, run (pass).
3. Use a package-level var guarded appropriately for the test's
   sequential use; document that SetRegion is startup-only.

### 2. TDD: config key

1. Extend the config test (config_frontend_test.go pattern):
   `region: "eu-west-1"` in config JSON -> parsed field
   `"eu-west-1"`; absent -> default `"us-east-1"`; `"EU-West-1"` ->
   normalized `"eu-west-1"` at load.
2. Add `Region string `json:"region"`` to the config struct; default
   and lowercase normalization at load (both defaultServerConfig and
   loadConfig, matching the zmetad_db_path pattern).

### 3. README

Add `region` to the config table (around line 199): default
`us-east-1`, links to the same metadata/S3 section as neighbors. One
row, matching table style.

### 4. Gate check

`make test` and `make lint NEW_FROM_REV=HEAD` green.

## Interface Contract (Exposed to Siblings)

Contract 1 accessors + the `Region` config field. Leaf 02 consumes all
three.

## Self-Verification Checklist

- [ ] `go test ./internal/frontend/s3/ -run Region -count=1` green.
- [ ] make test && make lint NEW_FROM_REV=HEAD green.
- [ ] Config default at load (not at provider init).
- [ ] README row matches table style.

## Review Checklist (for review agent)

- [ ] Contract 1 verbatim.
- [ ] Lowercase normalization happens at config load (config.go), AND
      SetRegion defensively lowercases (belt and suspenders).
- [ ] No changes outside config.go, config_frontend_test.go, region.go,
      region_test.go, README.md.

## Do NOT commit

The orchestrator stages and commits after review.
