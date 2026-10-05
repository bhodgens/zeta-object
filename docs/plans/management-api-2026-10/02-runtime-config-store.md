# Management API: Runtime Configuration Store - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit - the orchestrator
> handles all git operations after review. Do NOT use read_file on
> existing source files - explore with search_files or terminal cat.
> After writing a file, do NOT read it back to verify - write once and
> stop. After completing, report what you built, what files you touched,
> and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the in-memory authoritative configuration store, the
  validation path, the hot-apply table, and atomic persistence to the
  config file.
- **Dependencies:** none functionally, but it runs AFTER leaf 03: both
  edit `main.go` (and leaf 02 also edits `s3_wiring.go`), so they must
  not be in flight together.
- **Estimated Context:** 60K
- **Concurrency Group:** B

## Goal

A store that owns the live server configuration. Startup loads the file
into the store; the API reads and mutates the store; changes that have a
runtime apply path take effect immediately through the EXISTING seam
installers; everything else is reported as restart-required and never
reported as applied.

## Context

- `config.go:63-180` is `ServerConfig`. `loadConfig` is fail-loud
  (`DisallowUnknownFields`), and `reloadIdentityRegistry`
  (`config.go:574-587`) is the single existing runtime reload path.
- `s3_wiring.go:49-125` is the seam install point. It reads
  `serverConfig` global state directly today, so it cannot be re-run
  against a changed configuration until it takes the configuration as a
  parameter. That refactor is yours.
- `internal/backend/fsbackend/atomic.go:18` (`writeFileAtomic`) is the
  repo's atomic-write technique; package main has an equivalent helper
  from the pre-move storage layer. Reuse, do not reinvent.
- `main.go:60-181` is the startup sequence you must keep byte-identical
  in effect: load, validate, build the registry, install seams, mount
  frontends.

## Interface Contracts (From Parent)

### Contract 3 (restated, binding)

```go
// config_store.go (NEW, package main):
type ConfigStore struct{ /* RWMutex + live *ServerConfig + restart-required set */ }

func NewConfigStore(cfg *ServerConfig) *ConfigStore
func (s *ConfigStore) Snapshot() ServerConfig
func (s *ConfigStore) Apply(patch ConfigPatch) (applied []string, restartRequired []string, err error)
func (s *ConfigStore) Persist(path string) error
func (s *ConfigStore) RestartRequired() []string
```

- Hot-apply (through the existing installers): `identities`, `region`,
  `zfs_versioning`, `zfs_versioning_reflink_retention`,
  per-bucket `auditReads`, per-bucket `reflinkRetention`,
  `zfs_bucket_datasets`.
- Restart-required: `dataDir`, `listenAddr`, `certFile`, `keyFile`,
  `frontends`, `backends`, `auditLog`, `zmetad_db_path`,
  `zmetad_binary`.
- An invalid patch changes NOTHING and returns the validator's error.
- Secrets are masked on read; a masked value written back unchanged is
  a no-op.

## Tasks

### Task 1: make the seam installer take the configuration

**Objective:** seams can be re-installed from any configuration value.

**Files:**
- Modify: `s3_wiring.go` (`installS3Seams()` -> `installS3Seams(cfg *ServerConfig)`)
- Modify: `main.go:163` (the one call site)

**Step 1: Write a failing test** proving the installer applies the
CONFIGURATION PASSED IN, not the global: install with a config whose
`region` is `eu-central-1`, then assert the s3 seam's region is that
value.

**Step 2:** FAIL (signature). **Step 3: Implement** the parameter
threading; keep every existing install in the same order. **Step 4:**
PASS; whole root package green.

### Task 2: the store

**Objective:** validate, apply, and report.

**Files:**
- Create: `config_store.go`
- Test: `config_store_test.go`

**Step 1: Write failing tests** (table-driven):
- `Snapshot` is a deep copy: mutating the snapshot does not change the
  store (and vice versa), including the nested `Buckets` map;
- a patch touching a hot key is applied, reported in `applied`, and the
  effect is observable through the seam (assert via the same probe as
  Task 1);
- a patch touching a restart-required key is recorded, appears in
  BOTH `restartRequired` and `RestartRequired()`, and is NOT claimed as
  applied;
- an unknown JSON key in a patch is rejected and nothing changes;
- an invalid value (unknown `zfs_versioning`, unknown backend name) is
  rejected and nothing changes;
- a patch carrying a masked secret writes the mask back as a no-op: the
  live `secretKey` is unchanged;
- concurrent `Apply` and `Snapshot` are race-free (`-race`).

**Step 2:** `go test . -run TestConfigStore -count=1` -> FAIL.
**Step 3: Implement.** **Step 4:** PASS.

### Task 3: masked read

**Objective:** a configuration read never returns a secret.

**Files:** modify `config_store.go` + its test.

**Step 1: Write failing tests:** every `identities[].secretKey` and the
env-pair credential are masked in the snapshot handed to a caller; the
mask constant is pinned; the live values remain readable internally
(assert by re-installing seams after a masked round-trip).

**Step 2:** FAIL. **Step 3: Implement** an explicit `MaskedCopy()`
(never mutate the live struct). **Step 4:** PASS.

### Task 4: atomic persistence

**Objective:** `POST /config/save` writes the file safely.

**Files:** modify `config_store.go` + test.

**Step 1: Write failing tests:** `Persist` writes valid JSON that
`loadConfig` reads back to an equal configuration (round-trip); the file
is written via temp file plus rename (assert no partial file is visible
to a concurrent reader by checking the temp-then-rename call shape, or
by asserting the target inode changes); a write to an unwritable path
returns an error and leaves the previous file intact.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS.

### Task 5: wire the store into startup

**Objective:** one store instance owns the live configuration.

**Files:**
- Modify: `main.go` (create the store from the loaded config; install
  seams from the store's snapshot; keep the abort semantics unchanged)

**Step 1: Write a failing test** in the root package asserting that
after startup wiring the store exists and returns the loaded
configuration.

**Step 2:** FAIL. **Step 3: Implement.** **Step 4:** PASS; `make test`
green.

## Self-Verification Checklist

- [ ] All tasks implemented; `go test . -count=1` green (with `-race` on the store tests)
- [ ] No behaviour change when no patch is applied (startup path identical)
- [ ] Restart-required keys are never reported as applied
- [ ] Secrets never appear in a snapshot handed to a caller
- [ ] `gofmt -l` clean; `make lint NEW_FROM_REV=HEAD` 0 findings
- [ ] DO-NOT-TOUCH: `config.go` (read it; the loader and validation are
      reused as-is - do not edit), `internal/frontend/s3/*`, `frontends.go`,
      `main_server.go`, `scripts/zfs-validate/run-zfs-validation.sh`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] Contract 3 satisfied: method set, hot-apply set, restart-required set
- [ ] An invalid patch changes nothing (assert in tests, not just in prose)
- [ ] `Snapshot` deep-copies every map (Buckets, BucketBackends,
      BucketAuditReads, BucketReflinkRetention)
- [ ] Masking is applied on the way OUT only; the live config keeps real secrets
- [ ] Charter: no persistent server-owned state added (the store is memory; the file is the operator's)

Output: APPROVED or specific gaps with file:line.

## Notes

- The user chose the runtime-store model knowing the file becomes a
  snapshot. The honest reporting of restart-required keys is what makes
  that model safe; a reviewer should treat a missing restart-required
  report as a defect, not a nicety.
- `config.go` is deliberately NOT edited: reusing the existing loader
  and validation is what keeps "one place owns the defaults" true.
