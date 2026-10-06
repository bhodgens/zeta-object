# ZFS Bucket Datasets: Config Keys + Startup Validation - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you
> touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** Add `zfs_bucket_datasets` + `zfs_binary` config keys with
  fail-loud defaults and startup validation when the feature is on.
- **Dependencies:** none
- **Estimated Context:** 40K
- **Concurrency Group:** A

## Goal

The server config gains two keys: `zfs_bucket_datasets` (bool, default
false - CreateBucket makes a child ZFS dataset instead of a directory)
and `zfs_binary` (string, default `"zfs"` - PATH lookup, same
convention as `zmetad_binary`). When the feature is enabled, startup
aborts unless dataDir is on ZFS AND its dataset name resolves.
config.json.example documents both keys in the same commit.

## Context

zeta-object is a Go S3/WebDAV/FTP gateway; the filesystem is the source
of truth. Config lives in `config.go` (package main, repo root): a
`ServerConfig` struct with fail-loud JSON decoding
(DisallowUnknownFields), a defaults block near the top
(`defaultZmetadDBPath`, `defaultZfsVersioning`, etc.), and a
load/validate pass. The existing `zfs_versioning` key shows the exact
pattern to copy: constant default, struct field with frozen JSON tag,
default-fill in the loader, validation of the value set.
`metadata.DetectZFS(path)` (internal/metadata/fsdetect_*.go) is the
statfs ZFS probe, exported. Dataset resolution for the startup check
execs the zfs CLI - but the EXEC helper itself belongs to sibling leaf
02 (`internal/frontend/s3/zfsdatasets.go`); this leaf only needs the
startup-time check, which may call `exec.CommandContext` directly in
package main (one site, startup-only, argv-only) OR - preferred - a
small `resolveDatasetExists(ctx, binary, path) (string, error)`
helper in a NEW file `zfs_startup.go` (package main). Do NOT import
internal/frontend/s3 from package main for this; main already wires the
frontend through seams (see `s3_wiring.go`).

Key files to understand before implementing:
- `config.go` - ServerConfig struct, defaults block, load/validate pass.
- `config.json.example` - JSONC comments convention; every key documented.
- `config_backend_test.go` / `config_frontend_test.go` - config test style.
- `internal/metadata/fsdetect_linux.go` + `fsdetect_other.go` - DetectZFS
  signature (non-Linux returns false: startup validation must therefore
  also accept a test-injected resolver; see Task 3 seam).

## Interface Contracts (From Parent)

### What This Leaf Exposes

```go
// config.go (MODIFY) - ServerConfig fields (frozen JSON keys):
ZfsBucketDatasets bool   `json:"zfs_bucket_datasets"` // default false
ZfsBinary         string `json:"zfs_binary"`          // default "zfs"

// Defaults block (config.go, near defaultZfsVersioning):
// defaultZfsBinary = "zfs"

// zfs_startup.go (NEW, package main):
// validateZfsBucketDatasets(cfg *ServerConfig) error - when
// cfg.ZfsBucketDatasets is false: no-op nil. When true: abs(dataDir)
// must pass metadata.DetectZFS AND zfsDatasetForMount(ctx, cfg.ZfsBinary,
// absDataDir) must return a non-empty dataset name. Error text names
// the failed check and the path. Called from the config load/validate
// pass so startup ABORTS.
// zfsDatasetForMount(ctx, binary, path) (string, error) -
// exec.CommandContext(ctx, binary, "list", "-H", "-o", "name",
// "-t", "filesystem", path); 10s timeout; trims stdout; empty stdout
// with exit 0 = error "not a ZFS mountpoint".
// Test seam: package-level var zfsDatasetForMountFn = zfsDatasetForMount
// that validateZfsBucketDatasets calls (tests fake it; DetectZFS is
// also faked via a package-level var detectZFSFn = metadata.DetectZFS).
// When the feature is on, validation ALSO exec.LookPath's the
// configured binary: a missing zfs ABORTS startup (no lazy
// first-request 500s).
```

### What This Leaf Consumes

```go
// internal/metadata: func DetectZFS(path string) (bool, error)
```

## Tasks

### Task 1: Config fields + defaults + example docs

**Objective:** Decode both keys, fill defaults, document them.

**Files:**
- Modify: `config.go` (struct + defaults block + default-fill in loader)
- Modify: `config.json.example` (commented entries near `zfs_versioning`)
- Test: `config_backend_test.go` (or the closest existing config test file)

**Step 1: Write failing tests** - table-driven decode test: absent keys
-> `ZfsBucketDatasets == false`, `ZfsBinary == "zfs"`; explicit values
decode; unknown key inside the ROOT object still aborts
(DisallowUnknownFields unchanged).

**Step 2:** `go test . -run TestConfig -count=1` -> FAIL (unknown field
`zfs_bucket_datasets`).

**Step 3: Implement** - add the two fields with the exact JSON tags,
the `defaultZfsBinary = "zfs"` constant, and default-fill mirroring
how `ZmetadBinary` is filled. Add to config.json.example, in the same
comment style as `zfs_versioning`:

```
// When true, S3 CreateBucket under dataDir creates a child ZFS dataset
// (<dataDir dataset>/<bucket>) instead of a plain directory, and
// DeleteBucket destroys it. Startup aborts unless dataDir is a ZFS
// mountpoint. Requires the zfs CLI (and permission to create datasets)
// on the server host. Default false = plain directories everywhere.
// "zfs_bucket_datasets": true,
// Path or name of the zfs CLI binary (default "zfs" on PATH).
// "zfs_binary": "zfs",
```

**Step 4:** tests PASS; `gofmt -l .` clean on changed files.

### Task 2: zfs_startup.go resolver + validation

**Objective:** Fail-loud startup check when the feature is enabled.

**Files:**
- Create: `zfs_startup.go` (package main)
- Test: `zfs_startup_test.go`
- Modify: the config load/validate call site (find it with
  `grep -n "func loadConfig\|DisallowUnknownFields\|validate" config.go`)

**Step 1: Write failing tests** for `validateZfsBucketDatasets` using
the faked `detectZFSFn` / `zfsDatasetForMountFn` vars: feature off ->
nil, no fakes called; feature on + DetectZFS false -> error naming the
path; feature on + resolver error -> wrapped error; feature on + missing
binary (LookPath fails) -> error; feature on + both OK -> nil. Plus a parse test for `zfsDatasetForMount` output handling
via the fake (empty stdout + nil error = error).

**Step 2:** `go test . -run TestValidateZfsBucketDatasets -count=1` ->
FAIL (no such function).

**Step 3: Implement** - `zfsDatasetForMount` per Contract (argv-only,
`exec.CommandContext`, 10s timeout constant, stderr captured into the
error, trimmed single-line stdout; `#nosec G204` with the inline
justification "argv-only; binary is config-controlled, path is
abs(dataDir)"). `validateZfsBucketDatasets` per Contract. Hook the call
into the startup path where other config validation aborts (main or
loadConfig - match the existing abort style: log + exit non-zero).

**Step 4:** tests PASS.

### Task 3: Wire the resolved parent dataset for later leaves

**Objective:** Expose the startup-resolved dataset so leaf 03's
installer can pass it down without re-resolving.

**Files:**
- Modify: `zfs_startup.go` - `validateZfsBucketDatasets` returns
  `(parentDataset string, err error)` ("" when feature off); store in a
  package-main var `zfsBucketsParentDataset` set at startup.
- Test: extend `zfs_startup_test.go`.

**Step 1-4:** same TDD cycle; the package var is read by leaf 03's
wiring (it will pass it into the s3 frontend installer via
`installS3Seams`-style wiring in s3_wiring.go - leaf 03 owns that
edit, NOT this leaf; just export the var).

## Self-Verification Checklist

- [ ] All tasks implemented and tests passing (`go test . -count=1`)
- [ ] JSON tags EXACTLY `zfs_bucket_datasets` / `zfs_binary`
- [ ] config.json.example updated in the same change
- [ ] `gofmt -l .` clean; `golangci-lint run .` clean on changed files
- [ ] DO-NOT-TOUCH respected: no edits to
      `internal/frontend/s3/reflinkversions.go`,
      `internal/frontend/s3/versioning_handlers.go`,
      `scripts/zfs-validate/run-zfs-validation.sh`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Every Task implemented; every test present and passing
- [ ] Startup ABORTS (non-zero exit) when feature on + dataDir not ZFS
- [ ] No behavior change when feature off (default path untouched)
- [ ] argv-only exec, no shell string, timeout bounded

Output: APPROVED or specific gaps with file:line.

## Notes

- `metadata.DetectZFS` returns `(false, nil)` on non-Linux/BSD build
  targets (fsdetect_other.go) - the fake-var seam is what makes the
  validation testable on the macOS dev host.
- The 10s startup exec timeout is generous; zfs list on a mounted path
  is milliseconds. Keep it a named constant.
